---

description: "Task list for feature implementation"
---

# Tasks: Conexão da Instância com o WhatsApp

**Input**: Design documents from `/specs/002-instance-connection/`

**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md), [data-model.md](./data-model.md), [contracts/](./contracts/)

**Tests**: incluídos e **obrigatórios**. O Princípio V da constituição exige testes contra Postgres e Redis reais (testcontainers) e pipeline verde como pré-condição de merge; não é opção desta feature.

**Organization**: agrupadas por user story, para implementação e validação independentes.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: pode rodar em paralelo (arquivos distintos, sem dependência pendente)
- **[Story]**: user story a que a tarefa pertence (US1…US7)
- Todo caminho de arquivo é relativo à raiz do repositório

## Path Conventions

Layout fixado na constituição: `cmd/zappermeow/` + `internal/{api, worker, lease, events, wa, domain, store, config}` + `proto/` + `migrations/` + `deploy/`. Testes co-localizados por pacote (convenção Go).

---

## Phase 1: Setup (Shared Infrastructure)

- [X] T001 Adicionar dependências novas ao `go.mod` com justificativa no PR: `github.com/polymorfa/hypermeow` (pseudo-versão pinada em `@main`), `google.golang.org/grpc`, `google.golang.org/protobuf`, `github.com/coder/websocket`
- [X] T002 [P] Declarar `tool` directives no `go.mod` para `google.golang.org/protobuf/cmd/protoc-gen-go` e `google.golang.org/grpc/cmd/protoc-gen-go-grpc`, com alvo de geração documentado no README de desenvolvimento (decisão R14)
- [X] T003 [P] Estender `.github/workflows/ci.yml` com verificação de código gerado do protobuf, espelhando a do sqlc: gerar e falhar em `git diff --exit-code -- internal/pb`
- [X] T004 [P] Registrar o subcomando `session-worker` no dispatch de `cmd/zappermeow/main.go`, substituindo o stub "not implemented" da 001

---

## Phase 2: Foundational (Blocking Prerequisites)

**⚠️ Nenhuma user story pode começar antes desta fase.** Aqui nascem o schema, a fronteira com o HyperMeow, o lease, o barramento de eventos e o esqueleto do worker.

### Schema e configuração

- [X] T005 Escrever `migrations/0002_instance_connection.up.sql` e `.down.sql` conforme [data-model.md](./data-model.md): colunas de conexão em `instances` (`connection_state`, `connection_intent`, `wa_jid`, `wa_lid`, `phone_number`, `push_name`, `platform`, `business_name`, `paired_at`, `connected_at`, `last_disconnect_at`, `last_disconnect_reason`, `ban_expires_at`), migração do antigo `instances.state` para `connection_state` e remoção da coluna antiga, tabelas `session_leases` e `connection_events` com CHECKs, FKs `ON DELETE CASCADE` e índices — incluindo `UNIQUE (wa_jid) WHERE wa_jid IS NOT NULL` e `(tenant_id, phone_number)`
- [X] T006 Estender `internal/config/config.go` com as chaves da feature — `ZAPPERMEOW_WORKER_ADVERTISE_ADDR`, `ZAPPERMEOW_WORKER_GRPC_LISTEN_ADDR`, `ZAPPERMEOW_MAX_SESSIONS_PER_WORKER` (default 200), `ZAPPERMEOW_PAIRING_WINDOW` (default 180s), `ZAPPERMEOW_LEASE_HEARTBEAT_INTERVAL` (10s), `ZAPPERMEOW_LEASE_EXPIRY` (30s), `ZAPPERMEOW_RECONCILE_INTERVAL` (15s), `ZAPPERMEOW_CONNECTION_EVENTS_RETENTION` (720h) — com unit tests de defaults e overrides
- [X] T007 [P] Escrever as queries sqlc em `internal/store/queries/connection.sql`, `leases.sql` e `connection_events.sql` — atualização de estado da instância, identidade do device, aquisição atômica de lease com `RETURNING generation`, heartbeat em lote, release, varredura de órfãos, insert e listagem paginada de eventos, DELETE de retenção — e rodar `sqlc generate`. As queries **existentes** de `instances.sql` e `api_keys.sql` já foram adaptadas ao rename `state` → `connection_state`

