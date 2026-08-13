# Implementation Plan: Conexão da Instância com o WhatsApp

**Branch**: `002-instance-connection` | **Date**: 2026-08-13 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/002-instance-connection/spec.md`

## Summary

Dar vida à instância registrada pela 001: parear um dispositivo companheiro a um número de WhatsApp
(por QR ou por código de telefone), mantê-lo online de forma confiável e expor esse estado ao
tenant em tempo real, sob consulta e em histórico. Inclui o ciclo de vida completo (conectar,
desconectar, deslogar, excluir limpo), continuidade automática com posse exclusiva da sessão e
reação correta às invalidações do WhatsApp.

Abordagem técnica: nasce aqui o segundo plano de execução do produto — o subcomando
`zappermeow session-worker` (stateful), separado da `api` (stateless), com **posse exclusiva por
lease em Postgres** (aquisição atômica, heartbeat de 10s, expiração de 30s, fencing por
`generation`) e reconciliação descentralizada. A api comanda o dono da sessão por **gRPC** e, quando
não há dono, grava a intenção e acorda os workers por **Redis pub/sub** — o que mantém o primeiro
QR dentro dos 5s exigidos sem introduzir coordenador central. Os eventos voltam por
`events:{instance_id}` (pub/sub) e chegam ao tenant por **WebSocket por instância**, com snapshot
inicial e deduplicação por número de sequência. O HyperMeow compartilha o **mesmo pool pgx** da API
via `stdlib.OpenDBFromPool` + `sqlstore.NewWithDB(db, "pgx", …)`, versionando suas próprias tabelas.

Todas as decisões da fase de pesquisa foram verificadas contra o **código real do fork** no module
cache, não contra documentação upstream — e uma delas inverteu o desenho: `events.LoggedOut` e
`events.StreamReplaced` **não** implementam a interface `PermanentDisconnect` da biblioteca, então a
distinção entre queda e invalidação (US5) precisa de classificação explícita, não de type assertion.

## Technical Context

**Language/Version**: Go 1.25+

**Primary Dependencies**: já no projeto — chi v5, huma v2, pgx v5, sqlc, golang-migrate, go-redis
v9, redis_rate v10, golang-jwt v5, caarlos0/env v11, slog/prometheus/otel. **Novas nesta feature**:
`github.com/polymorfa/hypermeow` (core WhatsApp — pseudo-versão pinada, já prevista na
constituição), `google.golang.org/grpc` + `google.golang.org/protobuf` (contrato api↔worker exigido
pelo Princípio IV), `github.com/coder/websocket` (canal de eventos; zero dependências transitivas,
API context-first — R7). Ferramentas de build: `protoc-gen-go` e `protoc-gen-go-grpc` como `tool`
directives no `go.mod` (R14).

**Storage**: PostgreSQL 17 — migração `0002`: colunas de conexão em `instances`, tabelas
`session_leases` e `connection_events`; tabelas `whatsmeow_*` criadas e versionadas pelo próprio
HyperMeow (`Container.Upgrade`, versão em `whatsmeow_version`), nunca pelas nossas migrações.
Redis — pub/sub (`events:{id}`, `sessions:claim`, `sessions:stop`), sequência de eventos
(`wa:seq:{id}`), código de pareamento corrente (`wa:pairing:{id}`, TTL = validade) e cache de lease
(`wa:lease:{id}`, TTL 5s). Nenhum dado durável fora do Postgres.

**Testing**: `go test` + testify (unit, table-driven) para classificação de desconexão, máquina de
estados e envelope de eventos; testcontainers-go com **Postgres e Redis reais** para lease,
fencing, disputa entre dois workers no mesmo processo, fan-out do WebSocket, trilha e retenção. A
fronteira com o WhatsApp é a interface `wa.Session`, com `wa.FakeSession` encenando sequências
determinísticas de eventos (R13). Caminho contra número real: roteiro manual no
[quickstart.md](./quickstart.md).

**Target Platform**: Linux server (imagem distroless multi-stage); dois novos serviços no deploy —
`session-worker` com `stop_grace_period ≥ 60s` para drain limpo, em Swarm e Compose com paridade.

**Project Type**: Web service + worker stateful — subcomandos `serve` e `session-worker` do binário
único `zappermeow`.

**Performance Goals**: primeiro QR no WebSocket em ≤5s p95 (SC-001); escaneamento → conectada em
≤15s p95 (SC-003); transição refletida no WS em ≤2s (SC-008); `GET /connection` em <300ms p95
(SC-010); failover após morte abrupta em ≤60s (SC-005), com o orçamento vindo de heartbeat 10s +
expiração 30s + reconciliação 15s.

**Constraints**: posse exclusiva por sessão de dispositivo é invariante absoluta (Princípio III);
nenhum material criptográfico, QR, token ou API key em resposta, log, métrica ou trilha (FR-043);
a janela de pareamento é ditada pelo servidor do WhatsApp (~160s) e nossa configuração só pode
encurtá-la (R2); métricas sem label por instância (cardinalidade — R16); token de WebSocket nunca
em query string (R7); **nenhum teto de instâncias imposto pela plataforma** (constituição v2.4.0),
apenas o knob operacional `MAX_SESSIONS_PER_WORKER`.

**Scale/Scope**: centenas de sessões por worker (default 200), dezenas de milhares de eventos de
conexão por mês com retenção de 30 dias; 7 user stories, 45 FRs, 7 endpoints HTTP novos + 2
alterados, 1 serviço gRPC com 5 RPCs, 9 tipos de evento no WebSocket, 1 migração.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

Avaliado contra a constituição **v2.4.0** (emenda do modelo multi-dispositivo e da ausência de teto
imposto pela plataforma).

| # | Princípio | Avaliação | Status |
|---|-----------|-----------|--------|
| I | Simplicidade e Stdlib-First | Nenhuma peça nova de infra: o mesmo Postgres serve API, store do HyperMeow e leases; o mesmo Redis serve pub/sub, sequência e cache. SQL continua explícito via sqlc. Três dependências novas, todas justificadas: hypermeow (o core do produto, já na constituição), grpc/protobuf (o Princípio IV **exige** contrato interno em Protobuf) e coder/websocket (canal previsto na constituição; escolhido por não ter dependências transitivas) | ✅ PASS |
| II | Multi-Tenancy com Isolamento por Instância | Toda rota de conexão aceita JWT de tenant **ou** API key da instância, sempre validando que a credencial resolve para a instância da URL; WS autentica antes de qualquer frame e fecha em revogação (4403). Suspensão de tenant derruba sessões preservando a intenção do usuário (R12). Sem teto de instâncias — aderente à v2.4.0; `MAX_SESSIONS_PER_WORKER` é knob de dimensionamento, explicitamente não uma cota | ✅ PASS |
| III | Posse Exclusiva de Sessão (NON-NEGOCIÁVEL) | É a espinha dorsal da feature: lease com aquisição atômica, heartbeat, expiração, fencing por `generation` em toda RPC e todo evento, drain no SIGTERM, reconciliação descentralizada. Nenhum caminho fala com uma sessão sem passar pelo lease — a api não abre conexão WhatsApp em hipótese alguma. Duas travas extras: UNIQUE em `instances.wa_jid` (dois registros jamais representam o mesmo device) e `zappermeow_stream_replaced_total` como alarme de violação | ✅ PASS |
| IV | Contrato de API como Fonte de Verdade | Endpoints HTTP tipados em huma com envelope padrão e erros RFC 9457 + `code`; contrato interno api↔worker em `proto/session/v1/`, versionado, com verificação de código gerado no CI. Frames do WebSocket não são respostas HTTP e têm contrato próprio documentado — ver Complexity Tracking | ⚠️ PASS com desvio registrado |
| V | Testes Contra Infraestrutura Real | Postgres e Redis **reais** (testcontainers) para tudo que é infra: queries sqlc, lease, fencing, disputa entre workers, pub/sub, retenção. O WhatsApp é serviço de terceiros sem test double possível — ver Complexity Tracking | ⚠️ PASS com desvio registrado |
| VI | Observabilidade Estruturada | `slog` JSON com `tenant_id`/`instance_id` em toda transição; oito métricas novas em `/metrics` cobrindo sessões conectadas, pareamentos, reconexões, trocas de posse e clientes WS (R16); OTel segue desligado por padrão. A feature cria sessões e um canal de entrega externa, e expõe métrica para ambos, como o princípio exige | ✅ PASS |

**Resultado inicial**: PASS — dois desvios registrados e justificados abaixo, nenhum bloqueante.

**Re-check pós Phase 1**: PASS. O design não introduziu ORM, peça de infra nova, rota sem
autenticação, teto de produto disfarçado nem documentação manual dessincronizável. Uma decisão
mereceu registro extra: a varredura de retenção roda no `session-worker` sob advisory lock em vez
de virar task asynq (R11) — divergência da ARCHITECTURE.md, não da constituição, com migração
prevista para quando a fatia de webhooks levantar o serviço `jobs`.

## Project Structure

### Documentation (this feature)

```text
specs/002-instance-connection/
├── plan.md              # This file (/speckit-plan command output)
├── research.md          # Phase 0 output — 16 decisões verificadas contra o código do fork
├── data-model.md        # Phase 1 output — schema, máquina de estados, vocabulários, chaves Redis
├── quickstart.md        # Phase 1 output — validação fim-a-fim (automatizável × [manual])
├── contracts/           # Phase 1 output
│   ├── http-api.md      # 7 endpoints novos + 2 alterados
│   ├── websocket-events.md  # handshake, envelope, 9 tipos de evento, códigos de fechamento
│   └── session-grpc.md  # serviço gRPC api↔worker, fencing, mapeamento de erros
└── tasks.md             # Phase 2 output (/speckit-tasks command - NOT created by /speckit-plan)
```

### Source Code (repository root)

```text
zappermeow/
├── cmd/zappermeow/
│   ├── main.go                    # + registro do subcomando session-worker
│   └── session_worker.go          # NOVO — boot do worker: store WA, gRPC, lease, reconciliação
├── proto/
│   └── session/v1/session.proto   # NOVO — contrato api↔worker (Princípio IV)
├── internal/
│   ├── pb/sessionv1/              # NOVO — código gerado (protoc-gen-go + -go-grpc), verificado no CI
│   ├── api/
│   │   ├── handlers/
│   │   │   ├── connection.go      # NOVO — connect, pair-phone, disconnect, logout, status, events
│   │   │   └── instances.go       # ALTERADO — DELETE limpo, resumo de conexão no GET
│   │   ├── ws/                    # NOVO — upgrade, auth por header/subprotocolo, snapshot+stream
│   │   └── sessionclient/         # NOVO — cliente gRPC: resolve lease, cacheia, retenta 1× no novo dono
│   ├── worker/                    # NOVO — plano stateful
│   │   ├── supervisor.go          #   ciclo de vida das sessões que o worker possui
│   │   ├── session.go             #   uma sessão: pareamento, eventos, transições, publicação
│   │   ├── grpcserver.go          #   implementa SessionService com verificação de fencing
│   │   ├── reconcile.go           #   tick de 15s + assinatura de sessions:claim / sessions:stop
│   │   └── retention.go           #   varredura diária sob pg_try_advisory_lock (R11)
│   ├── lease/                     # NOVO — aquisição atômica, heartbeat em lote, release, fencing
│   ├── events/                    # NOVO — envelope, seq, publisher (worker) e subscriber (api)
│   ├── wa/                        # NOVO — fronteira com o HyperMeow
│   │   ├── session.go             #   interface wa.Session (Connect/Disconnect/Logout/QRChannel/PairPhone/Events)
│   │   ├── hypermeow.go           #   implementação real + container compartilhando o pool pgx (R1)
│   │   ├── classify.go            #   evento → (transitória | invalidação), estado e motivo (R3)
│   │   └── fake.go                #   FakeSession para os testes de integração (R13)
│   ├── domain/
│   │   ├── instance.go            # ALTERADO — estados de conexão, intenção, identidade do device
│   │   ├── connectionevent.go     # NOVO — tipos e motivos canônicos
│   │   └── services/connection.go # NOVO — casos de uso de conexão (orquestra lease + store + eventos)
│   ├── store/queries/             # + connection.sql, leases.sql, connection_events.sql
│   └── config/config.go           # ALTERADO — config do worker, janela de pareamento, retenção
├── migrations/                    # + 0002_instance_connection.{up,down}.sql
├── deploy/                        # NOVO — stack.yml e docker-compose.yml com api + session-worker
└── .github/workflows/ci.yml       # ALTERADO — verificação de código gerado do protobuf
```

**Structure Decision**: segue o layout fixado na constituição. Esta feature cria pela primeira vez
os pacotes `worker`, `lease` e `events` — todos previstos ali e agora com código real que os
justifica. O pacote `wa` não consta da lista da constituição e é uma adição consciente: concentrar
a fronteira com o HyperMeow atrás de uma interface é o que torna possível testar lease, fencing e
máquina de estados contra Postgres e Redis reais sem depender do WhatsApp; sem ele, essa lógica
ficaria espalhada por `worker` e intestável em CI. Os pacotes `jobs`, `media` e `webhooks`
continuam inexistentes — criá-los vazios agora violaria a simplicidade.

## Complexity Tracking

> Desvios conscientes que a revisão de PR deve conhecer.

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| Princípio IV: frames do WebSocket não usam o envelope `{status, data, timestamp}` nem aparecem na spec OpenAPI como schemas de resposta | O envelope é definido para **respostas HTTP**, e `status` carrega o código HTTP da resposta — não existe código HTTP em um frame de WS. O canal tem contrato próprio, versionado por subprotocolo (`zappermeow.v1`) e documentado em `contracts/websocket-events.md`; o endpoint de upgrade aparece na OpenAPI apontando para ele | Forçar o envelope exigiria inventar um `status` fictício por frame, contrariando a semântica RFC 9457 que a v2.3.0 adotou justamente para eliminar campos de estado inventados. Descrever as frames como schemas OpenAPI não é suportado pelo huma e criaria a documentação manual dessincronizável que o princípio proíbe |
| Princípio V: a fronteira com o WhatsApp é testada contra `wa.FakeSession`, não contra o serviço real | O WhatsApp é serviço de terceiros sem sandbox: exige número real, escaneamento humano por QR e pune automação com banimento do número. Toda a **infraestrutura** (Postgres, Redis, filas, locking distribuído) permanece real via testcontainers — inclusive dois workers disputando o mesmo lease. O caminho real é validado por roteiro manual no quickstart | Mockar Postgres/Redis seria a violação que o princípio ataca, e não é o que está sendo feito. Testar contra o WhatsApp real no CI tornaria o pipeline não determinístico, dependente de rede externa e capaz de queimar o número de teste — um pipeline bloqueante (Princípio V) não pode depender disso |
| ARCHITECTURE.md prevê "limpeza de eventos antigos" como task asynq; aqui ela roda no `session-worker` sob `pg_try_advisory_lock` | Levantar o serviço `jobs` inteiro (scheduler, filas, deploy nos dois alvos) para um único `DELETE` diário é desproporcional nesta fatia. O advisory lock garante execução única entre workers sem eleição de líder | `pg_cron` seria peça nova de infra (veto do Princípio I); ticker sem lock em todas as réplicas faria DELETEs concorrentes redundantes. Quando a fatia de webhooks trouxer o `jobs`, a varredura migra para lá — mudança de uma linha de registro, não reescrita |
