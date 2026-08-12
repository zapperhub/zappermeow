# HTTP API Contract — Fundação de Contas (001-account-foundation)

**Date**: 2026-08-12 | **Data model**: [../data-model.md](../data-model.md)

> **Fonte de verdade em runtime**: a spec OpenAPI 3.1 gerada pelo huma e servida pela própria API (`/openapi.json` + UI de docs). Este documento é o artefato de **design** que a implementação deve materializar; divergências se resolvem ajustando o código até a spec gerada cumprir este contrato, e o contrato é atualizado quando o design evoluir conscientemente.

## Convenções

- **Base**: JSON UTF-8. Toda requisição com corpo DEVE enviar `Content-Type: application/json`; outro tipo é recusado com `415` (`UNSUPPORTED_MEDIA_TYPE`). **Sucesso**: toda resposta com corpo usa o envelope padrão `{ "status": <código HTTP numérico>, "data": ..., "timestamp": ... }`, com `status` na mesma semântica do membro homônimo da RFC 9457 (ver [TECH_STACK.md — Envelope de resposta](../../../TECH_STACK.md)). **Erros**: RFC 9457 (`application/problem+json`, padrão do huma) estendida com `code` estável e `timestamp`; detalhes por campo em `errors[]` (`location` = campo). Únicas exceções: `204` (sem corpo) e `/metrics` (texto Prometheus).
- **Exemplos deste documento**: os exemplos de sucesso mostram a resposta **completa**, já com o envelope; exemplos inline abreviados indicam o conteúdo de `data` explicitamente.
- **Autenticação** (3 esquemas):
  - `bearerAuth (platform)`: `Authorization: Bearer <JWT aud=platform>` — rotas `/admin/*`.
  - `bearerAuth (tenant)`: `Authorization: Bearer <JWT aud=tenant>` — rotas `/instances*` administrativas; toda rota valida `recurso.tenant_id == jwt.tenant_id`.
  - `apiKeyAuth`: header `X-Api-Key: zmk_...` — rota operacional; valida key ativa ∧ key pertence à instância da URL ∧ tenant ativo; rate limit GCRA por key.
- **Sem autenticação**: apenas `GET /healthz` e `GET /metrics` (rede interna).
- **Recurso de outro tenant / inexistente**: sempre `404` idêntico (não confirma existência — FR-009).
- **Token com `pwd_change=true`**: toda rota exceto `POST /auth/password` responde `403` com `code: PASSWORD_CHANGE_REQUIRED` (US5).
- **IDs**: UUID; **datas**: RFC 3339 UTC.

## Resumo de endpoints

| # | Método e caminho | Auth | Story |
| --- | --- | --- | --- |
| 1 | `POST /auth/login` | pública (rate limited) | US1/US2/US6 |
| 2 | `POST /auth/password` | bearer (qualquer aud) | US5 |
| 3 | `POST /admin/tenants` | platform | US1 |
| 4 | `GET /admin/tenants` | platform | US1 |
| 5 | `GET /admin/tenants/{tenantId}` | platform | US1 |
| 6 | `PATCH /admin/tenants/{tenantId}` | platform | US1 |
| 7 | `POST /admin/tenants/{tenantId}/suspend` | platform | US4 |
| 8 | `POST /admin/tenants/{tenantId}/activate` | platform | US4 |
| 9 | `DELETE /admin/tenants/{tenantId}` | platform | US4 |
| 10 | `POST /admin/tenants/{tenantId}/admin/reset-password` | platform | US5 |
| 11 | `POST /instances` | tenant | US2 |
| 12 | `GET /instances` | tenant | US2 |
| 13 | `GET /instances/{instanceId}` | tenant | US2 |
| 14 | `PATCH /instances/{instanceId}` | tenant | US2 |
| 15 | `DELETE /instances/{instanceId}` | tenant | US2 |
| 16 | `POST /instances/{instanceId}/keys` | tenant | US3 |
| 17 | `GET /instances/{instanceId}/keys` | tenant | US3 |
| 18 | `DELETE /instances/{instanceId}/keys/{keyId}` | tenant | US3 |
| 19 | `GET /instances/{instanceId}/whoami` | apiKey | US3 |
| 20 | `GET /healthz` | — | infra |
| 21 | `GET /metrics` | — (interna) | infra |

## Detalhamento

### 1. `POST /auth/login`

Request:

```json
{ "email": "admin@acme.com", "password": "s3cr3t!!" }
```

`200`:

```json
{
  "status": 200,
  "data": {
    "access_token": "eyJ...",
    "token_type": "Bearer",
    "expires_in": 3600,
    "audience": "tenant",
    "must_change_password": false
  },
  "timestamp": "2026-08-12T12:00:00Z"
}
```

