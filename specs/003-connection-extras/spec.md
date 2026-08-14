# Feature Specification: Complementos de Conexão da Instância

**Feature Branch**: `003-connection-extras`

**Created**: 2026-08-14

**Status**: Draft

**Input**: User description: "vamos especificar os itens que faltam da parte 1 de features.md — Endpoints: pareamento por passkey, modo passivo (receber sem marcar online), proxy e HTTP clients customizados, códigos de verificação de identidade (LID); Eventos: StreamError, ManualLoginReconnect"

## Contexto

A feature 002 entregou o essencial da conexão: pareamento por QR e por código de telefone, ciclo de vida (conectar, desconectar, deslogar), reconexão supervisionada com posse exclusiva de sessão, e estado observável (consulta, canal de eventos em tempo real e trilha de conexão).

Esta feature completa a seção "Sessão, autenticação e conexão" do levantamento de funcionalidades com cinco capacidades que estendem esse alicerce:

1. **Proxy de saída por instância** — cada cliente do tenant pode sair para o WhatsApp por um endereço de rede dedicado (reputação de IP, isolamento entre clientes).
2. **Tratamento dos eventos de stream restantes** — erro de stream desconhecido e pedido de reconexão manual pós-login passam a ter classificação, registro e reação, fechando a cobertura de eventos de ciclo de vida iniciada na 002.
3. **Modo passivo** — a instância recebe tudo sem se anunciar como dispositivo ativo.
4. **Etapa de passkey no pareamento** — quando a conta do número exige passkey, o pareamento não falha: o desafio é encaminhado ao tenant e respondido dentro do próprio fluxo.
5. **Códigos de verificação de identidade** — consulta do "número de segurança" da conversa com um contato, para conferência de integridade da criptografia.

Um aprendizado da investigação técnica orienta o desenho: **a etapa de passkey não é um fluxo alternativo ao QR — é uma exigência que o WhatsApp pode inserir no meio do pareamento normal**. O desafio de autenticação precisa ser respondido pelo autenticador do dono do número (fora da plataforma), e em alguns casos um código de conferência precisa ser exibido e confirmado antes de concluir.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Conectar a instância através de um proxy dedicado (Priority: P1)

O admin do tenant define um endereço de proxy para uma instância (na criação ou a qualquer momento depois). A partir daí, todo o tráfego daquela instância com o WhatsApp — canal principal e transferência de mídia — sai exclusivamente por esse proxy, em toda conexão, reconexão e failover. A configuração fica visível na consulta da instância (com a senha sempre oculta) e pode ser alterada ou removida; se a instância estiver conectada no momento da mudança, o sistema religa a conexão automaticamente com a nova configuração.

**Why this priority**: É a lacuna operacional mais citada do levantamento ("falta configuração de proxy por instância"). Em uma plataforma multi-tenant de WhatsApp, sair por IP dedicado por cliente é proteção direta de reputação: um número bloqueado não contamina os demais. Sozinha, esta story já entrega valor a qualquer tenant que opere múltiplos números.

**Independent Test**: Com uma instância registrada (001) e um proxy de teste acessível, definir o proxy, conectar, e confirmar que toda a comunicação observável passa pelo proxy; alterar o proxy com a instância conectada e confirmar a reconexão automática; derrubar o proxy e confirmar que a instância não conecta direto em hipótese alguma.

**Acceptance Scenarios**:

1. **Given** uma instância registrada sem proxy, **When** o admin define um endereço de proxy válido, **Then** a configuração é gravada, aparece na consulta da instância com a credencial mascarada e a próxima conexão sai pelo proxy.
2. **Given** uma instância conectada com proxy A, **When** o admin altera para o proxy B, **Then** a alteração é gravada, a conexão é religada automaticamente pelo proxy B e as transições aparecem no canal de eventos em tempo real e na trilha de conexão.
3. **Given** uma instância conectada por proxy, **When** o admin remove a configuração de proxy, **Then** a alteração é gravada e a conexão é religada automaticamente em modo direto.
4. **Given** uma instância com proxy configurado, **When** o proxy fica inacessível ou recusa conexão, **Then** o sistema segue o ciclo padrão de novas tentativas com espera crescente sempre através do proxy, nunca conecta direto, e a falha fica visível no estado e na trilha da instância.
5. **Given** um endereço de proxy com formato inválido ou esquema não suportado, **When** o admin tenta gravá-lo, **Then** a operação é rejeitada com mensagem clara e a configuração vigente permanece intacta.
6. **Given** uma instância desconectada com proxy configurado, **When** ocorre reconexão automática ou failover para outro worker, **Then** a nova conexão também sai pelo proxy configurado.
7. **Given** uma instância de outro tenant, **When** o admin tenta ler ou alterar sua configuração de proxy, **Then** a operação é negada sem confirmar a existência da instância.

