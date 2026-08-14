<!--
Sync Impact Report
==================
Version change: 2.4.0 → 2.5.0
Rationale: MINOR — expande materialmente o Princípio V e acrescenta o Princípio VII. Nenhum
princípio é removido ou redefinido de forma incompatível, e o código atual já satisfaz as
regras novas; por isso MINOR, não MAJOR.

A emenda codifica o que a entrega 002 custou para aprender. Seis defeitos só apareceram em
teste manual contra um número real, e dois deles eram invisíveis do lado da plataforma —
manifestavam-se apenas no aparelho do cliente. A causa raiz não foi falta de testes, foi
teste que não simulava o WhatsApp: a suíte cobria o caminho feliz do adaptador sem exercitar
as ordens de chegada, as recusas e os tempos de vida que o serviço real impõe.

Modified principles:
- V. Testes Contra Infraestrutura Real: passa a exigir (a) sessão roteirizada que simule o
  comportamento observado do WhatsApp, com cobertura de todo evento que o adaptador
  classifica; (b) teste nas duas ordens quando eventos chegam por canais distintos; (c)
  cobertura nomeada do canal WebSocket, que não herda a cadeia de middlewares e por isso não
  é coberto de graça pelos testes de rota; (d) teste que prove a que contexto cada recurso de
  ciclo de vida está amarrado; (e) prova de que o teste de regressão falha sem a correção;
  (f) registro em quickstart.md do que só validação manual revela.
Added sections:
- VII. Fidelidade ao HyperMeow: a documentação publicada do fork é fonte de verdade e DEVE
  ser consultada antes de escrever código que fale com a biblioteca; sequências prescritas
  são literais; valores validados pelo servidor não são texto livre; divergência resolve-se
  em favor da biblioteca, com registro em research.md; patch local é proibido e o adaptador
  `internal/wa` permanece a única fronteira que conhece tipos do HyperMeow (verificado:
  apenas classify.go, classify_test.go e hypermeow.go importam a biblioteca).
Removed sections: nenhuma
Referências técnicas a alinhar nesta emenda: TECH_STACK.md e ARCHITECTURE.md.
Deferred items / TODOs: nenhum
-->

<!--
Sync Impact Report (2.4.0)
==========================
Version change: 2.3.0 → 2.4.0
Rationale: MINOR — corrige um erro factual no modelo de domínio e acrescenta duas regras
materiais sobre limites. (1) O WhatsApp é multi-dispositivo: um número mantém vários
dispositivos companheiros vinculados, cada um com sessão e identificador próprios. A
constituição definia "instância = um número WhatsApp", o que induzia a tratar duas
instâncias do mesmo número como conflito — na prática são dispositivos distintos que
convivem conectados. A unidade de isolamento continua sendo a instância; muda o que ela
representa. (2) Fica explícito que a plataforma não impõe teto de instâncias ou sessões:
sendo self-hosted, dimensionar capacidade é do operador. (3) Fica explícito que limites da
Meta sobre o número do cliente são regra externa que a plataforma não replica nem antecipa.
Não há remoção nem redefinição incompatível de princípio, e nenhum código depende hoje da
leitura anterior — por isso MINOR, não MAJOR.

Modified principles:
- II. Multi-Tenancy com Isolamento por Instância: definição de instância corrigida para
  "dispositivo companheiro vinculado a um número WhatsApp"; racional reescrito em torno da
  instância (não do número); dois bullets novos — ausência de teto imposto pela plataforma
  e não-replicação de limites da Meta.
- III. Posse Exclusiva de Sessão: escopo da exclusividade esclarecido — é por sessão de
  dispositivo, não por número; instâncias distintas do mesmo número podem estar conectadas
  simultaneamente, inclusive em workers diferentes.
Modified sections: nenhuma outra
Added sections: nenhuma
Removed sections: nenhuma
Referências técnicas já alinhadas a esta emenda: README.md ("O que é"),
ARCHITECTURE.md ("Modelo de contas e isolamento" e tabela de decisões) e
specs/001-account-foundation/ (spec.md US2/US3/Key Entities, data-model.md).
Deferred items / TODOs: nenhum
-->

# ZapperMeow Constitution

## Core Principles

### I. Simplicidade e Stdlib-First

Toda escolha técnica DEVE preferir a stdlib do Go e bibliotecas pequenas sem lock-in antes de
adicionar frameworks ou novas peças de infraestrutura. Regras não negociáveis:

- SQL explícito via sqlc; ORMs são PROIBIDOS. Queries vivem em arquivos SQL versionados e o
  código de acesso a dados é gerado e verificado em tempo de compilação.