### Domínio e fronteira com o WhatsApp

- [X] T008 [P] Criar `internal/domain/connectionevent.go` com os vocabulários canônicos fechados de `type` e `reason` (§5 do data-model), incluindo o conjunto de motivos **permanentes**, com unit tests
- [X] T009 [P] Estender `internal/domain/instance.go` com os estados de conexão, a intenção (`active`/`stopped`), a identidade do dispositivo e as transições válidas da máquina de estados (§4 do data-model), com unit tests table-driven cobrindo transições legais e ilegais
- [X] T010 [P] Definir a interface `wa.Session` em `internal/wa/session.go` (`Connect`, `Disconnect`, `Logout`, `QRChannel`, `PairPhone`, `Events`, `Status`) e os tipos de evento de domínio que ela emite
- [X] T011 Implementar `internal/wa/classify.go`: função pura evento do HyperMeow → `(kind, state, reason)` conforme a tabela da decisão R3, com unit tests cobrindo **todos** os eventos listados, um cross-check provando que a tabela e a interface `events.PermanentDisconnect` da biblioteca nunca divergem sobre permanência, e um default defensivo que trata como permanente qualquer evento desconhecido que implemente a interface
- [X] T012 Implementar `internal/wa/hypermeow.go`: container compartilhando o pool pgx via `stdlib.OpenDBFromPool` + `sqlstore.NewWithDB(db, "pgx", waLog)` e `Container.Upgrade` no boot (decisão R1), resolução device↔instância (`GetDevice` por JID salvo, `NewDevice` quando ausente — R9), criação do `whatsmeow.Client` com `EnableAutoReconnect` e `AutoReconnectHook` aplicando teto de 60s, jitter e veto em motivo permanente (R4)
- [X] T013 [P] Implementar `internal/wa/fake.go`: `FakeSession` encenando sequências determinísticas (QR → PairSuccess, Disconnected, LoggedOut, TemporaryBan, StreamReplaced, expiração de janela) para os testes de integração (R13)

### Lease, eventos e transporte

