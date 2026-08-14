# Tasks: Complementos de Conexão da Instância

**Input**: Design documents from `/specs/003-connection-extras/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: INCLUÍDOS — a constituição (Princípio V, v2.5.0) os torna obrigatórios: cobertura da
sessão roteirizada para todo evento novo classificado, teste nas duas ordens quando eventos chegam
por canais distintos, prova de regressão (teste visto falhar sem a correção) e infraestrutura real
via testcontainers.

**Organization**: Tarefas agrupadas por user story, na ordem de prioridade da spec (P1→P5), para
que cada story seja implementável e testável de forma independente.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: pode rodar em paralelo (arquivos diferentes, sem dependência pendente)
- **[Story]**: US1 (proxy), US2 (eventos), US3 (passivo), US4 (passkey), US5 (códigos)

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Schema, contrato gRPC e queries — os artefatos gerados dos quais tudo depende

- [X] T001 Criar migração `migrations/0003_connection_extras.up.sql` e `.down.sql`: colunas `proxy_url text NULL` e `passive_mode boolean NOT NULL DEFAULT false` em `instances`; recriar o CHECK de `connection_events.type` acrescentando `stream_error`, `manual_login_reconnect`, `proxy_updated`, `passive_mode_updated`, `passkey_challenge`, `passkey_responded`, `passkey_confirmed` (data-model §1–§2); o `down` deleta linhas com tipos novos antes de restaurar o CHECK da 0002
- [X] T002 [P] Estender `proto/session/v1/session.proto` com as RPCs `ApplySettings`, `SubmitPasskeyResponse`, `ConfirmPasskey`, `GetIdentityVerificationCodes` e suas mensagens (contracts/session-grpc.md, aditivo) e regenerar `internal/pb/sessionv1/` via `go generate ./proto`
- [X] T003 [P] Estender `internal/store/queries/connection.sql` com leitura/escrita de `proxy_url` e `passive_mode` (query de settings para o worker reler na `ApplySettings` — R2) e regenerar `internal/store/` via sqlc

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Vocabulários de domínio e extensões de fronteira que TODAS as stories usam

**⚠️ CRITICAL**: Nenhuma story começa antes desta fase terminar

- [X] T004 [P] Estender `internal/domain/instance.go` (campos `ProxyURL`, `PassiveMode` no agregado e no resumo de conexão) e `internal/domain/connectionevent.go` (tipos e motivos novos do data-model §2, com a classificação permanente/não-permanente: `stream_error`, `proxy_connect_failed`, `proxy_updated` todos não-permanentes)
- [X] T005 [P] Criar `internal/domain/proxy.go`: `ValidateProxyURL` (esquemas `http`/`https`/`socks5`, host obrigatório, limite 1024 — data-model §5, espelho do contrato de `SetProxyAddress` R1) e `MaskProxyURL` (senha → `***`); testes table-driven em `internal/domain/proxy_test.go` cobrindo URLs válidas, esquema não suportado, sem host, com/sem credencial, e garantia de que a máscara nunca ecoa a senha
- [X] T006 Estender a interface em `internal/wa/session.go`: métodos `SetPassive(ctx, bool) error`, `SendPasskeyResponse(ctx, webauthnJSON []byte) error`, `ConfirmPasskey(ctx) error`, `IdentityVerificationCodes(ctx, contact string) (*VerificationCodes, error)`; kinds `KindPasskeyChallenge`/`KindPasskeyCode` com campos novos no `Event` (challenge JSON, código); failure `FailurePasskeyError`; struct `VerificationCodes` (research: resumo de superfícies)
- [X] T007 Estender `internal/wa/fake.go`: a `FakeSession` ganha suporte a roteirizar os kinds/eventos novos (passkey challenge/code, stream error com código, manual login reconnect), registrar chamadas de `SetPassive`/`SendPasskeyResponse`/`ConfirmPasskey` com ordem observável, e responder `IdentityVerificationCodes` com material canned — pré-requisito de todos os testes de story (depende de T006)
- [X] T008 [P] Estender `internal/events/events.go`: tipos de frame `pairing.passkey_challenge` e `pairing.passkey_code`, snapshot de pareamento com discriminador `phase` (`qr`/`passkey_challenge`/`passkey_code`) e campos por fase (contracts/websocket-events.md, data-model §3)
- [X] T009 [P] Adicionar em `internal/metrics/metrics.go`: `zappermeow_proxy_connect_failures_total`, `zappermeow_passkey_pairings_total`, `zappermeow_stream_errors_total` (sem label por instância — decisão R16 da 002 mantida)

**Checkpoint**: Fundações prontas — stories podem começar (em paralelo, se houver braços)

---

## Phase 3: User Story 1 - Proxy de saída por instância (Priority: P1) 🎯 MVP

**Goal**: tenant define/altera/remove proxy persistido; todo o tráfego da instância sai por ele em
toda conexão; mudança a quente religa; nunca há fallback direto; senha mascarada em toda leitura.

**Independent Test**: quickstart §US1 — validação de URL, máscara, tráfego pelo proxy (manual),
reconexão a quente ≤30s.

### Tests for User Story 1 (escrever primeiro, ver falhar)

- [X] T010 [P] [US1] Teste de integração (testcontainers) em `internal/worker/session_proxy_test.go`: sessão roteirizada com proxy configurado — construção recebe a URL crua do banco; falha de conexão classificada como `proxy_connect_failed` vai à trilha e à métrica; **nenhum** caminho de código limpa o proxy em retry (FR-005)
- [X] T011 [P] [US1] Teste de integração em `internal/worker/grpcserver_settings_test.go`: `ApplySettings{proxy_changed}` com fence válido religa a sessão (stop → rebuild → connect na FakeSession, ordem observável), encerra tentativa de pareamento em curso com `pairing_expired(cancelled)` (R2), e com fence velho retorna `FAILED_PRECONDITION`
- [X] T012 [P] [US1] Teste de handler em `internal/api/handlers/connection_proxy_test.go`: `PUT/DELETE /instances/{id}/proxy` — 422 `INVALID_PROXY_URL`/`UNSUPPORTED_PROXY_SCHEME`, 200 com `proxy_url` mascarado e `reconnecting` correto nos dois ramos (dono vivo/morto), 404 para instância de outro tenant, senha ausente de **qualquer** byte da resposta (SC-007)

### Implementation for User Story 1

- [X] T013 [US1] `internal/wa/hypermeow.go`: na construção do cliente, chamar `SetProxyAddress(url)` quando houver proxy e `SetProxy(nil)` quando não houver — sempre uma das duas (R1, FR-006); erro de `Connect`/queda com proxy configurado classifica motivo `proxy_connect_failed` e incrementa a métrica (R3)
- [X] T014 [US1] `internal/worker/session.go` + `internal/worker/supervisor.go`: religar sessão sob o mesmo lease na mudança de proxy (parar, reconstruir com configuração relida do banco via query de T003, conectar); trilha `proxy_updated` com URL mascarada; WS `connection.disconnected{reason: proxy_updated}` → `connection.connected`
- [X] T015 [US1] `internal/worker/grpcserver.go`: RPC `ApplySettings` com verificação de fencing (contracts/session-grpc.md); `internal/api/sessionclient/client.go`: wrapper com a semântica "RPC falhou → resposta `reconnecting: false`, sem erro ao caller" (R2)
- [X] T016 [US1] `internal/domain/services/connection.go`: casos de uso `SetProxy`/`ClearProxy` — validar (T005), persistir, comandar dono se lease `running`, gravar trilha; nunca retornar a URL crua
- [X] T017 [US1] `internal/api/handlers/connection.go`: endpoints `PUT /instances/{instanceId}/proxy` e `DELETE /instances/{instanceId}/proxy` (huma, auth dupla da 002, envelope, códigos de erro novos registrados em `internal/api/httperr/`)
- [X] T018 [US1] `internal/api/handlers/instances.go` + `connection.go` (GET): expor `proxy_url` mascarado e `passive_mode` na consulta da instância e no resumo de conexão (contracts/http-api.md, endpoints alterados)

**Checkpoint**: US1 completa — proxy funcional de ponta a ponta; validar quickstart §US1

---

## Phase 4: User Story 2 - Eventos StreamError / ManualLoginReconnect (Priority: P2)

**Goal**: os dois eventos ganham classificação própria, registro na trilha com causa específica,
emissão no canal e reação automática do supervisor.

**Independent Test**: quickstart §US2 — roteiros de sessão roteirizada, incluindo prova de
regressão (testes falham sem os cases novos do classify).

### Tests for User Story 2 (escrever primeiro, ver falhar — e PROVAR a falha, v2.5.0-e)

- [X] T019 [P] [US2] Testes unit em `internal/wa/classify_test.go`: `events.StreamError{Code:"999"}` → motivo `stream_error`, não-permanente, detail só com o código (nunca o node cru — R9); `events.ManualLoginReconnect` → classificação própria; rodar os dois contra o classify **sem** os cases e registrar a falha no PR (prova de regressão)
- [X] T020 [P] [US2] Teste de integração em `internal/worker/session_events_test.go`: roteiro `StreamError` → trilha `stream_error` + WS `connection.disconnected{reason: stream_error}` + reconexão + material de sessão preservado (relogin sem novo pareamento); roteiro `ManualLoginReconnect` no fim de um pareamento → trilha `manual_login_reconnect` + reconexão imediata agendada fora do handler (nunca em linha — restrição de dispatch síncrono, R5) + instância termina `connected`

### Implementation for User Story 2

- [X] T021 [US2] `internal/wa/classify.go`: cases `*events.StreamError` (motivo `stream_error`, não-permanente, `detail.stream_error_code`) e `*events.ManualLoginReconnect` (R5/R9); `internal/wa/hypermeow.go` **não** muda flags — `DisableLoginAutoReconnect` permanece default (decisão R5)
- [X] T022 [US2] `internal/worker/session.go`: reação — `stream_error` segue o caminho de queda transitória existente; `manual_login_reconnect` agenda reconexão imediata via supervisor; incrementar `zappermeow_stream_errors_total`; garantir causa específica na trilha (FR-009, FR-010)

**Checkpoint**: US1 e US2 independentes e verdes

---

## Phase 5: User Story 3 - Modo passivo (Priority: P3)

**Goal**: flag persistida, alternável a quente, reaplicada após cada `Connected`; default
desligado; visível na consulta.

**Independent Test**: quickstart §US3 — ordem de aplicação após cada Connected (roteirizada) e
comportamento no aparelho (manual).

### Tests for User Story 3 (escrever primeiro, ver falhar)

- [X] T023 [P] [US3] Teste de integração em `internal/worker/session_passive_test.go`: com `passive_mode = true`, a FakeSession registra `SetPassive(true)` **após cada** evento `Connected` — conexão inicial, reconexão e failover entre dois workers no mesmo processo (R6); com `passive_mode = false`, **nenhuma** chamada; falha do `SetPassive` gera retry curto e nunca derruba a sessão; após aplicar o modo passivo, o roteiro segue emitindo eventos e o teste afirma que eles continuam processados e publicados normalmente (FR-014)
- [X] T024 [P] [US3] Teste de handler em `internal/api/handlers/connection_passive_test.go`: `PUT /instances/{id}/passive-mode` — persiste, `applied` correto nos dois ramos, idempotente, refletido no GET da instância; trilha `passive_mode_updated`

### Implementation for User Story 3

- [X] T025 [US3] `internal/wa/hypermeow.go`: implementar `SetPassive` (delegando ao IQ da biblioteca; erro `ErrNotConnected` traduzido — R6)
- [X] T026 [US3] `internal/worker/session.go` + `internal/worker/grpcserver.go`: reaplicar o modo persistido após cada `Connected`; braço `passive_changed` da `ApplySettings` aplica na sessão conectada e responde `passive_applied` (contracts/session-grpc.md)
- [X] T027 [US3] `internal/domain/services/connection.go` + `internal/api/handlers/connection.go`: caso de uso e endpoint `PUT /instances/{instanceId}/passive-mode` (persistir sempre; comandar dono quando conectada; trilha)

**Checkpoint**: US1–US3 independentes e verdes

---

## Phase 6: User Story 4 - Pareamento com passkey (Priority: P4)

**Goal**: desafio WebAuthn e código de conferência descem pelo WS (com snapshot por fase);
resposta e confirmação sobem por endpoints; confirmação automática com handoff fica com a
biblioteca; falhas viram `pairing.failed{passkey_error}`.

**Independent Test**: quickstart §US4 — fluxo roteirizado completo, variantes de erro e ordem, e
conta com passkey real (manual).

### Tests for User Story 4 (escrever primeiro, ver falhar)

- [X] T028 [P] [US4] Teste unit em `internal/wa/hypermeow_passkey_test.go`: `pumpQR` traduz `passkey-request` → `KindPasskeyChallenge` (payload JSON opaco) e `passkey-confirmation` → `KindPasskeyCode`; item `error` → `KindPairingFailed{FailurePasskeyError}`; itens continuam drenados sem bloqueio (R7)
- [X] T029 [P] [US4] Teste de integração em `internal/worker/session_passkey_test.go`: roteiro completo desafio → resposta → código → confirmação → `pairing.succeeded`; variante `SkipHandoffUX` (nenhum frame de código); erro em cada passo → `pairing.failed{passkey_error}` + trilha; **duas ordens** entre item do canal de QR e erro pelo handler global (v2.5.0-b, R10); snapshot Redis transita de fase (`qr` → `passkey_challenge` → `passkey_code`) e um segundo cliente WS que conecta no meio recebe a fase corrente como primeiro frame
- [X] T030 [P] [US4] Teste de handler em `internal/api/handlers/connection_passkey_test.go`: `POST .../passkey/response` e `.../confirm` — 202 no caminho feliz, `409 NO_PASSKEY_CHALLENGE`/`NO_PASSKEY_CODE` fora de ordem e em chamada dupla (biblioteca não reentrante — R7), `503 SESSION_UNAVAILABLE` sem dono

### Implementation for User Story 4

- [X] T031 [US4] `internal/wa/hypermeow.go`: traduzir os itens de passkey no `pumpQR` (substituindo o `continue` atual) e implementar `SendPasskeyResponse` (unmarshal opaco → tipo da biblioteca) e `ConfirmPasskey` (R7)
- [X] T032 [US4] `internal/worker/session.go`: orquestrar a etapa — estado de fase da tentativa, snapshot `wa:pairing:{id}` com fase (T008), publicação dos frames novos, trilha `passkey_challenge`/`passkey_responded`/`passkey_confirmed{automatic}`, métrica `zappermeow_passkey_pairings_total`, pré-condições que viram os erros do contrato
- [X] T033 [US4] `internal/worker/grpcserver.go` + `internal/api/sessionclient/client.go`: RPCs `SubmitPasskeyResponse`/`ConfirmPasskey` com fencing e mapeamento de `ErrorInfo.reason` → códigos HTTP (contracts/session-grpc.md)
- [X] T034 [US4] `internal/domain/services/connection.go` + `internal/api/handlers/connection.go`: endpoints `POST /instances/{instanceId}/pairing/passkey/response` e `POST /instances/{instanceId}/pairing/passkey/confirm` (202, corpo opaco repassado sem interpretação)
- [X] T035 [US4] `internal/api/ws/ws.go` + `internal/api/wsbridge.go`: snapshot inicial entrega a fase corrente de pareamento (fase no lugar do QR quando a etapa de passkey está ativa — R10); nota do contrato: nenhum `pairing.code` novo após o desafio

**Checkpoint**: US1–US4 independentes e verdes

---

## Phase 7: User Story 5 - Códigos de verificação de identidade (Priority: P5)

**Goal**: consulta operacional (API key da instância, somente) por contato LID ou telefone;
retorna código de 60 dígitos + 2 QRs; erros claros para toda pré-condição.

**Independent Test**: quickstart §US5 — erros de pré-condição (automatizável) e conferência com o
código exibido no aparelho do contato (manual).

### Tests for User Story 5 (escrever primeiro, ver falhar)

- [X] T036 [P] [US5] Teste de integração em `internal/worker/grpcserver_verification_test.go`: `GetIdentityVerificationCodes` — contato LID direto, telefone resolvido via store da FakeSession, telefone sem mapeamento → `identity_not_resolvable`, contato = próprio LID → `cannot_verify_self`, sessão não conectada → `not_connected`, fence velho → `FAILED_PRECONDITION` (R8)
- [X] T037 [P] [US5] Teste de handler em `internal/api/handlers/verification_test.go`: `GET /instances/{id}/identity-verification-codes` — **JWT de tenant recusado** (endpoint apiKey-only, FR-025), key de outra instância → 404 sem vazar existência, 409/422/404 conforme contrato, 200 com `contact`/`numeric_code`/QRs em base64

### Implementation for User Story 5

- [X] T038 [US5] `internal/wa/hypermeow.go`: implementar `IdentityVerificationCodes` — resolução telefone→LID pelo store (`GetLIDForPN`), chamada à biblioteca, tradução das pré-condições em erros tipados do adaptador (R8); validação de formato do contato em `internal/domain/validate.go` (agrupamento de validações existente desde a 001 — data-model §5)
- [X] T039 [US5] `internal/worker/grpcserver.go` + `internal/api/sessionclient/client.go`: RPC `GetIdentityVerificationCodes` (deadline 5s no cliente — SC-006) com mapeamento de erros
- [X] T040 [US5] Criar `internal/api/handlers/verification.go` + rota em `internal/api/routes.go`: `GET /instances/{instanceId}/identity-verification-codes?contact=` autenticado **exclusivamente** por `apiKeyAuth` da instância, resposta do contrato (QRs base64, campos null quando desconhecidos), efeito colateral TOFU documentado no doc do handler

**Checkpoint**: todas as stories independentes e verdes

---

## Phase 8: Polish & Cross-Cutting Concerns

- [X] T041 [P] Verificar a spec OpenAPI gerada (subir a api e inspecionar `/openapi.json`): endpoints novos com os dois esquemas de auth (exceto verification: apiKey-only), envelope e problem details corretos — nada escrito à mão (Princípio IV)
- [X] T042 [P] Atualizar `features.md`: marcar `[x]` em pareamento por passkey, modo passivo, proxy, códigos LID, `StreamError`/`ManualLoginReconnect` com anotação *(003)*; atualizar a linha de status da tabela de features e a lacuna de proxy nas rotas de gestão
- [ ] T043 Rodar quickstart §§ automatizáveis + `golangci-lint run` + `go test ./...` completos; executar os roteiros **[manual]** do quickstart.md e registrar os resultados nos checkboxes (constituição v2.5.0-f — a feature não está entregue sem isso); o orçamento de 30s do SC-002 é medido no passo 1.4 do quickstart
- [ ] T044 Regressão da 002: rodar o roteiro manual da 002 numa instância sem proxy/passivo e registrar no quickstart §Regressão (nada do caminho padrão pode ter mudado)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: sem dependências — T001/T002/T003 em paralelo
- **Foundational (Phase 2)**: depende do Setup (T003 → T004; T002 → T006 indiretamente para nomes); T007 depende de T006; bloqueia todas as stories
- **Stories (Phases 3–7)**: todas dependem da Phase 2; entre si são independentes, **exceto**: US3 e US4 tocam `ApplySettings`/`grpcserver.go` e `session.go` criados na US1 (T015) — se rodarem em paralelo com US1, coordenar merge nesses dois arquivos
- **Polish (Phase 8)**: depende das stories desejadas

### User Story Dependencies

- **US1 (P1)**: só Foundational — MVP
- **US2 (P2)**: só Foundational (arquivos próprios: classify, session events)
- **US3 (P3)**: Foundational; compartilha `ApplySettings` com US1 (T015) — preferir após US1
- **US4 (P4)**: Foundational; toca `session.go`/`grpcserver.go` — preferir após US1
- **US5 (P5)**: Foundational; toca `grpcserver.go` — preferir após US4 ou coordenar

### Parallel Opportunities

```text
Phase 1:  T001 ─┐
          T002 ─┼─ paralelo (arquivos disjuntos)
          T003 ─┘
