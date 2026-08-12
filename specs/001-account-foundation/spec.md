# Feature Specification: Fundação de Contas (Tenants, Instâncias e Credenciais)

**Feature Branch**: `001-account-foundation`

**Created**: 2026-08-12

**Status**: Draft

**Input**: User description: "me guie na descoberta da primeira funcionalidade a ser desenvolvida na zappermeow, considerando a documentação na raiz do projeto, e o fato de estarmos em um greenfield"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Super-admin faz login e gerencia tenants (Priority: P1)

O operador da plataforma instala a zappermeow, define a credencial inicial do super-admin na configuração do deploy, faz login com email/senha e recebe um token de plataforma de curta duração. Com ele, cria, lista, consulta e edita tenants — cada tenant nasce com seu admin (nome, email e senha inicial).

**Why this priority**: É a raiz de toda a hierarquia de contas. Sem super-admin autenticado e sem tenants, nenhum outro ator existe no sistema. É o menor MVP que já entrega valor: a plataforma consegue fazer onboarding de um cliente.

**Independent Test**: Pode ser testada de ponta a ponta com uma instalação limpa: configurar credencial inicial, fazer login como super-admin, criar um tenant e verificar que ele aparece na listagem — sem depender de nenhuma outra story.

**Acceptance Scenarios**:

1. **Given** uma instalação nova sem nenhum super-admin, **When** o sistema inicia com a credencial inicial definida na configuração do deploy, **Then** o super-admin é criado e consegue fazer login com essa credencial.
2. **Given** um super-admin já existente, **When** o sistema reinicia com credencial inicial na configuração, **Then** nenhum novo super-admin é criado e o existente permanece inalterado.
3. **Given** um super-admin válido, **When** ele envia email e senha corretos ao login, **Then** recebe um token de plataforma de curta duração com a identificação de audience de plataforma.
4. **Given** um token de plataforma válido, **When** o super-admin cria um tenant informando nome do tenant e nome/email/senha do admin, **Then** o tenant é criado como ativo e o admin do tenant consegue fazer login.
5. **Given** um token de plataforma válido, **When** o super-admin lista tenants, **Then** recebe todos os tenants com nome, status (ativo/suspenso) e data de criação.
6. **Given** um token de **tenant** (audience errado), **When** ele tenta acessar qualquer rota de gestão de tenants, **Then** o acesso é negado.
7. **Given** nenhuma autenticação, **When** qualquer rota de gestão é chamada, **Then** o acesso é negado sem revelar detalhes internos.

---

### User Story 2 - Admin de tenant faz login e registra instâncias (Priority: P2)

O admin de um tenant faz login com email/senha, recebe um token de tenant e passa a gerenciar o cadastro das suas instâncias (cada instância representará um número de WhatsApp): criar com um nome amigável, listar, consultar, renomear e excluir. Nesta feature a instância é apenas um registro — o pareamento com o WhatsApp fica para uma feature futura.

**Why this priority**: É a segunda camada da hierarquia e pré-requisito das API keys. Entrega valor próprio: o tenant já organiza seus números e enxerga seu espaço isolado na plataforma.

**Independent Test**: Com um tenant criado (via US1 ou carga direta de dados de teste), o admin faz login, cria duas instâncias, renomeia uma, exclui a outra e confirma que só enxerga instâncias do próprio tenant.

**Acceptance Scenarios**:

1. **Given** um tenant ativo com admin, **When** o admin envia email e senha corretos ao login, **Then** recebe um token de tenant de curta duração contendo a identificação do seu tenant.
2. **Given** um token de tenant válido, **When** o admin cria uma instância com um nome, **Then** a instância nasce no estado "registrada" (não pareada) vinculada ao seu tenant.
3. **Given** instâncias de tenants diferentes, **When** o admin de um tenant lista instâncias, **Then** vê somente as do próprio tenant.
4. **Given** uma instância de outro tenant, **When** o admin tenta consultá-la, alterá-la ou excluí-la pelo identificador, **Then** a operação é negada sem confirmar a existência da instância.
5. **Given** uma instância existente, **When** o admin a exclui, **Then** ela desaparece da listagem e todas as suas API keys deixam de funcionar imediatamente.
6. **Given** um token de plataforma (audience errado), **When** ele tenta acessar rotas de instâncias de tenant, **Then** o acesso é negado.

