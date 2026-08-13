# ZapperMeow

API RESTful **open source e self-hosted** para WhatsApp, multi-tenant, construída em Go sobre a biblioteca [HyperMeow](https://github.com/polymorfa/hypermeow) (fork production-focused do whatsmeow).

## O que é

Uma plataforma que você roda na sua própria infraestrutura para expor o WhatsApp como uma API REST completa:

- **Multi-tenant com isolamento por instância:** o operador da plataforma cria tenants; cada tenant é um admin que gerencia suas próprias instâncias. Cada instância é um dispositivo vinculado a um número — como o WhatsApp é multi-dispositivo, várias instâncias podem apontar para o mesmo número e ficar conectadas ao mesmo tempo. Cada uma tem suas próprias API keys e webhooks; credenciais de uma instância nunca afetam as demais.
- **API completa:** mensagens (texto, mídia, enquetes, botões/listas/flows), grupos e comunidades, contatos, chats, newsletters/canais, recursos de WhatsApp Business (catálogo, produtos, pedidos), privacidade — a superfície inteira do HyperMeow exposta via REST com OpenAPI 3.1.
- **Eventos em tempo real:** webhooks HTTP assinados (HMAC, retry com DLQ) e WebSocket por instância, cobrindo ~75 tipos de evento.
- **Feita para produção:** sessões stateful com posse exclusiva garantida por lease em Postgres (failover automático), API stateless com escala horizontal, deploy via Docker Swarm.

## Stack (resumo)

Go 1.25+ · chi + huma (OpenAPI 3.1) · PostgreSQL 17 (pgx + sqlc) · Redis (asynq, cache, rate limit) · MinIO · gRPC entre serviços · Docker Swarm + Traefik. Detalhes e justificativas em [TECH_STACK.md](TECH_STACK.md).

## Desenvolvimento

Duas etapas do código são geradas e **versionadas** no repositório; o CI reprova o merge se a saída
gerada estiver defasada em relação à fonte.

```bash
sqlc generate            # internal/store/ a partir de internal/store/queries/*.sql
go generate ./proto/...  # internal/pb/ a partir de proto/**/*.proto
```

O compilador (`buf`) e os plugins (`protoc-gen-go`, `protoc-gen-go-grpc`) vêm fixados como `tool`
directives no `go.mod`, então **não há nada a instalar** além do Go — nem `protoc`, nem imagem
Docker. O `sqlc`, por depender de um binário externo, continua vindo da release em versão fixa no
CI: um upgrade silencioso mudaria a saída gerada.

## Licença e modelo do projeto

Código sob licença [MIT](LICENSE) — use, modifique e hospede livremente, inclusive comercialmente.

## Aviso legal

Este projeto não é afiliado, associado, autorizado ou endossado pelo WhatsApp/Meta. O uso está sujeito aos [Termos de Serviço do WhatsApp](https://www.whatsapp.com/legal/terms-of-service); o uso indevido (spam, automação abusiva) pode resultar no banimento dos números. Use por sua conta e risco.