---

### User Story 2 - Reagir aos eventos de stream ainda não classificados (Priority: P2)

A plataforma passa a reconhecer os dois eventos de ciclo de vida que a 002 deixou de fora. Quando o WhatsApp encerra o stream com um erro desconhecido, a instância registra o código recebido na trilha, emite o evento no canal em tempo real e segue o ciclo padrão de reconexão — sem tratar a queda como logout. Quando o WhatsApp, ao final de um pareamento, pede que o cliente refaça a conexão por conta própria, o supervisor religa imediatamente, sem intervenção do tenant, e o pareamento conclui normalmente.

**Why this priority**: É robustez que protege todas as instâncias já em produção. Sem esse tratamento, existem quedas reais diante das quais a plataforma não reage corretamente — e um pareamento pode ficar pendurado no último passo.

**Independent Test**: Com a sessão roteirizada que simula o comportamento do WhatsApp (exigida pela constituição), injetar um erro de stream com código desconhecido e confirmar registro, emissão e reconexão; injetar o pedido de reconexão manual pós-login e confirmar que a plataforma religa sozinha e conclui o pareamento.

**Acceptance Scenarios**:

1. **Given** uma instância conectada, **When** o WhatsApp encerra o stream com um código de erro desconhecido, **Then** a trilha de conexão registra a ocorrência com o código recebido, o evento é emitido no canal em tempo real e o supervisor inicia a reconexão padrão com espera crescente.
2. **Given** um erro de stream desconhecido, **When** a reconexão subsequente é bem-sucedida, **Then** a instância volta a "conectada" sem exigir novo pareamento e sem perder o material de sessão.
3. **Given** uma tentativa de pareamento cuja troca final exige reconexão manual, **When** o pedido de reconexão manual é recebido, **Then** o supervisor religa a conexão imediatamente, a ocorrência entra na trilha e o pareamento conclui com a instância "conectada", sem qualquer ação do tenant.
4. **Given** qualquer um dos dois eventos, **When** ele ocorre, **Then** a causa registrada na trilha é específica daquele evento (não uma causa genérica), permitindo diagnóstico posterior.

---

### User Story 3 - Operar a instância em modo passivo (Priority: P3)

O admin do tenant liga o modo passivo de uma instância: ela continua conectada e recebendo tudo (mensagens, eventos, sincronizações), mas não se anuncia como dispositivo ativo. A escolha é persistida, aplicada imediatamente se a instância estiver conectada, reaplicada automaticamente em toda reconexão e failover, e visível na consulta da instância. Desligar o modo devolve o comportamento padrão.

**Why this priority**: Habilita casos de uso de escuta — monitoramento, auditoria, migração gradual — em que o tenant precisa receber sem interferir na percepção de presença do número. Depende apenas do alicerce da 002, não das demais stories.

**Independent Test**: Com uma instância pareada, ligar o modo passivo e confirmar que a escolha persiste e aparece na consulta; reconectar e confirmar que a instância volta já em modo passivo; desligar e confirmar o retorno ao comportamento padrão.

**Acceptance Scenarios**:

1. **Given** uma instância conectada em modo padrão, **When** o admin liga o modo passivo, **Then** a escolha é gravada, aplicada imediatamente na sessão ativa e refletida na consulta da instância.
2. **Given** uma instância com modo passivo ligado, **When** ocorre reconexão automática ou failover, **Then** a sessão restabelecida já opera em modo passivo — o processo de conexão nunca deixa a instância em modo ativo contra a escolha gravada.
3. **Given** uma instância desconectada, **When** o admin altera o modo passivo, **Then** a escolha é gravada e vale a partir da próxima conexão.
4. **Given** uma instância em modo passivo, **When** o admin desliga o modo, **Then** a instância volta a se anunciar como dispositivo ativo, imediatamente se conectada.
5. **Given** uma instância recém-criada, **When** nada é configurado, **Then** o modo passivo está desligado (comportamento idêntico ao da 002).

