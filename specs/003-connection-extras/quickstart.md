# Quickstart — Validação dos Complementos de Conexão (003-connection-extras)

**Date**: 2026-08-14 | **Spec**: [spec.md](./spec.md) | **Contracts**: [contracts/](./contracts/)

Guia de validação fim-a-fim. Cenários marcados **[manual]** só um proxy real, um número real ou o
aparelho revelam — a constituição (v2.5.0, Princípio V) exige executá-los e **registrar o
resultado neste arquivo** antes de dar a feature por entregue.

## Estado da validação automatizada (2026-08-14)

Suíte completa verde (`go test ./...`), `golangci-lint run` sem apontamentos e `go build ./...`
limpo. O que já está coberto sem intervenção humana:

| Story | Cobertura automatizada |
| --- | --- |
| US1 proxy | `internal/domain/proxy_test.go` (validação, máscara, propriedade "nunca ecoa a senha"), `internal/worker/session_proxy_test.go` (URL crua chega ao cliente, rebuild com a nova, failover preserva, falha classificada), `internal/api/connection_proxy_test.go` (contrato HTTP, senha ausente de resposta/consulta/trilha, isolamento) |
| US2 eventos | `internal/wa/classify_stream_test.go` (**com prova de regressão registrada abaixo**), `internal/worker/session_events_test.go` (trilha com o código, material preservado, reconexão automática) |
| US3 modo passivo | `internal/worker/session_passive_test.go` (reaplicação **após cada** `Connected`, nunca antes de conectar, sobrevive a failover, falha não derruba a sessão, eventos seguem fluindo) |
| US4 passkey | `internal/worker/session_passkey_test.go` (fluxo completo, confirmação automática, comandos fora de ordem, falha, **as duas ordens** entre canais) |
| US5 códigos | `internal/worker/verification_test.go` (LID, resolução de telefone, pré-condições), `internal/api/verification_test.go` (JWT de tenant recusado) |
| Contrato | `internal/api/openapi_contract_test.go` (6 operações na spec gerada com os esquemas de auth corretos) |

**Prova de regressão (v2.5.0-e), executada em 2026-08-14**: com os cases `*events.StreamError` e
`*events.ManualLoginReconnect` removidos de `internal/wa/classify.go`, os testes
`TestClassifyStreamError`, `TestClassifyStreamErrorWithoutACode` e
`TestClassifyManualLoginReconnect` falham — os eventos eram silenciosamente ignorados. Com os
cases restaurados, passam.

**Defeitos que os testes encontraram durante a implementação** (todos corrigidos):

1. `relink` atualizava a linha da instância mas **não escrevia na trilha** — a transição de troca
   de proxy seria invisível ao tenant. `recordDisconnect` e `record` são gravações separadas.
2. A sessão roteirizada devolvia a mesma instância já fechada após um rebuild, e devolvia uma
   sessão **sem material de dispositivo** quando havia JID salvo. Ambos escondiam o
   comportamento real do container, que constrói um cliente novo carregando o device.

## Pré-requisitos

```bash
docker compose -f deploy/docker-compose.yml up -d   # api + session-worker + postgres + redis + minio
go test ./...                                        # suíte verde antes de validar na mão
```

Para os cenários de proxy: um proxy HTTP e um SOCKS5 acessíveis (para teste local, um
`docker run -p 3128:3128 ubuntu/squid` e um `docker run -p 1080:1080 serjs/go-socks5-proxy`
resolvem). Para os cenários de passkey: um número cuja conta WhatsApp tenha passkey cadastrada.
Variáveis usadas abaixo: `$API` (base URL), `$JWT` (token de tenant), `$KEY` (API key da
instância), `$ID` (instance id).

## US1 — Proxy por instância (P1)

### 1.1 Validação de URL (automatizável)

```bash
curl -sX PUT "$API/instances/$ID/proxy" -H "Authorization: Bearer $JWT" \
  -d '{"url":"ftp://x"}'          # → 422 UNSUPPORTED_PROXY_SCHEME
curl -sX PUT "$API/instances/$ID/proxy" -H "Authorization: Bearer $JWT" \
  -d '{"url":"::not-a-url::"}'   # → 422 INVALID_PROXY_URL
```

### 1.2 Máscara da senha (automatizável)

```bash
curl -sX PUT "$API/instances/$ID/proxy" -H "Authorization: Bearer $JWT" \
  -d '{"url":"socks5://user:s3cret@127.0.0.1:1080"}' | jq .data.proxy_url
# → "socks5://user:***@127.0.0.1:1080"; grep -r s3cret nos logs deve voltar vazio (SC-007)
curl -s "$API/instances/$ID" -H "Authorization: Bearer $JWT" | jq .data.connection.proxy_url
# → mascarado também na consulta
```

### 1.3 Tráfego sai pelo proxy **[manual]**

Com proxy definido, conectar a instância (QR da 002) e verificar no log de acesso do proxy as
conexões a `web.whatsapp.com`/`.whatsapp.net` (websocket **e** mídia). Derrubar o proxy e
confirmar na trilha `disconnected(proxy_connect_failed)` com retries **sem nenhuma conexão
direta** (conferir por tcpdump/conntrack no host: zero SYN aos ranges da Meta fora do proxy) —
SC-001, FR-005.

