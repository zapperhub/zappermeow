# Feature Specification: Conexão da Instância com o WhatsApp

**Feature Branch**: `002-instance-connection`

**Created**: 2026-08-13

**Status**: Draft

**Input**: User description: "vamos para a próxima entrega, agora de valor: fatia de conexão da instância com a hypermeow"

## Contexto

A feature 001 (Fundação de Contas) entregou a instância como **registro**: um nome amigável vinculado a um tenant, com API keys próprias. Nenhuma instância fala com o WhatsApp ainda.

Esta feature dá vida ao registro: parear um número real, mantê-lo online de forma confiável e expor esse estado ao tenant. É a primeira entrega em que o produto faz o que promete — um número de WhatsApp controlável por API.

Escopo desta fatia: pareamento (QR e código de telefone), ciclo de vida da conexão (conectar, desconectar, deslogar), continuidade automática (reconexão e failover com posse exclusiva da sessão), estado observável (consulta, tempo real e histórico). Envio e recebimento de mensagens **não** fazem parte desta fatia.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Parear um número por QR code e vê-lo conectado (Priority: P1)

O admin do tenant escolhe uma instância registrada e pede para conectá-la. O sistema inicia uma tentativa de pareamento e passa a emitir QR codes renovados no canal de eventos em tempo real da instância. O admin abre esse canal, exibe o QR, escaneia com o aparelho e vê a instância transitar para conectada — com o número, o identificador WhatsApp completo do dispositivo companheiro e o nome de exibição já visíveis na consulta da instância.

**Why this priority**: É o coração da feature e o menor MVP com valor real: sem pareamento, nenhuma outra capacidade do produto existe. Com apenas esta story entregue, um número de WhatsApp já fica online sob controle da plataforma.

**Independent Test**: Com um tenant e uma instância registrada (herdados da 001), chamar a ação de conectar, abrir o canal de eventos, escanear o QR com um número de teste e confirmar que a instância aparece como conectada e com a identidade do número preenchida — sem depender de nenhuma outra story desta feature.

**Acceptance Scenarios**:

1. **Given** uma instância registrada e nunca pareada, **When** o admin aciona a conexão, **Then** a instância entra no estado "pareando" e um QR code válido é emitido no canal de eventos em tempo real em poucos segundos.
2. **Given** uma tentativa de pareamento em curso, **When** o QR corrente se aproxima do vencimento sem ser escaneado, **Then** um novo QR é emitido no mesmo canal antes que o anterior deixe de funcionar.
3. **Given** um QR válido exibido, **When** o número é escaneado e confirmado no aparelho, **Then** a instância transita para "conectada", o evento de pareamento bem-sucedido é emitido no canal e o número, o identificador WhatsApp completo (com o identificador do dispositivo companheiro) e o nome de exibição passam a constar na consulta da instância.
4. **Given** uma tentativa de pareamento em curso, **When** um segundo cliente abre o canal de eventos da mesma instância, **Then** ele recebe imediatamente o estado atual com o QR corrente e passa a acompanhar os mesmos eventos que o primeiro.
5. **Given** uma tentativa de pareamento em curso, **When** a janela de pareamento se esgota sem ninguém escanear, **Then** a tentativa expira, um evento de expiração é emitido e a instância volta ao estado anterior, podendo ser reiniciada por uma nova chamada de conexão.
6. **Given** uma instância de outro tenant, **When** o admin tenta conectá-la ou abrir seu canal de eventos, **Then** a operação é negada sem confirmar a existência da instância.

---

### User Story 2 - Controlar o ciclo de vida da conexão (Priority: P2)

Com a instância pareada, o admin controla explicitamente quando ela deve estar online: desconectar (colocar o número offline mantendo o pareamento, para reconectar depois sem novo QR) e deslogar (encerrar a sessão no WhatsApp, removendo o aparelho da lista de dispositivos conectados e apagando o material da sessão, o que exige novo pareamento). Excluir uma instância conectada desconecta e desloga antes de remover o registro.

