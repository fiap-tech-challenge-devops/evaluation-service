# Containerização — evaluation-service

**Projeto:** ToggleMaster (FIAP — Fase 2) · **Data:** Maio/2026

Duas alterações em `evaluator.go` e uma no Dockerfile para o build completar.

## Desafio 1 — go.sum incompleto impede compilação

**Problema:** O arquivo `go.sum` commitado não continha todas as entradas de hash necessárias para os pacotes da `aws-sdk-go`, `go-redis` e `godotenv`. O `go build` falha quando o `go.sum` não cobre todos os módulos importados.

**Erro:**
```
main.go:10:2: missing go.sum entry for module providing package github.com/aws/aws-sdk-go/aws (imported by evaluation-service); to add:
    go get evaluation-service
main.go:13:2: missing go.sum entry for module providing package github.com/go-redis/redis/v8 (imported by evaluation-service); to add:
    go get evaluation-service
```

**Correção:** adaptado o Dockerfile para não copiar `go.sum` e rodar `go mod tidy` antes de `go build` (mesmo padrão do `auth-service`).

```diff
-COPY go.mod go.sum ./
-RUN go mod download
-COPY . .
-RUN CGO_ENABLED=0 GOOS=linux go build -o evaluation-service .
+COPY go.mod ./
+RUN go mod download
+COPY . .
+RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -o evaluation-service .
```

---

## Desafio 2 — Import `"context"` não usado e `"os"` não importado em `evaluator.go`

**Problema:** `evaluator.go` declarava `"context"` no bloco de imports mas não o usava diretamente (o `ctx` global é declarado em `main.go`). Ao mesmo tempo, as funções `fetchFlag` e `fetchRule` chamavam `os.Getenv` sem que `"os"` estivesse importado.

**Erro:**
```
./evaluator.go:4:2: "context" imported and not used
./evaluator.go:106:12: undefined: os
./evaluator.go:133:12: undefined: os
```

**Correção:** em `evaluator.go`, removido `"context"` e adicionado `"os"` no bloco de imports.

```diff
 import (
-    "context"
     "crypto/sha1"
     "encoding/binary"
     "encoding/json"
     "fmt"
     "io/ioutil"
     "log"
     "net/http"
+    "os"
     "sync"
     "time"
 )
```
