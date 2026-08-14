# HTTP API Contract — Complementos de Conexão (003-connection-extras)

**Date**: 2026-08-14 | **Data model**: [../data-model.md](../data-model.md) | **Base**: [002 http-api.md](../../002-instance-connection/contracts/http-api.md)

> **Fonte de verdade em runtime**: a spec OpenAPI 3.1 gerada pelo huma e servida pela própria API.
> Este documento é o artefato de **design** que a implementação deve materializar.

## Convenções

Valem integralmente as da 001 e da 002 (envelope de sucesso, RFC 9457 + `code` + `timestamp` no
erro, `404` sem vazar existência, autenticação dupla das rotas de conexão, rate limiting GCRA).
Acréscimos desta feature:

- **Exceção de autenticação**: o endpoint de códigos de verificação (#6) aceita **apenas**
  `apiKeyAuth` da própria instância (FR-025 — decisão da spec). Todos os demais seguem a dupla da
  002 (`tenant | apiKey`).
- **Configurações respondem `200`** com o estado gravado (a gravação é síncrona); a **reconexão**
  disparada por mudança de proxy é assíncrona e acompanha pelo WebSocket ou pela consulta, como os
  comandos da 002.
- **Senha de proxy**: toda representação de `proxy_url` em resposta usa a forma mascarada
  (`socks5://user:***@host:1080`) — a senha em claro não aparece em nenhuma resposta, evento, log
  ou trilha (FR-007, SC-007).

## Resumo de endpoints

| # | Método e caminho | Auth | Story |
| --- | --- | --- | --- |
| 1 | `PUT /instances/{instanceId}/proxy` | tenant \| apiKey | US1 |
| 2 | `DELETE /instances/{instanceId}/proxy` | tenant \| apiKey | US1 |
| 3 | `PUT /instances/{instanceId}/passive-mode` | tenant \| apiKey | US3 |
| 4 | `POST /instances/{instanceId}/pairing/passkey/response` | tenant \| apiKey | US4 |
| 5 | `POST /instances/{instanceId}/pairing/passkey/confirm` | tenant \| apiKey | US4 |
| 6 | `GET /instances/{instanceId}/identity-verification-codes` | **apiKey somente** | US5 |
| — | `GET /instances/{instanceId}` *(alterado)* | tenant | US1, US3 |
| — | `GET /instances/{instanceId}/connection` *(alterado)* | tenant \| apiKey | US1, US3 |

## Códigos de erro novos

| `code` | HTTP | Quando |
| --- | --- | --- |
| `INVALID_PROXY_URL` | `422` | URL de proxy que não parseia ou sem host (FR-002) |
| `UNSUPPORTED_PROXY_SCHEME` | `422` | Esquema fora de `http`/`https`/`socks5` (FR-002) |
| `NO_PASSKEY_CHALLENGE` | `409` | Resposta de passkey sem desafio pendente (FR-016) |
| `NO_PASSKEY_CODE` | `409` | Confirmação antes do código existir ou etapa já consumida (FR-018) |
| `INSTANCE_NOT_CONNECTED` | `409` | Consulta de códigos com instância fora de `connected` (FR-024) |
| `IDENTITY_NOT_RESOLVABLE` | `422` | Telefone sem mapeamento LID conhecido (FR-022) |
| `INVALID_CONTACT` | `422` | Contato que não é LID nem telefone válido |
| `CANNOT_VERIFY_SELF` | `422` | Contato é o próprio número da instância |
| `CONTACT_UNAVAILABLE` | `404` | Contato sem dispositivos / desconhecido no WhatsApp (FR-024) |

`SESSION_UNAVAILABLE` (`503`, da 002) aplica-se aos endpoints #4, #5 e #6, que exigem dono vivo.

---

## Detalhamento

### 1. `PUT /instances/{instanceId}/proxy`

Define ou substitui o proxy de saída. Valida (R1/§5 do data-model), persiste e — se a instância
tem sessão ativa — comanda o dono a religar com a nova configuração (R2). Idempotente: repetir a
mesma URL grava e, se conectada, religa do mesmo jeito (a plataforma não compara valor).

Request:

```json
{ "url": "socks5://user:s3cret@203.0.113.10:1080" }
```

`200`:

```json
{
  "status": 200,
  "data": {
    "proxy_url": "socks5://user:***@203.0.113.10:1080",
    "reconnecting": true
  },
  "timestamp": "2026-08-14T12:00:00Z"
}
```

`reconnecting` é `true` quando havia sessão ativa e a reconexão foi comandada; `false` quando a
configuração só vale a partir da próxima conexão. Erros: `422 INVALID_PROXY_URL`,
`422 UNSUPPORTED_PROXY_SCHEME`.

### 2. `DELETE /instances/{instanceId}/proxy`

Remove o proxy (volta à conexão direta). Mesmo comportamento de reconexão do `PUT`. `200` com
`"proxy_url": null`. Idempotente: remover o que não existe responde `200`.

### 3. `PUT /instances/{instanceId}/passive-mode`

Liga/desliga o modo passivo. Persiste sempre; aplica imediatamente quando conectada (R6).

Request: `{ "enabled": true }`

`200`:

```json
{
  "status": 200,
  "data": { "passive_mode": true, "applied": true },
  "timestamp": "2026-08-14T12:00:00Z"
}
```

`applied` é `true` quando havia sessão conectada e o modo foi aplicado na hora; `false` quando
ficou gravado para a próxima conexão. Idempotente.

### 4. `POST /instances/{instanceId}/pairing/passkey/response`

Submete a resposta do autenticador WebAuthn ao desafio corrente. O corpo é **opaco** para a
plataforma: o JSON da asserção (`{id, rawId, type, response:{...}}`) produzido por
`navigator.credentials.get()`; a plataforma repassa sem interpretar.

Request:

```json
{ "response": { "id": "...", "rawId": "...", "type": "public-key", "response": { "clientDataJSON": "...", "authenticatorData": "...", "signature": "...", "userHandle": "..." } } }
```

`202` com o estado corrente do pareamento (a continuação chega pelo WebSocket:
`pairing.passkey_code`, confirmação automática ou `pairing.failed`). Erros:
`409 NO_PASSKEY_CHALLENGE` (sem desafio pendente ou já respondido), `503 SESSION_UNAVAILABLE`.

### 5. `POST /instances/{instanceId}/pairing/passkey/confirm`

Confirma que o código de conferência foi exibido e reconhecido pelo dono do número. Sem corpo.

`202` — a conclusão (`pairing.succeeded` → `connection.connected`) chega pelo WebSocket. Erros:
`409 NO_PASSKEY_CODE` (antes do código existir, etapa já consumida ou confirmação automática já
feita), `503 SESSION_UNAVAILABLE`.

### 6. `GET /instances/{instanceId}/identity-verification-codes?contact=<lid|phone>`

Consulta os códigos de verificação de identidade da conversa com um contato. **Somente API key da
instância.** Exige `connection_state = connected`. Envolve idas à rede do WhatsApp (R8) — timeout
do handler alinhado ao SC-006 (5s).

`200`:

```json
{
  "status": 200,
  "data": {
    "contact": {
      "lid": "123456789@lid",
      "phone_number": "5511999998888",
      "username": "fulano"
    },
    "numeric_code": "123456789012345678901234567890123456789012345678901234567890",
    "display_qr": "<base64>",
    "verification_qr": "<base64>"
  },
  "timestamp": "2026-08-14T12:00:00Z"
}
```

`phone_number` e `username` são `null` quando desconhecidos. Os QRs são payloads binários
(fingerprint combinado) em base64, para o tenant renderizar como QR code; a plataforma não gera
imagem (assumption da spec). Erros: `409 INSTANCE_NOT_CONNECTED`, `422 INVALID_CONTACT`,
`422 IDENTITY_NOT_RESOLVABLE`, `422 CANNOT_VERIFY_SELF`, `404 CONTACT_UNAVAILABLE`,
`503 SESSION_UNAVAILABLE`.

> Efeito colateral documentado: identidades de dispositivos ainda não conhecidos são gravadas no
> store da sessão durante a consulta (TOFU — comportamento padrão do protocolo, R8).

---

## Endpoints alterados

### `GET /instances/{instanceId}` e `GET /instances/{instanceId}/connection`

O objeto de conexão ganha os campos de configuração:

```json
{
  "connection": {
    "...campos da 002...": "...",
    "proxy_url": "socks5://user:***@203.0.113.10:1080",
    "passive_mode": false
  }
}
```

`proxy_url` sempre mascarado; `null` quando não configurado.