- [X] T014 Implementar o pacote `internal/lease/`: aquisição atômica com `RETURNING generation`, heartbeat em lote a cada 10s, release no shutdown, varredura de órfãos e verificação de fencing — com testes de integração contra Postgres real (testcontainers) cobrindo dois "workers" concorrentes disputando o mesmo lease e provando vencedor único
- [X] T015 [P] Implementar o pacote `internal/events/`: envelope (`seq`, `type`, `instance_id`, `generation`, `occurred_at`, `data`) conforme [websocket-events.md](./contracts/websocket-events.md), publisher no worker (`INCR wa:seq:{id}` + `PUBLISH events:{id}`, escrita de `wa:pairing:{id}` com TTL) e subscriber na api — com testes de integração contra Redis real
- [X] T016 Escrever `proto/session/v1/session.proto` conforme [session-grpc.md](./contracts/session-grpc.md) e gerar `internal/pb/sessionv1/`
- [X] T017 Implementar `internal/worker/grpcserver.go`: servidor gRPC do `SessionService` com verificação de fencing em toda RPC (`FAILED_PRECONDITION` + `WRONG_GENERATION`) e mapeamento de erros da tabela do contrato
- [X] T018 Implementar `internal/worker/supervisor.go` e `cmd/zappermeow/session_worker.go`: boot do worker (store WA, servidor gRPC, registro do `grpc_addr` no lease), ciclo de vida das sessões possuídas, shutdown gracioso em SIGTERM (draining → release dos leases → desconexão limpa)
- [X] T019 Implementar `internal/api/sessionclient/`: resolve o lease (com cache `wa:lease:{id}` de 5s), disca o `grpc_addr` do dono, e em `WRONG_GENERATION`/falha de conexão invalida o cache, relê o lease e **retenta uma única vez** no novo dono antes de devolver erro (R6)
- [X] T020 Implementar `internal/api/ws/`: upgrade com `coder/websocket`, subprotocolo obrigatório `zappermeow.v1`, autenticação por header `Authorization`/`X-Api-Key` **antes** do upgrade, ping a cada 30s com timeout de pong de 10s, códigos de fechamento `1000`/`1011`/`4403`/`4404`/`4429` — token em query string deve ser explicitamente recusado (R7)
- [X] T021 Estender o middleware de autenticação em `internal/api/middleware/` para aceitar, nas rotas de conexão, **JWT de tenant ou API key da instância**, sempre validando que a credencial resolve para a instância da URL (FR-039, FR-040), com testes de integração de isolamento
- [X] T022 Aplicar rate limiting às rotas de conexão em `internal/api/routes.go`: grupo huma próprio combinando a autenticação dupla (T021) com limitador GCRA — chaveado por `api_key_id` quando a credencial é a key da instância (reaproveitando `NewOperationalRateLimiter` e a cota `ZAPPERMEOW_OP_RATE_LIMIT` da 001) e por `tenant_id` quando é JWT de tenant — com teste de integração provando que a cota consumida por uma instância não afeta outra do mesmo tenant (Princípio II: rate limiting DEVE proteger todas as rotas operacionais)
- [X] T023 Aplicar observabilidade e limite ao endpoint WebSocket em `internal/api/ws/`: por ser handler chi puro, ele não atravessa os middlewares do huma — replicar explicitamente o request logging slog com `tenant_id`/`instance_id` (Princípio VI) e contabilizar o handshake no mesmo limitador GCRA das rotas de conexão, recusando com `429` **antes** do upgrade (Princípio II), com teste de integração

**Checkpoint**: worker sobe, adquire lease, responde gRPC e publica eventos; a api autentica, limita e faz upgrade de WebSocket. Nenhuma story está pronta ainda.

---

## Phase 3: User Story 1 - Parear por QR e ver conectada (Priority: P1) 🎯 MVP

**Goal**: uma instância registrada vira um número online — QR renovado no WebSocket, escaneamento, identidade do dispositivo persistida.

**Independent Test**: com um tenant e uma instância registrada, chamar `connect`, abrir o WS, escanear com um número de teste e ver `conectada` com `device.jid` contendo o sufixo de dispositivo.

