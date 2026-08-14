# Research — Complementos de Conexão da Instância (003-connection-extras)

**Date**: 2026-08-14 | **Spec**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md)

Toda decisão abaixo foi verificada contra o **código real do fork** na pseudo-versão pinada do
`go.mod` (`github.com/polymorfa/hypermeow@v0.0.0-20260811214557-f9db181f1dfa`, module cache local),
como exige o Princípio VII. Citações `arquivo:linha` referem-se a esse módulo, salvo indicação.

---

## R1 — Aplicação do proxy: na construção do cliente, com desativação explícita do ambiente

**Decision**: o worker aplica o proxy na **construção** da sessão, via
`Client.SetProxyAddress(url)` quando há proxy configurado e `Client.SetProxy(nil)` quando **não**
há — sempre uma das duas chamadas, nunca nenhuma. Sem opções (`SetProxyOptions` zero): o proxy
cobre websocket pré-login, websocket logado e mídia. Esquemas aceitos: `http`, `https`, `socks5` —
os mesmos que `SetProxyAddress` resolve.

**Rationale**:

- `SetProxy` documenta: *"Must be called before Connect() to take effect in the websocket
  connection. If you want to change the proxy after connecting, you must call Disconnect() and
  then Connect() again manually"* (`client.go:382-410`). Aplicar na construção, antes de qualquer
  `Connect`, é a única sequência que a biblioteca prescreve — e sequências prescritas são literais
  (Princípio VII).
- **Por padrão a biblioteca lê `https_proxy` do ambiente** (godoc de `SetProxy`; comportamento
  de `http.ProxyFromEnvironment` no transport default). FR-006 exige que instância sem proxy
  conecte direto ignorando o ambiente do worker — logo `SetProxy(nil)` explícito é obrigatório no
  caminho sem proxy. Sem essa chamada, um `https_proxy` herdado do container vazaria tráfego de
  todos os tenants por um proxy que ninguém configurou.
- `SetProxyAddress` (`client.go:351-378`): string vazia → `SetProxy(nil)`; `http/https` →
  `http.ProxyURL`; `socks5` → `proxy.FromURL` com `net.Dialer{Timeout: 30s, KeepAlive: 30s}` →
  `SetSOCKSProxy`; esquema desconhecido → erro `unsupported proxy scheme %q`. A validação da API
  (R2 do data-model) espelha exatamente esse contrato para rejeitar na gravação o que a biblioteca
  rejeitaria na conexão.
- Verificado o risco de panic em `SetSOCKSProxy` (`client.go:431` faz type-assert
  `px.(proxy.ContextDialer)` sem checagem): **não nos atinge** — `proxy.FromURL` com `*net.Dialer`
  (que implementa `ContextDialer`) retorna o dialer SOCKS5 de `golang.org/x/net/proxy`, que também
  implementa `ContextDialer`. O caminho `SetProxyAddress` é seguro; o construtor nunca chama
  `SetSOCKSProxy` diretamente.
- Os três HTTP clients (`mediaHTTP`, `websocketHTTP`, `preLoginHTTP`, `client.go:250-252`) apontam
  por padrão para o mesmo client compartilhado; `setTransport` (`client.go:435-445`) clona cada um
  com o transport do proxy. Sem `NoWebsocket`/`OnlyLogin`/`NoMedia`, a cobertura é total — o que
  FR-003 pede. Atenção de implementação: os setters diretos (`SetMediaHTTPClient` etc.,
  `client.go:453-469`) sobrescrevem o proxy e **não são usados** nesta feature.

**Alternatives considered**:

- *Chamar `SetProxy` num cliente vivo ao mudar a configuração* — rejeitado: para o websocket só
  vale na próxima conexão (godoc), o que criaria um estado misto (mídia pelo proxy novo, socket
  pelo velho). A mudança a quente religa a sessão inteira (R3).
- *`SetProxyOptions` granular (só websocket / só mídia)* — rejeitado pela decisão de produto
  (descoberta da spec): um proxy para todo o tráfego. A opção fica documentada para uma feature
  futura sem mudança de schema (a URL é a mesma).

---

