# Research — Conexão da Instância com o WhatsApp (002-instance-connection)

**Date**: 2026-08-13 | **Spec**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md)

Todas as decisões abaixo foram verificadas contra o código real do fork em uso
(`github.com/polymorfa/hypermeow@v0.0.0-20260811214557-f9db181f1dfa`, baixado no module cache),
não contra documentação upstream — o fork altera comportamento de conexão e reconexão.

---

## R1 — Pool Postgres único compartilhado com o HyperMeow

**Decision**: o `session-worker` abre um `*sql.DB` sobre o `pgxpool` existente com
`stdlib.OpenDBFromPool(pool)` e monta o store do HyperMeow com
`sqlstore.NewWithDB(db, "pgx", waLog)`, chamando `Container.Upgrade(ctx)` no boot, **depois** das
migrações golang-migrate da API.

**Rationale**: o Princípio I exige um único Postgres e a constituição pede "pool único
compartilhado entre API e HyperMeow". `dbutil.ParseDialect` (verificado em
`go.mau.fi/util/dbutil`) aceita literalmente `"pgx"` e o mapeia para `Postgres`, então não há
necessidade de registrar driver adicional. `Container.Upgrade` versiona as tabelas do HyperMeow
em `whatsmeow_version`, isolado do `schema_migrations` do golang-migrate — os dois sistemas de
migração convivem sem se enxergar, desde que as nossas migrações nunca toquem tabelas
`whatsmeow_*`.

**Alternatives considered**: `sqlstore.New(ctx, "pgx", dsn, log)` abriria um segundo pool para o
mesmo banco (rejeitado: dobra conexões e contraria a constituição); um `*sql.DB` independente com
`pgx/stdlib` (mesma objeção, sem ganho).

---

## R2 — Janela de pareamento: quem manda é o servidor do WhatsApp

**Decision**: a janela efetiva é a do próprio protocolo — o servidor entrega uma lista de códigos
(tipicamente 6) e a biblioteca emite o primeiro com 60s e os seguintes com 20s cada
(`qrchan.go:emitQRs`), totalizando ≈160s; ao esgotar, ela emite `QRChannelTimeout` e **desconecta
o cliente sozinha**. Nossa configuração `PAIRING_WINDOW` (default **180s**) atua apenas como
**teto de segurança** implementado por cancelamento de contexto: pode encurtar a janela, nunca
estendê-la.

**Rationale**: FR-011 pede "janela máxima configurável"; com default 180s o esgotamento natural
dos códigos (~160s) acontece primeiro, que é o comportamento correto — cortar antes desperdiçaria
códigos válidos que o usuário ainda podia escanear. O default de 2 minutos citado na spec como
exemplo foi ajustado para não truncar a janela do protocolo; o requisito (janela finita,
configurável, com evento de expiração) permanece atendido.

**Alternatives considered**: forçar 120s por fidelidade literal ao texto da spec (rejeitado:
encurta a experiência sem nenhum ganho); reiniciar o ciclo de códigos automaticamente ao expirar
(rejeitado: a spec decidiu por janela fixa com expiração explícita).

**Consequência de implementação**: `GetQRChannel` **deve** ser chamado antes de `Connect()` e
falha com `ErrQRAlreadyConnected` ou `ErrQRStoreContainsID` — ou seja, só é válido para device sem
JID. O canal tem buffer 8 e, se o consumidor não drenar, a biblioteca fecha o canal e desconecta
(`select ... default` em `emitQRs`); o consumidor no worker precisa ser um loop dedicado que nunca
bloqueia, publicando cada código no Redis imediatamente.

---

## R3 — Classificação de desconexão: transitória × invalidação

**Decision**: classificação explícita por tipo de evento, **não** pela interface
`events.PermanentDisconnect`.

