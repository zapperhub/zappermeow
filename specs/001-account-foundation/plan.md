# Implementation Plan: Fundação de Contas (Tenants, Instâncias e Credenciais)

**Branch**: `001-account-foundation` | **Date**: 2026-08-12 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/001-account-foundation/spec.md`

## Summary

Implementar a fundação de contas da ZapperMeow: bootstrap do super-admin via configuração, login por email/senha emitindo JWT de curta duração em dois audiences (plataforma e tenant), CRUD de tenants com suspensão reversível em cascata e exclusão definitiva, registro de instâncias (sem pareamento), API keys por instância (múltiplas, show-once, revogação imediata), gestão de senhas sem SMTP e proteção anti força bruta no login.

Abordagem técnica: serviço único `zappermeow serve` (plano stateless) com chi + huma (OpenAPI 3.1 gerado), Postgres 17 via pgx/sqlc para todo o estado de contas (incl. bloqueio de login, durável), Redis + redis_rate apenas para limite por origem no login e rate limit da rota operacional, senhas com Argon2id, API keys com hash SHA-256 de segredos de alta entropia, e revogação imediata garantida por checagem de status (usuário/tenant/key) no banco a cada requisição autenticada. **Nenhuma dependência do HyperMeow nesta feature** — instância aqui é só cadastro; sessão/lease/worker ficam para a feature de pareamento.

## Technical Context

**Language/Version**: Go 1.25+

**Primary Dependencies**: chi v5 (router), huma v2 (API + OpenAPI 3.1), pgx v5 (driver PG, pool único), sqlc (codegen SQL), golang-migrate (migrações embutidas via `embed.FS`), go-redis v9 + redis_rate v10 (limite por origem/GCRA), golang-jwt/jwt v5 (tokens admin), golang.org/x/crypto (Argon2id), caarlos0/env v11 (config), google/uuid (IDs UUID v7), log/slog + prometheus/client_golang + otel (observabilidade)

**Storage**: PostgreSQL 17 — tabelas `tenants`, `users`, `instances`, `api_keys`, `security_events` (migrações golang-migrate, apenas tabelas da API). Redis — contadores GCRA de rate limit (dados efêmeros; nenhum dado de conta)

**Testing**: `go test` + testify (unit, table-driven); testcontainers-go subindo Postgres e Redis reais (integração: queries sqlc, handlers huma, middlewares de auth, lockout, cascata de suspensão)

**Target Platform**: Linux server (container distroless via build multi-stage); dev local em macOS/Linux

**Project Type**: Web service (API REST) — subcomando `serve` do binário único `zappermeow`

**Performance Goals**: Rotas administrativas e operacional de verificação com p95 < 100ms sob carga nominal; validação de API key com 1 lookup indexado por hash

**Constraints**: Efeito de revogação/suspensão observável em ≤ 5s (SC-004 — atendido com checagem por requisição, efeito imediato); segredos exibidos exatamente uma vez e nunca logados (SC-006); respostas de falha de login indistinguíveis (FR-019); estado de bloqueio de login durável (FR-020)

**Scale/Scope**: Escala inicial: dezenas de tenants, centenas de instâncias, milhares de keys; 6 user stories, ~15 endpoints REST, 1 migração inicial

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| # | Princípio | Avaliação | Status |
|---|-----------|-----------|--------|
| I | Simplicidade e Stdlib-First | sqlc + SQL versionado (sem ORM); chi + huma sobre `net/http`; Postgres e Redis únicos já previstos, nenhuma peça nova de infra. Novas deps diretas justificadas: `golang-jwt/jwt/v5` (já prevista em TECH_STACK), `golang.org/x/crypto` (Argon2id — quase-stdlib) | ✅ PASS |
| II | Multi-Tenancy com Isolamento por Instância | Esta feature **implementa** o princípio: API keys por instância (hash no PG, revogação imediata, validação key↔instância da URL), JWT em dois audiences com validação `instance.tenant_id == jwt.tenant_id`, rotas sem auth apenas `/healthz` e `/metrics`. Rate limit GCRA por key na rota operacional com limite default global — *limites configuráveis por tenant* ficaram explicitamente fora do escopo da spec (Assumption) e entram na feature de limites; a proteção exigida existe | ✅ PASS |
| III | Posse Exclusiva de Sessão | N/A — nenhuma sessão WhatsApp nesta feature; nenhum código toca lease ou HyperMeow. A separação api/worker é preservada (só o plano stateless nasce aqui) | ✅ PASS |
| IV | Contrato de API como Fonte de Verdade | Todos os endpoints com request/response tipados em huma; OpenAPI 3.1 e UI de docs servidos pela própria API; sem contratos api↔worker ainda (não há worker) — `proto/` não é necessário nesta feature | ✅ PASS |
| V | Testes Contra Infraestrutura Real | testcontainers-go com Postgres + Redis reais para queries sqlc, middlewares e fluxos (bootstrap, login, lockout, cascata); unit tests para lógica pura (validação, geração de key, claims) | ✅ PASS |
| VI | Observabilidade Estruturada | slog JSON com `tenant_id`/`instance_id` em todo log de request; `/metrics` Prometheus (latência por rota, tentativas/bloqueios de login, keys ativas); OTel desligado por padrão; eventos de segurança (FR-021) persistidos em tabela própria e logados | ✅ PASS |

**Resultado inicial**: PASS — sem violações a justificar.

**Re-check pós Phase 1**: PASS — o design (data-model, contratos, quickstart) não introduziu ORM, infra nova, rota sem autenticação além de `/healthz`+`/metrics`, nem documentação manual dessincronizável (contracts/ descreve o contrato; a fonte de verdade em runtime é o OpenAPI gerado pelo huma).

## Project Structure

### Documentation (this feature)

```text
specs/001-account-foundation/
├── plan.md              # This file (/speckit-plan command output)
├── research.md          # Phase 0 output (/speckit-plan command)
├── data-model.md        # Phase 1 output (/speckit-plan command)
├── quickstart.md        # Phase 1 output (/speckit-plan command)
├── contracts/           # Phase 1 output (/speckit-plan command)
│   └── http-api.md
└── tasks.md             # Phase 2 output (/speckit-tasks command - NOT created by /speckit-plan)
```

### Source Code (repository root)

```text
zappermeow/
├── cmd/
│   └── zappermeow/
│       └── main.go              # subcomandos: serve (esta feature) | session-worker, jobs (futuros, stubs)
├── internal/
│   ├── api/
│   │   ├── server.go            # montagem chi + huma, OpenAPI, /healthz, /metrics
│   │   ├── middleware/          # auth JWT (2 audiences), auth API key, rate limit login/operacional, request logging
│   │   ├── handlers/
│   │   │   ├── auth.go          # login, troca de senha
│   │   │   ├── tenants.go       # CRUD + suspend/activate/delete + reset de senha (plataforma)
│   │   │   ├── instances.go     # CRUD de instâncias (tenant)
│   │   │   ├── apikeys.go       # criar/listar/revogar keys (tenant)
│   │   │   └── whoami.go        # consulta operacional autenticada por API key
│   │   └── httperr/             # erros de domínio → RFC 9457 + `code` estável; envelope de sucesso
│   ├── domain/
│   │   ├── tenant.go            # entidade + regras (status, transições)
│   │   ├── user.go              # entidade + senha (Argon2id), lockout
│   │   ├── instance.go          # entidade + estados
│   │   ├── apikey.go            # geração/verificação de key, formato zmk_
│   │   ├── securityevent.go     # tipos de evento de segurança
│   │   └── services/            # casos de uso: auth, tenants, instances, keys (orquestram store + eventos)
│   ├── store/
│   │   ├── migrate.go           # aplica migrações no boot (golang-migrate iofs — decisão R8)
│   │   ├── queries/             # *.sql fonte do sqlc
│   │   └── (gerado sqlc)        # código type-safe gerado
│   └── config/
│       └── config.go            # caarlos0/env + leitura de /run/secrets (bootstrap, JWT key, DSNs)
├── migrations/                  # 0001_account_foundation.{up,down}.sql + embed.go (raiz — decisão R8)
├── deploy/                      # fora do escopo desta feature (entra com a feature de deploy/pareamento)
├── sqlc.yaml
├── go.mod / go.sum
└── .github/workflows/ci.yml     # golangci-lint → testes (services PG/Redis) → build