- [X] T024 [US1] Implementar o fluxo de pareamento por QR em `internal/worker/session.go`: `GetQRChannel` **antes** do `Connect`, loop dedicado que drena o canal sem bloquear (a biblioteca fecha o canal e desconecta se o consumidor demorar — R2), publicação de cada código como `pairing.code` com `expires_at`, e encerramento da janela por cancelamento de contexto no teto configurado
- [X] T025 [US1] Implementar `Connect` no `SessionService` (`internal/worker/grpcserver.go` + `session.go`): inicia pareamento quando não há device salvo, reconecta quando há, devolvendo `pairing_started` e `pairing_expires_at`
- [X] T026 [US1] Implementar a persistência de `PairSuccess` em `internal/domain/services/connection.go`: grava `wa_jid` (JID **completo com sufixo de dispositivo**), `wa_lid`, `phone_number`, `push_name`, `platform`, `business_name`, `paired_at`, transiciona para `conectada` e registra `pairing_started`/`pairing_succeeded`/`connected` em `connection_events`
- [X] T027 [US1] Implementar `POST /instances/{instanceId}/connect` em `internal/api/handlers/connection.go`: grava `connection_intent = active`, limpa `last_disconnect_reason`, faz upsert do lease com `desired_state = running`, chama o dono por gRPC **ou** publica em `sessions:claim` quando não há dono, e responde `202` conforme o contrato
- [X] T028 [US1] Implementar a assinatura de `sessions:claim` em `internal/worker/reconcile.go`: ao receber um `instance_id`, tentar a aquisição atômica do lease imediatamente (garante o QR em ≤5s — SC-001)
- [X] T029 [US1] Implementar o handler WS de eventos da instância em `internal/api/ws/`: assinar **antes** de ler o snapshot, enviar `state.snapshot` como primeiro frame (estado do Postgres + código corrente de `wa:pairing:{id}`), drenar o buffer descartando `seq` já refletidos e seguir em streaming (R8, FR-032)
- [X] T030 [US1] Implementar a expiração da tentativa em `internal/worker/session.go`: ao esgotar a janela, publicar `pairing.expired` com `reason: window_exhausted`, registrar na trilha e devolver a instância ao estado anterior (FR-011)
- [X] T031 [US1] Emitir `pairing.failed` em `internal/worker/session.go` para os desfechos de erro do canal de QR — `QRChannelScannedWithoutMultidevice`, `QRChannelClientOutdated`, `QRChannelEventError` e `QRChannelErrUnexpectedEvent` — mapeando cada um para os `reason` do contrato, registrando `pairing_failed` na trilha e devolvendo a instância ao estado anterior (FR-019)
- [X] T032 [P] [US1] Testes de integração em `internal/worker/session_test.go` com `FakeSession` + Postgres/Redis reais: QR emitido e renovado, pareamento bem-sucedido persiste a identidade, expiração devolve ao estado anterior
- [X] T033 [P] [US1] Testes de integração em `internal/api/ws_test.go`: snapshot inicial com QR corrente para cliente que chega atrasado, dois clientes simultâneos recebendo os mesmos eventos, dedupe por `seq` sem duplicar nem perder frames
- [X] T034 [P] [US1] Testes de integração em `internal/api/connection_test.go`: `connect` em instância de outro tenant responde `404` sem confirmar existência; `connect` em instância já conectada é idempotente

**Checkpoint**: US1 entregue — a plataforma coloca um número real online. É o MVP.

---

## Phase 4: User Story 2 - Controlar o ciclo de vida da conexão (Priority: P2)

**Goal**: desconectar preservando o pareamento, reconectar sem QR, deslogar apagando o material, excluir sem deixar sessão órfã.

**Independent Test**: com uma instância conectada, desconectar → reconectar sem QR → deslogar → confirmar que o próximo `connect` exige QR novo; excluir uma instância conectada e conferir que ela é deslogada antes da remoção.

- [X] T035 [US2] Implementar `Disconnect` no worker e o handler `POST /instances/{instanceId}/disconnect`: `connection_intent = stopped`, `desired_state = stopped`, desconexão limpa, evento `disconnected` com `reason: user_requested`, resposta `202` idempotente
- [X] T036 [US2] Implementar `Logout` no worker: caminho remoto (`Client.Logout` com o dispositivo removido no servidor) e, quando a sessão está offline, conexão temporária de 15s e fallback local (`Store.Delete` + motivo `logout_local_only`) — a biblioteca aborta sem apagar nada se o IQ falhar (R10)
- [X] T037 [US2] Implementar `POST /instances/{instanceId}/logout` em `internal/api/handlers/connection.go`: devolve `logout_mode` (`remote` | `local_only`), limpa a identidade do dispositivo da instância, volta a `registrada` e registra `logged_out` na trilha
- [X] T038 [US2] Alterar `DELETE /instances/{instanceId}` em `internal/api/handlers/instances.go`: desconecta e desloga antes de remover, cancela pareamento em curso, encerra clientes WS com código `4404` e prossegue com a exclusão mesmo se o logout remoto falhar (FR-007)
- [X] T039 [US2] Garantir a reconexão sem novo pareamento em `internal/worker/session.go`: com `wa_jid` salvo, o boot da sessão usa `container.GetDevice` e nunca abre canal de QR
- [X] T040 [P] [US2] Testes de integração em `internal/worker/supervisor_test.go`: desconectar/reconectar preserva o device; logout apaga o material e o próximo connect volta a parear; logout offline devolve `local_only`
- [X] T041 [P] [US2] Teste de integração da exclusão limpa em `internal/api/connection_test.go` e `internal/worker/supervisor_test.go`: instância conectada excluída não deixa lease, eventos órfãos nem sessão ativa

