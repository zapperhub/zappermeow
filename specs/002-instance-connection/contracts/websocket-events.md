# WebSocket Contract — Eventos de Conexão da Instância (002-instance-connection)

**Date**: 2026-08-13 | **Endpoint**: `GET /instances/{instanceId}/ws` | **HTTP**: [http-api.md](./http-api.md)

Canal em tempo real por instância. Nesta fatia transporta **apenas** eventos de pareamento e de
conexão; mensagens, recibos e demais eventos do WhatsApp entram em features futuras, sem quebrar
este contrato (o campo `type` é extensível por prefixo).

## Handshake

**Subprotocolo**: `zappermeow.v1` (obrigatório). O servidor ecoa o subprotocolo aceito.

**Autenticação** — uma das duas formas (R7):

| Cliente | Como |
| --- | --- |
| Servidor, CLI, integração | Header `Authorization: Bearer <jwt tenant>` ou `X-Api-Key: zmk_...` |
| Navegador | `Sec-WebSocket-Protocol: zappermeow.v1, bearer.<token>` — o token é o JWT ou a API key |

Token em query string é **recusado**: apareceria em logs de acesso e no proxy, violando o
Princípio II.

Falhas do handshake acontecem **antes** do upgrade, como resposta HTTP normal: `401 WS_UNAUTHORIZED`
(credencial ausente/inválida), `404` (instância inexistente ou de outro tenant), `403`
(tenant suspenso). Nenhum frame é entregue antes da autenticação (FR-042).

**Múltiplos ouvintes**: N conexões simultâneas por instância, todas recebendo os mesmos eventos
(FR-034). Não há limite por instância nesta fatia; a métrica `zappermeow_ws_clients` acompanha o
total.

**Limites e observabilidade**: o handshake é contabilizado no mesmo limitador GCRA das rotas de
conexão (por `api_key_id` ou `tenant_id`); excedida a cota, o servidor responde `429` **antes** do
upgrade — nunca abre a conexão para fechá-la em seguida. Como o endpoint é um handler chi fora do
huma, o request logging estruturado com `tenant_id`/`instance_id` é aplicado explicitamente nele
(Princípio VI), e não herdado dos middlewares da API.

## Envelope

Todo frame é JSON UTF-8 com a mesma forma:

```json
{
  "seq": 42,
  "type": "pairing.code",
  "instance_id": "018f...",
  "generation": 7,
  "occurred_at": "2026-08-13T12:00:03.120Z",
  "data": { }
}
```

| Campo | Significado |
| --- | --- |
| `seq` | Monotônico por instância. Permite deduplicar o overlap entre snapshot e stream e detectar buracos |
| `type` | Vocabulário fechado (abaixo), namespaced por ponto |
| `generation` | Geração do lease que produziu o evento; um frame com `generation` menor que o último visto é de dono antigo e deve ser ignorado |
| `occurred_at` | RFC 3339 UTC com milissegundos |
| `data` | Payload específico do tipo; `{}` quando não há |

O envelope HTTP (`status`/`data`/`timestamp`) **não** se aplica aqui: frames não são respostas HTTP.

## Sequência de abertura

1. Servidor assina o canal da instância e começa a bufferizar.
2. Envia **`state.snapshot`** como primeiro frame, sempre.
3. Drena o buffer descartando `seq` já refletidos no snapshot.
4. Segue em streaming.

Isso garante que quem chega no meio de um pareamento recebe o QR corrente em vez de esperar o
próximo (FR-032, cenário 4 da US1).

## Tipos de evento

### `state.snapshot` — sempre o primeiro frame

```json
{
  "seq": 41, "type": "state.snapshot", "instance_id": "018f...", "generation": 7,
  "occurred_at": "2026-08-13T12:00:00.000Z",
  "data": {
    "state": "pairing",
    "intent": "active",
    "device": null,
    "connected_at": null,
    "last_disconnect": { "at": "2026-08-13T11:18:31Z", "reason": "network" },
    "pairing": {
      "method": "qr",
      "code": "2@AbC...",
      "expires_at": "2026-08-13T12:00:20Z"
    }
  }
}
```