- `401` (`INVALID_CREDENTIALS`) genérico e **idêntico** para: email inexistente, senha errada, conta bloqueada por lockout (FR-019/US6). `403` (`TENANT_SUSPENDED`) para tenant suspenso (mensagem de conta suspensa — decisão da descoberta: o próprio admin pode saber), revelada **somente após** a verificação da senha: senha incorreta em tenant suspenso retorna o mesmo `401` genérico. `429` (`RATE_LIMIT_EXCEEDED`) quando limite por origem excedido (FR-018).
- Claims do JWT: `sub`, `aud` (`platform`|`tenant`), `tenant_id` (só aud tenant), `pwd_change`, `iat`, `exp`.
- N falhas consecutivas (default 5) → lockout 15min; tentativas durante lockout retornam o mesmo `401`.

### 2. `POST /auth/password`

Troca a própria senha. Única rota permitida com `pwd_change=true`.

```json
{ "current_password": "temp-or-current", "new_password": "novaSenha123" }
```

`204`. Erros: `401` (`UNAUTHENTICATED`) token inválido; `422` (`VALIDATION_ERROR`) nova senha < 8 chars; `403` (`INVALID_CURRENT_PASSWORD`) senha atual incorreta. Sucesso zera `must_change_password` e invalida a senha anterior imediatamente (FR-015/US5); tokens emitidos antes da troca/reset (`iat < password_changed_at`) deixam de ser aceitos pelos middlewares (SC-004).

### 3–6. CRUD de tenants (platform)

`POST /admin/tenants`:

```json
{
  "name": "ACME Corp",
  "admin": { "name": "Alice", "email": "alice@acme.com", "password": "senhaInicial1" }
}
```

`201`:

```json
{
  "status": 201,
  "data": {
    "id": "0198...",
    "name": "ACME Corp",
    "status": "active",
    "admin": { "id": "0198...", "name": "Alice", "email": "alice@acme.com" },
    "created_at": "2026-08-12T12:00:00Z"
  },
  "timestamp": "2026-08-12T12:00:00Z"
}
```

- `409` (`RESOURCE_CONFLICT`) nome de tenant ou email de admin duplicado (FR-005), indicando o campo em conflito em `errors[].location`.
- `GET /admin/tenants` → `200` com `data`: `{ "tenants": [ ... ] }` (lista vazia = `[]`, nunca erro). Cada item: id, name, status, created_at, updated_at.
- `GET /admin/tenants/{tenantId}` → `200` tenant + admin (sem hashes); `404` inexistente.
- `PATCH /admin/tenants/{tenantId}` body `{ "name": "Novo Nome" }` → `200` tenant atualizado; `409` duplicado.

### 7–9. Suspensão / reativação / exclusão (platform)

- `POST .../suspend` → `200` com `data`: `{ "id": ..., "status": "suspended" }`. Efeito imediato: login negado, JWTs de tenant rejeitados na próxima requisição, keys das instâncias negadas (FR-006). Idempotente (suspender suspenso = `200`).
- `POST .../activate` → `200` com `data`: `{ "id": ..., "status": "active" }`. Restaura tudo sem recriar credenciais.
- `DELETE /admin/tenants/{tenantId}` — exige confirmação explícita:

```json
{ "confirm_name": "ACME Corp" }
```

`204`. `422` (`VALIDATION_ERROR`) se `confirm_name` não confere com o nome do tenant — comparação exata, case-sensitive (edge case da spec). Cascata irreversível: users, instances, keys (FR-007).

### 10. `POST /admin/tenants/{tenantId}/admin/reset-password`

`200` — **única** exibição da senha temporária (SC-006):

```json
{
  "status": 200,
  "data": { "temporary_password": "xK9...", "must_change_password": true },
  "timestamp": "2026-08-12T12:00:00Z"
}
```

Efeito: próxima autenticação do admin exige troca de senha antes de qualquer outra operação (US5).

### 11–15. CRUD de instâncias (tenant)

`POST /instances` body `{ "name": "vendas-01" }` → `201`:

```json
{
  "status": 201,
  "data": { "id": "0198...", "name": "vendas-01", "state": "registered", "created_at": "..." },
  "timestamp": "2026-08-12T12:00:00Z"
}
```

- `409` (`RESOURCE_CONFLICT`) nome duplicado no tenant; `422` (`VALIDATION_ERROR`) nome vazio/longo.
- `GET /instances` → `200` com `data`: `{ "instances": [ ... ] }` — somente do tenant do JWT.
- `GET /instances/{instanceId}` → `200`; `404` se de outro tenant ou inexistente (indistinguíveis).
- `PATCH /instances/{instanceId}` body `{ "name": "vendas-sp" }` → `200`.
- `DELETE /instances/{instanceId}` → `204`; revoga (remove) todas as keys em cascata na mesma operação (FR-012).

### 16–18. API keys (tenant)

`POST /instances/{instanceId}/keys` body `{ "label": "produção" }` (label opcional) → `201` — **única** exposição do segredo:

```json
{
  "status": 201,
  "data": {
    "id": "0198...",
    "label": "produção",
    "key_prefix": "zmk_a1b2c3d4",
    "api_key": "zmk_a1b2c3d4e5f6...43chars",
    "created_at": "..."
  },
  "timestamp": "2026-08-12T12:00:00Z"
}
```

