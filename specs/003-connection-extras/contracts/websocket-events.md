# WebSocket Contract — Complementos de Conexão (003-connection-extras)

**Date**: 2026-08-14 | **Endpoint**: `GET /instances/{instanceId}/ws` | **Base**: [002 websocket-events.md](../../002-instance-connection/contracts/websocket-events.md)

Extensão **aditiva** do contrato da 002: mesmo handshake, mesmo subprotocolo (`zappermeow.v1`),
mesmo envelope (`seq`/`type`/`generation`/`occurred_at`/`data`), mesmos códigos de fechamento.
Clientes da 002 que ignoram tipos desconhecidos continuam funcionando sem mudança.

## Tipos de evento novos

### `pairing.passkey_challenge`

Emitido quando o WhatsApp exige a etapa de passkey durante uma tentativa de pareamento (FR-015).
A tentativa continua ativa; o front deve coletar a asserção do autenticador e submetê-la em
`POST .../pairing/passkey/response`.

```json
{
  "seq": 12,
  "type": "pairing.passkey_challenge",
  "instance_id": "018f...",
  "generation": 7,
  "occurred_at": "2026-08-14T12:00:03.120Z",
  "data": {
    "public_key": {
      "challenge": "<base64url>",
      "timeout": 60000,
      "rpId": "whatsapp.com",
      "allowCredentials": [ { "type": "public-key", "id": "<base64url>" } ],
      "userVerification": "required"
    },
    "attempt_expires_at": "2026-08-14T12:02:30Z"
  }
}
```

`data.public_key` é o objeto `publicKey` para `navigator.credentials.get()`, repassado **opaco**
da biblioteca (bytes em base64url sem padding) — a plataforma não interpreta nem valida campos.

> **Efeito no QR**: ao entrar na etapa de passkey, os QR codes já exibidos deixam de ser
> escaneáveis (a biblioteca rotaciona o segredo que os valida — research R7). O front deve
> abandonar a exibição de QR ao receber este frame; nenhum `pairing.code` novo virá nesta
> tentativa.

### `pairing.passkey_code`

Emitido quando a conferência visual é exigida (FR-017): o código deve ser mostrado ao dono do
número para comparação com o telefone, e confirmado via `POST .../pairing/passkey/confirm`.
**Não é emitido** quando a continuidade da sessão dispensa a conferência — nesse caso a
confirmação é automática (feita pela própria biblioteca) e o fluxo segue direto para
`pairing.succeeded`.

```json
{
  "type": "pairing.passkey_code",
  "data": {
    "code": "1ABC-2DEF",
    "attempt_expires_at": "2026-08-14T12:02:30Z"
  }
}
```

## Tipos alterados (vocabulário estendido)

### `state.snapshot` — fase de pareamento

O snapshot (sempre o primeiro frame) ganha, dentro do bloco de pareamento ativo, o discriminador
de fase (data-model §3):

| `pairing.phase` | Significado | Campos presentes |
| --- | --- | --- |
| `qr` | como na 002 | `code`, `expires_at` |
| `passkey_challenge` | desafio pendente | `public_key`, `attempt_expires_at` |
| `passkey_code` | código de conferência pendente | `code`, `attempt_expires_at` |

Um cliente que conecta no meio da etapa de passkey recebe a fase corrente e consegue continuar o
fluxo sem ter visto os frames anteriores (research R10).

### `pairing.failed` — motivo novo

`data.failure` ganha o valor `passkey_error` (falha em qualquer passo da etapa de passkey —
FR-019). Vocabulário completo: `scanned_without_multidevice`, `client_outdated`, `pair_error`,
`unexpected_state`, **`passkey_error`**.

### `connection.disconnected` — motivos novos

`data.reason` ganha três valores (data-model §2):

| `reason` | Quando | Reconecta sozinho? |
| --- | --- | --- |
| `stream_error` | stream encerrado com código desconhecido; `data.detail.stream_error_code` presente | sim |
| `proxy_connect_failed` | falha de dial/handshake através do proxy configurado | sim (sempre pelo proxy) |
| `proxy_updated` | desconexão comandada por mudança de configuração de proxy | sim (imediata, com a nova configuração) |

## O que não muda

Keepalive, encerramento, códigos de fechamento (4403 em revogação etc.), deduplicação por `seq`,
regra de `generation` e a sequência de abertura são exatamente os da 002. Nenhum frame existente
mudou de forma — apenas vocabulários cresceram, o que o contrato da 002 já declara extensível.
