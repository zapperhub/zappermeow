# Tasks: Fundação de Contas (Tenants, Instâncias e Credenciais)

**Input**: Design documents from `/specs/001-account-foundation/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/http-api.md, quickstart.md

**Tests**: INCLUÍDOS — a constituição (Princípio V) exige testes contra infraestrutura real (testcontainers com Postgres 17 e Redis) e unit tests table-driven para lógica pura. CI verde é pré-condição de merge.

**Organization**: Tarefas agrupadas por user story para permitir implementação e teste independentes de cada story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Pode rodar em paralelo (arquivos diferentes, sem dependência de tarefa incompleta)
- **[Story]**: User story da tarefa (US1..US6)
- Caminhos de arquivo exatos nas descrições

## Path Conventions

Projeto único Go (layout constitucional): `cmd/zappermeow/`, `internal/{api,domain,store,config}`, `migrations/` na raiz (decisão R8). Testes co-localizados por pacote (`_test.go`).

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Inicialização do projeto Go e ferramentas de build/lint

- [X] T001 Criar módulo Go (`go.mod`, Go 1.25) e esqueleto do binário em `cmd/zappermeow/main.go` com dispatch de subcomandos: `serve` (implementado nesta feature), `session-worker` e `jobs` (stubs que retornam "not implemented")
- [X] T002 [P] Criar `sqlc.yaml` na raiz (engine postgresql, driver pgx/v5, queries em `internal/store/queries/`, output gerado em `internal/store/`)
- [X] T003 [P] Configurar `.golangci.yml` com linters do projeto (lint é bloqueante no CI)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Infraestrutura central que TODAS as user stories dependem — config, schema, conexões, envelope/erros, primitivas de credencial, servidor HTTP e harness de testes

**⚠️ CRITICAL**: Nenhuma user story pode começar antes desta fase completa

- [X] T004 Implementar carga de configuração em `internal/config/config.go` com caarlos0/env v11 + fallback de arquivos em `/run/secrets/` (produção): `ZAPPERMEOW_DATABASE_URL`, `ZAPPERMEOW_REDIS_ADDR`, `ZAPPERMEOW_JWT_SIGNING_KEY`, `ZAPPERMEOW_JWT_TTL` (default 1h), `ZAPPERMEOW_BOOTSTRAP_EMAIL`/`ZAPPERMEOW_BOOTSTRAP_PASSWORD`, `ZAPPERMEOW_LOCKOUT_MAX_FAILURES` (default 5), `ZAPPERMEOW_LOCKOUT_WINDOW` (default 15m), `ZAPPERMEOW_LOGIN_RATE_LIMIT` (default 30/min por IP), `ZAPPERMEOW_OP_RATE_LIMIT` (rota operacional por key), `ZAPPERMEOW_LISTEN_ADDR` — com unit tests
- [X] T005 Escrever migração `migrations/0001_account_foundation.up.sql` e `migrations/0001_account_foundation.down.sql`: extensão `citext`; tabelas `tenants`, `users`, `instances`, `api_keys`, `security_events` com todas as colunas, CHECKs (status/state/role/result, super-admin sem tenant), FKs ON DELETE CASCADE, UNIQUEs (email global, nome de tenant, nome de instância por tenant, secret_hash) e índices, conforme data-model.md
- [X] T006 Criar `migrations/embed.go` (pacote raiz com `embed.FS` — decisão R8) e runner de migração no boot em `internal/store/migrate.go` (golang-migrate driver iofs + pgx)
- [X] T007 Criar `internal/store/db.go`: construtores do pool pgx v5 (pool único) e do cliente go-redis v9
- [X] T008 [P] Implementar pacote `internal/api/httperr/`: modelo RFC 9457 (`application/problem+json`) estendido com `code` estável + `timestamp` via customização de `huma.NewError`, catálogo completo de códigos do contrato (INVALID_CREDENTIALS, UNAUTHENTICATED, WRONG_AUDIENCE, TENANT_SUSPENDED, PASSWORD_CHANGE_REQUIRED, INVALID_CURRENT_PASSWORD, RESOURCE_NOT_FOUND, RESOURCE_CONFLICT, VALIDATION_ERROR, RATE_LIMIT_EXCEEDED, INTERNAL_ERROR), supressão do membro `value` em campos sensíveis, e struct genérica do envelope de sucesso `{status, data, timestamp}` — com unit tests
- [X] T009 [P] Implementar hashing de senha Argon2id com encoding PHC em `internal/domain/password.go` (t=1, m=64MiB, p=4, salt 16B, tag 32B — decisão R1) com unit tests table-driven (hash/verify/formato PHC)
- [X] T010 [P] Implementar geração de IDs UUID v7 em `internal/domain/id.go` usando `github.com/google/uuid` (dependência listada e justificada no plan.md)
- [X] T011 [P] Implementar emissão/validação de JWT em `internal/domain/token.go` (golang-jwt v5, HS256, claims `sub`, `aud` platform|tenant, `tenant_id`, `pwd_change`, `iat`, `exp` — decisão R3) com unit tests
- [X] T012 Implementar `internal/api/server.go`: montagem chi + huma, OpenAPI 3.1 + UI de docs servidos pela API, `GET /healthz` (envelope padrão), `GET /metrics` Prometheus (histograma de latência por rota), middleware de request logging slog JSON com atributos `tenant_id`/`instance_id`
- [X] T013 [P] Implementar tipos de evento de segurança em `internal/domain/securityevent.go`, query sqlc `InsertSecurityEvent` em `internal/store/queries/security_events.sql` e recorder em `internal/domain/services/events.go` (INSERT + espelho em slog estruturado; NUNCA segredos no metadata/log — FR-021, SC-006)
- [X] T014 [P] Criar harness compartilhado de testcontainers em `internal/store/testutil/containers.go` (sobe Postgres 17 + Redis reais, aplica migrações) para uso pelos testes de integração de `store` e `api`
- [X] T015 Montar o subcomando `serve` em `cmd/zappermeow/main.go`: config → pool/redis → migrações → hook de bootstrap (preenchido na US1) → servidor HTTP com graceful shutdown

**Checkpoint**: Fundação pronta — `go run ./cmd/zappermeow serve` sobe, aplica migração, responde `/healthz`, `/metrics` e `/openapi.json`

---

## Phase 3: User Story 1 - Super-admin faz login e gerencia tenants (Priority: P1) 🎯 MVP

**Goal**: Bootstrap do super-admin via configuração, login por email/senha emitindo JWT (audience platform/tenant) e CRUD de tenants (criar com admin, listar, consultar, editar)

**Independent Test**: Instalação limpa → configurar credencial de bootstrap → login como super-admin → criar tenant → verificar na listagem (quickstart.md, bloco US1)

### Implementation for User Story 1

- [X] T016 [P] [US1] Escrever queries sqlc em `internal/store/queries/users.sql` (CreateUser, GetUserByEmail, GetUserByID, CountSuperAdmins) e rodar `sqlc generate`
- [X] T017 [P] [US1] Escrever queries sqlc em `internal/store/queries/tenants.sql` (CreateTenant, GetTenantByID, ListTenants, UpdateTenantName) e rodar `sqlc generate`
- [X] T018 [P] [US1] Criar entidades de domínio `internal/domain/tenant.go` e `internal/domain/user.go` (papéis, status, transições; validações FR-022: formato de email, senha ≥ 8 chars, nomes 1..120) com unit tests table-driven
- [X] T019 [US1] Implementar serviço de bootstrap em `internal/domain/services/bootstrap.go`: se nenhum super_admin existe e credencial configurada → cria em transação com `pg_advisory_xact_lock` (decisão R5); se já existe → ignora com log INFO; se ausente e sem super-admin → WARN por boot; registra evento `bootstrap_admin_created`; ligar no hook do `serve` (T015)
- [X] T020 [US1] Implementar `Login` no serviço de auth `internal/domain/services/auth.go`: lookup por email, verificação Argon2id, emissão de JWT com audience pelo papel (platform/tenant + `tenant_id`), resposta genérica única `401 INVALID_CREDENTIALS` para qualquer falha (FR-019); `403 TENANT_SUSPENDED` somente após a senha verificada correta — senha errada em tenant suspenso responde o mesmo 401 genérico (anti-enumeração); registra eventos `login_succeeded`/`login_failed`
- [X] T021 [US1] Implementar middleware JWT de plataforma em `internal/api/middleware/auth_platform.go`: valida assinatura/exp/`aud=platform`, carrega status atual do usuário no Postgres a cada requisição (decisão R3 — invalidação imediata), injeta usuário no context; `401 UNAUTHENTICATED` / `403 WRONG_AUDIENCE`
- [X] T022 [US1] Implementar handler `POST /auth/login` em `internal/api/handlers/auth.go` (request/response tipados huma, envelope de sucesso, erros conforme contrato §1)
- [X] T023 [US1] Implementar serviço de tenants em `internal/domain/services/tenants.go`: criação transacional tenant + admin (nome/email/senha), listagem, consulta e edição de nome; `409 RESOURCE_CONFLICT` com campo em `errors[].location` para nome/email duplicado (FR-005); registra `tenant_created`/`tenant_updated`
- [X] T024 [US1] Implementar handlers `POST/GET /admin/tenants`, `GET/PATCH /admin/tenants/{tenantId}` em `internal/api/handlers/tenants.go` atrás do middleware de plataforma (contrato §3–6)
- [X] T025 [US1] Testes de integração (testcontainers) em `internal/api/auth_login_test.go` e `internal/api/tenants_test.go`: bootstrap cria super-admin uma única vez e é idempotente em reinícios (cenários 1–2); login correto emite token aud=platform (cenário 3); criar/listar/consultar/editar tenant e login do admin criado (cenários 4–5); token de tenant negado em rota de plataforma com 403 (cenário 6); sem autenticação → 401 sem vazar internals (cenário 7); duplicidade de nome/email → 409

**Checkpoint**: US1 funcional de ponta a ponta — MVP entregável (onboarding de cliente possível)

---

## Phase 4: User Story 2 - Admin de tenant faz login e registra instâncias (Priority: P2)

**Goal**: Login do admin de tenant (token aud=tenant) e CRUD de instâncias escopado ao próprio tenant (criar, listar, consultar, renomear, excluir) — instância é só cadastro, estado `registered`

**Independent Test**: Com um tenant criado, admin faz login, cria duas instâncias, renomeia uma, exclui a outra e confirma que só enxerga instâncias do próprio tenant

### Implementation for User Story 2

- [X] T026 [US2] Implementar middleware JWT de tenant em `internal/api/middleware/auth_tenant.go`: valida `aud=tenant`, carrega usuário E tenant do Postgres a cada requisição (nega se excluído), injeta `tenant_id` no context; `403 WRONG_AUDIENCE` para token de plataforma
- [X] T027 [P] [US2] Escrever queries sqlc em `internal/store/queries/instances.sql` (CreateInstance, ListInstancesByTenant, GetInstanceByIDAndTenant, RenameInstance, DeleteInstance — todas filtradas por tenant_id) e rodar `sqlc generate`
- [X] T028 [P] [US2] Criar entidade de domínio `internal/domain/instance.go` (estado `registered`, regras de nome 1..120 único por tenant) com unit tests
- [X] T029 [US2] Implementar serviço de instâncias em `internal/domain/services/instances.go`: operações escopadas ao tenant do token; recurso de outro tenant ou inexistente → `404 RESOURCE_NOT_FOUND` idêntico (FR-009); nome duplicado no tenant → `409`; registra `instance_created`/`instance_updated`/`instance_deleted` (este último com contagem de keys removidas em cascata no metadata — decisão R9)
- [X] T030 [US2] Implementar handlers `POST/GET /instances`, `GET/PATCH/DELETE /instances/{instanceId}` em `internal/api/handlers/instances.go` atrás do middleware de tenant (contrato §11–15)
- [X] T031 [US2] Testes de integração em `internal/api/instances_test.go`: login do admin emite token aud=tenant com tenant_id (cenário 1); instância nasce `registered` (cenário 2); listagem só mostra instâncias do próprio tenant (cenário 3); consultar/alterar/excluir instância de outro tenant → 404 indistinguível (cenário 4); exclusão remove da listagem (cenário 5 — cascata de keys verificada na US3); token de plataforma negado com 403 (cenário 6)

**Checkpoint**: US1 e US2 funcionais e independentes — tenant organiza seus números com isolamento

---

## Phase 5: User Story 3 - Admin de tenant emite e revoga API keys da instância (Priority: P3)

**Goal**: Múltiplas API keys ativas por instância com segredo show-once (`zmk_...`), listagem sem segredo, revogação imediata, e rota operacional `whoami` autenticada por key fechando a cadeia de credenciais

**Independent Test**: Com uma instância registrada: criar key → usar o segredo no `whoami` → revogar → mesma consulta negada

### Implementation for User Story 3

- [X] T032 [P] [US3] Implementar domínio de API key em `internal/domain/apikey.go`: geração com `crypto/rand` 32B → `zmk_<base62>`, `key_prefix` = primeiros 12 chars, `secret_hash` = SHA-256 do token completo, verificação em tempo constante (decisão R2) — com unit tests table-driven
- [X] T033 [P] [US3] Escrever queries sqlc em `internal/store/queries/api_keys.sql` (CreateAPIKey, ListKeysByInstance, RevokeAPIKey, GetKeyForAuth — JOIN api_keys × instances × tenants retornando status da key, tenant da instância e status do tenant em 1 lookup indexado por secret_hash) e rodar `sqlc generate`
- [X] T034 [US3] Implementar serviço de keys em `internal/domain/services/apikeys.go`: criar (segredo retornado apenas na resposta de criação — SC-006), listar (nunca o segredo — FR-011), revogar com efeito imediato (`status=revoked`, `revoked_at`); valida posse da instância pelo tenant do token (404 para instância alheia); registra `api_key_created`/`api_key_revoked`
- [X] T035 [US3] Implementar handlers `POST/GET /instances/{instanceId}/keys` e `DELETE /instances/{instanceId}/keys/{keyId}` em `internal/api/handlers/apikeys.go` atrás do middleware de tenant (contrato §16–18)
- [X] T036 [US3] Implementar middleware operacional de API key em `internal/api/middleware/auth_apikey.go`: header `X-Api-Key` → SHA-256 → lookup exato; nega se key inexistente/revogada (`401`), se key não pertence à instância da URL (`404` — FR-013), se tenant suspenso (`403 TENANT_SUSPENDED`); rate limit GCRA por key via redis_rate com limite default global (`429`) — template das rotas operacionais futuras (decisão R7)
- [X] T037 [US3] Implementar handler `GET /instances/{instanceId}/whoami` em `internal/api/handlers/whoami.go` retornando instância + `key_prefix`/label da key usada (contrato §19, FR-014)
- [X] T038 [US3] Testes de integração em `internal/api/apikeys_test.go`: segredo aparece só na criação e nunca na listagem (cenários 1–2); duas keys ativas funcionam simultaneamente no whoami (cenário 3); key da instância A negada na instância B com 404, mesmo tenant (cenário 4); key revogada negada na requisição seguinte (cenário 5); criar/listar keys de instância alheia → 404 (cenário 6); DELETE da instância faz keys pararem de funcionar e o evento `instance_deleted` registra a contagem de keys removidas (US2 cenário 5); rate limit por key → 429

**Checkpoint**: Cadeia de credenciais completa (plataforma → tenant → instância) verificável fim a fim

---

## Phase 6: User Story 4 - Super-admin suspende, reativa e exclui tenants (Priority: P4)

**Goal**: Suspensão reversível com cascata imediata (login, JWTs emitidos e API keys param), reativação sem recriar credenciais e exclusão definitiva com confirmação explícita

**Independent Test**: Tenant ativo com instância e key funcionais → suspender e verificar que login/token/key param → reativar e verificar que voltam → excluir e verificar que nada permanece acessível

### Implementation for User Story 4

- [X] T039 [P] [US4] Adicionar queries sqlc em `internal/store/queries/tenants.sql` (SetTenantStatus, DeleteTenantByID) e rodar `sqlc generate`
- [X] T040 [US4] Implementar suspend/activate/delete no serviço de tenants `internal/domain/services/tenants.go`: suspend/activate idempotentes (FR-006); delete exige `confirm_name` idêntico ao nome do tenant (comparação exata, case-sensitive) senão `422 VALIDATION_ERROR` (FR-007), remoção em cascata via FK em transação; registra `tenant_suspended`/`tenant_activated`/`tenant_deleted` (este último com contagens de users/instances/keys removidos no metadata — decisão R9)
- [X] T041 [US4] Implementar handlers `POST /admin/tenants/{tenantId}/suspend`, `POST /admin/tenants/{tenantId}/activate`, `DELETE /admin/tenants/{tenantId}` em `internal/api/handlers/tenants.go` (contrato §7–9)
- [X] T042 [US4] Garantir cascata de suspensão nos três pontos de autenticação: login de admin de tenant suspenso → `403 TENANT_SUSPENDED` (T020), middleware tenant rejeita JWT válido de tenant suspenso (T026), middleware de API key rejeita key de tenant suspenso (T036) — completar/ajustar as checagens de status e cobrir a janela ≤ 5s (SC-004: efeito na requisição seguinte)
- [X] T043 [US4] Testes de integração em `internal/api/tenant_lifecycle_test.go`: suspender → login recusado com indicação de suspensão (cenário 1), token pré-emitido negado (cenário 2), keys das instâncias negadas (cenário 3); reativar → login e keys voltam sem recriação (cenário 4); excluir com `confirm_name` errado → 422; correto → 204 e tenant/instâncias/keys/usuários removidos irreversivelmente, com evento `tenant_deleted` carregando as contagens no metadata (cenário 5)

**Checkpoint**: Governança do operador completa — conter abuso e encerrar cliente

---

## Phase 7: User Story 5 - Gestão de senhas sem dependência de email (Priority: P5)

**Goal**: Troca da própria senha (com senha atual) para qualquer usuário autenticado; reset pelo super-admin gerando senha temporária show-once com troca obrigatória no primeiro login

**Independent Test**: Trocar a própria senha e validar que a antiga é recusada; resetar senha de um admin como super-admin e validar o fluxo de troca obrigatória

### Implementation for User Story 5

- [X] T044 [P] [US5] Adicionar queries sqlc em `internal/store/queries/users.sql` (UpdatePassword, SetMustChangePassword, GetTenantAdminByTenantID) e rodar `sqlc generate`
- [X] T045 [US5] Implementar serviço de senhas em `internal/domain/services/passwords.go`: troca da própria senha (verifica senha atual → `403 INVALID_CURRENT_PASSWORD`; nova ≥ 8 chars → `422`; invalida anterior imediatamente, atualiza `password_changed_at` e zera `must_change_password` — FR-015); reset pelo super-admin (senha temporária aleatória com as mesmas regras, exibida uma única vez, seta `must_change_password=true` — FR-016); registra `password_changed`/`password_reset`
- [X] T046 [US5] Aplicar enforcement de troca pendente nos middlewares JWT (T021/T026) usando o **estado do banco** carregado por requisição (não apenas o claim): usuário com `must_change_password=true` só acessa `POST /auth/password`, qualquer outra rota → `403 PASSWORD_CHANGE_REQUIRED`; rejeitar tokens com `iat < users.password_changed_at` (troca/reset derruba tokens em circulação — SC-004); login com senha temporária emite token com `pwd_change=true` e `must_change_password=true` no corpo (contrato §1–2)
- [X] T047 [US5] Implementar handlers `POST /auth/password` (qualquer audience) em `internal/api/handlers/auth.go` e `POST /admin/tenants/{tenantId}/admin/reset-password` em `internal/api/handlers/tenants.go` (contrato §2 e §10)
- [X] T048 [US5] Testes de integração em `internal/api/passwords_test.go`: troca com senha atual correta → antiga recusada no próximo login (cenário 1); senha atual incorreta → 403 (cenário 2); reset gera temporária exibida uma única vez (cenário 3); login com temporária → toda rota exceto troca de senha responde 403 PASSWORD_CHANGE_REQUIRED (cenário 4); após definir nova senha → acesso completo restaurado e temporária inválida (cenário 5); token emitido antes da troca/reset é recusado na requisição seguinte (`iat < password_changed_at`)

**Checkpoint**: Ciclo de vida de senha completo sem SMTP nem intervenção no banco

---

## Phase 8: User Story 6 - Proteção contra força bruta no login (Priority: P6)

**Goal**: Lockout durável por conta após N falhas consecutivas (default 5, janela 15min, expiração automática), limite por origem (IP) via GCRA, e respostas de falha indistinguíveis

**Independent Test**: Errar a senha 5 vezes e verificar bloqueio mesmo com senha correta; aguardar expiração e verificar que o login volta

### Implementation for User Story 6

- [X] T049 [US6] Implementar lockout por conta no serviço de auth `internal/domain/services/auth.go` (decisão R4): a cada falha, incremento transacional de `failed_login_count`; ao atingir N (config) → `locked_until = now() + janela` e contador zerado; conta bloqueada responde o MESMO `401 INVALID_CREDENTIALS` genérico (FR-019); desbloqueio passivo por expiração do timestamp (sem job); sucesso zera contador; estado durável em Postgres (FR-020); registra `account_locked` (o `account_unlocked` é registrado no primeiro login bem-sucedido após a expiração do bloqueio)
- [X] T050 [US6] Implementar middleware de rate limit por origem em `internal/api/middleware/ratelimit_login.go`: GCRA via redis_rate keyed por IP (`rl:login:{ip}`, respeitando `X-Forwarded-For` apenas de proxy confiável), limite/janela configuráveis → `429 RATE_LIMIT_EXCEEDED`; aplicar em `POST /auth/login` (FR-018)
- [X] T051 [US6] Testes de integração em `internal/api/lockout_test.go`: 5 falhas consecutivas → 6ª tentativa com senha CORRETA recusada com 401 genérico (cenário 1, SC-005); após expiração da janela login volta a funcionar (cenário 2) e contador é zerado (cenário 3); lockout sobrevive a restart simulado do serviço (FR-020); limite por origem excedido → 429 (cenário 4); corpos e status idênticos para email inexistente vs. senha errada (cenário 5, FR-019); eventos `account_locked`/`login_failed` registrados e `account_unlocked` no primeiro login bem-sucedido pós-expiração

**Checkpoint**: Todas as 6 user stories funcionais e independentes

---

## Phase 9: Polish & Cross-Cutting Concerns

**Purpose**: CI, métricas de negócio, auditoria de segredos e validação final do quickstart

- [X] T052 [P] Criar `Dockerfile` multi-stage na raiz (builder → distroless) e pipeline CI em `.github/workflows/ci.yml`: golangci-lint → `go test ./...` (com Docker disponível para testcontainers) → build da imagem — pipeline bloqueante (Princípio V)
- [X] T053 [P] Adicionar métricas de feature ao `/metrics` em `internal/api/server.go` e middlewares: contadores de tentativas/falhas/bloqueios de login, gauge de API keys ativas, latência por rota já existente (Princípio VI)
- [X] T054 Auditar vazamento de segredos (SC-006): revisar todos os pontos de log e respostas garantindo que senha, senha temporária e api_key nunca aparecem em slog nem em `security_events.metadata`; teste automatizado que captura a saída de log do fluxo completo e falha se algum segredo aparecer
- [X] T055 Executar validação fim-a-fim do `quickstart.md` (fluxo completo + tabela de validações negativas + consulta SQL de `security_events`) e marcar os critérios de aceite (SC-001..SC-007)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: sem dependências — começa imediatamente
- **Foundational (Phase 2)**: depende do Setup — BLOQUEIA todas as user stories
- **User Stories (Phases 3–8)**: todas dependem da Phase 2 completa
  - **US1 (P1)**: nenhuma dependência de outra story — MVP
  - **US2 (P2)**: independente de US1 em código novo, mas o teste independente usa um tenant existente (criado via US1 ou seed de teste); reusa o serviço de login (T020)
  - **US3 (P3)**: depende de US2 (instâncias e middleware de tenant)
  - **US4 (P4)**: depende de US1 (tenants); os testes de cascata completa usam US2/US3 (instância + key)
  - **US5 (P5)**: depende de US1 (login e middlewares JWT)
  - **US6 (P6)**: depende de US1 (fluxo de login em T020)
- **Polish (Phase 9)**: depende de todas as stories desejadas completas

### Within Each User Story

- Queries sqlc e entidades de domínio antes de serviços; serviços antes de handlers; middlewares antes dos handlers que protegem; testes de integração por último (contra PG+Redis reais)
- Story completa e testada antes de avançar para a próxima prioridade

### Parallel Opportunities

- Phase 1: T002 e T003 em paralelo após T001
- Phase 2: T008, T009, T010, T011, T013, T14 em paralelo (arquivos distintos) após T005–T007
- US1: T016, T017, T018 em paralelo; US2: T027, T028 em paralelo; US3: T032, T033 em paralelo
- Após a Phase 2: US1 pode andar sozinha; com US1 pronta, US2 → US3 em sequência enquanto US5/US6 andam em paralelo com US2+ (tocam arquivos distintos: senhas/lockout vs. instâncias/keys)
- Phase 9: T052 e T053 em paralelo

---

## Parallel Example: User Story 1

```bash
# Após Phase 2, lançar em paralelo:
Task: "T016 [US1] queries sqlc users.sql"
Task: "T017 [US1] queries sqlc tenants.sql"
Task: "T018 [US1] entidades tenant.go + user.go com unit tests"

