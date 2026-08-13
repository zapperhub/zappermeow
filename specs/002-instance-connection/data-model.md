# Data Model — Conexão da Instância com o WhatsApp (002-instance-connection)

**Date**: 2026-08-13 | **Spec**: [spec.md](./spec.md) | **Research**: [research.md](./research.md)

Migração desta feature: `migrations/0002_instance_connection.{up,down}.sql` (golang-migrate,
embutida via `embed.FS`, aplicada no boot). As tabelas `whatsmeow_*` **não** aparecem aqui nem em
nenhuma migração nossa: são criadas e versionadas pelo próprio HyperMeow em
`Container.Upgrade(ctx)`, com tabela de versão própria (`whatsmeow_version`). Ordem obrigatória no
boot: migrações da API → `Container.Upgrade`.

---

## 1. `instances` (estendida)

A 001 criou a tabela com identidade e vínculo ao tenant. Esta feature acrescenta o estado de
conexão e a identidade do dispositivo pareado.

| Coluna | Tipo | Regras |
| --- | --- | --- |
| *(colunas da 001)* | | `id`, `tenant_id`, `name`, `state`, timestamps |
| `connection_state` | `text NOT NULL DEFAULT 'registrada'` | CHECK no conjunto da máquina de estados (§4) |
| `connection_intent` | `text NOT NULL DEFAULT 'parada'` | CHECK `('ativa','parada')` — intenção do usuário (R12) |
| `wa_jid` | `text NULL` | JID completo **com sufixo de dispositivo**; UNIQUE parcial `WHERE wa_jid IS NOT NULL` (R9) |
| `wa_lid` | `text NULL` | LID do dispositivo, quando informado |
| `phone_number` | `text NULL` | E.164 sem `+`; **sem unicidade** — vários dispositivos do mesmo número são legítimos (FR-017) |
| `push_name` | `text NULL` | Nome de exibição do WhatsApp |
| `platform` | `text NULL` | Plataforma reportada pelo aparelho |
| `business_name` | `text NULL` | Preenchido quando a conta é Business |
| `paired_at` | `timestamptz NULL` | Momento do último pareamento bem-sucedido |
| `connected_at` | `timestamptz NULL` | Início da conexão corrente; `NULL` quando offline |
| `last_disconnect_at` | `timestamptz NULL` | Horário da última queda |
| `last_disconnect_reason` | `text NULL` | Motivo canônico (§5) |
| `ban_expires_at` | `timestamptz NULL` | Preenchido em `TemporaryBan` quando o servidor informa prazo |

**Índices**: `UNIQUE (wa_jid) WHERE wa_jid IS NOT NULL`; `(tenant_id, phone_number)` para responder
"quais outras instâncias compartilham este número" (FR-018) sem varredura.

**Notas**

- O campo `state` da 001 (`registrada`) é **absorvido** por `connection_state`; a migração copia o
  valor e remove a coluna antiga — o vocabulário passa a ser um só.
- Exclusão de instância continua em cascata para `api_keys`, e agora também para `session_leases` e
  `connection_events`. O material da sessão em `whatsmeow_*` **não** tem FK para `instances` (é
  chaveado por JID): apagá-lo é responsabilidade do fluxo de exclusão (FR-007), que desloga antes
  de remover o registro.

---

## 2. `session_leases` (nova)

Fonte de verdade sobre quem detém cada sessão. Uma linha por instância, criada de forma preguiçosa
(`INSERT ... ON CONFLICT DO NOTHING`) no primeiro comando de conexão.

| Coluna | Tipo | Regras |
| --- | --- | --- |
| `instance_id` | `uuid PRIMARY KEY` | FK → `instances(id) ON DELETE CASCADE` |
| `worker_id` | `text NULL` | Identidade do processo dono; `NULL` = livre |
| `grpc_addr` | `text NULL` | Endereço gRPC do dono na rede privada |
| `generation` | `bigint NOT NULL DEFAULT 0` | Fencing token; **incrementa a cada aquisição** |
| `heartbeat_at` | `timestamptz NULL` | Renovado a cada 10s pelo dono |
| `desired_state` | `text NOT NULL DEFAULT 'stopped'` | CHECK `('running','stopped','draining')` — estado efetivo (R12) |
| `updated_at` | `timestamptz NOT NULL DEFAULT now()` | |