---

## Phase 5: User Story 3 - Acompanhar estado atual e histórico (Priority: P3)

**Goal**: estado consultável e trilha investigável, com retenção limitada.

**Independent Test**: com uma instância que passou por conexão e queda, consultar estado (ver motivo da última desconexão) e trilha (ver transições em ordem cronológica).

- [X] T042 [US3] Implementar `GET /instances/{instanceId}/connection` em `internal/api/handlers/connection.go` conforme o contrato: estado, intenção, `connected_at`, `device` (ou `null`), `last_disconnect`, `ban_expires_at` e `shares_number_with` (outras instâncias do mesmo tenant com o mesmo número — FR-018)
- [X] T043 [US3] Implementar `GET /instances/{instanceId}/connection/events` com paginação por cursor opaco (`before`), `limit` de 1–200 e filtro opcional por `type`
- [X] T044 [US3] Implementar a gravação da trilha em `internal/domain/services/connection.go` para todos os tipos do vocabulário, garantindo que `detail` nunca carregue QR, token ou material de sessão (FR-043)
- [X] T045 [US3] Implementar a varredura de retenção em `internal/worker/retention.go`: ticker diário sob `pg_try_advisory_lock`, `DELETE` por `occurred_at` conforme `CONNECTION_EVENTS_RETENTION` (R11)
- [X] T046 [P] [US3] Testes de integração em `internal/api/connection_test.go`: instância nunca pareada responde `200` com `state: registrada` e `device: null` (não erro); instância de outro tenant responde `404`
- [X] T047 [P] [US3] Testes de integração em `internal/worker/retention_test.go`: eventos além da retenção somem, os dentro do período permanecem, e dois workers concorrentes não executam a varredura em duplicidade
- [X] T048 [P] [US3] Teste de integração da paginação da trilha em `internal/api/connection_test.go`: ordem cronológica inversa e cursor estável sob inserções concorrentes

---

## Phase 6: User Story 4 - Continuidade automática com posse exclusiva (Priority: P4)

**Goal**: a sessão sobrevive a deploy, morte de worker e reinício total — sempre com um único dono.

**Independent Test**: matar abruptamente o processo dono e verificar que a instância volta a `conectada` sozinha em ≤60s, com `generation` incrementada.

- [X] T049 [US4] Implementar o loop de reconciliação em `internal/worker/reconcile.go`: tick de 15s, adoção de leases `running` órfãos respeitando `MAX_SESSIONS_PER_WORKER`, ignorando instâncias cujo `last_disconnect_reason` é permanente
- [X] T050 [US4] Implementar o heartbeat em lote no worker (um único `UPDATE` para todos os leases possuídos, a cada 10s) e a perda de posse: ao falhar o heartbeat, encerrar as sessões correspondentes antes que outro worker as adote
- [X] T051 [US4] Implementar o drain no SIGTERM em `internal/worker/supervisor.go`: marcar `draining`, soltar os leases (`worker_id`/`grpc_addr`/`heartbeat_at` nulos, preservando `generation` e `desired_state`), desconectar limpo e só então encerrar
- [X] T052 [US4] Implementar a restauração de intenção no boot: instâncias com `desired_state = running` são adotadas e reconectadas; com `stopped` permanecem offline (FR-027)
- [X] T053 [US4] Implementar a cascata de suspensão de tenant em `internal/domain/services/`: `suspend` grava `desired_state = stopped` nos leases do tenant e publica `sessions:stop`; `activate` recalcula a partir de `connection_intent`, restaurando o que o usuário queria (R12, FR-041)
- [X] T054 [US4] Implementar a assinatura de `sessions:stop` em `internal/worker/reconcile.go`: ao receber um `instance_id`, o worker dono encerra a sessão imediatamente e registra `disconnected` com `reason: tenant_suspended` — com teste de integração provando que a suspensão derruba a sessão em segundos, sem depender do tick de reconciliação (FR-041). Sem esta assinatura, T053 publica em um canal sem ouvinte e a sessão permanece conectada
- [X] T055 [P] [US4] Teste de integração de failover em `internal/worker/failover_test.go`: dois workers no mesmo processo, morte abrupta do dono, adoção pelo outro em ≤60s com `generation` incrementada e sem novo pareamento
- [X] T056 [P] [US4] Teste de integração de fencing em `internal/worker/failover_test.go`: comando com geração antiga é rejeitado com `WRONG_GENERATION` e não toca a sessão; o `sessionclient` da api relê o lease e acerta o novo dono na segunda tentativa
- [X] T057 [P] [US4] Teste de integração do drain em `internal/worker/failover_test.go`: SIGTERM libera os leases e o outro worker adota mais rápido que a expiração de 30s

