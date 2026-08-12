# Research — Fundação de Contas (001-account-foundation)

**Date**: 2026-08-12 | **Plan**: [plan.md](./plan.md)

Nenhum item do Technical Context ficou como NEEDS CLARIFICATION; esta fase consolida as decisões técnicas em aberto identificadas durante o desenho do plano. Todas as decisões respeitam a constituição (stdlib-first, sem ORM, PG+Redis únicos).

---

## R1. Hash de senhas

- **Decision**: Argon2id via `golang.org/x/crypto/argon2`, parâmetros OWASP atuais (t=1, m=64MiB, p=4, salt 16B, tag 32B), codificado no formato PHC (`$argon2id$v=19$m=65536,t=1,p=4$...`) para permitir migração futura de parâmetros por verificação+rehash no login.
- **Rationale**: Argon2id é a recomendação corrente (OWASP/RFC 9106) para senhas de usuário; `x/crypto` é mantido pelo time do Go (quase-stdlib, sem árvore de dependências). Formato PHC autodescritivo evita coluna extra de parâmetros.
- **Alternatives considered**: `bcrypt` (x/crypto) — maduro, mas limite de 72 bytes e sem resistência a ataques com GPU/ASIC no nível do Argon2; `scrypt` — sem formato de encoding consolidado no ecossistema Go; lib externa `alexedwards/argon2id` — conveniente, porém é wrapper fino que viola o critério de dependência justificável (fazemos o encoding PHC nós mesmos, ~50 linhas testáveis).

## R2. Formato e verificação de API keys

- **Decision**: Segredo de alta entropia gerado com `crypto/rand` (32 bytes), apresentado como `zmk_<base62(32B)>` (~43 chars úteis + prefixo). Armazena-se apenas: `key_prefix` (primeiros 12 chars visíveis, para identificação em listagens) e `secret_hash = SHA-256(segredo completo)` com índice único. Verificação: hash do valor recebido no header + lookup exato pelo índice; comparação em tempo constante.
- **Rationale**: Para segredos aleatórios de 256 bits, KDF lento (Argon2/bcrypt) é desnecessário e inviável no hot path (toda requisição operacional); SHA-256 é o padrão da indústria para tokens de alta entropia (GitHub PATs, Stripe). Prefixo `zmk_` facilita detecção em scanners de vazamento de segredo e o `key_prefix` permite ao admin identificar a key listada sem nunca reexibir o segredo (FR-011).
- **Alternatives considered**: Argon2 nas keys — custo de CPU por requisição incompatível com rota operacional; UUID como segredo — entropia menor e sem prefixo identificável; armazenar segredo cifrado (reversível) — viola FR-011 (irrecuperável por design).

## R3. JWT: algoritmo, claims e invalidação imediata

- **Decision**: `golang-jwt/jwt/v5` com **HS256** e signing key de 512 bits vinda de secret do deploy (`/run/secrets/jwt_signing_key`, fallback env em dev). Claims: `sub` (user_id), `aud` (`platform` | `tenant`), `tenant_id` (somente aud tenant), `pwd_change` (bool — senha temporária pendente), `iat`, `exp` (TTL default 1h, configurável). Invalidação imediata (suspensão/exclusão) **não** usa blocklist: todo middleware admin, após validar assinatura/exp/aud, carrega status atual de usuário e tenant do Postgres (1 SELECT indexado) e rejeita se suspenso/excluído/senha-resetada; a mesma carga verifica `must_change_password` no banco (não apenas o claim `pwd_change`) e rejeita tokens com `iat` anterior a `users.password_changed_at` — troca ou reset de senha derruba imediatamente os tokens em circulação (SC-004).
- **Rationale**: Emissor e validador são o mesmo serviço — assimetria (RS256/EdDSA) não compra nada e adiciona gestão de par de chaves. A checagem de status por requisição atende o requisito de cascata imediata (US4, SC-004 ≤ 5s) com custo de um lookup por PK — barato na escala alvo e infinitamente mais simples que revocation list distribuída. O claim `pwd_change` permite ao middleware restringir o token pós-reset à rota de troca de senha (US5) sem estado adicional.
- **Alternatives considered**: Blocklist de `jti` em Redis — mais peças móveis, e ainda exigiria checar suspensão de tenant (o evento invalida N tokens desconhecidos); TTL curtíssimo (1min) sem checagem — viola a janela de 5s e degrada UX; sessões server-side em Postgres — reintroduz estado de sessão que o JWT existe para evitar; RS256 — justificável só quando terceiros validarem tokens.

## R4. Lockout de conta e limite por origem no login