**Why this priority**: Sem controle de saída, o pareamento é uma via de mão única — o tenant não consegue pausar um número, migrar de aparelho ou encerrar um cliente sem deixar sessão órfã. É também um requisito de privacidade: o material da sessão é credencial do WhatsApp do cliente.

**Independent Test**: Com uma instância conectada (por US1 ou carga de teste), desconectar e confirmar que ela fica offline; reconectar e confirmar que volta sem novo QR; deslogar e confirmar que a próxima conexão exige QR novo; excluir uma instância conectada e confirmar que ela é desconectada e deslogada antes da remoção.

**Acceptance Scenarios**:

1. **Given** uma instância conectada, **When** o admin aciona a desconexão, **Then** ela transita para "desconectada", a intenção de operação passa a "parada" e o evento correspondente é emitido no canal.
2. **Given** uma instância desconectada com pareamento preservado, **When** o admin aciona a conexão novamente, **Then** ela reconecta sem exigir novo QR e volta a "conectada".
3. **Given** uma instância conectada, **When** o admin aciona o logout, **Then** a sessão é encerrada no WhatsApp, o material da sessão é apagado, a instância volta ao estado "registrada" e a identidade do número pareado é preservada apenas no histórico.
4. **Given** uma instância deslogada, **When** o admin aciona a conexão, **Then** uma nova tentativa de pareamento por QR é iniciada.
5. **Given** uma instância conectada, **When** o admin exclui a instância, **Then** o sistema desconecta e desloga a sessão de forma limpa antes de remover o registro, e nenhuma sessão permanece ativa para aquele número.
6. **Given** uma instância já desconectada, **When** o admin aciona a desconexão de novo, **Then** a operação é aceita sem erro e o estado permanece "desconectada".

---

### User Story 3 - Acompanhar estado atual e histórico da conexão (Priority: P3)

O admin consulta, a qualquer momento, o estado da conexão de uma instância — se está online, desde quando, qual número está pareado, qual foi o motivo e o horário da última queda. Consulta também a trilha de eventos de conexão da instância (conectou, caiu, pareou, deslogou, expirou), com retenção limitada, para investigar instabilidade e dar suporte.

**Why this priority**: Transforma a conexão em algo operável. Sem visibilidade, o tenant descobre que o número caiu apenas quando uma mensagem falha — e não tem como diagnosticar. Entrega valor sozinha sobre qualquer instância já pareada.

**Independent Test**: Com uma instância que passou por pelo menos uma conexão e uma queda, consultar o estado e verificar o motivo da última desconexão; consultar a trilha e verificar que todas as transições aparecem em ordem cronológica com horário e motivo.

**Acceptance Scenarios**:

1. **Given** uma instância conectada, **When** o admin consulta seu estado, **Then** recebe o estado atual, a intenção de operação, o horário em que a conexão foi estabelecida e a identidade do número pareado.
2. **Given** uma instância que caiu por perda de rede, **When** o admin consulta seu estado, **Then** recebe o motivo e o horário da última desconexão.
3. **Given** uma instância com várias transições registradas, **When** o admin consulta a trilha de eventos de conexão, **Then** recebe os eventos em ordem cronológica, cada um com tipo, horário e motivo quando aplicável.
4. **Given** uma instância nunca pareada, **When** o admin consulta seu estado, **Then** recebe o estado "registrada" sem identidade de número e sem motivo de desconexão, em vez de um erro.
5. **Given** eventos de conexão mais antigos que o período de retenção, **When** o admin consulta a trilha, **Then** eles não aparecem, e os eventos dentro do período continuam disponíveis.
6. **Given** uma instância de outro tenant, **When** o admin consulta seu estado ou sua trilha, **Then** a operação é negada sem confirmar a existência da instância.

---

### User Story 4 - Continuidade automática com posse exclusiva da sessão (Priority: P4)

Uma instância conectada permanece com a intenção de estar online. Se o processo que mantinha a sessão morrer, for reiniciado em um deploy ou perder a máquina, outro processo assume a sessão automaticamente e reconecta o número, sem ninguém pedir e sem novo QR. Em nenhum momento dois processos mantêm a mesma sessão ao mesmo tempo.