- [ ] Resultado (data, executor, evidência):

### 1.4 Mudança a quente reconecta (automatizável com sessão roteirizada; **[manual]** ponta-a-ponta)

Com instância conectada, `PUT .../proxy` com URL nova → resposta `reconnecting: true`; no WS:
`connection.disconnected{reason: proxy_updated}` seguido de `connection.connected` em ≤30s
(SC-002). Trilha registra `proxy_updated` com URL mascarada.

- [ ] Resultado:

## US2 — StreamError / ManualLoginReconnect (P2, sessão roteirizada)

Automatizável por completo — é para isso que a sessão roteirizada existe:

- Roteiro emite `StreamError{Code:"999"}` → trilha ganha `stream_error` com
  `detail.stream_error_code = "999"`, WS emite `connection.disconnected{reason: stream_error}`,
  supervisor reconecta, material de sessão intacto (relogin sem QR).
- Roteiro emite `ManualLoginReconnect` ao fim de um pareamento → trilha `manual_login_reconnect`,
  reconexão imediata (≤10s, SC-003), instância termina `connected`.
- Prova de regressão (v2.5.0-e): os dois testes rodados sem os cases novos do `classify.go`
  falham (evento ignorado).

## US3 — Modo passivo (P3)

### 3.1 Persistência e reaplicação (automatizável)

`PUT .../passive-mode {"enabled":true}` com instância desconectada → `applied: false`; conectar
(roteirizada) → o worker chama `SetPassive(true)` após `Connected`; matar o worker → failover →
`SetPassive(true)` de novo. A sessão roteirizada registra as chamadas e o teste verifica a ordem
**após cada** `Connected` (research R6).

### 3.2 Comportamento no aparelho **[manual]**

Com número real conectado em modo passivo por período prolongado (~24h): mensagens recebidas
continuam chegando (visível em logs de eventos da sessão) e o número não aparece "online" para
contatos (SC-004). **Limitação conhecida e aceita** (plan.md, Complexity Tracking): a cada
conexão existe janela transitória de ~1 round-trip em modo ativo antes da convergência — não deve
ser observável no aparelho; registrar aqui se for.

- [ ] Resultado:

## US4 — Pareamento com passkey (P4)

### 4.1 Fluxo roteirizado (automatizável)

Sessão roteirizada encena: desafio → `pairing.passkey_challenge` no WS (e no snapshot de um
segundo cliente que conecta depois — fase `passkey_challenge`); resposta via endpoint →
código → `pairing.passkey_code`; confirmação → `pairing.succeeded`. Variantes: `SkipHandoffUX`
(nenhum frame de código; conclusão direta), erro em cada passo (→
`pairing.failed{passkey_error}`), chamadas fora de ordem (`409 NO_PASSKEY_CHALLENGE` /
`NO_PASSKEY_CODE`), e as **duas ordens** entre item do canal de QR e erro do handler global
(v2.5.0-b).

### 4.2 Conta com passkey de verdade **[manual]**

Parear um número cuja conta tem passkey: exibir o QR, escanear, seguir o desafio no navegador
(WebAuthn), conferir o código `XXXX-XXXX` contra o telefone, confirmar, ver a instância
`connected` com identidade preenchida dentro da janela de pareamento (SC-005). Registrar também o
que só o aparelho revela: o dispositivo listado, o nome de exibição correto.

- [ ] Resultado:

## US5 — Códigos de verificação (P5)

### 5.1 Erros de pré-condição (automatizável)

```bash
curl -s "$API/instances/$ID/identity-verification-codes?contact=abc" -H "X-Api-Key: $KEY"
# → 422 INVALID_CONTACT
# instância desconectada → 409 INSTANCE_NOT_CONNECTED
# telefone sem mapeamento → 422 IDENTITY_NOT_RESOLVABLE
# JWT de tenant → 401/404 (endpoint é apiKey-only, FR-025)
```

### 5.2 Contra contato real **[manual]**

Com instância conectada e uma conversa existente com um contato:

```bash
curl -s "$API/instances/$ID/identity-verification-codes?contact=<lid>" -H "X-Api-Key: $KEY"
curl -s "$API/instances/$ID/identity-verification-codes?contact=<phone>" -H "X-Api-Key: $KEY"
```

Os dois retornos devem ser idênticos (SC-006) e responder em ≤5s. **Verificação que só o aparelho
revela**: o `numeric_code` de 60 dígitos deve bater com o "código de segurança" exibido no
WhatsApp do contato (Conversa → nome → Criptografia).

- [ ] Resultado:

## Regressão da 002

Rodar o roteiro manual da [002](../002-instance-connection/quickstart.md) (pareamento QR simples,
desconectar/reconectar, logout) numa instância **sem** proxy e **sem** passivo: nada desta feature
pode ter mudado o caminho padrão.

- [ ] Resultado:
