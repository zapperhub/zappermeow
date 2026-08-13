# Quickstart — Validação da Conexão da Instância (002-instance-connection)

**Date**: 2026-08-13 | **Contratos**: [http-api.md](./contracts/http-api.md) · [websocket-events.md](./contracts/websocket-events.md) · [session-grpc.md](./contracts/session-grpc.md) | **Modelo**: [data-model.md](./data-model.md)

Roteiro de validação fim-a-fim das 7 user stories da [spec](./spec.md). Diferente da 001, parte
deste roteiro exige **um número de WhatsApp real** — não há emulador nem sandbox do WhatsApp, e a
automação de teste contra números reais leva a banimento (R13). O que dá para automatizar está nos
testes de integração; o que exige um humano com um celular está marcado como **[manual]**.

## Pré-requisitos

- Go 1.25+, Docker, `curl`, `jq`
- `websocat` (ou qualquer cliente WS que aceite subprotocolo) para acompanhar o canal de eventos
- **[manual]** um número de WhatsApp de teste, em um aparelho à mão
- Fundação de contas (001) funcionando: um tenant ativo, seu admin e uma instância registrada

## Setup

```bash
# 1. Infra local (mesma da 001 — um Postgres, um Redis)
docker run -d --name zm-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=zappermeow -p 5432:5432 postgres:17
docker run -d --name zm-redis -p 6379:6379 redis:7

export ZAPPERMEOW_DATABASE_URL="postgres://postgres:dev@localhost:5432/zappermeow?sslmode=disable"
export ZAPPERMEOW_REDIS_ADDR="localhost:6379"
export ZAPPERMEOW_JWT_SIGNING_KEY="$(openssl rand -hex 64)"
export ZAPPERMEOW_BOOTSTRAP_EMAIL="root@example.com"
export ZAPPERMEOW_BOOTSTRAP_PASSWORD="bootstrap-secret-1"

# 2. API (aplica as migrações 0001 e 0002 no boot)
go run ./cmd/zappermeow serve

# 3. Worker de sessões, em outro terminal (aplica Container.Upgrade do HyperMeow no boot)
export ZAPPERMEOW_WORKER_ADVERTISE_ADDR="127.0.0.1:9090"
export ZAPPERMEOW_MAX_SESSIONS_PER_WORKER=200
go run ./cmd/zappermeow session-worker
```

**Esperado no boot do worker**: log `whatsmeow_store_upgraded`, `grpc_listening addr=127.0.0.1:9090`
e o início dos loops de heartbeat e reconciliação. No Postgres devem existir as tabelas
`session_leases`, `connection_events` e as `whatsmeow_*` (estas últimas com versão própria em
`whatsmeow_version`, nunca tocadas pelas nossas migrações).

## Credenciais de trabalho

```bash
BASE=http://localhost:8080
JSON=(-H "Content-Type: application/json")

TOKEN=$(curl -s "${JSON[@]}" -X POST $BASE/auth/login \
  -d '{"email":"admin@acme.com","password":"..."}' | jq -r .data.access_token)
AUTH=(-H "Authorization: Bearer $TOKEN")

INST=$(curl -s "${AUTH[@]}" $BASE/instances | jq -r '.data.instances[0].id')
```

---

## US1 — Parear por QR e ver conectada **[manual]**

```bash
# Terminal A: ouvir o canal de eventos ANTES de conectar
websocat -H="Authorization: Bearer $TOKEN" --protocol zappermeow.v1 \
  "ws://localhost:8080/instances/$INST/ws"

# Terminal B: iniciar
curl -s "${AUTH[@]}" -X POST $BASE/instances/$INST/connect | jq
```

**Esperado**:

1. `connect` responde `202` com `state: "pareando"`.
2. No terminal A, o primeiro frame é sempre `state.snapshot`; em seguida vêm frames
   `pairing.code` — o primeiro em poucos segundos (**SC-001**: ≤5s), renovados a cada ~20s
   (**SC-002**: nunca há intervalo sem código válido).