## R2 — Mudança de proxy a quente: gravar, depois comandar o dono a religar

**Decision**: o fluxo de `PUT/DELETE /instances/{id}/proxy` é: (1) validar; (2) persistir em
`instances.proxy_url`; (3) se a instância tem dono ativo (lease `running`), RPC
`ApplySettings{fence, proxy_changed: true}` ao dono, que **para a sessão, reconstrói o cliente com
o proxy novo e reconecta sob o mesmo lease**; (4) sem dono ativo, nada a fazer — a próxima
conexão lê a configuração persistida. A tentativa de pareamento em curso, se houver, termina com
`pairing_expired (cancelled)` — religar no meio invalidaria o QR de qualquer forma.

**Rationale**: a reconexão precisa acontecer **no processo dono** (Princípio III — nenhum caminho
fala com a sessão por fora do lease) e a biblioteca exige reconstrução/reconexão para o proxy valer
no websocket (R1). Gravar antes de comandar garante que, se o worker morrer entre os passos, o
failover já nasce com a configuração nova — a reconexão comandada é otimização de latência (SC-002:
≤30s), não a fonte de verdade. O RPC não carrega a URL: o worker relê do Postgres, eliminando a
janela em que RPC e banco divergem.

**Alternatives considered**:

- *API publicar em `sessions:stop` + intenção* — rejeitado: derruba e re-adota via reconciliação
  (~15s a mais), e o caso comum é o dono estar vivo; o RPC com fence já existe como padrão na 002.
- *Recusar mudança com instância conectada* — rejeitado na descoberta da spec (decisão do usuário:
  gravar e reconectar).

---

## R3 — Falha de conexão via proxy: insistir pelo proxy, classificar como motivo próprio

**Decision**: falha de dial/handshake com proxy configurado é classificada com motivo canônico novo
`proxy_connect_failed` (não permanente). O ciclo de retry é o **existente** — `AutoReconnectHook`
com teto e jitter (002) para quedas pós-conexão, e o retry da reconciliação para falhas de
`Connect`. Nenhum caminho remove o proxy ou conecta direto: com `SetProxy(nil)`/`SetProxyAddress`
sempre chamados na construção (R1), não existe rota de código capaz de fazer fallback acidental.

**Rationale**: FR-005 proíbe o fallback (vazamento de IP da plataforma). O motivo próprio na trilha
é o que torna a falha diagnosticável pelo tenant (edge case da spec) sem a plataforma inspecionar o
proxy dele. A detecção é no adaptador: erro retornado por `Connect`/queda quando `proxyConfigured`
→ `proxy_connect_failed`; a métrica `zappermeow_proxy_connect_failures_total` conta ocorrências.

**Alternatives considered**: *health-check ativo do proxy antes de conectar* — rejeitado: seria um
segundo caminho de rede a manter, e o `Connect` já é o teste real; *falha rápida exigindo ação
manual* — rejeitado na descoberta (decisão: insistir pelo proxy).

---

## R4 — Senha do proxy: em claro no Postgres, mascarada em toda leitura

**Decision**: `instances.proxy_url` guarda a URL completa (incluindo `user:pass@` quando houver).
Toda leitura para fora (REST, WS, trilha, logs, erros) passa por `domain.MaskProxyURL`, que
substitui a senha por `***` e preserva esquema, usuário, host e porta. A URL crua só transita:
Postgres → worker (para construir o cliente) e Postgres → nunca mais.

**Rationale**: a senha precisa ser usada a cada conexão — hash é impossível. O mesmo banco, na
mesma rede privada Docker, já guarda o material Signal das sessões (`whatsmeow_*` — chaves de
identidade, pre-keys, sessões), estritamente mais sensível que uma senha de proxy. Criptografia de
aplicação adicionaria gestão de chave (Princípio I) sem mover a superfície real: a chave viveria no
mesmo runtime. O risco real é **exfiltração por resposta/log**, e é esse que o contrato fecha
(FR-007, SC-007). Registrado em Complexity Tracking do plan.md.

**Alternatives considered**: criptografia com chave em secret do runtime — rejeitada (complexidade
sem ganho real, ver plan.md); coluna separada para a senha — rejeitada (duas colunas para o mesmo
segredo, mesmas propriedades).

---

## R5 — `ManualLoginReconnect`: tratamento defensivo, sem mudar o flag da 002

**Decision**: manter `DisableLoginAutoReconnect` no default (`false`) — o pós-pareamento continua
como a 002 validou contra número real: a própria biblioteca reconecta no `515`. O adaptador passa a
**classificar** `events.ManualLoginReconnect` mesmo assim: tipo de trilha
`manual_login_reconnect`, e a reação do worker é agendar reconexão imediata (assíncrona, nunca no
corpo do handler). Cobertura pela sessão roteirizada.

**Rationale** (verificado no fork):

- O evento é emitido **exclusivamente** em `connectionevents.go:27-31`, e só quando
  `cli.DisableLoginAutoReconnect` é `true`. Nosso adaptador seta `EnableAutoReconnect = true` e
  **não** seta `DisableLoginAutoReconnect` (`internal/wa/hypermeow.go:115-118` do repositório) —
  em produção, hoje, o evento nunca dispara.
- Tratar mesmo assim é barato e fecha dois riscos: (a) o flag ser ligado no futuro sem que ninguém
  perceba a dependência; (b) mudança de comportamento do fork em atualização de pseudo-versão
  (o Princípio VII exige releitura, mas a defesa custa um case no classify).
- **Restrição da biblioteca**: esse `dispatchEvent` é **síncrono** (`connectionevents.go:30`, sem
  `go`), diferente dos outros branches. O handler não pode bloquear nem reconectar em linha — a
  reação do worker é sempre agendada fora do handler (o pipeline de eventos da 002 já é
  assíncrono por canal, então a restrição é satisfeita por construção).
- Trocar para `DisableLoginAutoReconnect = true` (plataforma dona do pós-pareamento) foi
  considerado e rejeitado: mexeria num caminho que os seis defeitos da 002 ensinaram a respeitar —
  validado manualmente contra número real — em troca de nenhum requisito da spec; FR-010 pede
  reação **quando o evento é recebido**, não que ele passe a ser o caminho de produção.

**Alternatives considered**: assumir o controle do pós-pareamento com o flag — rejeitado acima;
ignorar o evento por "nunca acontecer" — rejeitado: é exatamente a lacuna que a spec manda fechar,
e a sessão roteirizada torna o teste possível.

---

## R6 — `SetPassive`: reaplicar após **cada** `Connected`; janela ativa transitória é inevitável

**Decision**: interface `wa.Session` ganha `SetPassive(ctx, bool) error` (IQ `passive` —
`connectionevents.go:209-228`). O worker aplica a configuração persistida **imediatamente após
cada evento `Connected`** (conexão nova, reconexão, failover) e também na mudança a quente (RPC
`ApplySettings{passive_changed}` → chamada direta na sessão viva). Falha na aplicação gera retry
curto e log de erro — nunca derruba a sessão.

**Rationale** (verificado no fork — o ponto mais sutil da feature):

- **Nada persiste entre conexões.** O payload de login já nasce `passive = true`
  (`store/clientpayload.go:195`), e a própria biblioteca chama `SetPassive(ctx, false)` a cada
  conexão autenticada, dentro do goroutine pós-`<success>` (`connectionevents.go:200`), **antes**
  de emitir `events.Connected` (`connectionevents.go:204`). Ou seja: toda conexão termina ativa,
  por decisão da biblioteca, independentemente do que fizemos na conexão anterior.
- **A ordem nos salva da corrida**: `SetPassive(false)` automático e o dispatch de `Connected`
  acontecem em sequência no mesmo goroutine. Nosso `SetPassive(true)` disparado pelo `Connected`
  roda estritamente **depois** do `false` automático — convergência garantida, sem corrida.
- **A janela transitória é real e inevitável**: entre o `SetPassive(false)` da biblioteca e o
  nosso `SetPassive(true)` existe ~1 round-trip em que o servidor vê o device ativo. Suprimir a
  chamada automática exigiria patch (proibido — Princípio VII). SC-004 é lida como regime
  permanente; a limitação está no plan.md (Complexity Tracking) e no quickstart.