---

### User Story 3 - Admin de tenant emite e revoga API keys da instância (Priority: P3)

O admin do tenant cria API keys para uma instância — cada key pode receber um rótulo (ex.: "produção") e o segredo é exibido **uma única vez** na criação. Várias keys podem coexistir ativas (rotação sem downtime). A key autentica as rotas operacionais da própria instância; para torná-la verificável já nesta feature, existe uma consulta operacional mínima que retorna os dados da instância autenticada pela key. O admin também lista e revoga keys, com efeito imediato.

**Why this priority**: Fecha a cadeia de credenciais (plataforma → tenant → instância) e entrega o valor central da fundação: sistemas externos passam a ter uma credencial própria por número, isolada e revogável.

**Independent Test**: Com uma instância registrada, criar uma key, usar o segredo para consultar os dados da instância, revogar a key e confirmar que a mesma consulta passa a ser negada.

**Acceptance Scenarios**:

1. **Given** uma instância do próprio tenant, **When** o admin cria uma API key com rótulo opcional, **Then** o segredo completo é retornado uma única vez e nunca mais pode ser recuperado.
2. **Given** uma key criada, **When** o admin lista as keys da instância, **Then** vê rótulo, prefixo identificador, status e data de criação de cada key — nunca o segredo completo.
3. **Given** duas keys ativas da mesma instância, **When** qualquer uma é usada na consulta operacional da instância, **Then** ambas funcionam simultaneamente.
4. **Given** uma key válida da instância A, **When** ela é usada em rota operacional da instância B (mesmo do mesmo tenant), **Then** a operação é negada.
5. **Given** uma key revogada, **When** ela é usada em qualquer rota operacional, **Then** a operação é negada imediatamente após a revogação.
6. **Given** uma instância de outro tenant, **When** o admin tenta criar ou listar keys dela, **Then** a operação é negada sem confirmar a existência da instância.

---

### User Story 4 - Super-admin suspende, reativa e exclui tenants (Priority: P4)

O super-admin suspende um tenant (bloqueio reversível): o login do admin do tenant passa a ser recusado, tokens de tenant já emitidos deixam de valer e as API keys de todas as instâncias do tenant param de funcionar imediatamente. A reativação restaura tudo. A exclusão definitiva é uma ação separada e irreversível que remove o tenant com suas instâncias e keys.

**Why this priority**: É o mecanismo de governança do operador sobre a plataforma. Depende das camadas anteriores existirem, por isso vem depois — mas sem ela o operador não tem como conter abuso ou encerrar um cliente.

**Independent Test**: Com um tenant ativo possuindo instância e key funcionais, suspender o tenant e verificar que login, token e key param de funcionar; reativar e verificar que voltam; excluir e verificar que nada do tenant permanece acessível.

**Acceptance Scenarios**:

1. **Given** um tenant ativo, **When** o super-admin o suspende, **Then** novas tentativas de login do admin do tenant são recusadas com indicação de conta suspensa.
2. **Given** um token de tenant válido emitido antes da suspensão, **When** ele é usado após a suspensão, **Then** o acesso é negado.
3. **Given** API keys funcionais de instâncias do tenant, **When** o tenant é suspenso, **Then** todas as rotas operacionais autenticadas por essas keys passam a ser negadas.
4. **Given** um tenant suspenso, **When** o super-admin o reativa, **Then** login e API keys voltam a funcionar sem necessidade de recriar credenciais.
5. **Given** um tenant qualquer, **When** o super-admin executa a exclusão definitiva, **Then** o tenant, suas instâncias e suas keys são removidos de forma irreversível e qualquer credencial associada deixa de funcionar.

---

### User Story 5 - Gestão de senhas sem dependência de email (Priority: P5)

Qualquer usuário autenticado (super-admin ou admin de tenant) troca a própria senha informando a senha atual. Se o admin de um tenant esquecer a senha, o super-admin executa um reset que gera uma senha temporária; no primeiro login com ela, o sistema exige a definição de uma nova senha antes de liberar qualquer outra operação.

**Why this priority**: Sem isso, o esquecimento de senha vira intervenção manual no banco. Vem depois das stories estruturais porque o fluxo feliz já funciona sem ela.