| Evento do HyperMeow | Classificação | Estado resultante | Reconecta? |
| --- | --- | --- | --- |
| `Disconnected` | transitória | `conectando` | sim |
| `KeepAliveTimeout` / `KeepAliveRestored` | transitória (só telemetria) | mantém | sim |
| `LoggedOut` | invalidação | `deslogada` (motivo por `Reason`/`OnConnect`) | **não** |
| `TemporaryBan` | invalidação | `banida` (grava `Code` e `Expire`) | **não** |
| `StreamReplaced` | invalidação | `desconectada` + motivo `session_replaced` | **não** |
| `ClientOutdated` | invalidação | `desconectada` + motivo `client_outdated` | **não** |
| `ConnectFailure` (motivo desconhecido) | invalidação | `desconectada` + motivo `connect_failure` | **não** |
| `CATRefreshError` | invalidação | `desconectada` + motivo `cat_refresh_failed` | **não** |

**Rationale**: a interface `PermanentDisconnect` existe na biblioteca, mas é implementada apenas
por `TemporaryBan`, `ConnectFailure` e `CATRefreshError` (verificado por grep em
`types/events/events.go`) — **`LoggedOut` e `StreamReplaced` não a implementam**. Confiar só na
type assertion deixaria justamente o caso mais importante (logout pelo aparelho) sendo tratado
como queda transitória, com reconexão infinita contra uma sessão morta — exatamente o que a US5
proíbe. A tabela acima é a fonte de verdade e vira uma função pura testável
(`wa.ClassifyDisconnect(evt) (kind, state, reason)`).

**Nota sobre `StreamReplaced`**: com o lease funcionando, ele nunca deveria ocorrer — significa
que a mesma credencial de device foi aberta em outro lugar. Além de encerrar a sessão, ele DEVE
gerar log em nível `error` e incrementar contador próprio: é sinal de falha de posse exclusiva
(Princípio III), não evento de rotina.

**Estados terminais sem estado próprio**: `session_replaced`, `client_outdated`,
`connect_failure` e `cat_refresh_failed` ficam em `desconectada` com motivo permanente registrado.
A reconciliação ignora instâncias cujo `last_disconnect_reason` está no conjunto permanente até
que um comando explícito de conexão limpe o campo — não é preciso criar novos estados na máquina
para isso, e a spec (FR-026) fica atendida: o tenant vê estado + motivo e as tentativas param.

---

## R4 — Reconexão em duas camadas

**Decision**:

1. **Intra-processo** (queda de rede com o worker vivo): `EnableAutoReconnect = true` (default da
   biblioteca) com `AutoReconnectHook` nosso, que (a) veta a reconexão quando a última causa foi
   invalidação, (b) aplica teto e jitter no backoff.
2. **Inter-processo** (worker morreu, deploy, partição): lease + reconciliação (R5).

**Rationale**: a biblioteca já reconecta sozinha com delay `AutoReconnectErrors * 2s`
(`client.go:678`), que cresce indefinidamente e sem jitter — mil sessões caindo juntas
reconectariam em fase. O `AutoReconnectHook` é o ponto de extensão previsto pela própria
biblioteca (retornar `false` cancela a reconexão), então o teto (60s) e o jitter entram ali sem
fork nem goroutine paralela. FR-023 (intervalos progressivos, tentativas indefinidas) é atendido
pela camada 1; FR-019/FR-022 (sobreviver a morte de processo) pela camada 2.

**Alternatives considered**: desligar `EnableAutoReconnect` e reimplementar tudo (rejeitado:
reescreve lógica já testada da biblioteca, incluindo o tratamento de erros retryable em
`isRetryableConnectError`); deixar o default sem hook (rejeitado: backoff sem teto nem jitter).

---

## R5 — Lease, fencing e reconciliação

**Decision**: tabela `session_leases` conforme ARCHITECTURE.md, com aquisição atômica por
`UPDATE ... WHERE worker_id IS NULL OR heartbeat_at < now() - 30s RETURNING generation`,
heartbeat em lote a cada 10s, reconciliação a cada 15s e liberação explícita no SIGTERM. O
`generation` viaja em **todo** comando gRPC e **todo** evento publicado; o worker rejeita comandos
com generation diferente (`FAILED_PRECONDITION` + `WRONG_GENERATION`).