3. Renderize o `code` como QR (`qrencode -t ANSI "$CODE"`) e escaneie com o aparelho.
4. Chegam `pairing.succeeded` (com `device.jid` **contendo o sufixo de dispositivo**, ex.
   `...:11@s.whatsapp.net`) e `connection.connected` — **SC-003**: menos de 15s entre escanear e
   conectar.

```bash
curl -s "${AUTH[@]}" $BASE/instances/$INST/connection | jq .data
# state=conectada, intent=ativa, device preenchido, connected_at recente
```

**Cenário 4 da US1** — abra um segundo `websocat` durante o pareamento: ele recebe imediatamente um
`state.snapshot` já com o código corrente, sem esperar a próxima renovação.

**Cenário 5 (expiração)** — inicie o `connect` e não escaneie: após a janela (~160s, R2) chega
`pairing.expired` com `reason: "window_exhausted"` e a instância volta ao estado anterior.

## US2 — Ciclo de vida **[manual, exceto o último passo]**

```bash
curl -s "${AUTH[@]}" -X POST $BASE/instances/$INST/disconnect | jq .data   # 202 desconectada/parada
curl -s "${AUTH[@]}" -X POST $BASE/instances/$INST/disconnect | jq .data   # idempotente: 202 igual
curl -s "${AUTH[@]}" -X POST $BASE/instances/$INST/connect    | jq .data   # reconecta SEM novo QR
curl -s "${AUTH[@]}" -X POST $BASE/instances/$INST/logout     | jq .data   # 202, logout_mode=remote
```

**Esperado**: após o `logout`, o dispositivo **desaparece** da lista de aparelhos conectados no
celular, o estado volta a `registrada` e um novo `connect` inicia pareamento por QR do zero.

**Logout offline (R10)**: desconecte, depois faça logout — a resposta traz `logout_mode` refletindo
o que ocorreu de fato (`remote` se conseguiu conectar para remover; `local_only` se não).

**Exclusão limpa (FR-007)**: com a instância conectada, `DELETE /instances/$INST` → `204`; confira
no aparelho que o dispositivo foi removido e no banco que não restou lease nem evento órfão.

## US3 — Estado e trilha

```bash
curl -s "${AUTH[@]}" $BASE/instances/$INST/connection | jq .data
curl -s "${AUTH[@]}" "$BASE/instances/$INST/connection/events?limit=20" | jq .data.events
```

**Esperado**: a trilha traz, em ordem cronológica inversa, `pairing_started`, `pairing_succeeded`,
`connected`, `disconnected` (com `reason`), `logged_out`. Instância nunca pareada responde `200`
com `state: "registrada"` e `device: null` — nunca erro.

## US4 — Continuidade e posse exclusiva *(automatizável)*

```bash
# Dois workers, portas distintas
ZAPPERMEOW_WORKER_ADVERTISE_ADDR=127.0.0.1:9090 go run ./cmd/zappermeow session-worker &
ZAPPERMEOW_WORKER_ADVERTISE_ADDR=127.0.0.1:9091 go run ./cmd/zappermeow session-worker &

# Quem detém a sessão?
psql "$ZAPPERMEOW_DATABASE_URL" -c \
  "SELECT worker_id, grpc_addr, generation, desired_state FROM session_leases WHERE instance_id='$INST'"

kill -9 <pid do dono>   # morte abrupta
```

**Esperado** (**SC-005**): em ≤60s o outro worker adquire o lease, `generation` **incrementa** e a
instância volta a `conectada` sem novo QR. Com `SIGTERM` (encerramento planejado) a troca é bem
mais rápida — o dono libera o lease em vez de esperar os 30s de expiração (**SC-006**).

**SC-004 / SC-007**: nunca deve haver dois `worker_id` para a mesma instância — a query acima
retorna uma linha por instância, por construção. Reinicie tudo e confirme: instâncias com
`intent=parada` continuam offline; com `intent=ativa`, reconectam sozinhas.

## US5 — Invalidação **[manual]**

Com a instância conectada, **remova o dispositivo pelo celular** (WhatsApp → Dispositivos
conectados → sair).