---

## Phase 7: User Story 5 - Reagir a sessões invalidadas pelo WhatsApp (Priority: P5)

**Goal**: parar de tentar quando o WhatsApp invalida a sessão, registrar o motivo e avisar o tenant.

**Independent Test**: provocar logout pelo aparelho e verificar estado `deslogada`, motivo na trilha, evento no WS e **zero** tentativas de reconexão.

- [X] T058 [US5] Ligar `wa.ClassifyDisconnect` ao ciclo da sessão em `internal/worker/session.go`: invalidação interrompe a reconexão, aplica o estado terminal (`deslogada`, `banida` ou `desconectada` com motivo permanente) e persiste `last_disconnect_at`/`last_disconnect_reason`
- [X] T059 [US5] Persistir o prazo de banimento (`ban_expires_at`) a partir de `events.TemporaryBan` (`Code` e `Expire`) e expor no estado e na trilha (FR-030)
- [X] T060 [US5] Emitir `connection.logged_out` e `connection.banned` no WebSocket com os payloads do contrato, garantindo que a invalidação de uma instância **não** afete outras instâncias do mesmo número
- [X] T061 [US5] Implementar a reabilitação por comando explícito: `connect` limpa `last_disconnect_reason`, tornando a instância novamente elegível à reconciliação (FR-031)
- [X] T062 [US5] Tratar `StreamReplaced` como **alarme**: log em nível `error` e incremento de `zappermeow_stream_replaced_total`, além do estado terminal — é sinal de violação da posse exclusiva (R3)
- [X] T063 [P] [US5] Testes de integração em `internal/worker/invalidation_test.go` com `FakeSession`: cada evento de invalidação leva ao estado e motivo corretos e **nenhuma** reconexão é tentada; queda de rede continua reconectando

---

## Phase 8: User Story 6 - Parear por código de telefone (Priority: P6)

**Goal**: parear sem QR, informando o número e digitando um código no aparelho.

**Independent Test**: solicitar pareamento por código com um número de teste, digitar o código no aparelho e ver a instância conectada.

- [X] T064 [US6] Implementar `PairPhone` no worker (`internal/worker/session.go`): `Client.PairPhone(ctx, phone, showPushNotification, clientType, clientDisplayName)`, publicação do código como `pairing.code` com `method: phone` e mesma janela de expiração do fluxo de QR
- [X] T065 [US6] Implementar `POST /instances/{instanceId}/pair-phone` em `internal/api/handlers/connection.go`: valida E.164 sem `+` (`422 INVALID_PHONE_NUMBER` **sem** alterar estado), honra `replace_active` (`409 PAIRING_IN_PROGRESS` quando `false` e há tentativa ativa) e responde com `pairing_code` + `expires_at`
- [X] T066 [US6] Implementar a substituição de tentativa em curso em `internal/worker/session.go`: encerrar a anterior com `pairing.expired` e `reason: replaced_by_new_attempt` antes de iniciar a nova (FR-014)
- [X] T067 [P] [US6] Testes de integração em `internal/worker/supervisor_test.go` e `internal/api/connection_test.go`: número inválido não altera estado; troca de modalidade encerra a tentativa anterior; pareamento por código persiste a identidade como no fluxo de QR