- **Decision**: Duas camadas independentes no `POST /auth/login`:
  1. **Por conta (durável)**: colunas `failed_login_count` e `locked_until` em `users`, atualizadas transacionalmente a cada falha; ao atingir N falhas (default 5, config), `locked_until = now() + janela` (default 15min, config). Login com sucesso zera o contador. Conta bloqueada responde a falha genérica (FR-019) — sem revelar o bloqueio a terceiros; o desbloqueio é por expiração do timestamp (nenhum job necessário) e o evento `account_unlocked` é registrado no primeiro login bem-sucedido após a expiração. Ordem de checagem no login: a senha é verificada **antes** de revelar qualquer estado — `403 TENANT_SUSPENDED` só com senha correta; senha errada em tenant suspenso responde o mesmo `401` genérico (FR-019).
  2. **Por origem (efêmero)**: GCRA via `redis_rate` keyed por IP de origem (respeitando `X-Forwarded-For` só de proxy confiável/Traefik), limite configurável (default 30 tentativas/min por IP).
- **Rationale**: FR-020 exige estado de bloqueio durável → Postgres, não Redis. Desbloqueio por comparação `locked_until < now()` elimina scheduler. Limite por IP é caso clássico do redis_rate já presente na stack, e dado efêmero pode viver em Redis sem violar durabilidade.
- **Alternatives considered**: Lockout em Redis com TTL — perde estado em restart/failover do Redis (viola FR-020); tabela separada de tentativas — mais geral (auditoria já coberta por `security_events`), porém desnecessária para a regra de bloqueio; backoff progressivo por tentativa — complexidade sem exigência na spec (janela fixa configurável cobre o requisito).

## R5. Bootstrap do super-admin

- **Decision**: No boot do `serve`, após aplicar migrações: se **nenhum** usuário com papel `super_admin` existe e `ZAPPERMEOW_BOOTSTRAP_EMAIL` + `ZAPPERMEOW_BOOTSTRAP_PASSWORD` (ou secrets equivalentes em `/run/secrets/`) estão definidos, cria o super-admin (transação com lock advisory para tolerar N réplicas subindo juntas). Se já existe super-admin, a config é ignorada (logada em INFO). Se não existe e a config está ausente, o serviço sobe e loga WARN a cada boot (edge case da spec).
- **Rationale**: Padrão consolidado em self-hosted (MinIO `MINIO_ROOT_USER`, Grafana admin). Advisory lock (`pg_advisory_xact_lock`) resolve corrida entre réplicas sem tabela de controle. Ignorar a config quando já há super-admin implementa exatamente o edge case da spec (alterar bootstrap depois não tem efeito).
- **Alternatives considered**: Comando CLI `zappermeow bootstrap` — passo manual extra que quebra o "sobe e funciona" do Compose; gerar senha aleatória e imprimir no log — segredo em log viola SC-006/constituição; arquivo de estado "bootstrap done" — o próprio banco já é a fonte de verdade (existência de super-admin).

## R6. Endpoint de login único vs. separado por papel

- **Decision**: Um único `POST /auth/login` (email + senha). O papel do usuário encontrado determina o audience do token emitido (`platform` para super-admin, `tenant` + `tenant_id` para admin de tenant). Rotas de troca de senha (`POST /auth/password`) igualmente únicas, válidas para ambos os papéis.
- **Rationale**: Emails são únicos globalmente (FR-005) — o usuário identifica o papel sozinho; dois endpoints duplicariam contrato, validação, lockout e testes sem ganho. Resposta idêntica para email inexistente em qualquer papel reforça FR-019.
- **Alternatives considered**: `/admin/auth/login` + `/auth/login` separados — útil apenas se as políticas divergissem (não divergem nesta feature); login por tenant slug + email — email já é único global, campo extra sem função.

## R7. Rota operacional de verificação (FR-014)

- **Decision**: `GET /instances/{id}/whoami`, autenticada **exclusivamente** por API key (header `X-Api-Key`), retornando dados públicos da instância (id, nome, estado, tenant_id) + `key_prefix` e rótulo da key usada. Passa pelo middleware operacional completo: key válida → key pertence à instância `{id}` da URL → tenant ativo → rate limit GCRA por key (limite default global configurável).
- **Rationale**: Materializa a cadeia de credencial inteira de forma testável (US3) e estabelece desde já o **template do middleware operacional** que todas as rotas futuras (`/messages`, `/groups`...) reutilizarão — incluindo o rate limit por key exigido pela constituição para rotas operacionais. Nome `whoami` comunica o propósito (verificar credencial) sem colidir com o `GET /instances/{id}` administrativo (JWT).
- **Alternatives considered**: Aceitar JWT **ou** API key no mesmo `GET /instances/{id}` — dois esquemas de segurança na mesma rota complicam contrato/middlewares e mascaram a distinção plano administrativo × operacional; adiar qualquer rota operacional — deixaria FR-013/FR-014 e o isolamento key↔instância sem verificação fim-a-fim nesta feature.

