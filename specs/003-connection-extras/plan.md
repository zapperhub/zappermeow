# Implementation Plan: Complementos de Conexão da Instância

**Branch**: `003-connection-extras` | **Date**: 2026-08-14 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/003-connection-extras/spec.md`

## Summary

Completar a seção "Sessão, autenticação e conexão" sobre o alicerce entregue pela 002, em cinco
capacidades: **proxy de saída por instância** (persistido, aplicado a todo o tráfego, sem fallback
direto), **tratamento dos eventos `StreamError` e `ManualLoginReconnect`** (classificação, trilha e
reação do supervisor), **modo passivo** (flag persistida, aplicada a quente e reaplicada em toda
conexão), **etapa de passkey no pareamento** (desafio WebAuthn e código de conferência descem pelo
WebSocket, resposta e confirmação sobem por endpoints) e **códigos de verificação de identidade**
(consulta operacional por contato, LID ou telefone).

Abordagem técnica: nenhuma peça nova de arquitetura. A feature estende as fronteiras que a 002
criou — colunas novas em `instances` (migração `0003`), métodos novos na interface `wa.Session`,
RPCs novos em `proto/session/v1`, tipos novos no contrato do WebSocket e vocabulário novo na trilha
`connection_events`. O proxy é aplicado pelo worker na construção do cliente (a biblioteca exige
proxy antes do `Connect`); mudança a quente vira um comando gRPC ao dono do lease, que religa a
sessão. O modo passivo é reaplicado pelo worker após **cada** evento `Connected`, porque a
biblioteca reativa o modo ativo automaticamente a cada conexão. A etapa de passkey reaproveita o
canal de QR da 002 — os itens de passkey que o `pumpQR` hoje ignora passam a ser traduzidos — e a
confirmação automática com prova de continuidade (handoff) já é feita pela própria biblioteca.

Todas as decisões da fase de pesquisa foram verificadas contra o **código real do fork** no module
cache (`pair-passkey.go`, `connectionevents.go`, `client.go`, `security_code.go`, `qrchan.go`,
`store/clientpayload.go`), como manda o Princípio VII. Duas constatações de fidelidade merecem
destaque: a biblioteca **sempre** reativa o modo ativo após conectar (o modo passivo da plataforma
converge logo em seguida — janela transitória de um round-trip, registrada em R6) e o evento
`ManualLoginReconnect` só existe sob flag que a 002 não usa — o tratamento é defensivo e testável
via sessão roteirizada (R5).

## Technical Context

**Language/Version**: Go 1.25+

**Primary Dependencies**: **nenhuma dependência nova.** Tudo que a feature precisa já está no
projeto: `github.com/polymorfa/hypermeow` (passkey, proxy, passive, códigos de verificação — todos
símbolos da pseudo-versão pinada, conferidos em R1–R8), chi/huma (endpoints), grpc/protobuf (RPCs
novos no contrato existente), coder/websocket (tipos novos de frame), pgx/sqlc (colunas e queries
novas). `golang.org/x/net/proxy` entra apenas como dependência **indireta** já presente via
hypermeow (SOCKS5).

**Storage**: PostgreSQL 17 — migração `0003`: colunas `proxy_url` e `passive_mode` em `instances`;
vocabulário novo em `connection_events` (tipos `stream_error`, `manual_login_reconnect`,
`proxy_updated`, `passive_mode_updated`, `passkey_challenge`, `passkey_responded`,
`passkey_confirmed`; motivos `stream_error`, `proxy_connect_failed`, `proxy_updated`). Nenhuma
tabela nova. A senha do proxy fica na própria `proxy_url` no Postgres — mesmo banco que já guarda o
material Signal das sessões (`whatsmeow_*`) — e é mascarada em toda leitura (R4). Redis: nenhuma
chave nova; o desafio de passkey corrente entra no snapshot de pareamento existente
(`wa:pairing:{id}`).

**Testing**: `go test` + testify para classificação nova (StreamError/ManualLoginReconnect),
validação de URL de proxy e mascaramento; testcontainers-go (Postgres + Redis reais) para
persistência das configurações, reconexão comandada por mudança de proxy e trilha; sessão
roteirizada (`wa.FakeSession`) estendida com os eventos novos — incluindo as **duas ordens**
exigidas pela constituição v2.5.0 onde eventos chegam por canais distintos (passkey pelo canal de
QR × erro pelo handler global — R9). Caminho real: roteiro manual no [quickstart.md](./quickstart.md)
(proxy real, número com passkey, verificação no aparelho).

**Target Platform**: Linux server (distroless); nenhuma mudança de topologia de deploy — os mesmos
serviços `api` e `session-worker` da 002 em Swarm e Compose.

**Project Type**: Web service + worker stateful (subcomandos `serve` e `session-worker` do binário
único `zappermeow`).

**Performance Goals**: reconexão automática após mudança de proxy concluída em ≤30s (SC-002);
instância volta a `connected` em ≤10s após `ManualLoginReconnect` roteirizado (SC-003); consulta de
códigos de verificação em ≤5s com contato conhecido (SC-006 — envolve IQs de rede ao WhatsApp);
demais metas da 002 permanecem válidas e não podem regredir.

**Constraints**: proxy é aplicado **antes** do `Connect` — mudança a quente exige religar a sessão
via dono do lease, nunca por fora dele (Princípio III); conexão direta com proxy configurado é
PROIBIDA — a plataforma desativa explicitamente o proxy de ambiente em toda sessão (R3); senha de
proxy nunca em resposta, log, evento ou trilha (FR-007, Princípio II); o modo passivo tem janela
ativa transitória inevitável de um round-trip a cada conexão — limitação da biblioteca registrada
em R6, patch local é proibido (Princípio VII); `SendPasskeyConfirmation` não é reentrante e a
chave de handoff expira em 5 minutos — a ordem dos comandos é validada no worker (R2); a consulta
de códigos de verificação exige LID e sessão conectada, e grava identidades no store como efeito
colateral documentado (R8).

**Scale/Scope**: 5 user stories, 26 FRs, 7 SCs; 6 endpoints HTTP novos + 2 alterados (consulta da
instância e resumo de conexão ganham proxy mascarado e modo passivo); 4 RPCs novos no
`SessionService`; 3 tipos novos de evento no WebSocket + vocabulário estendido em 2 existentes;
4 métodos novos na interface `wa.Session`; 1 migração; nenhuma tabela nova.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

Avaliado contra a constituição **v2.5.0** (disciplina de teste da fronteira WhatsApp + fidelidade
ao HyperMeow).

| # | Princípio | Avaliação | Status |
|---|-----------|-----------|--------|
| I | Simplicidade e Stdlib-First | Zero dependências novas, zero peças de infra novas, zero tabelas novas. A feature é extensão de fronteiras existentes: colunas, RPCs, tipos de evento, métodos de interface. A senha do proxy fica no Postgres sem camada de criptografia própria — decisão justificada em R4 (o mesmo banco já guarda material Signal em claro; cofre de chaves seria peça nova vetada pelo princípio) | ✅ PASS |
| II | Multi-Tenancy com Isolamento por Instância | Proxy, modo passivo e passkey seguem a autenticação das rotas de conexão da 002 (JWT de tenant **ou** API key da instância, sempre resolvendo para a instância da URL); códigos de verificação são exclusivos da API key da instância, como a spec exige (FR-025). Todas as rotas novas passam pelo limitador GCRA existente. Segredo do proxy mascarado em toda leitura; negação sem confirmar existência preservada | ✅ PASS |
| III | Posse Exclusiva de Sessão (NON-NEGOCIÁVEL) | Nenhum caminho novo fala com a sessão por fora do lease: mudança de proxy/passivo a quente, resposta de passkey, confirmação e consulta de códigos são todos RPCs com `Fence` verificado pelo dono. A reconexão por mudança de proxy é executada pelo próprio dono (stop + rebuild + connect sob o mesmo lease), nunca por um segundo processo | ✅ PASS |
| IV | Contrato de API como Fonte de Verdade | Endpoints novos tipados em huma com envelope padrão e RFC 9457 + `code`; RPCs novos em `proto/session/v1` versionado (mudança aditiva, compatível); frames novos do WS documentados no contrato próprio da 002 (`websocket-events.md`), mesmo desvio já registrado e justificado na 002 | ✅ PASS (desvio do WS já registrado na 002, inalterado) |
| V | Testes Contra Infraestrutura Real | Postgres/Redis reais para tudo que é infra; sessão roteirizada estendida cobrindo **todo evento novo que o adaptador classifica** (v2.5.0-a), as **duas ordens** dos eventos de passkey que chegam por canais distintos (v2.5.0-b), e prova de regressão onde houver correção. O que só o aparelho revela — passkey de verdade, proxy visível no IP de saída, código de conferência na tela — vai para o roteiro manual do quickstart (v2.5.0-f) | ✅ PASS |
| VI | Observabilidade Estruturada | Logs com `tenant_id`/`instance_id` em toda transição nova; métricas novas: `zappermeow_proxy_connect_failures_total`, `zappermeow_passkey_pairings_total`, `zappermeow_stream_errors_total` (sem label por instância — cardinalidade, decisão R16 da 002 mantida) | ✅ PASS |
| VII | Fidelidade ao HyperMeow | Toda a Phase 0 foi conferida no código da pseudo-versão pinada, símbolo a símbolo, com arquivo:linha em research.md — incluindo três constatações que contradizem a expectativa ingênua: proxy só vale para o websocket na próxima conexão, o modo ativo é reimposto pela biblioteca a cada conexão, e a confirmação automática de passkey com handoff já é da biblioteca. Integração continua confinada a `internal/wa` | ✅ PASS |

**Resultado inicial**: PASS — nenhum desvio novo; o único desvio herdado (frames do WS fora do
envelope HTTP) foi registrado e justificado na 002 e permanece válido.

**Re-check pós Phase 1**: PASS. O design não introduziu dependência, infra, tabela nem rota sem
autenticação. Pontos que a revisão de PR deve conhecer: a senha de proxy em claro no Postgres
(R4, justificada), a janela ativa transitória do modo passivo (R6, limitação da biblioteca
registrada como manda o Princípio VII) e o tratamento defensivo de `ManualLoginReconnect` (R5 —
comportamento de produção hoje não emite o evento; a cobertura existe pela sessão roteirizada).

## Project Structure

### Documentation (this feature)

```text
specs/003-connection-extras/
├── plan.md              # This file (/speckit-plan command output)
├── research.md          # Phase 0 output — decisões verificadas contra o código do fork
├── data-model.md        # Phase 1 output — migração 0003, vocabulários, snapshot de pareamento
├── quickstart.md        # Phase 1 output — validação fim-a-fim (automatizável × [manual])
├── contracts/           # Phase 1 output
│   ├── http-api.md      # 6 endpoints novos + 2 alterados
│   ├── websocket-events.md  # 3 tipos novos de frame + vocabulário estendido
│   └── session-grpc.md  # 4 RPCs novos no SessionService
└── tasks.md             # Phase 2 output (/speckit-tasks command - NOT created by /speckit-plan)
```

### Source Code (repository root)

```text
zappermeow/
├── proto/
│   └── session/v1/session.proto   # ALTERADO — +ApplySettings, +SubmitPasskeyResponse,
│                                  #   +ConfirmPasskey, +GetIdentityVerificationCodes (aditivo)
├── internal/
│   ├── pb/sessionv1/              # REGERADO — protoc, verificado no CI
│   ├── api/
│   │   ├── handlers/
│   │   │   ├── connection.go      # ALTERADO — proxy (PUT/DELETE), passive (PUT), passkey (2×POST)
│   │   │   ├── verification.go    # NOVO — GET códigos de verificação (API key only)
│   │   │   └── instances.go       # ALTERADO — proxy mascarado + passive na consulta
│   │   └── sessionclient/         # ALTERADO — wrappers dos RPCs novos
│   ├── worker/
│   │   ├── session.go             # ALTERADO — aplicar proxy/passive, orquestrar passkey,
│   │   │                          #   reagir a stream_error / manual_login_reconnect
│   │   ├── supervisor.go          # ALTERADO — religar sessão em mudança de proxy
│   │   └── grpcserver.go          # ALTERADO — RPCs novos com fencing
│   ├── wa/
│   │   ├── session.go             # ALTERADO — interface: SetPassive, SendPasskeyResponse,
│   │   │                          #   ConfirmPasskey, IdentityVerificationCodes; kinds novos
│   │   ├── hypermeow.go           # ALTERADO — SetProxyAddress/SetProxy(nil) na construção,
│   │   │                          #   pumpQR traduz itens de passkey, métodos novos
│   │   ├── classify.go            # ALTERADO — StreamError e ManualLoginReconnect
│   │   └── fake.go                # ALTERADO — roteiros de passkey, stream_error, proxy
│   ├── domain/
│   │   ├── instance.go            # ALTERADO — ProxyURL (com mascaramento), PassiveMode
│   │   ├── connectionevent.go     # ALTERADO — tipos e motivos novos
│   │   ├── proxy.go               # NOVO — validação de URL de proxy (esquemas, formato)
│   │   └── services/connection.go # ALTERADO — casos de uso novos
│   ├── store/queries/
│   │   └── connection.sql         # ALTERADO — leitura/escrita de proxy_url e passive_mode
│   ├── events/events.go           # ALTERADO — tipos novos de frame WS
│   ├── metrics/metrics.go         # ALTERADO — 3 métricas novas
│   └── config/config.go           # sem mudança prevista
└── migrations/                    # + 0003_connection_extras.{up,down}.sql
```

**Structure Decision**: nenhum pacote novo além de dois arquivos (`domain/proxy.go`,
`api/handlers/verification.go`) dentro de pacotes existentes. A feature confirma o valor da
fronteira `internal/wa` criada na 002: todos os símbolos novos do HyperMeow (passkey, proxy,
passive, códigos) entram exclusivamente por `hypermeow.go`/`classify.go`, e a sessão roteirizada
ganha os roteiros novos sem que worker, lease ou API conheçam tipos da biblioteca.

## Complexity Tracking

> Desvios conscientes que a revisão de PR deve conhecer.

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| Senha do proxy armazenada em claro (dentro de `proxy_url`) no Postgres, sem criptografia de aplicação | A senha precisa ser **usada** (não apenas verificada) a cada conexão — hash é impossível. O mesmo Postgres, na mesma rede privada, já guarda o material criptográfico Signal das sessões (`whatsmeow_*`), que é estritamente mais sensível; proteger a senha do proxy com camada extra não reduziria a superfície real. O contrato garante que ela nunca sai: mascarada em toda leitura, ausente de logs/trilha/eventos (FR-007) | Criptografia de aplicação exigiria gestão de chave (peça/complexidade nova, Princípio I) e daria segurança ilusória: a chave viveria no mesmo runtime com acesso ao mesmo banco. Um cofre externo (Vault etc.) é peça de infra nova, vetada pelo Princípio I sem necessidade comprovada |
| Modo passivo tem janela ativa transitória de ~1 round-trip a cada conexão (SC-004 foi emendado na spec para admitir explicitamente essa exceção) | A biblioteca reimpõe o modo ativo incondicionalmente após autenticar, antes de emitir `Connected` (`connectionevents.go:200`); a plataforma converge para passivo imediatamente após `Connected`. Patch local é proibido (Princípio VII) | Suprimir a chamada automática exigiria patch no fork; interceptá-la é impossível pela API pública. A janela é de milissegundos, invisível na prática, e está registrada em R6 + quickstart como limitação conhecida |