---

### User Story 4 - Concluir pareamento de conta que exige passkey (Priority: P4)

Durante um pareamento normal (iniciado por QR ou código de telefone), o WhatsApp exige a etapa de passkey. Em vez de falhar, a plataforma emite no canal de eventos em tempo real da instância — o mesmo que entrega o QR — um evento com o desafio de autenticação. O front do tenant coleta a assinatura do autenticador do dono do número e a devolve por um endpoint. Se o WhatsApp exigir conferência visual, a plataforma emite um segundo evento com o código de conferência; o tenant o exibe ao dono do número, que compara com o telefone, e a confirmação sobe por outro endpoint. Quando a continuidade da sessão dispensa a conferência, a plataforma confirma sozinha. O pareamento então conclui como qualquer outro.

**Why this priority**: Destrava o pareamento de contas que usam passkey — que hoje falharia sem explicação útil. Prioridade menor que as anteriores por atingir um subconjunto de números, mas fecha um buraco real do fluxo principal do produto.

**Independent Test**: Com a sessão roteirizada simulando um pareamento que exige passkey, confirmar a emissão do evento de desafio, submeter uma resposta pelo endpoint, receber o evento com código de conferência, confirmá-lo pelo endpoint e ver o pareamento concluir. Testar também o caminho em que a conferência é dispensada e a plataforma confirma automaticamente.

**Acceptance Scenarios**:

1. **Given** uma tentativa de pareamento em curso, **When** o WhatsApp exige a etapa de passkey, **Then** um evento com o desafio de autenticação é emitido no canal em tempo real da instância e a tentativa permanece ativa aguardando resposta.
2. **Given** um desafio de passkey emitido, **When** o tenant submete a resposta do autenticador pelo endpoint, **Then** o sistema a encaminha ao WhatsApp e a tentativa avança.
3. **Given** uma resposta de passkey aceita que exige conferência visual, **When** o WhatsApp devolve o código de conferência, **Then** um evento com o código é emitido no canal para exibição ao dono do número.
4. **Given** um código de conferência exibido, **When** o tenant confirma pelo endpoint, **Then** o pareamento conclui e a instância transita para "conectada" com a identidade do número preenchida.
5. **Given** uma resposta de passkey aceita cuja continuidade de sessão dispensa a conferência, **When** o WhatsApp sinaliza a dispensa, **Then** o sistema confirma automaticamente sem envolver o tenant e o pareamento conclui.
6. **Given** uma etapa de passkey em curso, **When** qualquer passo falha (desafio ilegível, resposta rejeitada, erro na continuação), **Then** um evento de erro de pareamento é emitido no canal, a ocorrência entra na trilha e a tentativa segue as regras de expiração da 002.
7. **Given** nenhuma tentativa aguardando resposta ou confirmação de passkey, **When** o tenant chama o endpoint correspondente, **Then** a operação é rejeitada com erro claro indicando que não há etapa pendente.

---

### User Story 5 - Consultar códigos de verificação de identidade de um contato (Priority: P5)

Um sistema do tenant, autenticado com a API key da instância, consulta os códigos de verificação de identidade da conversa entre o número da instância e um contato — informado por LID ou por número de telefone. A resposta traz o código numérico de 60 dígitos e os dois materiais de QR (um para exibição, outro para verificação por leitura), permitindo ao tenant oferecer a conferência de "número de segurança" aos seus usuários finais.

**Why this priority**: Recurso de segurança/conferência com demanda mais pontual. Depende apenas de uma instância conectada; nenhuma outra story depende dela.

**Independent Test**: Com uma instância conectada e um contato conhecido, consultar por LID e por telefone e conferir que o mesmo resultado é retornado; consultar com a instância desconectada e com contato desconhecido e conferir os erros.

**Acceptance Scenarios**:

1. **Given** uma instância conectada e um contato identificado por LID, **When** o sistema do tenant consulta os códigos de verificação, **Then** a resposta traz o código numérico de 60 dígitos e os dois materiais de QR.
2. **Given** um contato informado por número de telefone cujo mapeamento para LID é conhecido pela instância, **When** a consulta é feita, **Then** o sistema resolve o telefone para o LID e responde normalmente, indicando o LID resolvido.
3. **Given** um telefone sem mapeamento para LID conhecido, **When** a consulta é feita, **Then** a operação falha com erro claro de identidade não resolvível, sem inventar resultado.
4. **Given** uma instância desconectada, **When** a consulta é feita, **Then** a operação falha com erro claro exigindo instância conectada.
5. **Given** um contato inexistente ou sem dispositivos ativos, **When** a consulta é feita, **Then** a operação falha com erro claro.
6. **Given** a API key de uma instância, **When** ela é usada para consultar códigos por outra instância, **Then** a operação é negada sem confirmar a existência da outra instância.

---

### Edge Cases

- **Proxy inacessível na conexão ou queda do proxy em uso**: a instância nunca conecta direto; o supervisor insiste pelo proxy com espera crescente e o estado/trilha mostram a falha para o tenant diagnosticar.
- **Alteração de proxy durante uma tentativa de pareamento em curso**: a tentativa é encerrada (com o evento de encerramento no canal, como na expiração da 002) e o tenant inicia novo pareamento já pelo proxy novo.
- **Endereço de proxy com credenciais embutidas**: aceito e gravado; nenhuma consulta, evento ou registro de trilha jamais expõe a senha — ela retorna sempre mascarada.
- **Proxy de ambiente do servidor da plataforma**: ignorado. Instância sem proxy configurado conecta sempre direto, independentemente do ambiente onde o worker roda.
- **Erro de stream desconhecido repetido em sequência**: cada ocorrência é registrada com seu código; a reconexão segue a espera crescente padrão da 002 — o tratamento não introduz ciclo novo de retry.
- **Erro de stream desconhecido não é logout**: o material de sessão é preservado e nenhuma limpeza de credencial é feita por causa dele.
- **Ativação automática de presença na conexão**: o processo padrão de conexão anuncia o dispositivo como ativo; com modo passivo ligado, o sistema garante que o estado final da sessão recém-estabelecida seja passivo — a ordem interna dos passos nunca pode deixar a instância ativa contra a configuração.
- **Resposta de passkey submetida duas vezes, ou confirmação antes do código existir**: a chamada fora de ordem é rejeitada com erro claro de etapa inexistente ou já consumida; a tentativa em curso não é corrompida.
- **Dono do número não reconhece o código de conferência**: o tenant simplesmente não confirma; a tentativa expira pelas regras da 002 e a instância volta ao estado anterior.
- **Janela de pareamento esgota durante a etapa de passkey**: mesma expiração da 002 — evento de expiração, estado anterior restaurado, nova tentativa possível.
- **Consulta de códigos de verificação para o próprio número da instância**: rejeitada com erro claro — o recurso existe para conferir a identidade de contatos, não a própria.
- **Todos os endpoints novos sob isolamento multi-tenant**: instância de outro tenant (ou outra instância, no caso da API key) é negada sem confirmar existência, no mesmo padrão da 001/002.

## Requirements *(mandatory)*

### Functional Requirements

**Proxy de saída por instância**

- **FR-001**: System MUST permitir ao admin do tenant definir, alterar e remover um endereço de proxy de saída por instância, aceitando os esquemas HTTP, HTTPS e SOCKS5, com credenciais opcionais embutidas.
- **FR-002**: System MUST rejeitar, no momento da gravação, endereços de proxy com formato inválido ou esquema não suportado, com mensagem clara e sem alterar a configuração vigente.
- **FR-003**: System MUST persistir a configuração de proxy como atributo da instância e aplicá-la a todo o tráfego da instância com o WhatsApp — canal principal (antes e depois do login) e transferência de mídia — em toda conexão, reconexão automática e failover.
- **FR-004**: System MUST, ao gravar uma alteração de proxy de instância conectada, religar a conexão automaticamente com a nova configuração, emitindo as transições no canal de eventos em tempo real e registrando-as na trilha de conexão.
- **FR-005**: System MUST, quando a conexão falhar por causa do proxy, insistir exclusivamente através do proxy com o ciclo padrão de novas tentativas e espera crescente; conexão direta sem proxy configurado pelo tenant é PROIBIDA como fallback.
- **FR-006**: System MUST conectar diretamente as instâncias sem proxy configurado, ignorando qualquer configuração de proxy do ambiente de execução da plataforma.
- **FR-007**: System MUST proteger a credencial do proxy: a senha nunca aparece em consultas, eventos, trilha ou mensagens de erro — sempre mascarada.
- **FR-008**: System MUST exibir a configuração de proxy vigente (mascarada) na consulta da instância.