- `SetPassive` é IQ: **exige socket conectado** (`ErrNotConnected` caso contrário). Por isso a
  mudança a quente com instância desconectada é só gravação — a aplicação acontece no próximo
  `Connected`, que é o mesmo caminho da reaplicação. Um único ponto de aplicação, sem estado extra.

**Alternatives considered**: *aplicar via payload de login* — impossível pela API pública (o campo
é interno ao `getLoginPayload`); *guardar "último modo aplicado" para economizar o IQ* — rejeitado:
a biblioteca reseta o modo a cada conexão, então o IQ pós-`Connected` é sempre necessário quando
`passive_mode = true`, e quando `false` não há chamada nenhuma (o default da biblioteca já é ativo).

---

## R7 — Passkey: traduzir os itens do canal de QR que a 002 ignora; confirmação automática é da biblioteca

**Decision**: a etapa de passkey entra pelo **canal de QR existente** — `pumpQR`
(`internal/wa/hypermeow.go:165`) deixa de ignorar os itens `passkey-request` e
`passkey-confirmation` e os traduz em dois `EventKind` novos: `KindPasskeyChallenge` (payload: o
`WebAuthnPublicKey` serializado em JSON, opaco para a plataforma) e `KindPasskeyCode` (código
`XXXX-XXXX`). A interface ganha `SendPasskeyResponse(ctx, webauthnResponseJSON []byte) error` e
`ConfirmPasskey(ctx) error`. Erros de passkey chegam pelo item `error` do canal e caem no caminho
existente de `pairing_failed`, com `Failure` novo `passkey_error`.

**Rationale** (verificado no fork):

- O passkey é **etapa dentro do pareamento por QR**, não fluxo paralelo: os eventos nascem de
  `<notification>` na conexão pré-login (`notification.go:558-563`) e o `GetQRChannel` já os
  converte em itens do canal sem fechá-lo (`qrchan.go:141-169` — o canal só fecha em
  sucesso/erro/timeout). A 002 já consome esse canal; a 003 completa a tradução.
- **`SkipHandoffUX` já é resolvido pela biblioteca**: quando a prova de continuidade (handoff) do
  QR escaneado há <5min está presente, o próprio `qrChannel.handleEvent` chama
  `SendPasskeyConfirmation` automaticamente e **não** emite item (`qrchan.go:147-156`). O item
  `passkey-confirmation` só chega quando `SkipHandoffUX == false` — exatamente o caso em que o
  tenant precisa exibir o código (FR-017). A plataforma não implementa auto-confirmação: ela já
  existe.
- Ordem e pré-condições dos métodos (validadas no worker antes de chamar a biblioteca, para
  devolver erros RFC 9457 claros em vez de erros crus):
  - `SendPasskeyResponse` (`pair-passkey.go:77-148`): exige desafio pendente; internamente busca o
    `companionRef` (IQ), monta o prólogo e guarda o `passkeyLinkingCache`. Erros são retornados,
    não emitidos como evento.
  - `SendPasskeyConfirmation` (`pair-passkey.go:208-252`): exige cache **com** `encryptionKey` —
    isto é, só após o evento de código. Em sucesso **limpa o cache**: não é reentrante; segunda
    chamada falha com `"no passkey linking cache available"`. O worker traduz para erro de
    "etapa já consumida" (FR-018).
  - Efeito colateral relevante: ao receber o desafio, a biblioteca **rotaciona o
    `AdvSecretKey`** em memória (`pair-passkey.go:69-73`) — QR codes já exibidos deixam de ser
    escaneáveis a partir daí. Comportamento correto: o front segue o fluxo de passkey; o QR
    antigo morre sozinho. Registrado no contrato do WS para o front não tentar re-exibir QR.
  - A chave de handoff expira em **5 minutos** (`pair-passkey.go:45-47`); a janela de pareamento
    da 002 (~160s) é mais curta, então nenhum prazo novo é necessário (assumption da spec
    confirmada pelo código).