**Índices**: `(desired_state, heartbeat_at)` — sustenta a varredura de reconciliação, que procura
leases `running` órfãos.

**Aquisição atômica** (o `RETURNING` é o que garante um único vencedor entre workers concorrentes):

```sql
UPDATE session_leases
   SET worker_id = $1, grpc_addr = $2, generation = generation + 1, heartbeat_at = now(), updated_at = now()
 WHERE instance_id = $3
   AND desired_state = 'running'
   AND (worker_id IS NULL OR heartbeat_at < now() - interval '30 seconds')
RETURNING generation;
```

**Invariantes**

- `generation` é monotônico e nunca reutilizado — é o que permite rejeitar o dono antigo (FR-025).
- Liberar o lease (SIGTERM) zera `worker_id`, `grpc_addr` e `heartbeat_at`, **preservando**
  `generation` e `desired_state`.
- `desired_state = 'stopped'` torna a linha invisível para a reconciliação; é como a suspensão de
  tenant e o `disconnect` explícito apagam a sessão do radar (FR-027, FR-041).

---

## 3. `connection_events` (nova)

Trilha consultável de transições, com retenção limitada (FR-036, FR-037).

| Coluna | Tipo | Regras |
| --- | --- | --- |
| `id` | `bigserial PRIMARY KEY` | Ordem de gravação |
| `instance_id` | `uuid NOT NULL` | FK → `instances(id) ON DELETE CASCADE` |
| `type` | `text NOT NULL` | Vocabulário fechado (§5) |
| `reason` | `text NULL` | Motivo canônico, quando aplicável |
| `detail` | `jsonb NULL` | Dados auxiliares — **nunca** material criptográfico (FR-043) |
| `occurred_at` | `timestamptz NOT NULL DEFAULT now()` | |

**Índices**: `(instance_id, occurred_at DESC, id DESC)` para a listagem paginada;
`(occurred_at)` para a varredura de retenção.

`detail` carrega, por exemplo, `{"previous_phone":"...","new_phone":"..."}` em `number_changed` ou
`{"ban_code":101,"expires_at":"..."}` em `banned`. Nunca chaves, tokens ou QR codes.

---

## 4. Máquina de estados

```
                    ┌──────────────┐
                    │  registrada  │◄──────────── logout ────────────┐
                    └──────┬───────┘                                 │
                     connect (sem sessão salva)                      │
                           ▼                                         │
                    ┌──────────────┐   expira / falha                │
              ┌────►│   pareando   │──────────────► (volta ao estado anterior)
              │     └──────┬───────┘                                 │
       connect│         PairSuccess                                  │
              │            ▼                                         │
              │     ┌──────────────┐   Connected   ┌──────────────┐  │
              └─────│  conectando  │──────────────►│  conectada   │──┘
                    └──────┬───────┘               └──────┬───────┘
                       ▲   │                              │
      queda transitória│   │ disconnect explícito         │ queda transitória
                       │   ▼                              ▼
                    ┌──────────────┐              (volta a conectando)
                    │ desconectada │
                    └──────────────┘
                           ▲
                           │ invalidação sem estado próprio
                           │ (session_replaced, client_outdated, ...)
   invalidação ────────────┼───────────────► ┌───────────┐   ┌─────────┐
   (LoggedOut / TemporaryBan)                │ deslogada │   │ banida  │
                                             └───────────┘   └─────────┘
```