**Why this priority**: É o que separa um brinquedo de uma plataforma de produção. Depende de haver pareamento (US1) para ser observável, por isso vem depois — mas sem ela todo deploy derruba os números dos clientes.

**Independent Test**: Com uma instância conectada, encerrar abruptamente o processo que a mantinha e verificar que ela volta ao estado "conectada" sozinha dentro do tempo alvo; em seguida, executar um encerramento planejado e verificar que a transição ocorre de forma limpa e mais rápida.

**Acceptance Scenarios**:

1. **Given** uma instância conectada com intenção "ativa", **When** o processo que mantinha a sessão é encerrado abruptamente, **Then** outro processo assume a sessão e a instância volta a "conectada" sem intervenção manual e sem novo pareamento.
2. **Given** uma instância conectada, **When** um encerramento planejado ocorre, **Then** a sessão é liberada de forma limpa e adotada por outro processo, com o mínimo de tempo offline.
3. **Given** uma instância cuja posse mudou de processo, **When** uma operação disparada contra o processo antigo chega atrasada, **Then** ela é rejeitada e não afeta a sessão, garantindo que apenas o dono corrente atue.
4. **Given** uma instância com intenção "parada" (desconectada explicitamente), **When** o sistema é reiniciado por completo, **Then** ela permanece offline e nenhum processo assume sua sessão.
5. **Given** uma instância conectada, **When** todo o sistema é reiniciado, **Then** ela volta a "conectada" sem exigir novo QR.
6. **Given** uma perda de conexão por instabilidade de rede, **When** as tentativas de reconexão ocorrem, **Then** elas respeitam intervalos progressivos entre tentativas e continuam indefinidamente enquanto a intenção for "ativa".

---

### User Story 5 - Reagir a sessões invalidadas pelo WhatsApp (Priority: P5)

Quando o próprio WhatsApp invalida a sessão — o usuário remove o aparelho pelo celular, o número recebe banimento temporário, ou a sessão é substituída em outro lugar — o sistema para de tentar reconectar, registra o motivo, marca a instância no estado terminal correspondente e avisa o tenant pelo canal de eventos. Reconectar exige ação humana.

**Why this priority**: É a exceção que protege o cliente e a plataforma: insistir em reconectar um número banido agrava o problema e queima recursos. Só faz sentido depois que a reconexão automática (US4) existe, pois é justamente o freio dela.

**Independent Test**: Com uma instância conectada, provocar um logout pelo aparelho e verificar que a instância vai para "deslogada", que o motivo é registrado, que o tenant é avisado e que nenhuma nova tentativa de reconexão ocorre.

**Acceptance Scenarios**:

1. **Given** uma instância conectada, **When** o aparelho remove o dispositivo companheiro correspondente àquela instância, **Then** ela transita para "deslogada", o motivo é registrado na trilha, o evento é emitido no canal, nenhuma reconexão automática é tentada e outras instâncias pareadas ao mesmo número permanecem conectadas.
2. **Given** uma instância conectada, **When** o WhatsApp informa banimento temporário do número, **Then** a instância transita para "banida", o prazo informado (quando houver) é registrado e nenhuma reconexão automática é tentada.
3. **Given** uma instância conectada, **When** a sessão é substituída por outra conexão do mesmo dispositivo em outro lugar, **Then** a instância transita para estado terminal com o motivo "sessão substituída" e nenhuma reconexão automática é tentada.
4. **Given** uma instância em estado terminal por invalidação, **When** o admin aciona a conexão, **Then** o sistema aceita e inicia uma nova tentativa de pareamento por QR.
5. **Given** uma instância que caiu apenas por falha de rede, **When** o sistema avalia a causa, **Then** ela **não** é tratada como invalidação e a reconexão automática continua.

---

### User Story 6 - Parear por código de telefone, sem QR (Priority: P6)

Em vez de escanear um QR, o admin informa o número de telefone e recebe um código de pareamento de 8 caracteres, que digita no aparelho para vincular o dispositivo. Útil quando não há como exibir imagem ou quando a pessoa que opera o número está longe do painel.