- O desafio corrente entra no snapshot de pareamento (`wa:pairing:{id}`) para que um segundo
  cliente WS que abre o canal no meio da etapa receba o estado atual — mesma semântica do QR na
  002.

**Alternatives considered**: *consumir `events.PairPasskey*` pelo handler global em vez do canal
de QR* — rejeitado: o `qrchan` já faz a tradução **e** a auto-confirmação com handoff; duplicar
pelo handler global criaria dois consumidores do mesmo evento (o handler da sessão **e** o do
qrchan) e reintroduziria a corrida que a biblioteca resolveu. O handler global continua vendo os
eventos, e o classify os ignora explicitamente (são do domínio do pareamento).

---

## R8 — Códigos de verificação de identidade: RPC ao dono, resolução PN→LID pelo store, efeitos colaterais documentados

**Decision**: `GET /instances/{id}/identity-verification-codes?contact=<jid|phone>` (API key da
instância) → RPC `GetIdentityVerificationCodes{fence, contact}` ao dono do lease → interface
`wa.Session.IdentityVerificationCodes(ctx, contact) (*VerificationCodes, error)`. Contato aceito
como LID (`@lid`) ou telefone; telefone é resolvido via `Store.LIDs.GetLIDForPN`
(`store/store.go:214` — lookup local no store, sem rede); sem mapeamento → erro
`identity_not_resolvable`. Resposta: LID resolvido, telefone (quando conhecido), username (quando
conhecido), código numérico de 60 dígitos, e os dois payloads de QR em base64 (`display` e
`verification`), para o tenant renderizar.

**Rationale** (verificado no fork — `security_code.go:65-132`):

- **LID-only na biblioteca**: `userID` precisa ser `@lid` (`ErrIdentityVerificationRequiresLID`,
  `security_code.go:26,70-72`) — a resolução de telefone é nossa, e `GetLIDForPN` é a única fonte
  que a spec permite (conhecimento já disponível na instância; descoberta ativa é da feature de
  contatos).
- **Não é chamada local**: faz `GetUserDevices` (IQ de rede) e, para identidades ausentes,
  `fetchPreKeys` + `EnsureIdentity` — **grava identidades no store** e consome pre-keys do
  servidor (`security_code.go:156-221`). Por isso: (a) exige sessão conectada — a API valida
  estado antes do RPC e o worker revalida; (b) o efeito colateral está documentado no contrato
  (é o comportamento padrão do protocolo Signal — TOFU).
- Pré-condições que viram erros claros da API: contato = próprio LID da instância → rejeitado
  (`"cannot generate ... for the local user"`); sem dispositivos → `"no devices found"`;
  identidade não confiável → erro. O store SQL do fork **implementa** `IdentityKeyReader`
  (`store/sqlstore/identity_reader_test.go:92-109` prova `GetManyIdentities`), então
  `ErrIdentityKeyReaderUnsupported` não ocorre com o nosso store — checado para não depender de
  sorte.
- O código de 60 dígitos e os QRs seguem o esquema safety-number do Signal
  (`security_code.go:223-316`); a plataforma trata tudo como material opaco de exibição — nenhuma
  interpretação, nenhum armazenamento (resultado não persistido, FR do data-model).

**Alternatives considered**: *executar na API com um cliente próprio* — violaria o lease
(Princípio III) e exigiria segundo acesso ao store da sessão; *persistir códigos para cache* —
rejeitado: mudam quando identidades mudam (re-instalação), e servir código velho é falha de
segurança do produto.

---

## R9 — `StreamError`: classificar como queda transitória com código na trilha

**Decision**: `classify.go` ganha o case `*events.StreamError` → motivo canônico `stream_error`,
**não permanente**, com `detail: {"stream_error_code": "<Code>"}` na trilha (o `Raw` completo não
é persistido). A reconexão segue o caminho padrão (AutoReconnectHook + reconciliação).

**Rationale** (verificado no fork — `connectionevents.go:19-70`, `types/events/events.go:255-264`):

- O evento é o branch `default` de `handleStreamError`: só chega com **códigos desconhecidos** —
  todos os códigos conhecidos (`515`, `401/conflict`, `replaced`, `503`, `413/414`) são tratados
  internamente e emitidos como outros eventos, que a 002 já classifica.