- Uma única instância de Redis serve cache, rate limiting, filas asynq e pub/sub; uma única
  instância de PostgreSQL serve as tabelas da API, o store do HyperMeow e os leases de sessão.
  Nova peça de infraestrutura só entra quando um papel existente comprovadamente não atende.
- Nenhum framework proprietário entre a aplicação e `net/http`: o router é chi e a camada de
  API é huma sobre chi.
- Toda nova dependência direta no `go.mod` DEVE ser justificada no PR que a introduz.

**Racional:** estabilidade de longo prazo, leveza operacional e previsibilidade de performance
valem mais que conveniência de curto prazo.

### II. Multi-Tenancy com Isolamento por Instância

O modelo de contas tem dois níveis — a plataforma (super-admin) gerencia tenants; cada tenant
é um admin que gerencia suas próprias instâncias. Uma **instância = um dispositivo companheiro
vinculado a um número WhatsApp = uma sessão**; o WhatsApp é multi-dispositivo, portanto várias
instâncias PODEM estar pareadas ao mesmo número, cada uma como um dispositivo distinto,
convivendo conectadas ao mesmo tempo. A **instância é a unidade de isolamento**: credenciais e
canais de eventos pertencem à instância, não ao tenant nem ao número. Regras:

- Rotas operacionais exigem API key **da instância** (armazenada como hash no Postgres,
  revogável instantaneamente); a API DEVE validar que a key pertence à instância da URL. A
  key de uma instância NÃO PODE operar outra instância, nem mesmo do mesmo tenant.
- Rotas administrativas exigem JWT de curta duração em dois audiences: **plataforma**
  (super-admin: gestão de tenants e seus limites de uso) e **tenant** (`tenant_id` nas
  claims: gestão das próprias instâncias, keys e webhooks). Toda rota de tenant DEVE validar
  `instance.tenant_id == jwt.tenant_id`. Nenhuma rota pode existir sem uma dessas
  autenticações, exceto health checks e `/metrics` internos.
- Rate limiting distribuído (GCRA via redis_rate) por API key (= por instância), com limites
  configuráveis por tenant, DEVE proteger todas as rotas operacionais — um tenant
  descontrolado NÃO PODE degradar os demais, e uma instância NÃO PODE consumir a cota das
  outras.
- A plataforma NÃO IMPÕE teto de instâncias cadastradas nem de sessões conectadas, por tenant
  ou global. A ZapperMeow é self-hosted: dimensionar a capacidade — servidor maior ou mais nós
  no cluster — é responsabilidade de quem hospeda. Parâmetros como sessões por worker são
  limites operacionais de dimensionamento, ajustáveis pelo operador, nunca cotas de produto.
- Limites impostos pela Meta ao número do cliente (quantidade de dispositivos vinculados,
  banimentos, políticas de uso) são regra externa: a plataforma NÃO os replica nem os antecipa;
  apenas reflete fielmente o resultado observado ao tenant.
- Webhooks são configurados **por instância** (URL, filtro de tipos de evento e segredo
  próprios); payloads DEVEM ser assinados com HMAC-SHA256 usando o segredo do webhook da
  instância.
- Mídia no MinIO DEVE usar object key prefixada por `tenant_id/instance_id` e ser servida
  apenas por URLs pré-assinadas.
- Credenciais e chaves (banco, HMAC, JWT signing) DEVEM vir do mecanismo de secrets do
  runtime de deploy em produção — Docker Swarm Secrets no Swarm; file-based secrets
  (`secrets:`) no Docker Compose, ambos montados em `/run/secrets` — com env vars só como
  fallback de desenvolvimento. Segredos NUNCA aparecem em logs.

**Racional:** isolamento por instância é requisito de segurança — vazamento ou rotação de
credenciais de uma instância não pode afetar as demais. Como cada instância é um dispositivo
independente, sistemas distintos de um mesmo tenant podem operar até o mesmo número por
instâncias separadas, com credenciais, webhooks e revogação independentes.

### III. Posse Exclusiva de Sessão (NON-NEGOCIÁVEL)

Cada instância mantém o estado criptográfico Signal do **seu** dispositivo e DEVE estar
conectada em exatamente um processo por vez — duplo-connect da mesma sessão corrompe o estado.
A restrição é por sessão de dispositivo, não por número: instâncias distintas do mesmo número
são sessões distintas e podem estar conectadas simultaneamente, inclusive em workers
diferentes. Consequências obrigatórias:

- O plano stateless (serviço `api`) e o plano stateful (serviço `session-worker`) permanecem
  separados. A `api` NUNCA abre conexões WhatsApp; ela escala horizontalmente sem restrição.