**Why this priority**: É um caminho alternativo para o mesmo resultado da US1. Entrega valor real de acessibilidade e provisionamento remoto, mas o produto é utilizável sem ele.

**Independent Test**: Com uma instância registrada, solicitar pareamento por código informando um número de teste, digitar o código recebido no aparelho e confirmar que a instância fica conectada.

**Acceptance Scenarios**:

1. **Given** uma instância registrada, **When** o admin solicita pareamento por código informando o número no formato internacional, **Then** o sistema devolve um código de pareamento e a instância entra em "pareando".
2. **Given** um código de pareamento emitido, **When** ele é digitado no aparelho dentro da janela de validade, **Then** a instância transita para "conectada" e o evento é emitido no canal.
3. **Given** um número em formato inválido, **When** o admin solicita pareamento por código, **Then** a solicitação é recusada com mensagem clara, sem alterar o estado da instância.
4. **Given** uma tentativa de pareamento por QR em curso, **When** o admin solicita pareamento por código para a mesma instância, **Then** o sistema encerra a tentativa anterior e passa a valer apenas a nova modalidade.
5. **Given** um código emitido e não utilizado, **When** a janela de pareamento se esgota, **Then** a tentativa expira do mesmo modo que no fluxo de QR.

---

### User Story 7 - Operar a conexão por integração, sem login humano (Priority: P7)

Um sistema integrado do tenant, autenticado pela API key da própria instância, executa as mesmas ações de conexão que o admin logado: conectar, consultar estado, consultar trilha, desconectar, deslogar e ouvir o canal de eventos em tempo real. Isso permite provisionamento e monitoramento automatizados, inclusive exibir o QR na interface do próprio integrador.

**Why this priority**: É paridade de credenciais sobre capacidades que já existem nas stories anteriores — valioso para automação, mas nenhum fluxo fica bloqueado sem ele.

**Independent Test**: Com uma instância que possua API key ativa, executar todo o ciclo (conectar, ouvir eventos, consultar estado, desconectar) usando apenas a API key, e confirmar que a key de outra instância é recusada nas mesmas rotas.

**Acceptance Scenarios**:

1. **Given** uma API key ativa da instância, **When** ela é usada para acionar a conexão, consultar estado/trilha, desconectar, deslogar ou abrir o canal de eventos, **Then** todas as operações são aceitas com o mesmo comportamento do admin logado.
2. **Given** uma API key da instância A, **When** ela é usada em qualquer ação de conexão da instância B, mesmo do mesmo tenant, **Then** a operação é negada.
3. **Given** uma API key revogada, **When** ela é usada em qualquer ação de conexão, **Then** a operação é negada imediatamente.
4. **Given** um tenant suspenso, **When** qualquer credencial dele é usada em ações de conexão, **Then** a operação é negada.
5. **Given** nenhuma credencial, **When** o canal de eventos de uma instância é acessado, **Then** a conexão é recusada antes de qualquer evento ser entregue.

---

### Edge Cases

