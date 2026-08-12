# Quickstart — Validação da Fundação de Contas (001-account-foundation)

**Date**: 2026-08-12 | **Contrato**: [contracts/http-api.md](./contracts/http-api.md) | **Modelo**: [data-model.md](./data-model.md)

Guia de validação fim-a-fim: prova as 6 user stories da [spec](./spec.md) contra uma instância local. Este roteiro é também a base do SC-001 (fluxo completo em < 10 min).

## Pré-requisitos

- Go 1.25+
- Docker (para Postgres 17 e Redis locais, e para os testes com testcontainers)
- `curl` e `jq`

## Setup

```bash
# 1. Infra local
docker run -d --name zm-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=zappermeow -p 5432:5432 postgres:17
docker run -d --name zm-redis -p 6379:6379 redis:7

# 2. Configuração mínima (dev usa env vars; produção usa /run/secrets)
export ZAPPERMEOW_DATABASE_URL="postgres://postgres:dev@localhost:5432/zappermeow?sslmode=disable"
export ZAPPERMEOW_REDIS_ADDR="localhost:6379"
export ZAPPERMEOW_JWT_SIGNING_KEY="$(openssl rand -hex 64)"
export ZAPPERMEOW_BOOTSTRAP_EMAIL="root@example.com"
export ZAPPERMEOW_BOOTSTRAP_PASSWORD="bootstrap-secret-1"

# 3. Subir a API (aplica migrações e cria o super-admin no boot — R5/R8)
go run ./cmd/zappermeow serve
```

**Esperado no boot**: logs JSON (slog) informando migrações aplicadas e `bootstrap_admin_created`; `GET http://localhost:8080/healthz` → `{"status":200,"data":{"status":"ok"},"timestamp":"..."}` (envelope padrão); OpenAPI em `http://localhost:8080/openapi.json` e UI de docs servida pela API.

## Fluxo de validação fim-a-fim (US1 → US3)

```bash
BASE=http://localhost:8080

# A API só aceita JSON. Sem este header o curl envia
# application/x-www-form-urlencoded e a resposta é 415 (UNSUPPORTED_MEDIA_TYPE).
JSON=(-H "Content-Type: application/json")

# US1 — login do super-admin (audience platform)
PLATFORM_TOKEN=$(curl -sf $BASE/auth/login "${JSON[@]}" \
  -d '{"email":"root@example.com","password":"bootstrap-secret-1"}' | jq -r .data.access_token)

# US1 — criar tenant com admin
TENANT_ID=$(curl -sf $BASE/admin/tenants "${JSON[@]}" -H "Authorization: Bearer $PLATFORM_TOKEN" \
  -d '{"name":"ACME","admin":{"name":"Alice","email":"alice@acme.com","password":"senhaAlice1"}}' | jq -r .data.id)

# US2 — login do admin do tenant (audience tenant) e registro de instância
TENANT_TOKEN=$(curl -sf $BASE/auth/login "${JSON[@]}" \
  -d '{"email":"alice@acme.com","password":"senhaAlice1"}' | jq -r .data.access_token)
INSTANCE_ID=$(curl -sf $BASE/instances "${JSON[@]}" -H "Authorization: Bearer $TENANT_TOKEN" \
  -d '{"name":"vendas-01"}' | jq -r .data.id)

# US3 — emitir a key. A resposta desta chamada é o ÚNICO lugar, em toda a vida
# da credencial, onde o segredo completo existe: guarde-o agora.
CREATED_KEY=$(curl -sf $BASE/instances/$INSTANCE_ID/keys "${JSON[@]}" \
  -H "Authorization: Bearer $TENANT_TOKEN" -d '{"label":"produção"}')
echo "$CREATED_KEY" | jq          # <-- `data.api_key` aparece aqui, e só aqui
API_KEY=$(echo "$CREATED_KEY" | jq -r .data.api_key)

# US3 — verificar a credencial fim-a-fim com a key recém-emitida
curl -sf $BASE/instances/$INSTANCE_ID/whoami -H "X-Api-Key: $API_KEY" | jq
```