## R8. Localização das migrações e aplicação no boot

- **Decision**: Migrações SQL em `migrations/` na **raiz do repositório** (aderência literal à constituição), embutidas via `embed.FS` por um pacote raiz mínimo (`migrations/embed.go`) e aplicadas no boot do `serve` com golang-migrate (driver `iofs` + pgx), antes do bootstrap do super-admin. Par `up`/`down` obrigatório; `0001_account_foundation.{up,down}.sql` cobre todo o schema desta feature.
- **Rationale**: A constituição fixa o layout com `migrations/` na raiz e migrações embutidas aplicadas no boot — segui-la literalmente evita debate; `embed.go` na própria pasta é o idioma Go para embed fora de `internal/`. Aplicar antes do bootstrap garante que o schema existe quando o super-admin for criado.
- **Alternatives considered**: `internal/store/migrations/` — embed mais "próximo" do consumidor, mas diverge do layout constitucional sem ganho que justifique emenda; aplicar migrações por job/init-container — peça extra de deploy contra o princípio de simplicidade.

## R9. Registro de eventos de segurança (FR-021)

- **Decision**: Tabela append-only `security_events` (tipo, ator, alvo, resultado, origem, metadata jsonb, created_at), gravada **na mesma transação** da ação quando há escrita (criação/revogação de key, suspensão, reset) e em transação própria para eventos de leitura/negação (login falho, lockout); exclusões em cascata registram apenas o evento pai (`tenant_deleted`/`instance_deleted`) com as contagens dos recursos removidos no `metadata`, sem eventos filhos. Espelhada em log slog estruturado (sem segredos). Consulta via SQL direto pelo operador; endpoint de listagem fica fora do escopo (spec exige registro rastreável, não API de auditoria).
- **Rationale**: Mesma transação garante que evento e efeito são atômicos (SC-007: 100% localizáveis); jsonb dá extensibilidade sem migração por tipo de evento; não expor endpoint evita crescer a superfície além da spec.
- **Alternatives considered**: Apenas logs slog — logs são efêmeros/rotacionados, frágil para SC-007; fila asynq para gravação assíncrona — perde atomicidade e adiciona consumidor sem necessidade nesta escala.

## R10. Padrões huma para auth, envelope e erros

- **Decision**: Security schemes declarados no OpenAPI via huma (`bearerAuth` JWT para rotas administrativas; `apiKey` header `X-Api-Key` para operacionais); enforcement nos middlewares chi por grupo de rotas (plataforma / tenant / operacional), injetando o contexto autenticado (`user`, `tenant_id`, `instance`, `key`) no `context.Context`. **Respostas de sucesso** com corpo usam envelope padrão `{ "status": <código HTTP numérico>, "data": ..., "timestamp": ... }` — `status` com a mesma semântica do membro homônimo da RFC 9457 —, implementado como struct genérica nos outputs tipados do huma (aparece fielmente no OpenAPI gerado). **Erros de domínio** mapeados para RFC 9457 (`application/problem+json`, formato nativo do huma) estendida com `code` estável e `timestamp` via customização de `huma.NewError`, com catálogo fixo de códigos (ver [contrato](./contracts/http-api.md)): 401 credencial ausente/inválida, 403 audience/escopo errado ou senha temporária pendente, 404 recurso de outro tenant (sem confirmar existência — FR-009), 409 duplicidade (email/nome), 422 validação de campos, 429 rate limit/lockout indireto via resposta genérica de login.
- **Rationale**: Middleware por grupo mantém handlers puros (request tipado → resposta tipada) e o esquema aparece corretamente na spec gerada (Princípio IV). Como a API é o "frontend" do produto, o formato de resposta é contrato rígido: o envelope de sucesso dá aos clientes um shape único e previsível, e o `code` estável permite tratamento programático de erros sem parsear mensagens. RFC 9457 permanece a base dos erros — é o default do huma e um padrão IETF machine-readable; a extensão com membros adicionais é explicitamente prevista pela própria RFC. Retornar 404 (não 403) para recurso alheio implementa "negar sem confirmar existência".
- **Alternatives considered**: Resolvers de auth por operação do huma — espalha lógica de segurança por handler, dificulta garantir FR-003 ("toda rota exige auth") por construção; envelope próprio também nos erros (estilo `{status, data, errors[]}` uniforme) — substituiria o padrão IETF nativo do huma sem ganho, já que `code` + `errors[]` sobre RFC 9457 cobrem o mesmo contrato programático; respostas sem envelope (recurso direto no corpo) — deixa clientes sem shape uniforme e sem lugar canônico para metadados de resposta.

---

**Status**: todas as decisões fechadas; nenhum NEEDS CLARIFICATION remanescente. Prosseguir para Phase 1 (data-model, contratos, quickstart).