- **Conexão pedida em instância já conectada**: a operação é aceita sem efeito colateral, mantém o estado "conectada" e reafirma a intenção "ativa" — nunca reinicia a sessão nem gera novo QR.
- **Chamadas simultâneas de conexão para a mesma instância**: apenas uma tentativa de pareamento existe por instância; chamadas concorrentes convergem para a mesma tentativa em vez de criar duas.
- **Nenhum processo de sessão disponível**: a intenção "ativa" é registrada e a instância fica em "conectando"; assim que um processo estiver disponível, a sessão é assumida automaticamente, sem o admin precisar repetir o comando.
- **Falha ao alcançar o processo dono da sessão**: o sistema reavalia quem é o dono corrente e tenta uma vez o novo destino antes de responder erro ao chamador.
- **Cliente do canal de eventos chega atrasado**: recebe imediatamente o estado atual (incluindo o QR corrente, se houver pareamento em curso) e depois o fluxo contínuo; eventos anteriores à conexão não são reenviados.
- **Queda de rede do cliente que ouvia o canal**: o pareamento em curso não é cancelado pela saída do ouvinte; ao reconectar, o cliente recebe novo estado inicial.
- **Mesmo número pareado em várias instâncias**: é cenário legítimo — cada pareamento é um dispositivo companheiro distinto do mesmo número e as sessões convivem sem se derrubar. O sistema permite, distingue as instâncias pelo identificador de dispositivo e apenas informa, na consulta de estado, quais outras instâncias compartilham o número.
- **Conta do cliente sem espaço para novo dispositivo vinculado**: o bloqueio ocorre no próprio aplicativo do WhatsApp, que impede o escaneamento — a plataforma não recebe recusa nem erro e observa apenas a expiração normal da tentativa. Nada de específico a implementar; a mensagem de expiração pode citar essa possibilidade entre as causas.
- **Dispositivo desvinculado pelo aparelho**: afeta apenas a instância correspondente àquele dispositivo; as demais instâncias pareadas ao mesmo número seguem conectadas.
- **Re-pareamento com número diferente do anterior**: é permitido; a troca é registrada na trilha e a identidade da instância é atualizada para o novo número.
- **Exclusão de instância durante pareamento em curso**: a tentativa é cancelada, o canal de eventos é encerrado e o registro é removido.
- **Tenant suspenso com instâncias conectadas**: as sessões são desconectadas e nenhuma conexão nova é aceita enquanto durar a suspensão; a intenção de operação é preservada e restaurada na reativação.
- **Instância sem resposta do WhatsApp (keepalive perdido)**: tratada como perda de conexão, não como invalidação — a instância transita para "conectando" e a reconexão automática atua.
- **Logout acionado em instância desconectada**: o material da sessão é apagado localmente mesmo sem conexão ativa, e a instância volta a "registrada".
- **Encerramento planejado durante um pareamento em curso**: a tentativa é encerrada e sinalizada como expirada ao cliente, em vez de ficar pendurada sem QR.

## Requirements *(mandatory)*

### Functional Requirements

**Ciclo de vida e estados**

- **FR-001**: O sistema MUST manter, para cada instância, um estado de conexão observável entre: `registrada`, `pareando`, `conectando`, `conectada`, `desconectada`, `deslogada` e `banida`.
- **FR-002**: O sistema MUST manter, separadamente do estado observado, uma **intenção de operação** por instância com os valores `ativa` e `parada`, persistida e sobrevivente a reinícios.
- **FR-003**: O sistema MUST definir a intenção como `ativa` ao receber um comando de conexão e como `parada` ao receber um comando de desconexão.
- **FR-004**: Usuários MUST ser capazes de acionar a conexão de uma instância, o que inicia pareamento quando não há sessão salva, ou restabelece a conexão quando há.
- **FR-005**: Usuários MUST ser capazes de desconectar uma instância, colocando-a offline sem descartar o material da sessão, de modo que a reconexão posterior não exija novo pareamento.
- **FR-006**: Usuários MUST ser capazes de deslogar uma instância, o que encerra a sessão junto ao WhatsApp, remove o dispositivo vinculado, apaga o material da sessão e devolve a instância ao estado `registrada`.
- **FR-007**: O sistema MUST desconectar e deslogar de forma limpa qualquer instância conectada antes de remover seu registro na exclusão, sem deixar sessão ativa órfã.
- **FR-008**: Comandos de conexão e desconexão MUST ser idempotentes: repetir o comando no estado já alcançado é aceito sem erro e sem efeito colateral.

**Pareamento**