**Eventos de stream restantes**

- **FR-009**: System MUST, ao receber um encerramento de stream com código desconhecido, registrar a ocorrência na trilha de conexão com causa específica e o código recebido, emitir o evento no canal em tempo real e acionar a reconexão padrão, preservando o material de sessão.
- **FR-010**: System MUST, ao receber o pedido de reconexão manual pós-login ao final de um pareamento, religar a conexão imediatamente sem intervenção do tenant, registrar a ocorrência na trilha com causa específica e concluir o pareamento normalmente.

**Modo passivo**

- **FR-011**: System MUST manter, por instância, uma configuração persistida de modo passivo, desligada por padrão e visível na consulta da instância.
- **FR-012**: System MUST permitir ligar e desligar o modo passivo a qualquer momento; se a instância estiver conectada, a mudança é aplicada imediatamente na sessão ativa.
- **FR-013**: System MUST garantir que toda sessão estabelecida (conexão, reconexão ou failover) termine o processo de conexão no modo configurado — a instância nunca permanece ativa quando o modo passivo está ligado, independentemente da ordem interna dos passos de conexão.
- **FR-014**: System MUST, em modo passivo, continuar recebendo e processando normalmente tudo o que a instância recebe hoje (eventos, mensagens, sincronizações).

**Etapa de passkey no pareamento**

- **FR-015**: System MUST, quando o WhatsApp exigir a etapa de passkey durante uma tentativa de pareamento, emitir no canal de eventos em tempo real da instância um evento contendo o desafio de autenticação necessário ao autenticador do dono do número, mantendo a tentativa ativa.
- **FR-016**: System MUST oferecer uma operação para o tenant submeter a resposta do autenticador da tentativa em curso, rejeitando com erro claro chamadas sem etapa de passkey pendente.
- **FR-017**: System MUST, quando a conferência visual for exigida, emitir no canal um evento com o código de conferência; quando a continuidade da sessão dispensar a conferência, confirmar automaticamente sem envolver o tenant.
- **FR-018**: System MUST oferecer uma operação para o tenant confirmar o código de conferência, rejeitando com erro claro chamadas fora de ordem (antes do código existir ou após a etapa consumida).
- **FR-019**: System MUST tratar falhas de qualquer passo da etapa de passkey como falha de pareamento: evento de erro no canal, registro na trilha e aplicação das regras de expiração e retomada da 002.
- **FR-020**: System MUST, após a etapa de passkey bem-sucedida, concluir o pareamento pelo caminho normal, com a instância "conectada" e a identidade do número preenchida na consulta.

**Códigos de verificação de identidade**

- **FR-021**: System MUST oferecer uma consulta operacional, autenticada pela API key da instância, dos códigos de verificação de identidade da conversa com um contato, aceitando o contato por LID ou por número de telefone.
- **FR-022**: System MUST resolver telefone para LID quando o mapeamento for conhecido pela instância, indicando na resposta o LID efetivamente usado; quando o mapeamento não for conhecido, falhar com erro claro de identidade não resolvível.
- **FR-023**: System MUST retornar o código numérico de 60 dígitos e os dois materiais de QR (exibição e verificação) do par instância–contato.
- **FR-024**: System MUST exigir instância conectada para a consulta e falhar com erro claro quando desconectada, quando o contato for desconhecido ou sem dispositivos, e quando o contato informado for o próprio número da instância.

**Transversais**

