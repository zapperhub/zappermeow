# Data Model — Complementos de Conexão da Instância (003-connection-extras)

**Date**: 2026-08-14 | **Spec**: [spec.md](./spec.md) | **Research**: [research.md](./research.md)

Migração desta feature: `migrations/0003_connection_extras.{up,down}.sql` (golang-migrate,
embutida via `embed.FS`, aplicada no boot). Nenhuma tabela nova, nenhuma chave Redis nova — a
feature estende o modelo da [002](../002-instance-connection/data-model.md).

---

## 1. `instances` (estendida)

| Coluna | Tipo | Regras |
| --- | --- | --- |
| *(colunas da 001/002)* | | inalteradas |
| `proxy_url` | `text NULL` | URL completa do proxy de saída (`http`, `https` ou `socks5`), com credenciais embutidas quando houver; `NULL` = conexão direta. Validada na aplicação (§5), não por CHECK — o formato de URL não é expressável em CHECK sem duplicar a validação |
| `passive_mode` | `boolean NOT NULL DEFAULT false` | modo passivo desejado; aplicado após cada `Connected` (R6) |

**Sem índices novos**: as duas colunas são atributos lidos por PK, nunca critério de busca.

**Regra de leitura (FR-007)**: `proxy_url` **nunca** sai da camada de domínio sem passar por
`MaskProxyURL` (§5). As queries sqlc devolvem a URL crua apenas para o caminho
worker → construção do cliente; handlers recebem o valor já mascarado do serviço de domínio.

**Interação com a máquina de estados da 002**: nenhuma transição nova. A mudança de proxy com
sessão ativa percorre `connected → disconnected(reason=proxy_updated) → connecting → connected`
usando os estados existentes; a tentativa de pareamento em curso, se houver, termina com
`pairing_expired (cancelled)` (R2).

---

## 2. `connection_events` (vocabulário estendido)

A tabela não muda de forma; a migração `0003` **recria o CHECK** de `type` (o `0002` fixou a lista
em `connection_events.type CHECK (type IN (...))`) acrescentando os tipos novos. O `down` restaura
o CHECK anterior — e por isso DEVE apagar antes as linhas com os tipos novos, senão o `down` falha.

**Tipos novos** (somam-se aos 12 da 002):

| `type` | Quando | `detail` |
| --- | --- | --- |
| `stream_error` | encerramento de stream com código desconhecido (R9) | `{"stream_error_code": "<code>"}` — nunca o node cru |
| `manual_login_reconnect` | pedido de reconexão manual pós-login recebido (R5) | — |
| `proxy_updated` | configuração de proxy definida/alterada/removida | `{"proxy": "<url mascarada>"}` ou `{"proxy": null}` |
| `passive_mode_updated` | modo passivo ligado/desligado | `{"passive": true/false}` |
| `passkey_challenge` | WhatsApp exigiu a etapa de passkey no pareamento | — (o desafio **não** é persistido) |
| `passkey_responded` | resposta do autenticador aceita pela plataforma | — |
| `passkey_confirmed` | confirmação enviada (pelo tenant ou automática) | `{"automatic": true/false}` |

**Motivos novos** em `reason` / `last_disconnect_reason` (somam-se à tabela §5 da 002):

| Motivo | Origem | Permanente? |
| --- | --- | --- |
| `stream_error` | `events.StreamError` (código desconhecido) | **não** |
| `proxy_connect_failed` | falha de dial/handshake com proxy configurado (R3) | **não** |
| `proxy_updated` | desconexão comandada pela mudança de proxy (R2) | não (religa em seguida) |

A falha da etapa de passkey **não** cria motivo de desconexão: ela é falha de **pareamento** e usa
o caminho existente `pairing_failed`, com o vocabulário de falha estendido (§4).

> **Nota**: o token `proxy_updated` aparece nos **dois** vocabulários com papéis distintos — como
> `type` (o tenant alterou a configuração) e como `reason` da desconexão comandada pela mudança.
> Os conjuntos permanecem separados nos CHECKs e nos testes; não misturar.

---

## 3. Snapshot de pareamento em Redis (`wa:pairing:{instance_id}`, estendido)

Mesma chave e mesmo TTL da 002 (validade da tentativa). O valor ganha um discriminador de **fase**
para que o primeiro frame de um cliente WS tardio reflita a etapa corrente (R10):

| Campo | Fase `qr` | Fase `passkey_challenge` | Fase `passkey_code` |
| --- | --- | --- | --- |
| `phase` | `"qr"` | `"passkey_challenge"` | `"passkey_code"` |
| `code` | QR corrente | — | código `XXXX-XXXX` |
| `expires_at` | validade do QR | validade da tentativa | validade da tentativa |
| `challenge` | — | desafio WebAuthn (JSON opaco) | — |

O desafio vive **apenas** aqui e na memória do worker — nunca no Postgres. Ele morre com a
tentativa (TTL), com a resposta aceita (transição de fase) ou com o worker (mesma justificativa da
002 §7: tentativa não ressuscita).

---

## 4. Vocabulários novos no domínio (`internal/wa`, `internal/domain`)

- `wa.EventKind`: `+ passkey_challenge`, `+ passkey_code` (emitidos pelo `pumpQR` — R7).
- `wa.PairingFailure`: `+ passkey_error` (item `error` do canal de QR vindo de
  `PairPasskeyError`).
- `wa.PairingExpiry`: sem valores novos — `cancelled` já cobre o encerramento por mudança de
  proxy (R2).
- `domain.DisconnectReason`: `+ stream_error`, `+ proxy_connect_failed`, `+ proxy_updated`
  (tabela §2; os três não-permanentes).

> **Idioma**: como na 002, identificadores em banco, API e eventos são em inglês; a prosa da spec
> usa o vocabulário de domínio em português.

---

## 5. Regras de validação (aplicação, não schema)

**`proxy_url`** (`domain/proxy.go`, espelhando o contrato de `SetProxyAddress` — R1):

1. Parse por `url.Parse`; falha → `invalid_proxy_url`.
2. Esquema ∈ {`http`, `https`, `socks5`}; outro → `unsupported_proxy_scheme`.
3. Host não vazio; porta, usuário e senha opcionais.
4. Comprimento máximo 1024 caracteres (limite sanitário; nenhum proxy legítimo excede).

**`MaskProxyURL`**: preserva `esquema://usuário@host:porta`; senha presente vira `***`
(`socks5://user:***@1.2.3.4:1080`). Aplicada em toda resposta, evento, trilha e log.

**Contato da consulta de códigos de verificação** (R8):

1. Se contém `@lid` → usa direto (normalizado sem sufixo de device).
2. Se é telefone (E.164 sem `+`, mesmo formato do `PairPhone` da 002) → resolve via store
   (`GetLIDForPN`); sem mapeamento → `identity_not_resolvable`.
3. Igual ao LID da própria instância → `cannot_verify_self`.
4. Qualquer outro formato → `invalid_contact`.

---

## 6. Entidades da spec → modelo

| Entidade (spec) | Materialização |
| --- | --- |
| Configuração de conexão da instância | colunas `proxy_url` + `passive_mode` em `instances` (§1) |
| Etapa de passkey da tentativa de pareamento | fase no snapshot Redis (§3) + estado em memória do worker; marcos em `connection_events` (§2) |
| Registro de trilha (estendido) | tipos e motivos novos em `connection_events` (§2) |
| Códigos de verificação de identidade | **não persistido** — resultado de RPC, montado sob demanda (R8); mudanças de identidade do contato invalidariam qualquer cache |