**Rationale**: já é decisão arquitetural do projeto (Princípio III, não-negociável). O ponto novo
resolvido aqui é a **latência do primeiro pareamento**: reconciliação de 15s violaria SC-001 (QR
em ≤5s). Ver R6.

**Números escolhidos**: heartbeat 10s, expiração 30s, reconciliação 15s → pior caso de failover
≈45s + handshake, dentro do alvo de 60s de SC-005.

---

## R6 — Como um comando de conexão chega ao worker

**Decision**: fluxo em dois caminhos, decidido pelo estado do lease:

- **Com dono vivo** (heartbeat fresco): a api chama o worker por **gRPC** no `grpc_addr` do lease,
  passando o `generation`. Caminho síncrono, usado por `connect` de instância já pareada,
  `disconnect`, `logout` e `pair-phone`.
- **Sem dono** (lease livre ou expirado): a api grava a intenção (`desired_state = running`) e
  publica o `instance_id` no canal Redis `sessions:claim`. Todos os workers escutam; cada um tenta
  a aquisição atômica e o SQL garante um único vencedor, que conecta imediatamente. O tick de 15s
  permanece como rede de segurança para quando o pub/sub se perder.

A resposta HTTP de `connect` é **202 Accepted** com o estado corrente — o QR e a confirmação
chegam pelo WebSocket.

**Rationale**: pub/sub dá latência de milissegundos sem introduzir coordenador central nem
peça de infra nova (Redis já existe). Responder 202 evita segurar o request HTTP durante um
handshake que pode levar segundos e que, em caso de pareamento, só termina quando um humano
escanear.

**Alternatives considered**: só reconciliação por tick (rejeitado: viola SC-001 no primeiro
pareamento); a api escolher o worker e chamar por gRPC direto sem lease (rejeitado: viola o
Princípio III, a posse tem de nascer do lease); streaming gRPC do QR api←worker (rejeitado: o
Redis pub/sub já é o caminho de eventos previsto e serve N réplicas da api; um stream gRPC
amarraria o QR à réplica que originou o comando).

---

## R7 — WebSocket: biblioteca e autenticação

**Decision**: `github.com/coder/websocket` para o upgrade, montado como handler chi puro fora do
huma. Autenticação por `Authorization: Bearer <jwt>` / `X-Api-Key` quando o cliente controla
headers, **ou** por subprotocolo `Sec-WebSocket-Protocol: zappermeow.v1, bearer.<token>` para
navegadores. **Token em query string é proibido.**

**Rationale**: o `WebSocket` do navegador não permite headers customizados; o truque do
subprotocolo é o padrão de fato para esse caso e mantém o segredo fora da URL — que apareceria em
logs de acesso, no Traefik e no histórico do browser, violando "segredos NUNCA aparecem em logs"
(Princípio II). `coder/websocket` (sucessor do nhooyr.io/websocket) tem zero dependências
transitivas e API `context`-first, aderente ao Princípio I.

**Alternatives considered**: `gorilla/websocket` (maior, API pré-context, e traria o hub de
conexões junto); query param com redação no middleware de log (rejeitado: redação é frágil e o
token ainda vaza para proxies).

---

## R8 — Fan-out de eventos e snapshot inicial

**Decision**: worker publica em `events:{instance_id}` (Redis pub/sub). O handler WS da api:
(1) assina o canal e começa a **bufferizar**; (2) lê o snapshot — estado em Postgres + código de
pareamento corrente em Redis; (3) envia o snapshot como primeira mensagem; (4) drena o buffer
descartando eventos com `seq` menor ou igual ao do snapshot; (5) segue em streaming.

Cada evento carrega `seq`, um contador monotônico por instância obtido com `INCR wa:seq:{id}` no
worker. O código de pareamento corrente vive em `wa:pairing:{id}` com TTL igual à validade do
código.

**Rationale**: assinar antes de ler o snapshot elimina a janela em que um evento se perderia entre
as duas operações; o `seq` resolve a duplicação que essa ordem introduz. Sem o `seq`, um cliente
que abre o WS durante o pareamento poderia ver o mesmo QR duas vezes ou, pior, um estado velho
sobrescrevendo um novo. Atende FR-032 e o cenário 4 da US1.