# Depois, em sequência: T019 (bootstrap) → T020 (login) → T021 (middleware) → T022 (handler login)
# T023 (serviço tenants) → T024 (handlers tenants) → T025 (testes de integração)
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Phase 1: Setup (T001–T003)
2. Phase 2: Foundational (T004–T015) — CRÍTICO, bloqueia tudo
3. Phase 3: US1 (T016–T025)
4. **STOP e VALIDAR**: bootstrap → login → criar tenant → listar (bloco US1 do quickstart)
5. Deploy/demo se pronto — a plataforma já faz onboarding de cliente

### Incremental Delivery

1. Setup + Foundational → fundação de serviço no ar (`/healthz`, OpenAPI)
2. US1 → testar → MVP (super-admin + tenants)
3. US2 → testar → tenant organiza instâncias
4. US3 → testar → cadeia de credenciais completa (valor central da feature)
5. US4 → testar → governança (suspensão/exclusão)
6. US5 → testar → gestão de senhas sem SMTP
7. US6 → testar → endurecimento do login
8. Polish → CI + métricas + auditoria de segredos + quickstart completo

---

## Notes

- [P] = arquivos diferentes, sem dependência entre si
- Tarefas de queries sqlc terminam com `sqlc generate` — se rodarem em paralelo, o generate é idempotente sobre o diretório inteiro
- Toda escrita sensível registra evento em `security_events` na MESMA transação da ação (decisão R9)
- Middlewares carregam status de usuário/tenant/key do Postgres a CADA requisição — é isso que garante SC-004 (≤ 5s) sem blocklist
- Commit após cada tarefa ou grupo lógico; parar em qualquer checkpoint para validar a story de forma independente