**Esperado**: a criação da key devolve `data.api_key` por extenso (`zmk_` + 43 chars); o `whoami` responde `200` com a instância + `key_prefix`/label, provando que a key funciona. Cronometrado do zero, o fluxo inteiro fica abaixo de 10 minutos (SC-001).

> **O segredo não é recuperável.** Depois desta resposta, nem a listagem (`GET .../keys`), nem o `whoami`, nem consulta direta ao Postgres devolvem o valor completo — o banco guarda apenas o SHA-256 do token (FR-011/SC-006). O `key_prefix` identifica a key em listagens e no registro de eventos, mas os 35 caracteres restantes são aleatórios e não deriváveis dele. Key perdida se resolve emitindo outra e revogando a antiga, que é exatamente o que permite rotação sem downtime.

## Validações negativas (isolamento e revogação)

| Cenário | Comando (resumo) | Esperado |
| --- | --- | --- |
| Key na instância errada (FR-013) | criar 2ª instância; `whoami` da 2ª com a key da 1ª | `404` |
| JWT de tenant em rota de plataforma | `GET /admin/tenants` com `$TENANT_TOKEN` | `403` |
| Revogação imediata (SC-004) | `DELETE .../keys/{keyId}` e repetir `whoami` | `401` na requisição seguinte |
| Suspensão em cascata (US4) | `POST /admin/tenants/$TENANT_ID/suspend`; testar `$TENANT_TOKEN` e `$API_KEY` | `403` em ambos, imediato |
| Reativação (US4) | `POST .../activate`; repetir | credenciais voltam sem recriação |
| Exclusão com confirmação (FR-007) | `DELETE /admin/tenants/$TENANT_ID` com `confirm_name` errado / certo | `422` / `204` + tudo do tenant some |
| Reset de senha (US5) | `POST .../admin/reset-password`; login com senha temporária; tentar `GET /instances` | única rota permitida é `POST /auth/password` (`403` nas demais) |
| Lockout (US6/SC-005) | 5 logins com senha errada; 6ª tentativa com senha **correta** | `401` genérico; após 15 min (ou janela configurada) login volta |
| Anti-enumeração (FR-019) | login com email inexistente vs. senha errada | corpos e status `401` idênticos |
| Corpo sem `Content-Type: application/json` | `POST /auth/login` sem o header | `415` (`UNSUPPORTED_MEDIA_TYPE`), sem ecoar o corpo enviado |
| Segredo nunca reexibido (SC-006) | `GET .../keys` após criação | lista traz `key_prefix`, nunca `api_key`; segredo ausente dos logs |

## Testes automatizados

```bash
# Unit (lógica pura: Argon2id/PHC, formato de key, claims, validações)
go test ./internal/domain/...

# Integração (testcontainers sobe Postgres 17 e Redis reais — Princípio V)
go test ./internal/store/... ./internal/api/...

# Lint (bloqueante no CI)
golangci-lint run
```

**Esperado**: todos verdes; os testes de integração cobrem bootstrap idempotente, unicidade (email/nome), cascatas de suspensão/exclusão, lockout durável (sobrevive a restart simulado) e o catálogo de erros do [contrato](./contracts/http-api.md).

## Critérios de aceite da validação

- [X] Fluxo fim-a-fim completo em < 10 min usando só esta página e a doc da API (SC-001)
- [X] Todas as validações negativas da tabela retornam exatamente o esperado (SC-002, SC-003)
- [X] Revogação/suspensão/troca de senha têm efeito na requisição seguinte (SC-004)
- [X] Eventos de segurança do fluxo localizáveis em `security_events` com ator, alvo e momento (SC-007): `SELECT event_type, actor_user_id, target_type, result, created_at FROM security_events ORDER BY created_at`
- [X] Nenhum segredo (senha, senha temporária, api_key) aparece em logs ou em respostas além da exibição única (SC-006)