- **FR-009**: O sistema MUST oferecer pareamento por QR code, emitindo códigos sucessivos no canal de eventos em tempo real da instância enquanto a tentativa estiver ativa.
- **FR-010**: O sistema MUST renovar o QR antes do vencimento do código corrente, de modo que sempre exista um código válido durante a tentativa.
- **FR-011**: O sistema MUST encerrar automaticamente a tentativa de pareamento após uma janela máxima configurável (padrão de 2 minutos), emitindo evento de expiração e devolvendo a instância ao estado anterior.
- **FR-012**: O sistema MUST oferecer pareamento por código de telefone, recebendo o número em formato internacional e devolvendo o código a ser digitado no aparelho.
- **FR-013**: O sistema MUST recusar solicitações de pareamento por código com número em formato inválido, sem alterar o estado da instância.
- **FR-014**: O sistema MUST manter no máximo uma tentativa de pareamento ativa por instância; uma nova solicitação substitui a anterior e chamadas concorrentes convergem para a mesma tentativa.
- **FR-015**: Ao concluir o pareamento, o sistema MUST persistir a identidade do dispositivo pareado — número de telefone, identificador WhatsApp completo **incluindo o identificador do dispositivo companheiro** e nome de exibição — e a data/hora do pareamento.
- **FR-016**: O sistema MUST permitir que uma instância seja re-pareada com um número diferente do anterior, registrando a troca na trilha de eventos.
- **FR-017**: O sistema MUST permitir que o mesmo número de telefone esteja pareado em várias instâncias simultaneamente, tratando cada pareamento como um dispositivo companheiro independente, sem sinalizar erro ou conflito entre elas.
- **FR-018**: O sistema MUST tornar visível, na consulta de estado, quais outras instâncias do mesmo tenant compartilham o número de telefone da instância consultada — informação de contexto operacional, nunca um bloqueio.
- **FR-019**: O sistema MUST tratar falhas de pareamento reportadas pelo WhatsApp como resultado esperado, e não como erro interno: a tentativa é encerrada, o motivo informado é registrado na trilha e comunicado ao usuário de forma inteligível.
- **FR-020**: O sistema MUST tratar o desvinculamento de um dispositivo pelo aparelho como invalidação de sessão daquela instância especificamente, sem afetar as demais instâncias pareadas ao mesmo número.

**Continuidade e posse exclusiva**

- **FR-021**: O sistema MUST garantir que cada instância esteja conectada ao WhatsApp em exatamente um processo por vez, em qualquer circunstância, incluindo reinícios, falhas e partições.
- **FR-022**: O sistema MUST reconectar automaticamente, sem intervenção humana e sem novo pareamento, toda instância com intenção `ativa` cuja conexão tenha se perdido por falha de rede, falha de processo ou reinício do sistema.
- **FR-023**: O sistema MUST aplicar intervalos progressivos entre tentativas de reconexão e MUST continuar tentando indefinidamente enquanto a intenção permanecer `ativa` e a causa não for uma invalidação de sessão.
- **FR-024**: O sistema MUST assumir automaticamente sessões órfãs — cujo processo dono deixou de dar sinal de vida — dentro de um intervalo máximo definido, sem coordenação manual.
- **FR-025**: O sistema MUST rejeitar comandos e eventos originados de um processo que não é mais o dono corrente da sessão.
- **FR-026**: Em encerramento planejado, o sistema MUST liberar as sessões de forma limpa antes de terminar, permitindo adoção imediata por outro processo.
- **FR-027**: O sistema MUST manter offline, após qualquer reinício, toda instância cuja intenção seja `parada`.

**Invalidação de sessão**

- **FR-028**: O sistema MUST distinguir perda de conexão (falha de rede, keepalive perdido, falha de processo) de invalidação de sessão (logout pelo aparelho, banimento, sessão substituída).
- **FR-029**: Diante de invalidação de sessão, o sistema MUST interromper as tentativas de reconexão, transitar a instância para o estado terminal correspondente (`deslogada` ou `banida`), registrar o motivo e emitir evento no canal em tempo real.
- **FR-030**: O sistema MUST registrar, quando informado pelo WhatsApp, o prazo de um banimento temporário junto ao estado da instância.
- **FR-031**: O sistema MUST permitir que uma instância em estado terminal por invalidação inicie uma nova tentativa de pareamento por comando explícito do usuário.

**Estado observável, eventos e histórico**