---

## Phase 9: User Story 7 - Operar a conexão por integração (Priority: P7)

**Goal**: paridade total de capacidades entre JWT de tenant e API key da instância, inclusive no WebSocket.

**Independent Test**: executar o ciclo completo (connect, ouvir eventos, consultar estado, desconectar) usando só a API key, e confirmar que a key de outra instância é recusada.

- [X] T068 [US7] Habilitar a API key da instância em **todas** as rotas de conexão e declarar os dois esquemas de segurança como alternativas nos handlers huma, para que a OpenAPI gerada reflita a paridade (FR-039)
- [X] T069 [US7] Implementar a autenticação por subprotocolo no WebSocket (`Sec-WebSocket-Protocol: zappermeow.v1, bearer.<token>`) para navegadores, mantendo a recusa de token em query string (R7)
- [X] T070 [US7] Implementar a revogação em vigor no WebSocket: revalidação periódica da credencial e fechamento com `4403` quando a key for revogada ou o tenant suspenso (FR-042)
- [X] T071 [US7] Garantir a recusa de ações de conexão para tenant suspenso em todas as rotas da feature (FR-041), com o `404`/`403` do contrato
- [X] T072 [P] [US7] Testes de integração em `internal/api/connection_test.go`: paridade de comportamento entre JWT e API key; key da instância A contra a instância B responde `404`; key revogada é recusada imediatamente
- [X] T073 [P] [US7] Teste de integração em `internal/api/ws_route_test.go`: handshake sem credencial não entrega frame algum; revogação durante a sessão fecha com `4403`; token em query string é recusado

---

## Phase 10: Polish & Cross-Cutting Concerns

- [X] T074 [P] Implementar as métricas de `internal/metrics/metrics.go` conforme R16 — `zappermeow_sessions_connected`, `zappermeow_session_state_transitions_total`, `zappermeow_pairing_attempts_total`, `zappermeow_session_reconnects_total`, `zappermeow_lease_acquisitions_total`/`_lost_total`, `zappermeow_stream_replaced_total`, `zappermeow_ws_clients`, `zappermeow_session_command_duration_seconds` — **sem** label por instância
- [X] T075 [P] Garantir logs `slog` estruturados em toda transição de conexão com `tenant_id`/`instance_id`, e um teste que falhe se QR, token, API key ou material de sessão aparecerem em log, resposta ou trilha (FR-043), no espírito do `secrets_audit_test.go` da 001
- [X] T076 Criar `deploy/stack.yml` (Swarm) e `deploy/docker-compose.yml` (Compose) com paridade funcional incluindo o novo serviço `session-worker` (`stop_grace_period ≥ 60s`, rede privada para gRPC/Postgres/Redis, sticky sessions no Traefik só para WebSocket) — a constituição exige refletir a topologia nos dois alvos
- [X] T077 [P] Atualizar `ARCHITECTURE.md` e `TECH_STACK.md` com o que a implementação fixou (canal `sessions:claim`, autenticação do WS por subprotocolo, varredura de retenção no worker) — divergências entre docs e código resolvem-se corrigindo um dos lados
- [X] T078 [P] Validar os alvos de performance do quickstart contra a implementação: primeiro QR ≤5s (SC-001), disponibilidade contínua de código válido durante toda a tentativa, sem intervalo morto (SC-002), escaneamento→conectada ≤15s (SC-003), transição no WS ≤2s (SC-008), `GET /connection` <300ms p95 (SC-010)
- [ ] T079 ⏸️ **Depende de um número real e de um aparelho — não automatizável.** Executar o roteiro **manual** de [quickstart.md](./quickstart.md) com um número real: pareamento por QR e por código, logout visto no aparelho, e duas instâncias do mesmo número conectadas por 30 min sem se derrubarem (SC-014)
- [X] T080 [P] Rodar `golangci-lint` e a suíte completa (`go test ./...`) garantindo pipeline verde — pré-condição de merge (Princípio V)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)** → sem dependências
- **Foundational (Phase 2)** → depende do Setup; **bloqueia todas as user stories**
- **US1 (Phase 3)** → depende da Foundational
- **US2 (Phase 4)** → depende da Foundational; usa uma instância pareada, que a US1 produz (ou carga de teste)
- **US3 (Phase 5)** → depende da Foundational; testável com trilha semeada, sem exigir US1
- **US4 (Phase 6)** → depende da Foundational; observável com uma sessão ativa (US1 ou `FakeSession`)
- **US5 (Phase 7)** → depende da Foundational e é o freio da US4; implementar depois dela
- **US6 (Phase 8)** → depende da Foundational; caminho alternativo à US1, independente dela
- **US7 (Phase 9)** → depende das rotas existirem (US1–US3); é paridade de credencial sobre elas
- **Polish (Phase 10)** → depois de todas