**Alternatives considered**: snapshot antes de assinar (rejeitado: perde eventos na janela);
replay persistente de N minutos (rejeitado: a spec escolheu explicitamente "sem replay").

---

## R9 — Identidade do dispositivo e re-pareamento

**Decision**: `instances.wa_jid` guarda o JID **completo com sufixo de device**
(ex.: `5511999999999:11@s.whatsapp.net`), com índice **UNIQUE parcial** (`WHERE wa_jid IS NOT
NULL`). `instances.phone_number` guarda só o número, **sem** restrição de unicidade.

No boot de uma sessão: se `wa_jid` existe → `container.GetDevice(ctx, jid)`; senão →
`container.NewDevice()`. Após `PairSuccess`, persistimos `ID` (JID), `LID`, `PushName`,
`BusinessName` e `Platform` do `store.Device`; se o número diferir do anterior, gravamos evento
`number_changed`.

**Rationale**: o modelo multi-dispositivo (constituição v2.4.0) permite N instâncias por número —
daí `phone_number` sem unicidade, atendendo FR-017. Já o **JID de device** é irrepetível por
definição: duas instâncias com o mesmo JID seriam a mesma sessão em dois lugares, exatamente o que
o Princípio III proíbe. O UNIQUE é uma trava de segurança de última linha, no banco, contra bug de
código.

---

## R10 — Logout sem conexão ativa

**Decision**: `logout` tenta primeiro o caminho limpo — se a sessão não estiver conectada, o
worker conecta (com timeout curto, 15s) e chama `Client.Logout(ctx)`, que envia
`remove-companion-device` ao servidor e só então apaga o store local. Se a conexão não puder ser
estabelecida, o worker apaga o material local (`Store.Delete`), coloca a instância em `registrada`
e registra o motivo `logout_local_only`, sinalizando ao tenant que o dispositivo pode continuar
listado no aparelho.