**Independent Test**: Trocar a própria senha e validar que a antiga deixa de funcionar; resetar a senha de um admin de tenant como super-admin e validar o fluxo de troca obrigatória no primeiro login.

**Acceptance Scenarios**:

1. **Given** um usuário autenticado, **When** ele troca a senha informando a senha atual correta e uma nova senha válida, **Then** a nova senha passa a valer e a antiga é recusada no próximo login.
2. **Given** um usuário autenticado, **When** ele tenta trocar a senha informando a senha atual incorreta, **Then** a troca é recusada.
3. **Given** um admin de tenant que esqueceu a senha, **When** o super-admin reseta a senha dele, **Then** uma senha temporária é gerada e exibida uma única vez ao super-admin.
4. **Given** uma senha temporária, **When** o admin faz login com ela, **Then** o sistema só permite a operação de definição de nova senha até que a troca seja concluída.
5. **Given** uma senha nova definida após reset, **When** o admin faz login com ela, **Then** o acesso completo é restaurado e a senha temporária deixa de valer.

---

### User Story 6 - Proteção contra força bruta no login (Priority: P6)

Após um número configurável de falhas consecutivas de login para uma mesma conta, a conta entra em bloqueio temporário que expira sozinho (sem intervenção manual). Um limite adicional por origem contém varreduras em massa. Todos os eventos (falha, bloqueio, desbloqueio) ficam registrados de forma rastreável.

**Why this priority**: Endurece um endpoint que ficará exposto à internet. É a última prioridade porque protege as stories anteriores, mas não entrega funcionalidade nova ao usuário final.

**Independent Test**: Errar a senha N vezes seguidas e verificar o bloqueio da conta (mesmo com a senha correta); aguardar a expiração e verificar que o login volta a funcionar.

**Acceptance Scenarios**:

1. **Given** uma conta válida, **When** ocorrem N falhas consecutivas de login (N configurável, padrão 5), **Then** a conta entra em bloqueio temporário e novas tentativas — mesmo com senha correta — são recusadas durante o bloqueio.
2. **Given** uma conta em bloqueio temporário, **When** o período de bloqueio (configurável, padrão 15 minutos) expira, **Then** o login volta a funcionar normalmente.
3. **Given** uma conta bloqueada, **When** o login correto é feito após a expiração, **Then** o contador de falhas é zerado.
4. **Given** um volume anômalo de tentativas de uma mesma origem contra contas variadas, **When** o limite por origem é excedido, **Then** novas tentativas dessa origem são recusadas temporariamente.
5. **Given** tentativas de login com email inexistente ou senha errada, **When** a falha é retornada, **Then** a resposta é indistinguível entre os dois casos (não revela se o email existe).

---

### Edge Cases