### Within Each User Story

Worker → api → testes. Dentro do worker: máquina de estados antes da publicação de eventos; na api: handler antes do teste de integração.

### Parallel Opportunities

- **Setup**: T002, T003, T004 em paralelo após T001
- **Foundational**: T007, T008, T009, T010, T013 em paralelo; T015 em paralelo com T014; T011 depende de T010; T022 depende de T021 e T023 depende de T020 + T022
- **US1**: T032, T033, T034 em paralelo ao final
- **US2–US7**: os testes marcados `[P]` ao final de cada fase
- **Entre stories**: US3 e US6 são independentes entre si e da US2 — três frentes podem correr em paralelo depois da Foundational

## Parallel Example: Foundational

```bash
# Após T005 (migração) e T006 (config):
T007  # queries sqlc
T008  # vocabulários de evento
T009  # estados da instância
T010  # interface wa.Session
T013  # FakeSession

# Depois, em sequência: T011 (classify) → T012 (hypermeow) → T014 (lease) → T016..T018 (proto, gRPC, worker)
# T015 (events) em paralelo com T014
```

## Implementation Strategy

### MVP First (User Story 1)

Setup + Foundational + US1 = a plataforma coloca um número real online, com QR em tempo real e identidade persistida. É o primeiro momento em que o produto faz o que promete. Vale parar aqui, validar com um número de teste e só então seguir.

### Incremental Delivery

1. **MVP** (US1) — parear e conectar
2. **US2** — controle do ciclo de vida; a partir daqui o número é operável de verdade
3. **US4 + US5** — continuidade e seus freios; é o que torna a plataforma confiável em produção (não separe as duas por muito tempo: reconexão automática sem o freio da US5 martela número banido)
4. **US3** — visibilidade e diagnóstico
5. **US6, US7** — caminhos alternativos de pareamento e de credencial

### Notes

- **Ordem do boot é obrigatória**: migrações golang-migrate → `Container.Upgrade` do HyperMeow. Nossas migrações nunca tocam tabelas `whatsmeow_*`.
- **O canal de QR não pode ser bloqueado**: a biblioteca fecha o canal e desconecta o cliente se o consumidor não drenar (R2). O loop do T024 é o ponto mais sensível da feature.
- **`zappermeow_stream_replaced_total` deve ficar em zero**: qualquer incremento é violação da posse exclusiva, não estatística.
- **Nenhum teto de instâncias** pode ser introduzido: `MAX_SESSIONS_PER_WORKER` é dimensionamento operacional, não cota de produto (constituição v2.4.0).