- `StreamError` **não** implementa `PermanentDisconnect` (`events.go:105-119`) e o handler não
  chama `expectDisconnect()` — a própria biblioteca espera a queda do socket e o auto-reconnect.
  Classificar como não-permanente é aderir ao comportamento dela, não uma escolha nossa.
- Antes do switch, a biblioteca já marcou `isLoggedIn = false` e falhou as requisições pendentes
  (`connectionevents.go:20-21`) — nenhuma limpeza extra cabe ao adaptador; material de sessão é
  preservado (edge case da spec: stream error não é logout).
- O `Raw` (`*waBinary.Node`) pode conter payload arbitrário do servidor; persistir só o `Code`
  evita lixo/bloat na trilha e qualquer risco de material sensível em `detail` (FR-043 da 002).
- Teste (constituição v2.5.0-a/e): sessão roteirizada emite `StreamError` com código inédito;
  prova-se que sem o case novo o evento era ignorado (regressão provada) e que com ele a trilha
  registra e a reconexão ocorre.

**Alternatives considered**: *tratar códigos específicos do `StreamError`* — rejeitado: por
definição só chegam códigos que a biblioteca não conhece; enumerar os desconhecidos é adivinhar.
Se um código recorrente aparecer em produção, a trilha o revela e a decisão vira dado.

---

## R10 — Snapshot e ordem de eventos do passkey no WebSocket

**Decision**: o snapshot de pareamento em `wa:pairing:{id}` passa a ter um campo de **fase**:
`qr` (código corrente, como na 002), `passkey_challenge` (desafio pendente) ou `passkey_code`
(código de conferência pendente). O primeiro frame de um cliente WS que conecta no meio da etapa
entrega a fase corrente. Os frames novos (`pairing_passkey_challenge`, `pairing_passkey_code`)
seguem o mesmo envelope e sequenciamento (`wa:seq:{id}`) da 002.

**Rationale**: a 002 garante que um segundo observador recebe o estado atual imediatamente
(cenário 4 da US1 da 002); a etapa de passkey não pode quebrar essa propriedade — sem fase no
snapshot, um front que reconectar o WS durante a etapa ficaria cego (o QR corrente já morreu com a
rotação do AdvSecret — R7). A ordem entre os canais (item do canal de QR × erro pelo handler
global) é exercitada **nas duas ordens** na sessão roteirizada, como exige a constituição
v2.5.0-b.

**Alternatives considered**: *manter só o QR no snapshot* — quebraria o observador tardio;
*persistir o desafio no Postgres* — rejeitado: é efêmero como a tentativa (morre com ela, mesma
justificativa do data-model da 002 §7).

---

## Resumo das superfícies novas (para a Phase 1)

| Superfície | Itens |
| --- | --- |
| `wa.Session` (interface) | `SetPassive`, `SendPasskeyResponse`, `ConfirmPasskey`, `IdentityVerificationCodes`; kinds `KindPasskeyChallenge`, `KindPasskeyCode`; failure `passkey_error`; reasons `stream_error`, `proxy_connect_failed` |
| gRPC `SessionService` | `ApplySettings`, `SubmitPasskeyResponse`, `ConfirmPasskey`, `GetIdentityVerificationCodes` |
| HTTP | `PUT/DELETE /instances/{id}/proxy`, `PUT /instances/{id}/passive-mode`, `POST /instances/{id}/pairing/passkey/response`, `POST /instances/{id}/pairing/passkey/confirm`, `GET /instances/{id}/identity-verification-codes` |
| WebSocket | frames `pairing_passkey_challenge`, `pairing_passkey_code`; snapshot com fase; vocabulário estendido em `pairing_failed` e `disconnected` |
| Postgres | migração `0003`: `instances.proxy_url`, `instances.passive_mode`; tipos/motivos novos em `connection_events` |
| Métricas | `zappermeow_proxy_connect_failures_total`, `zappermeow_passkey_pairings_total`, `zappermeow_stream_errors_total` |