tests (convenção Go, co-localizados):
├── internal/domain/...          # unit (table-driven)
├── internal/store/..._test.go   # integração sqlc × Postgres real (testcontainers)
└── internal/api/..._test.go     # integração handlers/middlewares × PG+Redis reais
```

**Structure Decision**: Segue o layout fixado na constituição (`cmd/zappermeow` + `internal/{api, domain, store, config}`); os pacotes `worker`, `jobs`, `lease`, `events` e `media` não são criados nesta feature (nenhum código os exigiria — criá-los vazios violaria a simplicidade). Migrações ficam na raiz `migrations/` (aderência literal à constituição), embutidas via `embed.FS` pelo pacote raiz mínimo `migrations/embed.go` e aplicadas no boot por `internal/store/migrate.go` (decisão R8 em research.md). Testes co-localizados por pacote (convenção Go), com testcontainers nos pacotes `store` e `api`.

## Complexity Tracking

Sem violações de design; um diferimento registrado para auditabilidade:

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| Princípio II pede rate limit operacional "com limites configuráveis por tenant"; esta feature entrega GCRA por key com limite default global (configurável, porém único) | O modelo de limites de uso por tenant ficou explicitamente fora do escopo da spec (Assumption) e entra na feature de limites | Antecipar tabela/gestão de limites por tenant criaria superfície sem requisito na spec; o mecanismo de proteção exigido (GCRA por key em toda rota operacional) já existe nesta feature |