Phase 2:  T004, T005, T008, T009 em paralelo; T006 → T007
US1:      T010, T011, T012 (testes) em paralelo → T013..T018 em sequência curta
US2 ∥ US1: arquivos disjuntos (classify_test/classify) — paralelizável de verdade
Dentro de cada story: os testes [P] sempre em paralelo entre si
```

---

## Implementation Strategy

### MVP First (US1 apenas)

1. Phases 1–2 (Setup + Foundational)
2. Phase 3 (US1 — proxy): implementar, rodar quickstart §US1, validar SC-001/SC-002/SC-007
3. **PARAR e VALIDAR**: proxy de ponta a ponta com proxy real (roteiro manual)
4. Entregar — o MVP já fecha a lacuna mais citada do levantamento

### Incremental Delivery

Ordem de entrega = ordem de prioridade da spec: US1 (proxy) → US2 (robustez de eventos) →
US3 (passivo) → US4 (passkey) → US5 (códigos). Cada checkpoint é um estado entregável; US2 pode
ser adiantada em paralelo com US1 por não compartilhar arquivos quentes.

### Notas

- Commits por tarefa ou grupo lógico; PR referencia FRs cobertos
- Provas de regressão (T019) registradas no PR como exige a v2.5.0-e
- Nenhuma tarefa altera `deploy/` — topologia inalterada (plan.md)