**Esperado**: chega `connection.logged_out`, o estado vai para `deslogada`, a trilha registra
`logged_out_from_phone` e **nenhuma tentativa de reconexão acontece** (**SC-009** — acompanhe
`zappermeow_session_reconnects_total` em `/metrics`, que deve ficar parado). Um `connect` explícito
reabilita o pareamento.

**SC-014 [manual]**: pareie **duas instâncias com o mesmo número** e deixe as duas conectadas por
30 minutos. Ambas devem permanecer `conectada` — são dispositivos companheiros distintos, e
nenhuma derruba a outra. `GET /connection` de cada uma lista a outra em `shares_number_with`.

## US6 — Pareamento por código **[manual]**

```bash
curl -s "${AUTH[@]}" "${JSON[@]}" -X POST $BASE/instances/$INST/pair-phone \
  -d '{"phone_number":"5511999999999"}' | jq .data     # → pairing_code

curl -s "${AUTH[@]}" "${JSON[@]}" -X POST $BASE/instances/$INST/pair-phone \
  -d '{"phone_number":"invalido"}' -o /dev/null -w '%{http_code}\n'   # → 422
```

Digite o código no aparelho (WhatsApp → Dispositivos conectados → Conectar com número de telefone).
**Esperado**: `pairing.succeeded` no WS; o `422` do número inválido **não** altera o estado.

## US7 — Operação por API key

```bash
KEY=$(curl -s "${AUTH[@]}" "${JSON[@]}" -X POST $BASE/instances/$INST/keys \
  -d '{"label":"integracao"}' | jq -r .data.secret)
AKEY=(-H "X-Api-Key: $KEY")

curl -s "${AKEY[@]}" $BASE/instances/$INST/connection | jq .data.state
curl -s "${AKEY[@]}" -X POST $BASE/instances/$INST/connect | jq .data.state
websocat -H="X-Api-Key: $KEY" --protocol zappermeow.v1 "ws://localhost:8080/instances/$INST/ws"

# Isolamento (SC-012): key da instância A contra a instância B → 404
curl -s "${AKEY[@]}" $BASE/instances/$OUTRA/connection -o /dev/null -w '%{http_code}\n'
```

**Esperado**: paridade total com o JWT de tenant; key revogada ou tenant suspenso derruba inclusive
o WebSocket já aberto, com código de fechamento `4403`.

---

## Testes automatizados

```bash
go test ./...                                  # unit + integração (testcontainers sobe PG e Redis)
go test ./internal/worker/... -run Lease -v    # aquisição, heartbeat, fencing, failover
go test ./internal/api/... -run WS -v          # snapshot inicial, múltiplos ouvintes, dedupe por seq
```

O que a suíte cobre com **infra real** (Postgres e Redis via testcontainers) e uma sessão WhatsApp
**falsa** (`wa.FakeSession`, R13): máquina de estados completa, classificação de desconexões,
disputa de lease entre dois workers no mesmo processo, fencing por geração, fan-out para múltiplos
clientes WS, persistência da trilha, varredura de retenção e o comportamento de suspensão de
tenant.

O que **só** o roteiro manual acima cobre: handshake real do WhatsApp, escaneamento de QR, código
por telefone, logout visto no aparelho e convivência de dois dispositivos do mesmo número.

## Observabilidade esperada

```bash
curl -s localhost:8080/metrics | grep zappermeow_
```

Devem aparecer `zappermeow_sessions_connected`, `zappermeow_pairing_attempts_total`,
`zappermeow_session_state_transitions_total`, `zappermeow_lease_acquisitions_total`,
`zappermeow_ws_clients` e `zappermeow_stream_replaced_total`. Este último **precisa ficar em zero**:
qualquer incremento significa a mesma credencial de dispositivo aberta em dois lugares, ou seja,
falha na posse exclusiva (Princípio III) — é alarme, não estatística.

Nos logs (`slog` JSON), toda transição carrega `tenant_id` e `instance_id`; nenhum log pode conter
QR code, token, API key ou material de sessão.
