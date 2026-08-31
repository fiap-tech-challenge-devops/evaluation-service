# Fase 3 — Achados do `golangci-lint` na esteira de CI

**Projeto:** ToggleMaster (FIAP — Fase 3)
**Escopo:** Seis correções em `evaluator.go` e `handlers.go` para o job `Linter`

---

## Visão geral

O job `Linter` da esteira de CI reprovava com seis achados: cinco do `errcheck` (retorno de erro descartado) e um do `staticcheck` (pacote obsoleto).

Nenhum era defeito grave, mas dois merecem atenção: a gravação no cache Redis e o fechamento do corpo das respostas HTTP. Este documento registra o diagnóstico e a validação.

---

## O que o linter acusou

Execução de referência: [run 33431572976](https://github.com/fiap-tech-challenge-devops/evaluation-service/actions/runs/33431572976), `golangci-lint 2.13.2`, sem `.golangci.yml` no repositório.

```
evaluator.go:8     SA1019: io/ioutil has been deprecated since Go 1.19 (staticcheck)
evaluator.go:60    Error return value of `(*redis.baseCmd).Err` is not checked (errcheck)
evaluator.go:114   Error return value of `resp.Body.Close` is not checked (errcheck)
evaluator.go:141   Error return value of `resp.Body.Close` is not checked (errcheck)
handlers.go:18     Error return value of `(*json.Encoder).Encode` is not checked (errcheck)
handlers.go:53     Error return value of `(*json.Encoder).Encode` is not checked (errcheck)
```

O job tem `continue-on-error: true`: aparece vermelho na interface, mas não reprova a esteira. Estilo de código não impede uma imagem de subir; vulnerabilidade impede.

---

## Desafio 1 — `io/ioutil` obsoleto

### O que estava errado

```go
import (
    "io/ioutil"
)
...
body, _ := ioutil.ReadAll(resp.Body)
```

O pacote `io/ioutil` está **deprecado desde o Go 1.19**. Desde o Go 1.16, `ioutil.ReadAll` é literalmente um alias que chama `io.ReadAll` — a implementação foi movida, e o pacote antigo permanece só por compatibilidade.

### Correção aplicada

```diff
-	"io/ioutil"
+	"io"
```

```diff
-	body, _ := ioutil.ReadAll(resp.Body)
+	body, _ := io.ReadAll(resp.Body)
```

Em dois lugares: `fetchFlag` e `fetchRule`.

**Zero mudança de comportamento** — mesma função, mesmo pacote de origem, só o caminho de import atualizado. É a correção mais segura do conjunto.

---

## Desafio 2 — `evaluator.go:60`, gravação no cache sem verificação

### O que estava errado

```go
jsonData, err := json.Marshal(info)
if err == nil {
    a.RedisClient.Set(ctx, cacheKey, jsonData, CACHE_TTL).Err()
}
```

A linha chama `.Err()` e **descarta o resultado**. Chamar `.Err()` explicitamente e jogar fora indica que alguém sabia que havia um erro ali e decidiu ignorá-lo — só que sem registrar a decisão em lugar nenhum.

### Por que importa, e por que não é grave

O cache é uma otimização. Se a gravação falhar, a próxima requisição da mesma flag simplesmente volta a buscar dos serviços — resultado idêntico, latência maior.

O problema é o **diagnóstico**: com o erro descartado, um Redis inacessível se manifesta como toda requisição sendo `Cache MISS`, sem nenhuma pista de por quê. O serviço fica lento e o log não diz nada.

### Correção aplicada

```diff
-		a.RedisClient.Set(ctx, cacheKey, jsonData, CACHE_TTL).Err()
+		if err := a.RedisClient.Set(ctx, cacheKey, jsonData, CACHE_TTL).Err(); err != nil {
+			log.Printf("Falha ao gravar cache da flag '%s': %v", flagName, err)
+		}
```

O comportamento é o mesmo: falha de cache continua não interrompendo a requisição. A diferença é que agora ela aparece no log, nomeando a flag afetada.

---

## Desafio 3 — `evaluator.go:114` e `:141`, fechamento do corpo da resposta

### O que estava errado

```go
defer resp.Body.Close()
```

### Análise

Forma idiomática em Go, presente em praticamente toda chamada HTTP. Diferente do caso equivalente no `auth-service` — onde o `defer` está em `main` e nunca executa —, **aqui ele executa a cada requisição**, porque `fetchFlag` e `fetchRule` retornam normalmente.

Ainda assim não há ação corretiva possível: se o `Close` falha, o corpo já foi lido e a resposta já está montada.

### Correção aplicada

```diff
-	defer resp.Body.Close()
+	defer func() {
+		if err := resp.Body.Close(); err != nil {
+			log.Printf("Erro ao fechar corpo da resposta: %v", err)
+		}
+	}()
```

O `defer` passa a receber uma função anônima porque `defer` aceita uma chamada, não uma instrução `if`.

A alternativa seria um `.golangci.yml` excluindo `errcheck` para `Close`. Não foi adotada: um arquivo de configuração criado para silenciar duas linhas custa mais manutenção do que a correção, e abre o precedente de resolver achado de lint desligando o linter.

---

## Desafio 4 — `handlers.go:18` e `:53`, escrita da resposta

### O que estava errado

```go
// healthHandler
json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

// evaluationHandler
json.NewEncoder(w).Encode(EvaluationResponse{ ... })
```

### Análise

Se o `Encode` falhar, o cliente caiu ou a conexão quebrou. Nada fica inconsistente: a avaliação da flag é uma leitura, e o evento para o SQS já foi disparado em goroutine antes da resposta.

É um caso mais brando que o equivalente no `auth-service`, onde o `Encode` entrega uma chave que o banco só guarda como hash — lá, falhar na escrita deixa estado órfão. Aqui, não há nada a reconciliar.

### Correção aplicada

```diff
-	json.NewEncoder(w).Encode(EvaluationResponse{
+	if err := json.NewEncoder(w).Encode(EvaluationResponse{
 		FlagName: flagName,
 		UserID:   userID,
 		Result:   result,
-	})
+	}); err != nil {
+		log.Printf("Erro ao escrever resposta da avaliacao da flag '%s': %v", flagName, err)
+	}
```

---

## Resumo das mudanças por arquivo

### `evaluator.go`

| Linha | O que mudou | Por quê |
|---|---|---|
| 8 | `io/ioutil` → `io` | `SA1019`; mesma função desde Go 1.16 |
| 60 | gravação no cache com checagem e log | `errcheck`; Redis indisponível ficava invisível no log |
| 114, 141 | `defer resp.Body.Close()` vira closure | `errcheck` |
| 126, 157 | `ioutil.ReadAll` → `io.ReadAll` | consequência da linha 8 |

### `handlers.go`

| Linha | O que mudou | Por quê |
|---|---|---|
| 18 | `Encode` com checagem e log | `errcheck` |
| 53 | `Encode` com checagem e log | `errcheck` |

Nenhum import novo — `log` já estava presente nos dois arquivos. Nenhuma dependência nova. **Nenhuma alteração de comportamento**: os mesmos status, os mesmos corpos de resposta, o mesmo tratamento de falha de cache.

---

## Validação

### Lint

```
golangci-lint v2.13.2 → 0 issues.
```

### Build

```
docker build → ok, imagem de 40.3 MB
```

O build executa `go build` dentro do `golang:1.21-alpine` — é a compilação.

### Execução, cadeia completa

O `evaluation-service` não roda isolado: ele consulta o `flag-service` e o `targeting-service`, autenticando no `auth-service`, e usa Redis como cache. Todos foram subidos em containers para exercitar os caminhos alterados.

| passo | resultado |
|---|---|
| `GET /health` | `200` `{"status":"ok"}` — exercita `handlers.go:18` |
| Criar flag no `flag-service` | `201` |
| Criar regra no `targeting-service` | `201` |
| `GET /evaluate` para quatro usuários | `200`, resultados `true`/`false`/`false`/`true` |
| Repetir a chamada | `200`, resposta idêntica |

Os logs confirmam os dois caminhos do cache:

```
Cache MISS para flag 'enable-new-dashboard'
Cache HIT para flag 'enable-new-dashboard'
Cache HIT para flag 'enable-new-dashboard'
```

O `MISS` da primeira chamada executa `fetchFlag` e `fetchRule` — exatamente o código alterado: `io.ReadAll`, o `defer` do `Body.Close` e a gravação no Redis. O `HIT` seguinte prova que a gravação funcionou; se o `Set` tivesse falhado, toda chamada seria `MISS`.

Nenhuma das novas mensagens de erro apareceu no log, o que é o esperado num cenário saudável.

A regra `PERCENTAGE 50` distribuiu os quatro usuários em `true`/`false`/`false`/`true` — a lógica de bucketing por hash segue intacta.

---

## O que não foi feito

**`.golangci.yml` com exclusões.** Discutido nos desafios 3 e 4: silencia sem escrever código, mas cria um arquivo de configuração para poupar poucas linhas e habitua o projeto a desligar o linter.

**Fazer o `Linter` bloquear a esteira.** O `continue-on-error` vive no `go-ci` do [`reusable-workflows`](https://github.com/fiap-tech-challenge-devops/reusable-workflows) e vale para os dois serviços em Go. A decisão está registrada lá.

**Tratamento de erro no `ReadAll`.** As linhas `body, _ := io.ReadAll(resp.Body)` continuam descartando o erro — o `errcheck` não as acusa porque o descarte é explícito com `_`. Corrigir mudaria o fluxo de erro das duas funções, o que está além do que o lint pediu.