- **FR-032**: Usuários MUST ser capazes de consultar, por instância, o estado atual da conexão, a intenção de operação, o horário da conexão corrente, a identidade do dispositivo pareado e o motivo e horário da última desconexão.
- **FR-033**: O sistema MUST oferecer um canal de eventos em tempo real por instância que entregue QR codes, códigos de pareamento, transições de estado e motivos de desconexão.
- **FR-034**: O canal de eventos MUST aceitar múltiplos ouvintes simultâneos para a mesma instância, todos recebendo os mesmos eventos.
- **FR-035**: Ao abrir o canal, o sistema MUST entregar imediatamente uma mensagem com o estado atual da instância, incluindo o código de pareamento corrente quando houver tentativa em andamento.
- **FR-036**: O sistema MUST persistir uma trilha de eventos de conexão por instância — pareamento, conexão estabelecida, desconexão, invalidação, expiração de pareamento, recusa por limite de dispositivos, troca de número — com tipo, horário e motivo, e MUST torná-la consultável pelo tenant.
- **FR-037**: O sistema MUST reter a trilha de eventos por um período configurável (padrão de 30 dias) e descartar automaticamente registros mais antigos.
- **FR-038**: O sistema MUST refletir toda transição de estado tanto na consulta de estado quanto no canal de eventos, sem divergência entre os dois.

**Autorização e isolamento**

- **FR-039**: Todas as ações de conexão — conectar, parear (QR e código), consultar estado, consultar trilha, desconectar, deslogar e abrir o canal de eventos — MUST aceitar tanto a credencial administrativa do tenant dono da instância quanto a API key da própria instância.
- **FR-040**: O sistema MUST recusar qualquer ação de conexão em que a credencial não pertença à instância alvo, sem confirmar a existência da instância.
- **FR-041**: O sistema MUST recusar toda ação de conexão de tenants suspensos e MUST desconectar as instâncias do tenant no momento da suspensão, preservando a intenção de operação para restaurá-la na reativação.
- **FR-042**: O sistema MUST autenticar o canal de eventos antes de entregar qualquer evento e MUST encerrar a conexão quando a credencial for revogada ou o tenant suspenso.
- **FR-043**: O sistema MUST NOT expor material criptográfico da sessão em respostas de API, eventos, logs ou trilha de eventos.

**Observabilidade**

- **FR-044**: O sistema MUST registrar logs estruturados de toda transição de conexão, incluindo identificação de tenant e instância, sem expor segredos.
- **FR-045**: O sistema MUST expor métricas operacionais de sessões conectadas, tentativas de pareamento, reconexões e trocas de posse de sessão.

### Key Entities

- **Instância (estendida)**: registro de um **dispositivo companheiro** de um número WhatsApp, pertencente a um tenant, agora com estado de conexão, intenção de operação, identidade do dispositivo pareado (telefone, identificador WhatsApp completo com o identificador de dispositivo, nome de exibição), horário do último pareamento, e horário e motivo da última desconexão.
- **Sessão**: material criptográfico que representa o dispositivo companheiro vinculado ao WhatsApp; é próprio de cada instância, permite reconectar sem novo pareamento, é apagado no logout e nunca é exposto externamente.
- **Posse de sessão**: vínculo entre uma instância e o processo que detém sua conexão no momento, com sinal de vida periódico, marca de geração para invalidar donos antigos e a intenção de operação da instância.
- **Tentativa de pareamento**: processo temporário de vinculação de um número a uma instância; tem modalidade (QR ou código de telefone), código corrente, validade e janela máxima.
- **Evento de conexão**: registro histórico de uma transição relevante da instância — tipo, horário e motivo — com retenção limitada.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Após acionar a conexão de uma instância nunca pareada, o primeiro QR chega ao cliente do canal de eventos em até 5 segundos em 95% dos casos.
- **SC-002**: Durante uma tentativa de pareamento ativa, existe sempre um código válido disponível ao cliente — nenhuma janela sem código utilizável.
- **SC-003**: Do escaneamento do QR até a instância aparecer como conectada, o tempo é inferior a 15 segundos em 95% dos pareamentos.
- **SC-004**: Nenhuma instância fica conectada em dois processos simultaneamente — zero ocorrências em testes de falha, reinício e disputa de posse.
- **SC-005**: Após a morte abrupta do processo dono, uma instância com intenção `ativa` volta ao estado conectada em até 60 segundos, sem intervenção humana, em 99% dos casos.
- **SC-006**: Em um encerramento planejado (deploy), 100% das instâncias com intenção `ativa` voltam a conectar sem exigir novo pareamento, com tempo offline mediano inferior a 10 segundos.
- **SC-007**: Após reinício completo do sistema, 100% das instâncias com intenção `parada` permanecem offline e 100% das com intenção `ativa` reconectam.
- **SC-008**: Qualquer transição de estado aparece no canal de eventos em até 2 segundos e na consulta de estado imediatamente após.
- **SC-009**: Nenhuma instância em estado terminal por invalidação (deslogada, banida, sessão substituída) gera novas tentativas automáticas de reconexão — zero tentativas registradas.
- **SC-010**: A consulta de estado de uma instância responde em menos de 300 ms no percentil 95.
- **SC-011**: 100% das transições de conexão ocorridas nos últimos 30 dias são recuperáveis na trilha de eventos, com motivo preenchido em todas as desconexões não solicitadas.
- **SC-012**: Nenhuma ação de conexão executada com credencial de outra instância ou de outro tenant é aceita — zero acessos cruzados em teste de isolamento.
- **SC-013**: Um administrador consegue levar uma instância recém-criada de "registrada" a "conectada" em menos de 2 minutos, contando o tempo de escanear o QR.
- **SC-014**: Duas ou mais instâncias pareadas ao mesmo número permanecem simultaneamente conectadas por pelo menos 30 minutos sem que nenhuma derrube a outra — zero desconexões atribuídas a compartilhamento de número.

