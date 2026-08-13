# HTTP API Contract — Conexão da Instância (002-instance-connection)

**Date**: 2026-08-13 | **Data model**: [../data-model.md](../data-model.md) | **Base**: [001 http-api.md](../../001-account-foundation/contracts/http-api.md)

> **Fonte de verdade em runtime**: a spec OpenAPI 3.1 gerada pelo huma e servida pela própria API.
> Este documento é o artefato de **design** que a implementação deve materializar.

## Convenções

Valem integralmente as da 001 (envelope `{ "status", "data", "timestamp" }` no sucesso; RFC 9457 +
`code` estável + `timestamp` no erro; `404` idêntico para recurso inexistente ou de outro tenant;
IDs UUID; datas RFC 3339 UTC). Acréscimos desta feature:

- **Autenticação dupla**: toda rota de conexão aceita **`bearerAuth (tenant)`** *ou* **`apiKeyAuth`**
  da própria instância (FR-039). O huma declara os dois esquemas como alternativas no mesmo
  endpoint. Regra invariável: a credencial precisa resolver para a instância da URL — key de outra
  instância, JWT de outro tenant e tenant suspenso resultam em `404`/`403` sem vazar existência
  (FR-040, FR-041).
- **Comandos são assíncronos**: `connect`, `disconnect` e `logout` respondem **`202 Accepted`** com o
  estado corrente. A transição final chega pelo WebSocket ([websocket-events.md](./websocket-events.md))
  ou pela consulta de estado.
- **Idempotência**: repetir um comando no estado já alcançado responde `202` sem efeito colateral
  (FR-008).

## Resumo de endpoints

| # | Método e caminho | Auth | Story |
| --- | --- | --- | --- |
| 1 | `POST /instances/{instanceId}/connect` | tenant \| apiKey | US1, US2, US5, US7 |
| 2 | `POST /instances/{instanceId}/pair-phone` | tenant \| apiKey | US6 |
| 3 | `POST /instances/{instanceId}/disconnect` | tenant \| apiKey | US2 |
| 4 | `POST /instances/{instanceId}/logout` | tenant \| apiKey | US2 |
| 5 | `GET /instances/{instanceId}/connection` | tenant \| apiKey | US3 |
| 6 | `GET /instances/{instanceId}/connection/events` | tenant \| apiKey | US3 |
| 7 | `GET /instances/{instanceId}/ws` | tenant \| apiKey | US1, US7 |
| — | `DELETE /instances/{instanceId}` *(alterado)* | tenant | US2 |
| — | `GET /instances/{instanceId}` *(alterado)* | tenant | US3 |

## Códigos de erro novos

| `code` | HTTP | Quando |
| --- | --- | --- |
| `INSTANCE_NOT_PAIRED` | `409` | Operação que exige sessão salva numa instância `registrada` |
| `PAIRING_IN_PROGRESS` | `409` | `pair-phone` durante pareamento por QR já ativo, quando o cliente pede `replace=false` |
| `INVALID_PHONE_NUMBER` | `422` | Número fora do formato internacional (FR-013) |
| `SESSION_UNAVAILABLE` | `503` | Nenhum worker disponível **e** o comando exige dono vivo (não se aplica a `connect`) |
| `WS_UNAUTHORIZED` | `401` | Handshake do WebSocket sem credencial válida (FR-042) |

---

## Detalhamento

### 1. `POST /instances/{instanceId}/connect`

Liga a instância. Sem sessão salva, inicia pareamento por QR; com sessão salva, restabelece a
conexão. Define `connection_intent = ativa` e limpa `last_disconnect_reason` (reabilitando a
adoção automática mesmo após invalidação — FR-031).

Sem corpo.

`202`:

```json
{
  "status": 202,
  "data": {
    "instance_id": "018f...",
    "state": "pareando",
    "intent": "ativa",
    "pairing": { "method": "qr", "expires_at": "2026-08-13T12:03:00Z" }
  },
  "timestamp": "2026-08-13T12:00:00Z"
}
```

- Instância já `conectada` → `202` com `state: "conectada"`, sem novo QR (edge case de idempotência).
- `pairing` só aparece quando uma tentativa foi iniciada; o QR em si **nunca** vem por HTTP — só
  pelo WebSocket, porque é um stream de códigos que se renovam.

### 2. `POST /instances/{instanceId}/pair-phone`

Pareamento por código de 8 caracteres, sem QR (US6).

```json
{ "phone_number": "5511999999999", "replace_active": true }
```

`200`:

```json
{
  "status": 200,
  "data": { "pairing_code": "ABCD-2345", "expires_at": "2026-08-13T12:03:00Z", "state": "pareando" },
  "timestamp": "2026-08-13T12:00:00Z"
}
```

