<!--
Sync Impact Report
==================
Version change: 1.0.0 → 2.0.0
Rationale: MAJOR — o Princípio II foi redefinido de forma incompatível: credenciais que antes
podiam ter escopo de tenant (API keys "por tenant/instância", HMAC "por tenant", mídia "por
tenant") agora DEVEM ter escopo de instância. Alinha a constituição às emendas de 2026-08-12
em ARCHITECTURE.md ("Modelo de contas e isolamento"), TECH_STACK.md e features.md: modelo de
contas em 2 níveis (plataforma → tenant → N instâncias), API keys e webhooks por instância,
JWT em dois audiences, e remoção da noção de "plano" (billing) — o projeto é open source
(MIT); limites são configuração operacional por tenant.

Modified principles:
- II. "Multi-Tenancy Segura por Padrão" → "Multi-Tenancy com Isolamento por Instância"
  (modelo de contas explícito; keys/webhooks/HMAC por instância; JWT em dois audiences;
  autorização tenant↔instância; rate limit por key com limites por tenant, sem "plano")
Modified sections:
- Restrições Tecnológicas: bullet "Eventos para tenants" agora explicita configuração de
  webhook por instância (URL, filtro, segredo próprios).
- Fluxo de Desenvolvimento e Qualidade: entidade api_key citada no layout de domain.
- Governance: features.md incluído nas referências técnicas complementares.
Added sections: nenhuma
Removed sections: nenhuma
Deferred items / TODOs: nenhum
-->

# zappermeow Constitution

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
é um admin que gerencia suas próprias instâncias (cada instância = um número WhatsApp = uma
sessão). A **instância é a unidade de isolamento**: credenciais e canais de eventos pertencem
à instância, não ao tenant. Regras:

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
- Webhooks são configurados **por instância** (URL, filtro de tipos de evento e segredo
  próprios); payloads DEVEM ser assinados com HMAC-SHA256 usando o segredo do webhook da
  instância.
- Mídia no MinIO DEVE usar object key prefixada por `tenant_id/instance_id` e ser servida
  apenas por URLs pré-assinadas.
- Credenciais e chaves (banco, HMAC, JWT signing) DEVEM vir de Docker Swarm Secrets em
  produção; env vars só como fallback de desenvolvimento. Segredos NUNCA aparecem em logs.

**Racional:** isolamento por número é requisito de segurança — vazamento ou rotação de
credenciais de um número não pode afetar os demais, e sistemas distintos de um mesmo tenant
consomem números distintos.

### III. Posse Exclusiva de Sessão (NON-NEGOCIÁVEL)

Cada instância WhatsApp mantém estado criptográfico Signal e DEVE estar conectada em
exatamente um processo por vez — duplo-connect corrompe o estado. Consequências obrigatórias:

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
- Contratos internos api↔worker DEVEM ser definidos em Protobuf (`proto/`) e versionados no
  repositório; mudanças incompatíveis exigem coordenação explícita de deploy.
- Mudanças incompatíveis em rotas públicas exigem versionamento de API e registro da
  quebra no changelog do release.

**Racional:** com o contrato derivado do código, a documentação nunca mente; consumidores
integram contra a spec com confiança.

### V. Testes Contra Infraestrutura Real

Testes DEVEM validar comportamento contra as dependências reais, não contra mocks de
infraestrutura:

- Testes unitários: `go test` + testify, table-driven, para lógica de domínio pura.
- Testes de integração: testcontainers-go DEVE subir Postgres e Redis reais; queries sqlc,
  handlers, leases e filas são testados contra essas instâncias.
- Toda correção de bug DEVE incluir um teste que reproduza o defeito antes do fix.
- O pipeline de CI (lint → testes → build) é bloqueante: código que não passa em
  golangci-lint ou nos testes NÃO PODE ser mergeado.

**Racional:** o núcleo do sistema é SQL, filas e locking distribuído — exatamente as áreas
onde mocks escondem bugs.

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

## Restrições Tecnológicas

Stack fixa do projeto — desvios exigem emenda a esta constituição:

- **Linguagem:** Go 1.25+.
- **Core WhatsApp:** `github.com/polymorfa/hypermeow` (`@main`, pseudo-versão pinada no
  `go.mod`; importa como `whatsmeow`). As tabelas do HyperMeow são migradas pela própria
  biblioteca.
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
- **Configuração:** 12-factor via env vars (`caarlos0/env`) + Swarm Secrets.
- **Runtime:** binário único `zappermeow` com três subcomandos — `serve` (api, stateless),
  `session-worker` (stateful, ~100–300 sessões/worker) e `jobs` (consumidores asynq) — cada
  um como service no Docker Swarm, atrás de Traefik (TLS, sticky sessions só para WebSocket).
- **Imagem:** build multi-stage (builder → distroless).
- Comunicação interna (gRPC, Postgres, Redis) trafega apenas em overlay network privada; o
  gRPC api→worker disca o `grpc_addr` registrado no lease, nunca o VIP do Swarm.

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
  em especial isolamento por instância (Princípio II) e respeito ao lease de sessão
  (Princípio III). Complexidade adicional DEVE ser justificada no PR.
- **Deploy:** `docker stack deploy` com rolling update; `session-worker` com
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

**Version**: 2.0.0 | **Ratified**: 2026-08-12 | **Last Amended**: 2026-08-12