## Assumptions

- A fundação de contas (feature 001) está entregue: tenants, admins, instâncias como registro, API keys por instância e suspensão de tenant já existem e são reaproveitados aqui.
- Envio e recebimento de mensagens, mídia, grupos e demais capacidades operacionais estão **fora** desta fatia; ela entrega apenas conexão e seu ciclo de vida.
- Webhooks de eventos estão fora desta fatia — o canal em tempo real por instância é o único meio de notificação; a trilha de eventos cobre a consulta posterior.
- Não há cota de instâncias conectadas por tenant nem limite de capacidade nesta fatia. A ZapperMeow é self-hosted: quem instala é dono da própria infraestrutura e responde por dimensioná-la — aumentar o servidor ou acrescentar nós ao cluster é a resposta natural para mais sessões, e um teto imposto pela aplicação só atrapalharia esse operador.
- O WhatsApp é multi-dispositivo: um mesmo número mantém vários dispositivos companheiros vinculados simultaneamente, cada um com identificador próprio dentro do identificador WhatsApp. Portanto **uma instância corresponde a um dispositivo companheiro**, não a um número — várias instâncias podem apontar para o mesmo número sem conflito.
- Quantos dispositivos uma conta WhatsApp pode vincular é regra da Meta, aplicada dentro do aplicativo do cliente: ao atingir o limite, o próprio app impede o escaneamento. A plataforma não conhece, não replica e não consegue observar esse limite — para ela, a tentativa apenas expira.
- **Não existe teto de instâncias conectadas imposto pela plataforma**, nem por tenant nem global. A capacidade é responsabilidade de quem hospeda.
- A janela máxima de pareamento tem padrão de 2 minutos e é configurável no deploy.
- A retenção da trilha de eventos de conexão tem padrão de 30 dias e é configurável no deploy.
- O tempo máximo até a adoção de uma sessão órfã segue a política de posse já definida na arquitetura do projeto (sinal de vida periódico com expiração da ordem de dezenas de segundos).
- A suspensão de um tenant desconecta suas instâncias e preserva a intenção de operação para restauração na reativação — comportamento derivado das regras de suspensão da feature 001.
- Configuração de proxy por instância, seleção manual do processo que hospeda a sessão e migração dirigida de sessões entre processos estão fora do escopo.
- O cliente que exibe o QR (painel web, CLI ou integração do tenant) é responsabilidade de quem consome a API; esta fatia entrega o canal e os códigos, não uma interface visual.