- O que acontece se dois tenants forem criados com o mesmo email de admin? O sistema recusa: o email identifica unicamente um usuário em toda a plataforma.
- O que acontece se o super-admin tentar criar um tenant com nome já existente? O sistema recusa nomes duplicados de tenant para evitar ambiguidade operacional.
- O que acontece com um token de tenant válido quando o tenant é suspenso ou excluído no meio da sessão? O token deixa de ser aceito imediatamente na próxima requisição.
- O que acontece se uma API key for revogada enquanto está em uso? A próxima requisição com essa key é negada; não há período de tolerância.
- O que acontece ao excluir uma instância que possui keys ativas? As keys são revogadas em cascata na mesma operação.
- O que acontece se a exclusão definitiva de um tenant for chamada sobre um tenant com instâncias e keys? Tudo é removido em cascata, de forma irreversível; a operação exige confirmação explícita na requisição.
- O que acontece se a credencial de bootstrap estiver ausente na primeira inicialização sem nenhum super-admin? O sistema inicia, mas registra alerta claro de que nenhum acesso administrativo é possível até a credencial ser configurada.
- O que acontece se o operador alterar a credencial de bootstrap depois que o super-admin já existe? Nada — o bootstrap só se aplica quando não existe super-admin.
- O que acontece com campos vazios ou malformados (email inválido, nome em branco, senha curta)? A requisição é recusada com mensagem apontando o campo e a regra violada, sem criar estado parcial.
- O que acontece quando uma listagem não tem resultados (nenhum tenant, nenhuma instância, nenhuma key)? A resposta é uma lista vazia bem-sucedida, nunca um erro.
- O que acontece se um admin consultar uma instância recém-excluída por identificador guardado? A resposta nega sem distinguir entre "nunca existiu" e "foi excluída".
- O que acontece se duas keys forem criadas simultaneamente para a mesma instância? Ambas são criadas com segredos distintos; não há limite de concorrência nesta operação.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST criar o super-admin inicial a partir de credencial definida na configuração do deploy quando nenhum super-admin existir, e ignorar essa configuração quando já existir um.
- **FR-002**: System MUST autenticar super-admin e admins de tenant por email e senha, emitindo tokens de curta duração com audiences distintos (plataforma e tenant); o token de tenant MUST carregar a identificação do tenant.
- **FR-003**: System MUST negar acesso a toda rota administrativa sem token válido do audience correto, e a toda rota operacional sem API key válida da instância alvo; apenas verificações de saúde e métricas internas ficam fora dessa regra.
- **FR-004**: System MUST permitir ao super-admin criar, listar, consultar e editar tenants, cada um criado com nome único e um admin (nome, email, senha inicial).
- **FR-005**: System MUST garantir unicidade global de email de usuário e unicidade de nome de tenant, recusando duplicatas com erro claro.
- **FR-006**: System MUST permitir ao super-admin suspender e reativar tenants; a suspensão MUST bloquear login, invalidar tokens de tenant já emitidos e desativar as API keys de todas as instâncias do tenant imediatamente; a reativação MUST restaurar tudo sem recriação de credenciais.
- **FR-007**: System MUST permitir ao super-admin excluir definitivamente um tenant mediante confirmação explícita, removendo em cascata e de forma irreversível suas instâncias, keys e usuários.
- **FR-008**: System MUST permitir ao admin de tenant criar, listar, consultar, renomear e excluir instâncias do próprio tenant; instâncias nascem no estado "registrada" (não pareada) com nome amigável.
- **FR-009**: System MUST validar em toda operação de tenant que o recurso alvo pertence ao tenant do token, negando acesso a recursos de outros tenants sem confirmar sua existência.
- **FR-010**: System MUST permitir ao admin de tenant criar múltiplas API keys ativas por instância, cada uma com rótulo opcional, exibindo o segredo completo somente na criação.
- **FR-011**: System MUST armazenar API keys de forma irrecuperável (apenas material de verificação, nunca o segredo em claro) e exibir nas listagens somente rótulo, prefixo identificador, status e datas.
- **FR-012**: System MUST permitir revogação individual de API keys com efeito imediato, e revogar em cascata as keys de instâncias excluídas.
- **FR-013**: System MUST validar que a API key usada pertence exatamente à instância da rota chamada, negando keys de outras instâncias mesmo dentro do mesmo tenant.
- **FR-014**: System MUST oferecer uma consulta operacional autenticável por API key que retorna os dados da própria instância, tornando a credencial verificável de ponta a ponta nesta feature.
- **FR-015**: Users MUST be able to trocar a própria senha informando a senha atual; a troca MUST invalidar a senha anterior imediatamente.
- **FR-016**: System MUST permitir ao super-admin resetar a senha de um admin de tenant gerando senha temporária exibida uma única vez; o login com senha temporária MUST exigir a definição de nova senha antes de liberar qualquer outra operação.
- **FR-017**: System MUST bloquear temporariamente uma conta após N falhas consecutivas de login (configurável, padrão 5 falhas / 15 minutos), com desbloqueio automático por expiração e reset do contador após login bem-sucedido.
- **FR-018**: System MUST limitar tentativas de login por origem para conter varreduras em massa, com janela e limite configuráveis.
- **FR-019**: System MUST responder a falhas de login de forma indistinguível entre "email inexistente" e "senha incorreta".
- **FR-020**: System MUST persistir todos os dados de contas (tenants, usuários, instâncias, keys, estados de bloqueio) de forma durável, sobrevivendo a reinicializações.
- **FR-021**: System MUST registrar de forma rastreável os eventos de segurança: logins (sucesso/falha), bloqueios e desbloqueios de conta, criação/revogação de keys, suspensão/reativação/exclusão de tenants e resets de senha.
- **FR-022**: System MUST validar entradas (formato de email, senha com tamanho mínimo de 8 caracteres, nomes não vazios) e recusar requisições inválidas com mensagem indicando campo e regra violada, sem criar estado parcial.
- **FR-023**: System MUST expor toda a superfície desta feature como API documentada por contrato gerado automaticamente, consultável na própria plataforma.

