# Fase 4 — Instrumentação com OpenTelemetry

**Projeto:** ToggleMaster (FIAP — Fase 4)
**Escopo:** `otel.go` (novo), `main.go`, `handlers.go`, `evaluator.go`, `sqs.go`, `go.mod` e `go.sum`

---

## Visão geral

Último serviço instrumentado da rodada, e o mais completo. O `evaluation-service` é o orquestrador do fluxo de leitura: consulta cache no Redis, chama dois serviços **em paralelo**, e publica um evento numa fila.

Isso faz dele o lugar onde a propagação de contexto deixa de ser conceito e vira trabalho. No `auth-service`, o refactor foram duas linhas. Aqui, o `context` atravessa cinco funções, duas goroutines e uma fronteira assíncrona.

O resultado é o trace mais rico do projeto: **quatro serviços numa requisição**.

---

## A propagação de contexto, de ponta a ponta

O `traceparent` chega no header, o `otelhttp` o coloca no `context`, e a partir daí **tudo depende de alguém carregar esse context adiante**.

| Arquivo | Mudança |
|---|---|
| `handlers.go` | `getDecision(r.Context(), …)` |
| `evaluator.go` | `ctx` como primeiro parâmetro de `getDecision`, `getCombinedFlagInfo`, `fetchFromServices`, `fetchFlag`, `fetchRule` e `runEvaluationLogic` |
| `evaluator.go` | `http.NewRequest` → `http.NewRequestWithContext` nas duas chamadas |
| `main.go` | Removido o `var ctx = context.Background()` global |
| `sqs.go` | `SendMessage` → `SendMessageWithContext` |

### O contexto global que precisava morrer

```diff
-// Contexto global para o Redis
-var ctx = context.Background()
```

As operações de Redis usavam esse contexto compartilhado. Funcionava — o `go-redis` só precisa de um `context.Context` qualquer — mas significava que **nenhuma operação de cache jamais apareceria dentro do trace da requisição**. Elas nasceriam soltas.

### As duas goroutines paralelas

```go
go func() {
    defer wg.Done()
    flagInfo, flagErr = a.fetchFlag(ctx, flagName)
}()
```

O `ctx` é capturado pelo closure e entra na goroutine. É isso que faz os dois spans de cliente aparecerem como **irmãos sob a mesma raiz**, em vez de dois traces órfãos.

### A goroutine que sobrevive à resposta

```go
eventCtx := context.WithoutCancel(ctx)
go a.sendEvaluationEvent(eventCtx, userID, flagName, result)
```

Este é o caso sutil. O evento é publicado **depois** de a resposta ser enviada, e o contexto da requisição é cancelado nesse momento. Passar `ctx` direto faria o envio ser abortado em plena execução.

O `context.WithoutCancel` preserva o trace e descarta o cancelamento — exatamente o que se quer num trabalho que continua após a resposta.

---

## Desafio 1 — o `redisotel` e duas paredes de versão

O serviço usava `github.com/go-redis/redis/v8`. Instrumentá-lo tinha três caminhos, e dois estavam bloqueados:

| Caminho | Situação |
|---|---|
| `redisotel/v8 v8.11.5` | Última versão. Congelada na API do OTel **v1.5.0**, de 2022, e sem manutenção desde o fim de vida do v8 |
| `redisotel/v9 v9.23.0` | Atual, mas exige **Go 1.26** — mesma parede do OTel 1.47 |
| `redisotel/v9 v9.22.0` | Exige Go 1.25 e OTel 1.43 |

### Decisão: subir para o `go-redis` v9 na v9.22.0

A migração do v8 para o v9 estava registrada como pendência em [`04-cve-x-net.md`](04-cve-x-net.md), adiada por ser "troca de versão maior, com mudança de API, e sem relação com os CVEs". Agora havia motivo.

Na prática, para o uso deste serviço — `ParseURL`, `NewClient`, `Get`, `Set`, `Ping` —, **a mudança foi o caminho de import**. As assinaturas são as mesmas.

```go
rdb := redis.NewClient(opt)
if err := redisotel.InstrumentTracing(rdb); err != nil { … }
```

E ficar na linha `9.22` mantém a coerência com a decisão tomada no `auth-service`: não subir o toolchain por uma versão menor, sem ganho funcional.

---

## Desafio 2 — o SQS, onde não existe instrumentação pronta

Este serviço usa **AWS SDK for Go v1**, e o `otelaws` oficial só cobre o v2. O contexto vai à mão:

```go
ctx, span := otel.Tracer(serviceName).Start(ctx, "send "+queueName,
    trace.WithSpanKind(trace.SpanKindProducer),
    trace.WithAttributes(
        attribute.String("messaging.system", "aws_sqs"),
        attribute.String("messaging.operation.name", "send"),
        attribute.String("messaging.destination.name", queueName),
    ),
)
defer span.End()

carrier := propagation.MapCarrier{}
otel.GetTextMapPropagator().Inject(ctx, carrier)

messageAttributes := make(map[string]*sqs.MessageAttributeValue, len(carrier))
for key, value := range carrier {
    messageAttributes[key] = &sqs.MessageAttributeValue{
        DataType:    aws.String("String"),
        StringValue: aws.String(value),
    }
}
```

O `traceparent` viaja como atributo da mensagem. Do outro lado, o `analytics-service` o extrai e **liga por span link** — não por parent-child, porque um poll traz até dez mensagens de traces diferentes e um span tem um pai só.

Os atributos seguem a convenção de mensageria: `messaging.system` e `messaging.operation.name` são obrigatórios.

---

## O resto do padrão

Igual ao `auth-service`: `otel.go` com os três providers e o propagador, `otelhttp` embrulhando o mux com nome por rota e filtro do `/health`, `http.Server` com `SIGTERM` para o `BatchSpanProcessor` conseguir dar flush, e os pontos de `log` migrados para `slog` com as variantes de contexto.

O `http.Client` existente ganhou o transport instrumentado:

```go
httpClient := &http.Client{
    Timeout:   5 * time.Second,
    Transport: otelhttp.NewTransport(http.DefaultTransport),
}
```

É isso que injeta o `traceparent` nas chamadas ao `flag-service` e ao `targeting-service`.

---

## Validação

Ambiente local, com Collector `0.162.0` e exporter `debug`.

### Cache MISS — quatro serviços, 19 spans

```
evaluation-service  GET /evaluate                          Server  span=bd2de773  (raiz)
├── get                                                    Client  → Redis (vazio)
├── HTTP GET                                               Client  span=267852f7 ─┐ paralelas
│   └── flag-service       GET /flags/<string:name>        Server  parent=267852f7
│       ├── GET → auth-service → GET /validate             Server  + 4 spans de SQL
│       └── SELECT                                         Client
├── HTTP GET                                               Client  span=c2c1a2f2 ─┘
│   └── targeting-service  GET /rules/<string:flag_name>   Server  parent=c2c1a2f2
│       ├── GET → auth-service → GET /validate             Server  + 3 spans de SQL
│       └── SELECT                                         Client
└── set                                                    Client  → Redis (grava)
```

As duas chamadas paralelas aparecem como irmãs sob a mesma raiz, cada uma levando o contexto para dentro da sua goroutine. O `auth-service` aparece duas vezes porque tanto o `flag` quanto o `targeting` validam a chave.

### Cache HIT — dois spans

```
evaluation-service  GET /evaluate   Server
└── get             Client          → Redis
```

**De 19 spans para 2.** O que antes era um `log.Printf("Cache HIT")` virou a diferença visível entre uma requisição que consultou quatro serviços e uma que resolveu em memória — e, com ela, a diferença de latência.

### Verificações

| Verificação | Resultado |
|---|---|
| `go build ./...` | ok |
| `go vet ./...` | ok |
| `golangci-lint` v2.14.0, a mesma da esteira | `0 issues.` |
| Resposta da API | inalterada |

---

## O que não foi validado localmente

**A cadeia do SQS.** O `.env` local não tem credenciais da AWS nem URL de fila — o serviço loga `[SQS_DISABLED]` e segue. Do outro lado, o `analytics-service` nem sobe sem elas.

Fecha no cluster, onde as credenciais vêm do IRSA. O que se espera: o span `send <fila>` com kind `Producer` aqui, e o `process <fila>` com kind `Consumer` lá, ligados por link.

---

## O que não foi feito

**Métricas de negócio.** O cache hit/miss agora é visível pela diferença de spans, mas um contador explícito permitiria alertar sobre queda na taxa de acerto.

**Migrar o SDK da AWS para o v2**, o que traria o `otelaws` e tornaria a injeção manual desnecessária. É mudança de API maior e fora do caminho crítico.

**Enxugar os spans do Redis.** O `redisotel` é mais econômico que o `otelsql`, mas ainda assim gera um span por comando.