| Estado | Significado | `connection_intent` típico | Reconecta sozinho? |
| --- | --- | --- | --- |
| `registrada` | Sem material de sessão; exige pareamento | `parada` | não |
| `pareando` | Tentativa ativa (QR ou código) | `ativa` | n/a |
| `conectando` | Pareada, tentando estabelecer conexão | `ativa` | sim |
| `conectada` | Online | `ativa` | — |
| `desconectada` | Offline por decisão explícita **ou** por invalidação sem estado próprio | `parada` (explícita) / `ativa` (invalidação) | só se `last_disconnect_reason` não for permanente |
| `deslogada` | Sessão removida pelo aparelho | `ativa` (preservada) | **não** |
| `banida` | Banimento temporário informado pelo WhatsApp | `ativa` (preservada) | **não** |

**Regra de retomada** (R3): a reconciliação só adota instâncias com `desired_state = 'running'`
**e** `last_disconnect_reason` fora do conjunto permanente. Um comando explícito de conexão limpa
`last_disconnect_reason` e reabilita a adoção — é o que faz FR-031 funcionar sem estado extra.

---

## 5. Vocabulários canônicos

**`connection_events.type`**: `pairing_started`, `pairing_succeeded`, `pairing_expired`,
`pairing_failed`, `connected`, `disconnected`, `logged_out`, `banned`, `number_changed`,
`lease_acquired`, `lease_lost`, `deleted`.

**`reason` / `last_disconnect_reason`**:

| Motivo | Origem | Permanente? |
| --- | --- | --- |
| `user_requested` | comando de desconexão | sim (intenção `parada`) |
| `network` | `events.Disconnected` | não |
| `keepalive_timeout` | `events.KeepAliveTimeout` | não |
| `worker_lost` | lease expirado / worker morto | não |
| `logged_out_from_phone` | `events.LoggedOut` | sim |
| `temporary_ban` | `events.TemporaryBan` | sim |
| `session_replaced` | `events.StreamReplaced` | sim (**e alarme** — R3) |
| `client_outdated` | `events.ClientOutdated` | sim |
| `connect_failure` | `events.ConnectFailure` | sim |
| `cat_refresh_failed` | `events.CATRefreshError` | sim |
| `logout_local_only` | logout sem conexão (R10) | sim |
| `tenant_suspended` | suspensão do tenant | sim, enquanto durar |

---

## 6. Chaves em Redis

Nenhum dado durável — tudo é cache, sinalização ou efêmero (Princípio I: um Redis, vários papéis).

| Chave / canal | Papel | TTL |
| --- | --- | --- |
| `events:{instance_id}` | pub/sub de eventos worker → réplicas da api (WS) | — |
| `sessions:claim` | pub/sub: acorda workers para disputar um lease livre (R6) | — |
| `sessions:stop` | pub/sub: manda o dono parar a sessão imediatamente | — |
| `wa:seq:{instance_id}` | `INCR` — número de sequência dos eventos no WS (R8) | 24h desde o último uso |
| `wa:pairing:{instance_id}` | código de pareamento corrente + validade (snapshot do WS) | validade do código |
| `wa:lease:{instance_id}` | cache do `grpc_addr` + `generation` para o caminho síncrono | 5s |

O `seq` do WebSocket é **independente** do `id` de `connection_events`: nem todo evento publicado é
persistido (códigos de QR não são) e nem toda persistência gera publicação.

---

## 7. Entidades da spec → modelo

| Entidade (spec) | Materialização |
| --- | --- |
| Instância (estendida) | colunas novas em `instances` (§1) |
| Sessão | tabelas `whatsmeow_*`, geridas pelo HyperMeow; referenciadas por `instances.wa_jid` |
| Posse de sessão | `session_leases` (§2) |
| Tentativa de pareamento | estado efêmero no worker + `wa:pairing:{id}` no Redis; marcos em `connection_events` |
| Evento de conexão | `connection_events` (§3) |

A tentativa de pareamento é deliberadamente **não persistida**: ela vive ≤3 minutos, morre com o
processo e, se o worker cair no meio, o comportamento correto é a tentativa terminar — não
ressuscitar em outro worker com um QR que o servidor já invalidou.