**Rationale**: `Client.Logout` (verificado em `client.go:766`) exige sessão ativa — envia um IQ e
**aborta sem apagar nada** se o envio falhar. Sem esse tratamento, o logout de uma instância
desconectada simplesmente falharia, contrariando o edge case decidido na spec ("o material é
apagado localmente mesmo sem conexão ativa"). A distinção nos motivos preserva a honestidade do
que aconteceu de fato.

---

## R11 — Retenção da trilha de eventos

**Decision**: varredura diária `DELETE FROM connection_events WHERE occurred_at < now() -
$retention`, executada pelo `session-worker` sob `pg_try_advisory_lock` (um único worker por
ciclo), com `CONNECTION_EVENTS_RETENTION` default de 30 dias.

**Rationale**: atende FR-037 sem exigir que esta fatia levante o serviço `jobs` inteiro (asynq
scheduler, filas, deploy) para um único DELETE diário. O advisory lock resolve a concorrência
entre workers sem eleição de líder. Quando a fatia de webhooks trouxer o `jobs`, a varredura migra
para lá como task periódica — uma linha de registro, não uma reescrita.

**Alternatives considered**: `pg_cron` (peça nova de infra, veta pelo Princípio I); ticker sem
lock em todas as réplicas (DELETEs concorrentes redundantes); subir o serviço `jobs` agora
(escopo desproporcional).

---

## R12 — Intenção do usuário × estado desejado do lease

**Decision**: dois campos com donos distintos.

- `instances.connection_intent` (`ativa` | `parada`) — **intenção do usuário**, alterada só por
  comando explícito de conectar/desconectar.
- `session_leases.desired_state` (`running` | `stopped` | `draining`) — **estado efetivo** que o
  worker obedece, derivado de `connection_intent AND tenant.status = 'active'`.

Suspender um tenant escreve `stopped` em todos os leases das suas instâncias e publica
`sessions:stop`; reativar recalcula a partir de `connection_intent`, restaurando o que o usuário
queria.

**Rationale**: um único campo obrigaria a suspensão a sobrescrever a intenção do usuário e perder
a informação de quais instâncias voltar a ligar na reativação — quebrando FR-041. Com os dois
campos, a suspensão é uma projeção reversível.

---

## R13 — Testes: WhatsApp real não entra no CI

**Decision**: o worker fala com o HyperMeow através da interface `wa.Session`
(`Connect`, `Disconnect`, `Logout`, `QRChannel`, `PairPhone`, `Events`), com duas implementações:
`wa.hypermeowSession` (produção) e `wa.FakeSession` (testes), esta última capaz de encenar
sequências determinísticas de eventos (QR → PairSuccess, Disconnected, LoggedOut, TemporaryBan,
StreamReplaced). Postgres e Redis continuam **reais** via testcontainers, incluindo dois workers
em processo disputando o mesmo lease. O caminho contra um número real é um roteiro manual no
[quickstart.md](./quickstart.md).

**Rationale**: o Princípio V proíbe mockar **infraestrutura** — Postgres, Redis, filas, locking —
exatamente onde mocks escondem bugs, e nada disso é mockado aqui. O WhatsApp não é nossa
infraestrutura: é um serviço de terceiros que exige um número real, escaneamento humano e que
pune automação de teste com banimento. Não existe test double oficial. O que a fake substitui é
apenas a fronteira externa; toda a lógica de estado, lease, fencing, fan-out e persistência é
exercitada contra infra real.

---

## R14 — Toolchain do gRPC

**Decision**: `.proto` versionado em `proto/session/v1/session.proto`, código gerado em
`internal/pb/sessionv1/` via `protoc-gen-go` + `protoc-gen-go-grpc` fixados como `tool` directives
no `go.mod` (Go 1.25). O CI ganha uma verificação de código gerado espelhando a que já existe para
o sqlc (`git diff --exit-code`).

**Rationale**: o Princípio IV exige contratos internos api↔worker em Protobuf versionados no
repositório. Tool directives no `go.mod` mantêm as versões dos geradores fixas e reproduzíveis sem
`buf` nem imagem extra. A verificação no CI impede que código gerado desatualizado seja mergeado —
mesmo problema, mesma solução já adotada para o sqlc.

**Alternatives considered**: `buf` (ótimo, mas é ferramenta e configuração a mais para um único
serviço interno); commitar sem verificação no CI (rejeitado: geração silenciosamente defasada).

**Transporte**: gRPC em texto claro na rede privada (overlay no Swarm, bridge no Compose),
conforme a constituição — o `grpc_addr` do lease é discado diretamente, nunca um VIP com
balanceamento, porque o destino precisa ser o dono exato do lease.

---

## R15 — Capacidade do worker

**Decision**: `MAX_SESSIONS_PER_WORKER` (default 200) limita quantos leases um worker adquire na
reconciliação; `WORKER_ADVERTISE_ADDR` (default `hostname:porta`) é o endereço registrado no
lease.

**Rationale**: é knob **operacional de dimensionamento**, não cota de produto — distinção agora
explícita na constituição v2.4.0. Uma instância que não encontra worker com vaga fica em
`conectando` e é adotada assim que abrir capacidade (edge case previsto na spec), e o operador
resolve escalando workers.

---

## R16 — Métricas (Princípio VI)

**Decision**: sem `instance_id` como label — cardinalidade explodiria com milhares de instâncias.

| Métrica | Tipo | Labels |
| --- | --- | --- |
| `zappermeow_sessions_connected` | gauge | `worker_id` |
| `zappermeow_session_state_transitions_total` | counter | `from`, `to`, `reason` |
| `zappermeow_pairing_attempts_total` | counter | `method` (qr/phone), `result` |
| `zappermeow_session_reconnects_total` | counter | `layer` (client/lease) |
| `zappermeow_lease_acquisitions_total` / `_lost_total` | counter | `worker_id` |
| `zappermeow_stream_replaced_total` | counter | — (alarme de posse exclusiva) |
| `zappermeow_ws_clients` | gauge | — |
| `zappermeow_session_command_duration_seconds` | histogram | `method`, `result` |

Correlação por instância continua nos **logs** (`slog` com `tenant_id`/`instance_id`), que é o
lugar certo para alta cardinalidade.