- **FR-025**: System MUST aplicar o isolamento multi-tenant da 001/002 a todas as operações novas: configuração de proxy, modo passivo e operações de passkey exigem o vínculo do tenant com a instância; a consulta de códigos exige a API key da própria instância; acesso indevido é negado sem confirmar a existência do recurso.
- **FR-026**: System MUST registrar na trilha de conexão existente todas as transições e ocorrências introduzidas por esta feature (mudanças de proxy com reconexão, eventos de stream, etapa de passkey), com causas específicas que permitam diagnóstico.

### Key Entities

- **Configuração de conexão da instância**: novos atributos duráveis da instância — endereço de proxy (esquema, host, porta, usuário; senha nunca exposta em leituras — sempre mascarada) e indicador de modo passivo. Definem como toda sessão daquela instância se estabelece e se comporta.
- **Etapa de passkey da tentativa de pareamento**: estado transitório dentro de uma tentativa de pareamento da 002 — desafio de autenticação recebido, resposta do autenticador submetida, código de conferência pendente ou dispensado, confirmação enviada. Vive e morre com a tentativa; nunca sobrevive a ela.
- **Registro de trilha de conexão (estendido)**: os registros da 002 ganham novas causas específicas — erro de stream desconhecido (com o código recebido), reconexão manual pós-login, reconexão por mudança de proxy, falha de conexão via proxy, passos da etapa de passkey.
- **Códigos de verificação de identidade**: resultado de consulta (não persistido pela plataforma) — contato (LID e, quando houver, telefone e username), código numérico de 60 dígitos e dois materiais de QR (exibição e verificação).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Em instância com proxy configurado, 100% das conexões com o WhatsApp (incluindo mídia) saem pelo proxy; nenhuma conexão direta é observada em teste, inclusive durante falhas do proxy.
- **SC-002**: Alteração de proxy em instância conectada resulta em reconexão automática concluída em até 30 segundos, sem qualquer ação adicional do tenant.
- **SC-003**: 100% das ocorrências de erro de stream desconhecido e de pedido de reconexão manual ficam registradas na trilha com causa específica; após o pedido de reconexão manual, a instância volta a "conectada" em até 10 segundos.
- **SC-004**: Com modo passivo ligado, a instância completa um período de observação de 24 horas recebendo eventos normalmente e, em regime permanente, sem nenhuma janela em que a sessão opere como dispositivo ativo — a única exceção é a convergência transitória (<1s) no estabelecimento de cada conexão, limitação do serviço registrada no plano.
- **SC-005**: Um número cuja conta exige passkey conclui o pareamento de ponta a ponta usando apenas o canal de eventos e as operações desta feature, dentro da mesma janela de pareamento da 002.
- **SC-006**: A consulta de códigos de verificação responde em até 5 segundos para contato conhecido com instância conectada, e o mesmo contato consultado por LID e por telefone retorna códigos idênticos.
- **SC-007**: Nenhuma resposta, evento ou registro produzido pela plataforma contém a senha do proxy em claro (zero ocorrências em varredura de testes).

## Assumptions

- A janela de expiração e as regras de retomada de tentativa de pareamento da 002 valem integralmente para tentativas que passam pela etapa de passkey; nenhum prazo novo é criado.
- Se o dono do número não reconhecer o código de conferência, o caminho é não confirmar: a tentativa expira naturalmente e a instância volta ao estado anterior.
- Configuração de proxy, modo passivo e operações de passkey seguem a autenticação dupla das rotas de conexão da 002 (JWT de tenant **ou** API key da própria instância); apenas a consulta de códigos de verificação é exclusiva da API key da instância.
- O modo passivo não altera o que a instância recebe — apenas como ela se anuncia; qualquer efeito colateral do serviço WhatsApp sobre o volume de eventos recebidos em modo passivo é comportamento externo, não controlado pela plataforma.
- A resolução telefone→LID usa apenas o conhecimento já disponível na instância (mapeamentos aprendidos pela sessão); descoberta ativa de identidade fica para a feature de contatos (seção 5 do levantamento).
- Os materiais de QR dos códigos de verificação são entregues como dados para o tenant renderizar; a plataforma não gera imagem.
- Instâncias existentes antes desta feature permanecem com comportamento inalterado: sem proxy (conexão direta) e modo passivo desligado.
