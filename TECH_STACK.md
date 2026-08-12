# Tech Stack

Stack tecnológica da **ZapperMeow** — API RESTful multi-tenant sobre a biblioteca [HyperMeow](https://github.com/polymorfa/hypermeow) (ver [features.md](features.md)).

**Princípios que guiaram as escolhas:** multi-tenancy segura, estabilidade, performance e leveza operacional. Preferência por stdlib e bibliotecas pequenas sem lock-in; SQL explícito em vez de ORM; uma única instância de Redis e Postgres servindo múltiplos papéis antes de adicionar novas peças de infra.

## Visão geral

| Camada | Escolha |
| --- | --- |
| Linguagem | Go 1.25+ |
| Core WhatsApp | `github.com/polymorfa/hypermeow` (`@main`, pseudo-versão pinada) |
| HTTP router | chi |
| Framework de API / OpenAPI | huma (sobre chi) |
| Banco de dados | PostgreSQL 17 |
| Acesso a dados | pgx v5 + sqlc |
| Migrações | golang-migrate |
| Fila / jobs assíncronos | Redis + asynq |
| Cache e rate limiting | Redis + redis_rate |
| Autenticação | API Key por instância + JWT (admin da plataforma e do tenant) |
| Armazenamento de mídia | MinIO (S3-compatible) |
| Entrega de eventos | Webhooks HTTP (HMAC) + WebSocket |
| Configuração | Env vars (caarlos0/env) + secrets do runtime de deploy |
| Logs / métricas / traces | log/slog + Prometheus + OpenTelemetry |
| Testes | testify + testcontainers-go |
| CI/CD | GitHub Actions (golangci-lint, testes, build de imagem) |
| Orquestração / deploy | Docker Swarm (`stack.yml`) ou Docker Compose (`docker-compose.yml`) |
| Proxy / TLS | Traefik |
| Secrets | Swarm Secrets (Swarm) / file-based secrets (Compose) |

## Backend

### Go 1.25+ e HyperMeow
Backend em Go — mesma linguagem da biblioteca core, integração direta sem bindings. O HyperMeow é instalado com `go get github.com/polymorfa/hypermeow@main` e a pseudo-versão resultante fica pinada no `go.mod` (as versões semânticas do projeto foram retraídas). O pacote raiz importa como `whatsmeow`.

### chi (router HTTP)
Router minimalista 100% compatível com `net/http`, zero dependências. Todo o ecossistema de middlewares padrão funciona sem adaptação, e não há framework proprietário entre a aplicação e a stdlib — o que favorece estabilidade de longo prazo.

### huma (camada de API + OpenAPI)
Montado sobre o chi. Handlers Go tipados geram automaticamente a spec **OpenAPI 3.1** com validação de request embutida — a documentação nunca dessincroniza do código. Serve a spec e o UI de documentação na própria API.

## Padrões da API

Como a API RESTful é o "frontend" do produto, o formato das respostas é contrato rígido — nenhum handler monta resposta de sucesso ou de erro manualmente.

### Envelope de resposta

Toda resposta JSON de **sucesso** com corpo usa envelope padrão, com campos alinhados à semântica da RFC 9457: `status` carrega o **código HTTP numérico** da resposta (mesma semântica do membro `status` dos problem details), nunca strings como `"success"`/`"error"`:

```json
{
  "status": 200,
  "data": {},
  "timestamp": "2026-08-12T22:00:00.000Z"
}
```

**Erros** seguem a **RFC 9457** (*Problem Details for HTTP APIs*, `application/problem+json`) — formato nativo do huma —, estendida com o membro `code`:

```json
{
  "type": "https://zappermeow.dev/errors/validation",
  "title": "Unprocessable Entity",
  "status": 422,
  "detail": "Request validation failed",
  "code": "VALIDATION_ERROR",
  "errors": [
    { "message": "expected length >= 8", "location": "body.password" }
  ],
  "timestamp": "2026-08-12T22:00:00.000Z"
}
```

Regras:

- Clientes distinguem sucesso e erro pelo status HTTP e pelo `Content-Type` (`application/json` vs `application/problem+json`); o membro `status` espelha o código HTTP nos dois formatos, como define a RFC 9457.
- `code` é **estável e único** por tipo de erro — contrato para clientes tratarem erros programaticamente. O catálogo de códigos vive no contrato de cada feature.
- `title`, `detail` e `message` são descrições legíveis em **inglês**, voltadas ao desenvolvedor; consumidores traduzem a partir do `code`.
- `errors[]` (extensão nativa do huma) carrega os detalhes por campo em erros de validação, com `location` apontando o campo (`body.name`). O membro `value` do huma é suprimido em campos sensíveis — respostas de erro **nunca** ecoam senhas ou segredos.
- Status HTTP adequado a cada erro (`400`, `401`, `403`, `404`, `409`, `422`, `429`, `5xx`); o mapeamento de erros de domínio → problem details é centralizado (pacote `httperr`), mantendo o domínio desacoplado do transporte.
- No huma: o envelope de sucesso entra como struct genérica nos outputs tipados (aparece fielmente na spec OpenAPI gerada); os erros customizam `huma.NewError` para adicionar `code` e `timestamp` ao modelo padrão.
- Fora do envelope apenas: respostas sem corpo (`204 No Content`) e formatos não-JSON (`/metrics` em texto Prometheus).

## Dados

### PostgreSQL 17
Banco único para dois papéis:
1. **Tabelas do HyperMeow** (sessões, chaves Signal, contatos) — gerenciadas pela própria biblioteca via `store/sqlstore`, que é otimizada para Postgres (operações em lote, índices de alias).
2. **Tabelas da API** (tenants, instâncias — N por tenant —, API keys e webhooks — por instância —, filas de campanha) — gerenciadas por nós.

### pgx v5 + sqlc
`pgx` é o driver Postgres mais rápido do ecossistema e é o mesmo que o HyperMeow usa — um único pool de conexões compartilhado entre a API e a biblioteca. `sqlc` gera código Go type-safe a partir de SQL puro: performance de SQL cru, verificação em tempo de compilação, zero reflection em runtime.

### golang-migrate
Migrações em SQL puro (`up`/`down`) embutidas no binário via `embed.FS` e aplicadas no boot. Cobrem **apenas as tabelas da API** — as tabelas do HyperMeow são migradas pela própria biblioteca.

## Processamento assíncrono

### Redis + asynq
Task queue sobre Redis para tudo que não pode bloquear o request: entrega de webhooks com retry exponencial e DLQ, campanhas/broadcast, processamento de mídia. Filas com prioridades permitem isolar tenants e classes de trabalho. Dashboard de inspeção via asynqmon.

O mesmo Redis serve cache de leitura e rate limiting — uma única peça de infra, três papéis.

## Segurança

### Autenticação: API Key + JWT
- **Rotas operacionais** (mensagens, grupos, mídia): API key **por instância**, transmitida em header, armazenada como hash no Postgres. Revogável instantaneamente; a API valida que a key pertence à instância da URL. Cada número tem suas próprias keys — vazamento ou rotação de uma não afeta os demais.
- **Rotas administrativas**: JWT de curta duração em dois audiences — **plataforma** (super-admin: gestão de tenants e seus limites) e **tenant** (`tenant_id` nas claims: gestão das próprias instâncias, keys e webhooks). Toda rota de tenant valida `instance.tenant_id == jwt.tenant_id`.
- Padrão consolidado no mercado (Twilio, Evolution API, WAHA).

### Rate limiting: redis_rate
Algoritmo GCRA no Redis via middleware chi, com limites por API key (= por instância), configuráveis por tenant. Por ser distribuído, funciona corretamente com múltiplas réplicas — um tenant descontrolado não degrada os demais, e uma instância descontrolada não consome a cota das outras.

### Webhooks assinados
Payloads de webhook assinados com HMAC-SHA256 (segredo por webhook de instância) para o consumidor validar a origem.

### Secrets (Swarm e Compose)
Credenciais de banco, chaves HMAC e signing keys de JWT via secrets do runtime de deploy. No Swarm, Docker Swarm Secrets — criptografados no Raft, invisíveis em `docker inspect`. No Compose, file-based secrets (`secrets:` apontando para arquivos locais). Em ambos os casos os secrets são montados em `/run/secrets`, então a camada de config é idêntica nos dois alvos: lê os arquivos de secret com fallback para env vars em desenvolvimento.

## Mídia

### MinIO
Object storage S3-compatible rodando como serviço na própria stack (Swarm ou Compose). Mídias recebidas/enviadas são persistidas com chave prefixada por tenant/instância, servidas por URLs pré-assinadas e expiradas por lifecycle policy. Se a operação migrar para nuvem, a troca por S3/R2 é transparente (mesma API).

## Eventos para os tenants

Dois canais complementares, alimentados pelos ~75 tipos de eventos do HyperMeow:

1. **Webhooks HTTP** — canal principal. POST assinado (HMAC) na URL configurada **por instância** (cada uma com sua URL, filtro de tipos de evento e segredo próprios), com retries exponenciais via asynq e dead-letter queue. Instâncias diferentes do mesmo tenant podem apontar para consumidores diferentes.
2. **WebSocket** — canal em tempo real por instância (`/instances/{id}/ws`), útil para o fluxo de QR code no pareamento e para clientes atrás de NAT que não podem expor URL pública.

## Configuração

**12-factor**: toda configuração via variáveis de ambiente, parseadas em struct tipada com `caarlos0/env` (biblioteca mínima, sem árvore de dependências). `.env` apenas em desenvolvimento local; em produção, valores sensíveis vêm de secrets do runtime de deploy (Swarm Secrets ou file-based secrets do Compose), montados em `/run/secrets`.

## Observabilidade

- **Logs:** `log/slog` (stdlib) em JSON estruturado, com `tenant_id`/`instance_id` como atributos em todo log de request.
- **Métricas:** endpoint `/metrics` Prometheus — latência por rota, profundidade das filas asynq, sessões conectadas, taxa de entrega de webhooks.
- **Traces:** OpenTelemetry com exporter OTLP configurável (desligado por padrão; liga via env var quando houver coletor).

Coletores (Grafana, Jaeger, etc.) ficam fora do escopo da API — ela apenas expõe os dados em formato padrão.

## Qualidade

- **Testes unitários:** `go test` + testify (table-driven).
- **Testes de integração:** testcontainers-go sobe **Postgres e Redis reais** em Docker — as queries sqlc e os handlers são testados contra infraestrutura real, não mocks.
- **Lint:** golangci-lint.
- **CI (GitHub Actions):** lint → testes (com services de Postgres/Redis) → build multi-stage da imagem → push no registry.

## Runtime e deploy

### Docker Swarm e Docker Compose
A entrega inicial suporta **dois alvos de deploy** com paridade funcional — mesma imagem, mesmos serviços (api, session-worker, jobs, postgres, redis, minio, traefik):

- **Docker Swarm** (`deploy/stack.yml`): operação multi-node — `docker stack deploy` com overlay networks, secrets nativos e rolling updates.
- **Docker Compose** (`deploy/docker-compose.yml`): host único — `docker compose up -d`, para usuários que precisam de menos infra e para facilitar a avaliação e distribuição do projeto.

**Implicação arquitetural importante — sessões são stateful:** cada instância WhatsApp mantém um WebSocket persistente com estado criptográfico; uma sessão deve estar ativa em **exatamente um processo** por vez. Em qualquer dos alvos isso significa:

- Separar o serviço **api** (stateless, escala horizontal livre) do serviço **session-worker** (dono das conexões WhatsApp).
- Garantir posse exclusiva de cada sessão (lease/lock em Postgres ou Redis) para que duas réplicas nunca conectem o mesmo device.
- Postgres, Redis e MinIO com volume persistente — no Swarm, placement constraints em nodes com volume; no Compose, volumes locais nomeados.

A imagem é multi-stage (builder → distroless), resultando em ~20 MB.

### Traefik
Proxy de borda com integração nativa ao Docker (providers Swarm e Compose): service discovery por labels, TLS automático via Let's Encrypt, load balancing entre réplicas da API e sticky sessions para os WebSockets.

## Dependências principais (resumo)

```
github.com/polymorfa/hypermeow          // core WhatsApp (import: whatsmeow)
github.com/go-chi/chi/v5                // router HTTP
github.com/danielgtaylor/huma/v2        // API framework + OpenAPI 3.1
github.com/jackc/pgx/v5                 // driver PostgreSQL (pool compartilhado)
github.com/sqlc-dev/sqlc                // (ferramenta) geração de código SQL
github.com/golang-migrate/migrate/v4    // migrações
github.com/hibiken/asynq                // task queue sobre Redis
github.com/redis/go-redis/v9            // cliente Redis
github.com/go-redis/redis_rate/v10      // rate limiting (GCRA)
github.com/golang-jwt/jwt/v5            // JWT admin
github.com/minio/minio-go/v7            // cliente S3/MinIO
github.com/caarlos0/env/v11             // config por env vars
github.com/prometheus/client_golang     // métricas
go.opentelemetry.io/otel                // traces
github.com/stretchr/testify             // asserts em testes
github.com/testcontainers/testcontainers-go // integração com infra real
```