- A posse de sessão é garantida por lease na tabela `session_leases` do Postgres, com
  aquisição atômica, heartbeat periódico e failover automático por expiração.
- Todo comando gRPC e todo evento emitido DEVE carregar o fencing token (`generation`); um
  worker que perdeu o lease tem suas operações rejeitadas na comparação de generations.
- Shutdown gracioso é obrigatório: em SIGTERM o worker entra em draining, solta os leases e
  desconecta as sessões de forma limpa antes de encerrar.
- Nenhuma feature nova pode contornar o lease para falar com uma sessão diretamente.

**Racional:** toda a arquitetura deriva desta restrição; violá-la corrompe dados de clientes
de forma irrecuperável.

### IV. Contrato de API como Fonte de Verdade

A especificação OpenAPI 3.1 DEVE ser gerada automaticamente dos handlers tipados (huma) —
documentação escrita à mão que possa dessincronizar do código é PROIBIDA. Regras:

- Todo endpoint DEVE ter request e response tipados com validação declarada no handler.
- A spec e a UI de documentação são servidas pela própria API.
- Toda resposta JSON de **sucesso** com corpo DEVE usar o envelope padrão
  `{ "status": <código HTTP numérico>, "data": ..., "timestamp": ... }` — `status` carrega
  o código HTTP da resposta, na mesma semântica do membro `status` dos problem details da
  RFC 9457; strings de estado (`"success"`/`"error"`) são PROIBIDAS. O envelope entra nos
  outputs tipados do huma e aparece fielmente na spec gerada. Únicas exceções: respostas
  `204` (sem corpo) e formatos não-JSON (`/metrics` em texto Prometheus).
- Toda resposta de **erro** DEVE seguir a RFC 9457 (`application/problem+json`, formato
  nativo do huma) estendida com membro `code` estável e `timestamp`; detalhes por campo
  vão em `errors[]`. O `code` é contrato para tratamento programático por clientes —
  alterar ou remover um `code` publicado é mudança incompatível.
- O mapeamento de erros de domínio → problem details é centralizado (pacote `httperr`);
  handlers NÃO PODEM montar respostas de sucesso ou de erro manualmente. Respostas de erro
  NUNCA ecoam senhas ou segredos.
- Contratos internos api↔worker DEVEM ser definidos em Protobuf (`proto/`) e versionados no
  repositório; mudanças incompatíveis exigem coordenação explícita de deploy.
- Mudanças incompatíveis em rotas públicas exigem versionamento de API e registro da
  quebra no changelog do release.

**Racional:** a API RESTful é o "frontend" do produto — o formato de resposta é parte do
contrato tanto quanto os campos; com o contrato derivado do código, a documentação nunca
mente e consumidores integram contra a spec com confiança.

### V. Testes Contra Infraestrutura Real

Testes DEVEM validar comportamento contra as dependências reais, não contra mocks de
infraestrutura:

- Testes unitários: `go test` + testify, table-driven, para lógica de domínio pura.
- Testes de integração: testcontainers-go DEVE subir Postgres e Redis reais; queries sqlc,
  handlers, leases e filas são testados contra essas instâncias.
- O pipeline de CI (lint → testes → build) é bloqueante: código que não passa em
  golangci-lint ou nos testes NÃO PODE ser mergeado.

**Fronteira com o WhatsApp — a única exceção.** O serviço não tem sandbox e automatizá-lo
arriscaria o banimento do número, então a sessão é substituída por uma **sessão roteirizada**
que implementa a mesma interface do adaptador. A exceção é o mock; simular mal o WhatsApp
não é permitido:

- A sessão roteirizada DEVE reproduzir o comportamento **observado** do serviço, não o
  comportamento conveniente. Todo evento que o adaptador classifica DEVE ter teste — em
  especial os que só o serviço real produz: logout feito pelo aparelho, desconexão
  permanente, expiração da janela de pareamento, recusa de pareamento e stream substituído.
- Quando dois eventos chegam por **canais distintos** e nada ordena os dois, o teste DEVE
  exercitar as duas ordens. Ordem estável em teste que é instável em produção é o pior
  resultado possível: esconde o defeito e ainda dá confiança.
- Todo recurso com ciclo de vida — socket, lease, assinatura, janela de pareamento — DEVE ter
  teste que prove **a que contexto ele está amarrado**. Confundir o fim de uma etapa com o
  fim da sessão é a família de defeitos mais cara desta base.
- O que só o aparelho revela (o dispositivo aparecer na lista, o nome exibido, o pareamento
  concluir) DEVE ter roteiro de validação manual em `quickstart.md`, com o resultado
  registrado. Ausência de teste automatizado não dispensa a verificação; muda quem a executa.

