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

## Licença e modelo do projeto

Código sob licença [MIT](LICENSE) — use, modifique e hospede livremente, inclusive comercialmente.

## Aviso legal

Este projeto não é afiliado, associado, autorizado ou endossado pelo WhatsApp/Meta. O uso está sujeito aos [Termos de Serviço do WhatsApp](https://www.whatsapp.com/legal/terms-of-service); o uso indevido (spam, automação abusiva) pode resultar no banimento dos números. Use por sua conta e risco.