`pairing` é `null` quando não há tentativa em curso. O conteúdo de `data` espelha
`GET /instances/{id}/connection`, acrescido do código corrente.

### `pairing.code` — novo QR ou código de telefone

```json
{ "data": { "method": "qr", "code": "2@AbC...", "expires_at": "2026-08-13T12:00:23Z" } }
```

Emitido a cada renovação (primeiro código válido por 60s, seguintes por 20s — R2). Para
`method: "phone"` o `code` é o de 8 caracteres digitado no aparelho, emitido uma única vez.

O cliente é responsável por renderizar o QR a partir de `code` (a API não gera imagem).

### `pairing.succeeded`

```json
{ "data": { "device": { "jid": "5511999999999:11@s.whatsapp.net", "phone_number": "5511999999999",
  "push_name": "Suporte ACME", "platform": "android" }, "number_changed": false } }
```

`number_changed: true` quando o número pareado difere do anterior (FR-016).

### `pairing.expired`

```json
{ "data": { "method": "qr", "reason": "window_exhausted" } }
```

A janela se esgotou sem escaneamento (FR-011). A instância volta ao estado anterior; um novo
`connect` inicia outra rodada. Possíveis `reason`: `window_exhausted`, `cancelled`,
`replaced_by_new_attempt`, `worker_shutdown`.

> Uma conta que já atingiu o limite de dispositivos vinculados da Meta produz este mesmo evento: o
> bloqueio ocorre dentro do aplicativo do cliente, que nem chega a escanear, e a plataforma observa
> apenas a expiração normal.

### `pairing.failed`

```json
{ "data": { "reason": "scanned_without_multidevice" } }
```

Falhas reportadas pelo WhatsApp durante o pareamento (FR-019). `reason`:
`scanned_without_multidevice`, `client_outdated`, `pair_error`, `unexpected_state`.

### `connection.connected`

```json
{ "data": { "connected_at": "2026-08-13T12:00:31Z" } }
```

### `connection.disconnected`

```json
{ "data": { "reason": "network", "permanent": false, "at": "2026-08-13T12:10:00Z" } }
```

`permanent: false` → o sistema vai reconectar sozinho. `permanent: true` → parou de tentar; exige
ação humana. Vocabulário de `reason` em [../data-model.md](../data-model.md) §5.

### `connection.logged_out`

```json
{ "data": { "reason": "logged_out_from_phone", "from_phone": true } }
```

Sessão invalidada pelo aparelho ou pelo servidor (FR-029). A instância volta a `registered` e
exige novo pareamento. Não afeta outras instâncias do mesmo número.

### `connection.banned`

```json
{ "data": { "ban_code": 101, "expires_at": "2026-08-14T12:00:00Z" } }
```

`expires_at` é `null` quando o servidor não informa prazo (FR-030).

## Keepalive e encerramento

- **Ping/pong**: o servidor envia ping a cada 30s; ausência de pong em 10s fecha a conexão.
- **Revogação em vigor**: se a credencial usada no handshake for revogada ou o tenant suspenso, o
  servidor fecha com código `4403` (FR-042) — a autorização não é só de entrada.
- **Instância excluída**: fecha com código `4404`.
- **Nada de replay**: reconectar traz um novo `state.snapshot`, nunca o histórico do intervalo. A
  trilha persistida (`GET /connection/events`) é o caminho para o que passou.

## Códigos de fechamento

| Código | Significado |
| --- | --- |
| `1000` | Encerramento normal (cliente ou shutdown gracioso do servidor) |
| `1011` | Erro interno |
| `4403` | Credencial revogada ou tenant suspenso durante a sessão |
| `4404` | Instância excluída |
| `4429` | Cliente lento — buffer de saída estourado |