- `phone_number`: E.164 sem `+`; inválido → `422 INVALID_PHONE_NUMBER` **sem alterar o estado**.
- `replace_active` (default `true`): encerra uma tentativa em curso e assume o lugar (FR-014);
  com `false`, uma tentativa ativa resulta em `409 PAIRING_IN_PROGRESS`.
- Instância já pareada → `409 INSTANCE_NOT_PAIRED` invertido: aqui responde `409` com
  `code: ALREADY_PAIRED`; use `logout` antes de parear outro dispositivo.

### 3. `POST /instances/{instanceId}/disconnect`

Coloca offline **preservando** o material da sessão. Define `connection_intent = parada` e
`desired_state = stopped`.

`202` com `{ "state": "desconectada", "intent": "parada" }`. Repetir em instância já desconectada
responde `202` idêntico (FR-008).

### 4. `POST /instances/{instanceId}/logout`

Encerra a sessão junto ao WhatsApp, remove o dispositivo da lista do aparelho, apaga o material e
devolve a instância a `registrada` (FR-006).

`202`:

```json
{
  "status": 202,
  "data": { "state": "registrada", "intent": "parada", "logout_mode": "remote" },
  "timestamp": "2026-08-13T12:00:00Z"
}
```

`logout_mode` é honesto sobre o que aconteceu (R10): `remote` quando o dispositivo foi removido no
servidor; `local_only` quando não houve como conectar e apenas o material local foi apagado — nesse
caso o dispositivo pode continuar listado no aparelho do cliente.

### 5. `GET /instances/{instanceId}/connection`

Estado corrente (FR-032).

`200`:

```json
{
  "status": 200,
  "data": {
    "instance_id": "018f...",
    "state": "conectada",
    "intent": "ativa",
    "connected_at": "2026-08-13T11:20:00Z",
    "device": {
      "jid": "5511999999999:11@s.whatsapp.net",
      "phone_number": "5511999999999",
      "push_name": "Suporte ACME",
      "platform": "android",
      "paired_at": "2026-08-10T09:00:00Z"
    },
    "last_disconnect": { "at": "2026-08-13T11:18:31Z", "reason": "network" },
    "ban_expires_at": null,
    "shares_number_with": ["018f...b2", "018f...c9"]
  },
  "timestamp": "2026-08-13T12:00:00Z"
}
```

- `device` é `null` em instância nunca pareada; o endpoint responde `200` com
  `state: "registrada"`, nunca erro (cenário 4 da US3).
- `shares_number_with`: outras instâncias **do mesmo tenant** com o mesmo `phone_number` —
  informativo, nunca bloqueio (FR-018). Lista vazia é o caso comum.

### 6. `GET /instances/{instanceId}/connection/events`

Trilha paginada, mais recentes primeiro (FR-036).

Query: `limit` (1–200, default 50), `before` (cursor opaco — `id` codificado), `type` (filtro
opcional, repetível).

`200`:

```json
{
  "status": 200,
  "data": {
    "events": [
      { "type": "disconnected", "reason": "network",  "occurred_at": "2026-08-13T11:18:31Z", "detail": null },
      { "type": "connected",    "reason": null,       "occurred_at": "2026-08-13T09:02:10Z", "detail": null },
      { "type": "number_changed", "reason": null, "occurred_at": "2026-08-10T09:00:00Z",
        "detail": { "previous_phone": "5511888888888", "new_phone": "5511999999999" } }
    ],
    "next_before": "MTIzNA=="
  },
  "timestamp": "2026-08-13T12:00:00Z"
}
```

Eventos além da retenção (default 30 dias) simplesmente não existem — sem erro, sem indicação de
truncamento (FR-037).

### 7. `GET /instances/{instanceId}/ws`

Upgrade WebSocket. Contrato completo das mensagens em
[websocket-events.md](./websocket-events.md). Aparece na spec OpenAPI como endpoint de upgrade
(`101`), com a descrição apontando para o contrato de mensagens — as frames não são respostas HTTP
e por isso não usam o envelope JSON.

### Alterações em endpoints da 001

**`DELETE /instances/{instanceId}`** — antes removia só o registro. Agora, se houver sessão, o
fluxo desconecta e desloga de forma limpa **antes** de remover (FR-007); uma tentativa de
pareamento em curso é cancelada e os clientes WS são encerrados. Resposta segue `204`. Se o logout
remoto falhar, a exclusão prossegue com o material apagado localmente e o evento registrado — a
exclusão não fica refém do WhatsApp.

**`GET /instances/{instanceId}`** e **`GET /instances`** — o objeto de instância ganha um resumo
`connection`: `{ "state", "intent", "phone_number", "connected_at" }`. O detalhe completo continua
em `GET /instances/{id}/connection`.

**Suspensão de tenant** (`POST /admin/tenants/{id}/suspend`) — passa a marcar
`desired_state = stopped` em todos os leases do tenant e publicar `sessions:stop`, derrubando as
sessões; `connection_intent` é preservado e restaurado em `activate` (FR-041, R12).