### Key Entities *(include if feature involves data)*

- **Tenant**: cliente da plataforma. Atributos: nome único, status (ativo/suspenso), datas de criação/atualização. Possui um usuário admin e zero ou mais instâncias.
- **Usuário**: pessoa que autentica por email/senha. Atributos: nome, email (único global), credencial de senha, papel (super-admin de plataforma ou admin de tenant), indicador de senha temporária pendente de troca, estado de bloqueio por falhas de login. Admin de tenant pertence a exatamente um tenant.
- **Instância**: registro de um futuro número de WhatsApp. Atributos: nome amigável, estado ("registrada" nesta feature; estados de pareamento/conexão virão em features futuras), datas. Pertence a exatamente um tenant; possui zero ou mais API keys.
- **API Key**: credencial operacional de uma instância. Atributos: rótulo opcional, prefixo identificador visível, material de verificação do segredo (segredo completo jamais armazenado ou reexibido), status (ativa/revogada), datas de criação e revogação. Pertence a exatamente uma instância.
- **Evento de segurança**: registro rastreável de ação sensível (login, bloqueio, criação/revogação de key, suspensão, reset de senha). Atributos: tipo, ator, alvo, resultado, origem e momento.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Partindo de uma instalação limpa, um operador consegue completar o fluxo bootstrap → login → criar tenant → login do tenant → registrar instância → emitir key → consulta operacional com a key em menos de 10 minutos, usando apenas a documentação da API.
- **SC-002**: 100% das rotas da feature (exceto saúde/métricas) recusam requisições sem credencial válida do tipo e escopo corretos.
- **SC-003**: Em 100% dos testes de isolamento cruzado, credenciais de um tenant ou instância não conseguem ler nem alterar recursos de outro tenant ou de outra instância.
- **SC-004**: Revogação de key, suspensão de tenant e troca de senha têm efeito observável na requisição seguinte — janela máxima de 5 segundos entre a ação e a recusa da credencial antiga.
- **SC-005**: Após 5 falhas consecutivas de login, a 6ª tentativa é recusada em 100% dos casos, e o desbloqueio ocorre automaticamente após o período configurado, sem intervenção manual.
- **SC-006**: O segredo completo de uma API key ou senha temporária aparece exatamente uma vez em respostas da API e zero vezes em registros de log.
- **SC-007**: 100% dos eventos de segurança listados em FR-021 são localizáveis nos registros com ator, alvo e momento.

## Assumptions

- A interface desta feature é exclusivamente a API REST documentada (produto API-first); nenhuma interface web/painel faz parte do escopo.
- Todos os dados são persistidos de forma durável (confirmado pela constituição do projeto); nenhum dado de conta é mantido apenas em memória.
- Tokens administrativos têm curta duração (padrão assumido: 1 hora, configurável) e não há refresh token nesta feature — expirou, faz-se novo login.
- Cada tenant possui exatamente um usuário admin nesta feature; múltiplos usuários por tenant e papéis adicionais ficam para o futuro.
- Limites de uso por tenant (base do rate limiting operacional) ficam fora desta feature, por decisão explícita de escopo na descoberta.
- O pareamento da instância com o WhatsApp (QR code, código de telefone, status de conexão) é a feature seguinte; aqui a instância é somente cadastro.
- API keys não expiram automaticamente nesta feature (expiração opcional e rastreio de último uso foram considerados e deixados para evolução futura).
- O super-admin não pode ser suspenso nem excluído via API nesta feature; sua gestão além do bootstrap e troca de senha fica para o futuro.
- A senha temporária de reset segue as mesmas regras de validação de senha e tem uso único.
- Não há verificação de email (confirmação de posse da caixa postal); o email é apenas identificador de login, coerente com a decisão de não depender de SMTP.
