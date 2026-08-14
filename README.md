# ZapperMeow

API RESTful **open source e self-hosted** para WhatsApp, multi-tenant, construída em Go sobre a biblioteca [HyperMeow](https://github.com/polymorfa/hypermeow) (fork production-focused do whatsmeow).

## O que é

Uma plataforma que você roda na sua própria infraestrutura para expor o WhatsApp como uma API REST completa:

- **Multi-tenant com isolamento por instância:** o operador da plataforma cria tenants; cada tenant é um admin que gerencia suas próprias instâncias. Cada instância é um dispositivo vinculado a um número — como o WhatsApp é multi-dispositivo, várias instâncias podem apontar para o mesmo número e ficar conectadas ao mesmo tempo. Cada uma tem suas próprias API keys e webhooks; credenciais de uma instância nunca afetam as demais.
- **API completa:** mensagens (texto, mídia, enquetes, botões/listas/flows), grupos e comunidades, contatos, chats, newsletters/canais, recursos de WhatsApp Business (catálogo, produtos, pedidos), privacidade — a superfície inteira do HyperMeow exposta via REST com OpenAPI 3.1.
- **Eventos em tempo real:** webhooks HTTP assinados (HMAC, retry com DLQ) e WebSocket por instância, cobrindo ~75 tipos de evento.
- **Feita para produção:** sessões stateful com posse exclusiva garantida por lease em Postgres (failover automático), API stateless com escala horizontal, deploy via Docker Swarm.

## Stack (resumo)

Go 1.25+ · chi + huma (OpenAPI 3.1) · PostgreSQL 17 (pgx + sqlc) · Redis (asynq, cache, rate limit) · MinIO · gRPC entre serviços · Docker Swarm + Traefik. Detalhes e justificativas em [TECH_STACK.md](TECH_STACK.md).

## Ambiente de desenvolvimento

### Pré-requisitos

- **Go 1.25+**
- **Docker** — sobe o Postgres e o Redis locais, e é o que a suíte de testes usa (testcontainers)

Opcionais, só para tarefas específicas:

- [`sqlc`](https://docs.sqlc.dev) 1.31.1 — apenas se você for **alterar queries SQL**
- [`golangci-lint`](https://golangci-lint.run) v2.12.2 — o mesmo do CI, para rodar o lint antes do push

### 1. Infraestrutura local

```bash
docker run -d --name zm-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=zappermeow -p 5432:5432 postgres:17-alpine
docker run -d --name zm-redis -p 6379:6379 redis:7-alpine
```

### 2. Configuração

Toda a configuração é 12-factor. Em produção os valores sensíveis vêm de `/run/secrets`; em
desenvolvimento, variáveis de ambiente bastam.

```bash
export ZAPPERMEOW_DATABASE_URL="postgres://postgres:dev@localhost:5432/zappermeow?sslmode=disable"
export ZAPPERMEOW_REDIS_ADDR="localhost:6379"
export ZAPPERMEOW_JWT_SIGNING_KEY="$(openssl rand -hex 64)"   # mínimo de 64 caracteres

# Credencial inicial do super-admin, criada no primeiro boot.
export ZAPPERMEOW_BOOTSTRAP_EMAIL="root@example.com"
export ZAPPERMEOW_BOOTSTRAP_PASSWORD="bootstrap-secret-1"

# Aponta para longe do /run/secrets real, para que a máquina não influencie o ambiente local.
export ZAPPERMEOW_SECRETS_DIR="/tmp/zappermeow-secrets"
```

### 3. Subir os dois planos

O binário é um só, com um subcomando por papel. **Comece pela API**: é ela que aplica as migrações
e cria o super-admin. O worker assume que o schema já existe.

```bash
go run ./cmd/zappermeow serve            # plano stateless: REST + WebSocket, porta 8080
go run ./cmd/zappermeow session-worker   # plano stateful: sessões WhatsApp, gRPC na porta 9090
```

O worker precisa das mesmas variáveis da API (banco e Redis) e aceita as suas próprias, todas com
default utilizável: `ZAPPERMEOW_MAX_SESSIONS_PER_WORKER` (200), `ZAPPERMEOW_PAIRING_WINDOW` (180s),
`ZAPPERMEOW_LEASE_EXPIRY` (30s), `ZAPPERMEOW_RECONCILE_INTERVAL` (15s).

A api tem uma que vale conhecer: `ZAPPERMEOW_CLAIM_WAIT` (3s) é quanto um
`connect` espera por um worker assumir a sessão antes de reportar falta de
capacidade. Comandos de conexão precisam ser **entregues** a um worker —
adotar um lease não inicia pareamento sozinho.

Para simular uma frota, rode um segundo worker mudando a porta — é assim que se observa o failover
de sessão:

```bash
ZAPPERMEOW_WORKER_GRPC_LISTEN_ADDR=":9091" go run ./cmd/zappermeow session-worker
```

### 4. Conferir que subiu

```bash
curl -s localhost:8080/healthz | jq       # envelope padrão com status 200
curl -s localhost:8080/metrics | grep zappermeow_
open http://localhost:8080/docs           # UI de documentação, gerada do código
```

A spec OpenAPI 3.1 fica em `/openapi.json`. Ela é **gerada dos handlers**, nunca escrita à mão: se
divergir do código, é bug.

O roteiro completo de validação da conexão — pareamento por QR, failover, logout — está em
[specs/002-instance-connection/quickstart.md](specs/002-instance-connection/quickstart.md).

### Parear por QR sem sofrer com o relógio

O QR se renova a cada 20s, o que torna o pareamento por linha de comando uma
corrida contra o tempo. `tools/pairing-qr/` é uma página que abre o canal de
eventos e redesenha cada código conforme ele chega:

```bash
(cd tools/pairing-qr && python3 -m http.server 8090)
# abra http://localhost:8090 e informe o id da instância e a X-Api-Key
```

O código é convertido em QR **dentro do navegador** — quem tem esse código
consegue vincular um dispositivo ao número do cliente, então ele não vai para
nenhum serviço externo de imagem. O encoder está vendorizado em
`tools/pairing-qr/qrcode.js` (MIT), e a página funciona offline.

Um navegador só alcança a API a partir de uma origem que o deploy autorize —
tanto o canal de eventos quanto as rotas REST. Para usar a página, declare a
origem dela:

```bash
export ZAPPERMEOW_ALLOWED_ORIGINS="localhost:8090"
```

Sem essa variável a API só aceita uma página servida por ela mesma, que é o
default certo para um deploy servidor-a-servidor. Se o navegador bloquear a
chamada, a página continua ouvindo o canal e mostra o `curl` equivalente.

### Testes

```bash
go test ./...                                        # unit + integração
ZAPPERMEOW_REQUIRE_DOCKER=1 go test ./... -count=1   # falha se o Docker não estiver disponível
```

Os testes de integração sobem Postgres e Redis **reais** via testcontainers — a constituição proíbe
mockar infraestrutura, porque SQL, locking distribuído e filas são justamente onde mocks escondem
bugs. Sem Docker eles são pulados; a variável acima transforma isso em erro, que é como o CI roda.

A fronteira com o WhatsApp é a única exceção: ela é substituída por uma sessão roteirizada, já que
o serviço não tem sandbox e automatizá-lo arriscaria banir o número.

### Lint

```bash
golangci-lint run ./...
```

Pipeline verde é pré-condição de merge: lint → testes → build da imagem.

### Código gerado

Duas etapas são geradas e **versionadas** no repositório; o CI reprova o merge se a saída estiver
defasada em relação à fonte.

```bash
sqlc generate            # internal/store/ a partir de internal/store/queries/*.sql
go generate ./proto/...  # internal/pb/ a partir de proto/**/*.proto
```

O compilador de protobuf (`buf`) e os plugins (`protoc-gen-go`, `protoc-gen-go-grpc`) vêm fixados
como `tool` directives no `go.mod`, então **não há nada a instalar** além do Go — nem `protoc`, nem
imagem Docker. O `sqlc`, por ser binário externo, tem a versão fixada: um upgrade silencioso mudaria
a saída gerada.

## Licença e modelo do projeto

Código sob licença [MIT](LICENSE) — use, modifique e hospede livremente, inclusive comercialmente.

## Aviso legal

Este projeto não é afiliado, associado, autorizado ou endossado pelo WhatsApp/Meta. O uso está sujeito aos [Termos de Serviço do WhatsApp](https://www.whatsapp.com/legal/terms-of-service); o uso indevido (spam, automação abusiva) pode resultar no banimento dos números. Use por sua conta e risco.