**Canal WebSocket.** O canal é montado como handler chi fora do huma e por isso **não herda a
cadeia de middlewares** — não é coberto de graça pelos testes das rotas REST e DEVE ter
cobertura própria: handshake autenticado antes do upgrade (cada credencial aceita e cada
recusa com seu status), recusa de token em query string, política de origem, snapshot como
primeiro frame, fan-out para múltiplos ouvintes, cliente lento e códigos de fechamento.

**Regressão.** Toda correção de bug DEVE incluir um teste que reproduza o defeito, e o teste
DEVE ser **provado**: rodado contra o código sem a correção e visto falhar. Teste de
regressão que passa dos dois lados não testa a correção — documenta uma crença.

**Racional:** o núcleo do sistema é SQL, filas e locking distribuído — exatamente as áreas
onde mocks escondem bugs. E a fronteira que não dá para testar de verdade é justamente a que
mais engana: os defeitos que chegaram ao cliente na entrega 002 passaram por uma suíte verde
porque ela exercitava o caminho feliz do adaptador, não o serviço que ele imita.

### VI. Observabilidade Estruturada

Todo comportamento relevante em produção DEVE ser observável em formato padrão:

- Logs: `log/slog` em JSON estruturado; todo log de request DEVE carregar `tenant_id` e
  `instance_id` como atributos.
- Métricas: endpoint `/metrics` Prometheus cobrindo latência por rota, profundidade das filas
  asynq, sessões conectadas e taxa de entrega de webhooks. Toda feature nova que crie fila,
  sessão ou entrega externa DEVE expor métrica correspondente.
- Traces: OpenTelemetry com exporter OTLP configurável por env var (desligado por padrão).
- Coletores (Grafana, Jaeger etc.) ficam fora do escopo da API — ela apenas expõe dados em
  formato padrão.

**Racional:** operação multi-tenant sem correlação por tenant/instância torna qualquer
incidente indiagnosticável.

### VII. Fidelidade ao HyperMeow

Ao escrever ou alterar qualquer código que fale com `github.com/polymorfa/hypermeow`, a
documentação publicada em <https://pkg.go.dev/github.com/polymorfa/hypermeow> é a fonte de
verdade e DEVE ser consultada **antes** — não a memória, não o whatsmeow upstream, não a
analogia com outra biblioteca de WhatsApp. Regras:

- Assinatura, pré-condições e ordem de chamadas DEVEM ser conferidas no símbolo real da
  versão pinada. Inferir a API a partir do nome de um método é PROIBIDO.
- Sequências que a biblioteca prescreve são **literais**: obter o canal de QR antes de
  conectar, ajustar as propriedades do dispositivo antes do pareamento, e amarrar o socket ao
  contexto que deve viver tanto quanto a sessão — `Connect` deriva o tempo de vida da conexão
  do contexto recebido.
- Valores que o **servidor do WhatsApp** valida — nome de exibição do dispositivo, tipo de
  cliente, formato de número — NÃO SÃO texto livre. DEVEM usar as constantes e os formatos
  que a biblioteca documenta; o servidor recusa o resto, e a recusa chega como erro genérico.
- Divergência entre o comportamento observado e a expectativa do código resolve-se em favor
  da biblioteca e do protocolo. A constatação DEVE ser registrada com evidência no
  `research.md` da feature, para que a próxima pessoa não repita a investigação.
- HyperMeow é fork: funcionalidade nova é **aditiva salvo nota no changelog**, portanto a
  documentação do upstream NÃO substitui a do fork. Atualizar a pseudo-versão pinada exige
  releitura do changelog e revisão dos pontos de integração.
- Patch local na biblioteca é PROIBIDO. Toda adaptação vive no adaptador `internal/wa`, que
  DEVE permanecer a única fronteira do repositório a conhecer tipos do HyperMeow — o resto do
  código fala com a interface de sessão, o que é o que torna a sessão roteirizada possível.

**Racional:** as falhas mais caras desta base vieram de inferir a API em vez de conferi-la.
Um nome de exibição que o servidor recusa com `400` e um socket amarrado ao contexto errado
não apareceram em nenhum teste nem em nenhum log da plataforma — apareceram no aparelho do
cliente, que ficou "processando" para sempre.

## Restrições Tecnológicas

Stack fixa do projeto — desvios exigem emenda a esta constituição:

- **Linguagem:** Go 1.25+.
- **Core WhatsApp:** `github.com/polymorfa/hypermeow` (`@main`, pseudo-versão pinada no
  `go.mod`; importa como `whatsmeow`). As tabelas do HyperMeow são migradas pela própria
  biblioteca. O uso da biblioteca é regido pelo Princípio VII.
- **HTTP/API:** chi + huma (OpenAPI 3.1).
- **Dados:** PostgreSQL 17 com pgx v5 (pool único compartilhado entre API e HyperMeow) e
  sqlc; migrações das tabelas da API via golang-migrate, embutidas no binário (`embed.FS`) e
  aplicadas no boot.
- **Assíncrono:** Redis + asynq (webhooks com retry exponencial e DLQ, campanhas, mídia);
  redis_rate para rate limiting; Redis pub/sub para eventos em tempo real.
- **Mídia:** MinIO (S3-compatible), com lifecycle policies e URLs pré-assinadas.
- **Eventos para tenants:** webhooks HTTP assinados configurados por instância (URL, filtro
  de eventos e segredo próprios — canal principal) + WebSocket por instância
  (`/instances/{id}/ws`).
- **Configuração:** 12-factor via env vars (`caarlos0/env`) + secrets do runtime de deploy
  (Swarm Secrets no Swarm; file-based secrets no Compose).
- **Runtime:** binário único `zappermeow` com três subcomandos — `serve` (api, stateless),
  `session-worker` (stateful, ~100–300 sessões/worker) e `jobs` (consumidores asynq) — cada
  um como service Docker, atrás de Traefik (TLS, sticky sessions só para WebSocket).
- **Distribuição:** a entrega inicial DEVE suportar dois alvos de deploy com paridade
  funcional — Docker Swarm (`deploy/stack.yml` + instruções, operação multi-node) e Docker
  Compose (`deploy/docker-compose.yml` + instruções, host único com menos infra). Toda
  mudança de topologia de serviços DEVE ser refletida nos dois arquivos.
- **Imagem:** build multi-stage (builder → distroless).
- Comunicação interna (gRPC, Postgres, Redis) trafega apenas em rede Docker privada
  (overlay no Swarm; bridge no Compose); o gRPC api→worker disca o `grpc_addr` registrado
  no lease, nunca um VIP ou nome de service com balanceamento.

## Fluxo de Desenvolvimento e Qualidade

- **Estrutura:** o código segue o layout `cmd/zappermeow` + `internal/{api, worker, jobs,
  lease, events, domain, store, media, config}` + `proto/` + `migrations/` + `deploy/`.
  As entidades de domínio são tenant, instance, api_key, webhook e message. Código novo DEVE
  respeitar essas fronteiras de pacote.
- **Migrações:** toda mudança de schema das tabelas da API entra como par `up`/`down` em
  `migrations/`; nunca editar migração já aplicada.
- **CI (GitHub Actions):** golangci-lint → testes (com services de Postgres/Redis) → build
  multi-stage da imagem → push no registry. Pipeline verde é pré-condição de merge.
- **Review:** todo PR DEVE ser revisado verificando conformidade com esta constituição —
  em especial isolamento por instância (Princípio II), respeito ao lease de sessão
  (Princípio III) e, quando tocar a biblioteca, aderência à documentação do HyperMeow
  (Princípio VII). Complexidade adicional DEVE ser justificada no PR.
- **Deploy:** Swarm via `docker stack deploy` (rolling update) ou Compose via
  `docker compose up -d`; em ambos os alvos o `session-worker` DEVE ter
  `stop_grace_period` folgado (≥60s) para drain limpo dos leases.

## Governance

- Esta constituição prevalece sobre quaisquer outras práticas ou documentos do projeto em
  caso de conflito.
- **Emendas:** propostas via PR alterando este arquivo, com justificativa e, quando a emenda
  for incompatível com o código existente, um plano de migração. Aprovação de um mantenedor é
  obrigatória.
- **Versionamento:** semântico — MAJOR para remoções/redefinições incompatíveis de princípios,
  MINOR para princípio ou seção nova (ou expansão material), PATCH para clarificações e
  correções de texto. Toda emenda atualiza a linha de versão e o Sync Impact Report.
- **Conformidade:** revisões de PR e o comando `/speckit-analyze` DEVEM verificar aderência
  aos princípios; violações identificadas bloqueiam merge até correção ou emenda formal.
- TECH_STACK.md, ARCHITECTURE.md e features.md são as referências técnicas detalhadas que
  complementam esta constituição; divergências entre eles e este documento resolvem-se em
  favor da constituição e DEVEM gerar correção em um dos lados.

**Version**: 2.5.0 | **Ratified**: 2026-08-12 | **Last Amended**: 2026-08-13