`GET /instances/{instanceId}/keys` → `200`:

```json
{
  "status": 200,
  "data": {
    "keys": [
      { "id": "...", "label": "produção", "key_prefix": "zmk_a1b2c3d4", "status": "active", "created_at": "...", "revoked_at": null }
    ]
  },
  "timestamp": "2026-08-12T12:00:00Z"
}
```

(nunca contém `api_key` — FR-011).

`DELETE /instances/{instanceId}/keys/{keyId}` → `204`; efeito imediato na requisição operacional seguinte (FR-012/SC-004). `404` key de outra instância/tenant.

### 19. `GET /instances/{instanceId}/whoami` (operacional — API key)

Header: `X-Api-Key: zmk_...`

`200`:

```json
{
  "status": 200,
  "data": {
    "instance": { "id": "0198...", "name": "vendas-01", "state": "registered", "tenant_id": "0198..." },
    "key": { "key_prefix": "zmk_a1b2c3d4", "label": "produção" }
  },
  "timestamp": "2026-08-12T12:00:00Z"
}
```

- `401` (`UNAUTHENTICATED`) key ausente/inválida/revogada; `404` (`RESOURCE_NOT_FOUND`) key válida mas de **outra** instância que não a da URL (FR-013 — não confirma nada); `403` (`TENANT_SUSPENDED`) tenant suspenso (FR-006); `429` (`RATE_LIMIT_EXCEEDED`) rate limit por key excedido.

### 20–21. Infra

- `GET /healthz` → `200` `{ "status": "ok" }` dentro do envelope, como toda resposta JSON (sem auth — exceção constitucional):

```json
{ "status": 200, "data": { "status": "ok" }, "timestamp": "2026-08-12T12:00:00Z" }
```

- `GET /metrics` → Prometheus text format (rede interna; sem auth — exceção constitucional).

## Catálogo de erros (RFC 9457 + `code` estável)

Formato: `application/problem+json` com `type`, `title`, `status`, `detail` (padrão do huma) + extensões `code` e `timestamp`. Exemplo:

```json
{
  "type": "https://zappermeow.dev/errors/validation",
  "title": "Unprocessable Entity",
  "status": 422,
  "detail": "Request validation failed",
  "code": "VALIDATION_ERROR",
  "errors": [
    { "message": "expected length >= 8", "location": "body.new_password" }
  ],
  "timestamp": "2026-08-12T12:00:00Z"
}
```

| Status | `code` | Uso |
| --- | --- | --- |
| 401 | `INVALID_CREDENTIALS` | Falha de login — resposta sempre genérica e **idêntica** (email inexistente, senha errada, lockout) |
| 401 | `UNAUTHENTICATED` | JWT ou API key ausente, inválida, expirada ou revogada |
| 403 | `WRONG_AUDIENCE` | JWT com audience errado para a rota |
| 403 | `TENANT_SUSPENDED` | Tenant suspenso (login e rotas autenticadas) |
| 403 | `PASSWORD_CHANGE_REQUIRED` | Senha temporária pendente de troca (`pwd_change=true`) |
| 403 | `INVALID_CURRENT_PASSWORD` | Senha atual incorreta na troca de senha |
| 404 | `RESOURCE_NOT_FOUND` | Recurso inexistente **ou** de outro tenant/instância — respostas idênticas |
| 409 | `RESOURCE_CONFLICT` | Duplicidade (email, nome de tenant, nome de instância) com campo em `errors[].location` |
| 422 | `VALIDATION_ERROR` | Validação de campos (email malformado, senha curta, nome vazio, confirm_name divergente) com campo e regra em `errors[]` |
| 429 | `RATE_LIMIT_EXCEEDED` | Rate limit por origem (login) ou por key (operacional) |
| 405 | `METHOD_NOT_ALLOWED` | Método não suportado na rota |
| 406/415 | `UNSUPPORTED_MEDIA_TYPE` | `Accept` ou `Content-Type` não suportado — a API só fala JSON; toda requisição com corpo DEVE enviar `Content-Type: application/json` |
| 413 | `REQUEST_TOO_LARGE` | Corpo acima do limite aceito |
| 4xx | `BAD_REQUEST` | Recusa de protocolo sem código mais específico (nenhum 4xx reporta `INTERNAL_ERROR`) |
| 500 | `INTERNAL_ERROR` | Falha inesperada — `detail` genérico, sem vazamento de internals |

Toda resposta de erro indica campo e regra violada quando aplicável (FR-022) e **nunca** ecoa senhas ou segredos. O membro `value` do modelo de erro do huma é suprimido tanto quando o `location` aponta para um campo sensível quanto quando o próprio valor contém material de credencial — o caso de uma recusa no nível do corpo inteiro (`location: "body"`), em que o payload recusado pode ser uma requisição de login.
