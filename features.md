# ZapperMeow — Levantamento de Funcionalidades do HyperMeow

Levantamento das funcionalidades disponíveis na biblioteca [polymorfa/hypermeow](https://github.com/polymorfa/hypermeow) (fork production-focused do `tulir/whatsmeow`, escrito em Go) para servir de base à construção de uma API RESTful completa.

- **Import:** `import whatsmeow "github.com/polymorfa/hypermeow"` (o pacote raiz mantém o nome `whatsmeow`)
- **Instalação:** `go get github.com/polymorfa/hypermeow@main` (versões semânticas foram retraídas; usar sempre `@main` e pinar a pseudo-versão no `go.mod`)
- **Licença:** MPL-2.0 (código upstream) + MIT (código Polymorfa)
- **Levantado em:** 2026-08-12, commit mais recente de 2026-08-11

## Como ler as marcações

- `[x]` — já desenvolvido e exposto pela API da ZapperMeow; a spec que entregou vem indicada entre parênteses.
- `[ ]` — ainda não desenvolvido (backlog).
- Itens sem checkbox são **contexto**, não backlog: características da biblioteca, notas de arquitetura e listas de eventos já cobertas em outra seção.

**Estado atual do produto** (branch `003-connection-extras`):

| Spec | Escopo | Situação |
| --- | --- | --- |
| [001-account-foundation](specs/001-account-foundation/spec.md) | Tenants, usuários, instâncias como registro, API keys, autenticação | Entregue |
| [002-instance-connection](specs/002-instance-connection/spec.md) | Pareamento, ciclo de vida da conexão, posse exclusiva de sessão, estado e trilha, WebSocket | Entregue (resta apenas a validação manual T079, que depende de um número real) |
| [003-connection-extras](specs/003-connection-extras/spec.md) | Proxy por instância, modo passivo, etapa de passkey, códigos de verificação de identidade, eventos `StreamError`/`ManualLoginReconnect` | Entregue (restam as validações manuais do quickstart, que dependem de proxy e número reais) |

Tudo que envolve **mensagens, mídia, grupos, contatos, presença, newsletters, business, chamadas, privacidade e webhooks** continua no backlog.

---

## 1. Sessão, autenticação e conexão

Base para endpoints de gerenciamento de instâncias/sessões (multi-sessão é o caso de uso alvo do fork).

- [x] **Criar cliente a partir de um device store** — `NewClient` *(002)*
- [x] **Conectar / conectar com contexto** — `Connect`, `ConnectContext`, `WaitForConnection` *(002)*
- [x] **Login via QR Code** (stream de códigos p/ exibir no front) — `GetQRChannel`, evento `QR` *(002)*
- [x] **Pareamento por código de telefone** (sem QR) — `PairPhone` *(002)*
- [x] **Pareamento por passkey** — `SendPasskeyResponse`, `SendPasskeyConfirmation` (eventos `PairPasskeyRequest/Confirmation/Error`) *(003 — etapa dentro do pareamento por QR; desafio e código de conferência descem pelo WebSocket, resposta e confirmação sobem por endpoints)*
- [x] **Status de sessão** — `IsConnected`, `IsLoggedIn` *(002)*
- [x] **Desconectar / reconectar / logout** — `Disconnect`, `Logout` *(002)*; a reconexão é feita pelo supervisor com backoff e `EnableAutoReconnect` — `ResetConnection` não é usado
- [x] **Modo passivo** (receber sem marcar online) — `SetPassive` *(003 — flag persistida por instância, reaplicada após cada `Connected`)*
- [x] **Proxy e HTTP clients customizados** — `SetProxy`, `SetProxyAddress` *(003 — proxy por instância cobrindo websocket e mídia, sem fallback direto; `SetSOCKSProxy` é usado indiretamente por `SetProxyAddress`, e os setters de HTTP client seguem sem uso por sobrescreverem o proxy)*
- [x] **Handlers de eventos** (base p/ webhooks) — `AddEventHandler`, `RemoveEventHandler` *(002, consumo interno; a exposição como webhook ainda não existe — `AddEventHandlerWithSuccessStatus` não é usado)*
- [x] **Códigos de verificação de identidade (LID)** — `GetIdentityVerificationCodes` *(003 — consulta operacional por LID ou telefone, exclusiva da API key da instância)*

Eventos de ciclo de vida:

- [x] Tratados pela classificação de desconexão *(002)*: `Connected`, `Disconnected`, `LoggedOut`, `StreamReplaced`, `TemporaryBan`, `ClientOutdated`, `ConnectFailure`, `CATRefreshError`, `KeepAliveTimeout`, `KeepAliveRestored`, `PairSuccess`, `PairError`, `QRScannedWithoutMultidevice`
- [x] Tratados pela 003: `StreamError` (motivo próprio com o código na trilha, não permanente) e `ManualLoginReconnect` (reconexão agendada pelo supervisor)

## 2. Envio de mensagens

- [ ] **Enviar mensagem** (DM, grupo, status/broadcast) — `SendMessage` (+ `SendRequestExtra`: ID custom, timeout, media handle, bot inline, peer message)
- [ ] **Tipos de conteúdo** (proto `waE2E.Message`) — texto/extended text, imagem, vídeo, áudio/PTT, documento, sticker (+ packs via `FetchStickerPack`), localização (+ live location), contato (vCard), botões, listas, templates, view-once
- [ ] **Gerar ID de mensagem** — `GenerateMessageID`, `GenerateFacebookMessageID`
- [ ] **Editar mensagem** — `BuildEdit` + `SendMessage`
- [ ] **Revogar/apagar p/ todos** — `RevokeMessage`, `BuildRevoke`, `BuildMessageKey`
- [ ] **Reações** — `BuildReaction`; criptografia: `EncryptReaction`, `DecryptReaction`
- [ ] **Enquetes (polls)** — `BuildPollCreation`, `BuildPollVote`, `EncryptPollVote`, `DecryptPollVote`, `HashPollOptions`
- [ ] **Comentários (channels)** — `EncryptComment`, `DecryptComment`
- [ ] **Mensagens efêmeras** (timer de desaparecimento) — `SetDisappearingTimer`, `SetDefaultDisappearingTimer`, `ParseDisappearingTimerString`
- [ ] **Mensagens de consentimento de número** — `BuildRequestPhoneNumberMessage`, `BuildSharePhoneNumberMessage`
- [ ] **Mensagens FB/Messenger (armadillo)** — `SendFBMessage`
- [ ] **Solicitar mensagem indisponível / history sync sob demanda** — `BuildUnavailableMessageRequest`, `BuildHistorySyncRequest`
- [ ] **Parse de mensagens da web** — `ParseWebMessage`

## 3. Mídia (upload/download)

- [ ] **Upload criptografado** (imagem, vídeo, áudio, doc, sticker) — `Upload`, `UploadReader`
- [ ] **Upload para newsletters/canais** (plaintext) — `UploadNewsletter`, `UploadNewsletterReader`
- [ ] **Download de qualquer mídia de mensagem** — `Download`, `DownloadAny`, `DownloadThumbnail`, `DownloadFB`
- [ ] **Download direto para arquivo** (streaming, memória constante) — `DownloadToFile`, `DownloadFBToFile`, `DownloadMediaWithPathToFile`, `DownloadMediaWithOnlyPathToFile`
- [ ] **Download por path direto** — `DownloadMediaWithPath`, `DownloadMediaWithOnlyPath`
- [ ] **Deletar mídia do servidor** — `DeleteMedia`
- [ ] **Retry de mídia expirada** — `SendMediaRetryReceipt`, `DecryptMediaRetryNotification` (eventos `MediaRetry`, `MediaRetryError`)

## 4. Grupos

- [ ] **Criar grupo** — `CreateGroup`
- [ ] **Info do grupo / grupos que participo** — `GetGroupInfo`, `GetJoinedGroups`
- [ ] **Comunidades** (subgrupos, vincular/desvincular) — `GetSubGroups`, `GetLinkedGroupsParticipants`, `LinkGroup`, `UnlinkGroup`
- [ ] **Sair do grupo** — `LeaveGroup`
- [ ] **Participantes** (adicionar, remover, promover, rebaixar) — `UpdateGroupParticipants`
- [ ] **Solicitações de entrada** (aprovar/rejeitar) — `GetGroupRequestParticipants`, `UpdateGroupRequestParticipants`, `SetGroupJoinApprovalMode`
- [ ] **Modo de adição de membros** — `SetGroupMemberAddMode`
- [ ] **Foto, nome, tópico/descrição** — `SetGroupPhoto`, `SetGroupName`, `SetGroupTopic`, `SetGroupDescription`
- [ ] **Travar edição de info / modo somente-admins** (announce) — `SetGroupLocked`, `SetGroupAnnounce`
- [ ] **Link de convite** (obter/resetar, entrar, preview) — `GetGroupInviteLink`, `JoinGroupWithLink`, `GetGroupInfoFromLink`, `JoinGroupWithInvite`, `GetGroupInfoFromInvite`

Eventos: `GroupInfo`, `JoinedGroup` (+ mismatch de hash de participantes exposto como erro — hardening do fork).

## 5. Contatos e usuários

- [ ] **Verificar se números estão no WhatsApp** — `IsOnWhatsApp`
- [ ] **Info de usuários** (status, devices, business) — `GetUserInfo`, `GetUserDevices`, `GetUserDevicesContext`
- [ ] **Foto de perfil** (usuário/grupo, preview ou full) — `GetProfilePictureInfo`
- [ ] **Status/recado próprio** — `SetStatusMessage`
- [ ] **Bloqueio** (listar, bloquear, desbloquear) — `GetBlocklist`, `UpdateBlocklist` (eventos `Blocklist`, `BlocklistChange`)
- [ ] **QR link de contato** — `GetContactQRLink`, `ResolveContactQRLink`
- [ ] **Resolver username / LID** — `ResolveUsername`, `ResolveLID`, `StoreLIDPNMapping`
- [ ] **Bots** (Meta AI etc.) — `GetBotListV2`, `GetBotProfiles`
- [ ] **Store de contatos** (nomes, push names, usernames) — `ContactStore`, `ContactUsernameStore` (+ leitura em lote)

**Modelo de identidade LID-first (diferencial do fork):** LIDs são a identidade Signal estável; números de telefone e usernames são aliases persistidos, com resolução em lote (`LIDBatchReverseStore`) e cache negativo. Relevante para o design de rotas: aceitar/retornar JID de telefone, LID e username.

## 6. Presença, recibos e estado de chat

- [ ] **Presença global** (online/offline) — `SendPresence`
- [ ] **Presença em chat** (digitando/gravando/pausado) — `SendChatPresence` (`composing`, `paused`, mídia `audio`)
- [ ] **Assinar presença de contatos** — `SubscribePresence` (evento `Presence`)
- [ ] **Marcar como lido / recibos** — `MarkRead`, `SetForceActiveDeliveryReceipts` (evento `Receipt` — entregue/lido/tocado)
- [ ] **Arquivar, fixar, silenciar, favoritar, marcar lido** — app state: `BuildArchive`, `BuildPin`, `BuildMute`/`BuildMuteAbs`, `BuildStar`, `BuildMarkChatAsRead` via `SendAppState`
- [ ] **Apagar chat / apagar p/ mim** — `BuildDeleteChat` (+ eventos `ClearChat`, `DeleteChat`, `DeleteForMe`)
- [ ] **Labels/etiquetas** (criar/editar, associar a chat/mensagem, substituição atômica) — `BuildLabelEdit`, `BuildLabelChat`, `BuildLabelChatChanges`, `BuildLabelMessage` (eventos `LabelEdit`, `LabelAssociationChat`, `LabelAssociationMessage`)
- [ ] **Respostas rápidas** (quick replies) — `BuildQuickReply`, `BuildQuickReplyDelete` (evento `QuickReply`)
- [ ] **Push name** — `BuildSettingPushName`

## 7. Sincronização de estado e histórico

- [ ] **Sincronizar app state** (contatos, chats, configurações) — `FetchAppState`, `SendAppState`, `MarkNotDirty`
- [ ] **History sync** (histórico de conversas no pareamento) — `DownloadHistorySync`, evento `HistorySync`
- [ ] **Políticas independentes de history sync** (fork): recibo, persistência e deleção de mídia configuráveis separadamente — config do client
- [ ] **Controle de eventos de full-sync e de labels** (fork) — config do client
- [ ] **Eventos de sync** — `AppState`, `AppStateSyncComplete`, `AppStateSyncError`, `OfflineSyncPreview`, `OfflineSyncCompleted`
- [ ] **Recuperação de decriptação** (retry receipts) — automático + `SetMaxParallelRetryReceiptHandling` (eventos `UndecryptableMessage`, `UndecryptedMessage`)

## 8. Newsletters / Canais

- [ ] **Criar / deletar canal** — `CreateNewsletter`, `DeleteNewsletter` (deleção é exclusiva do fork, via MEX)
- [ ] **Seguir / deixar de seguir** — `FollowNewsletter`, `UnfollowNewsletter`
- [ ] **Info e lista de inscritos** — `GetNewsletterInfo`, `GetNewsletterInfoWithInvite`, `GetSubscribedNewsletters`
- [ ] **Mensagens e atualizações** — `GetNewsletterMessages`, `GetNewsletterMessageUpdates`, `NewsletterSubscribeLiveUpdates`
- [ ] **Reagir / marcar visualizado / silenciar** — `NewsletterSendReaction`, `NewsletterMarkViewed`, `NewsletterToggleMute`
- [ ] **Aceitar termos** — `AcceptTOSNotice`

Eventos: `NewsletterJoin`, `NewsletterLeave`, `NewsletterLiveUpdate`, `NewsletterMessageMeta`, `NewsletterMuteChange`.

## 9. WhatsApp Business (grande diferencial do fork)

### Leituras

- [ ] Perfil business de terceiros: `GetBusinessProfile`
- [ ] Contas vinculadas (Instagram/Facebook): `GetBusinessLinkedAccounts`
- [ ] Elegibilidade de recursos business: `GetBusinessEligibility`
- [ ] Catálogo e produtos: `GetCatalog`, `GetCatalogProducts`, `GetCatalogProduct`
- [ ] Coleções: `GetProductCollections`, `GetProductCollection`
- [ ] Pedidos: `GetOrderDetails`
- [ ] Compliance de comerciante: `GetBusinessMerchantCompliance`
- [ ] Resolver link de mensagem business: `ResolveBusinessMessageLink`

### Mutações

- [ ] Perfil: `UpdateBusinessProfile`, `SetBusinessCoverPhoto`, `DeleteBusinessCoverPhoto`
- [ ] Catálogo: `CreateBusinessCatalog`
- [ ] Produtos: `CreateBusinessProduct`, `UpdateBusinessProduct`, `DeleteBusinessProducts`, `UploadBusinessProductImage`, `SetBusinessProductVisibility`, `AppealBusinessProduct`
- [ ] Coleções: `CreateBusinessCollection`, `UpdateBusinessCollection`, `DeleteBusinessCollections`, `ReorderBusinessCollections`, `AppealBusinessCollection`
- [ ] Carrinho: `SetBusinessCartEnabled`
- [ ] Compliance: `SetBusinessMerchantCompliance`

### Builders de mensagens interativas (com validação de limites, UTF-8 e preservação exata de números JSON)

- [ ] `BuildBusinessProductMessage`, `BuildBusinessProductListMessage`
- [ ] `BuildBusinessOrderMessage`, `BuildBusinessAddressMessage`
- [ ] `BuildBusinessListMessage`, `BuildBusinessNativeFlowButtonsMessage`, `BuildBusinessFlowMessage` (Native Flows tipados)

## 10. Chamadas

- [ ] Rejeitar chamada: `RejectCall`
- [ ] Eventos (somente sinalização; não há mídia de voz/vídeo): `CallOffer`, `CallAccept`, `CallPreAccept`, `CallReject`, `CallTerminate`, `CallTransport`, `CallRelayLatency`, `CallOfferNotice`, `UnknownCallEvent`

## 11. Privacidade e segurança

- [ ] **Ler configurações de privacidade** — `GetPrivacySettings`, `TryFetchPrivacySettings`
- [ ] **Alterar** (visto por último, foto, status, recibos, grupos, online, call add) — `SetPrivacySetting` (evento `PrivacySettings`)
- [ ] **Timer padrão de mensagens efêmeras** — `SetDefaultDisappearingTimer`
- [ ] **Mudança de identidade de contato** (re-instalação) — evento `IdentityChange` (com limpeza de estado Signal PN+LID)
- [ ] **Códigos de segurança/verificação** — `GetIdentityVerificationCodes`
- [x] **Redação de payloads sensíveis nos logs** (tokens, cookies, perfis business) — automático (fork); a ZapperMeow reforça com a proibição de expor material de sessão em API, eventos, logs e trilha *(002, FR-043)*

## 12. Push notifications

- [ ] Config do servidor: `GetServerPushNotificationConfig`
- [ ] Registro (FCM/APNs/web): `RegisterForPushNotifications`

## 13. Eventos disponíveis (base para sistema de webhooks da API)

Total de ~75 tipos em `types/events`, agrupados. O consumo hoje é **interno** (canal WebSocket por instância); não existe entrega por webhook.

- [ ] **Mensagens:** `Message`, `FBMessage`, `Receipt`, `UndecryptableMessage`, `UndecryptedMessage`, `MediaRetry(Error)`
- [x] **Conexão:** `Connected`, `Disconnected`, `LoggedOut`, `StreamReplaced`, `TemporaryBan`, `ConnectFailure`, `ClientOutdated`, `KeepAlive*` *(002)*, `StreamError`, `ManualLoginReconnect` *(003)*
- [x] **Pareamento:** `QR`, `PairSuccess`, `PairError`, `QRScannedWithoutMultidevice` *(002)*, `PairPasskeyRequest/Confirmation/Error` *(003)*
- [ ] **Grupos:** `GroupInfo`, `JoinedGroup`
- [ ] **Contatos/perfil:** `Contact`, `LIDContact`, `PushName(Setting)`, `Picture`, `BusinessName`, `UserAbout`, `IdentityChange`, `Blocklist(Change)`
- [ ] **Estado de chat:** `Archive`, `Pin`, `Mute`, `Star`, `MarkChatAsRead`, `ClearChat`, `DeleteChat`, `DeleteForMe`, `UnarchiveChatsSetting`, `UserStatusMute`
- [ ] **Labels/quick replies (fork):** `LabelEdit`, `LabelAssociationChat`, `LabelAssociationMessage`, `QuickReply`
- [ ] **Presença:** `Presence`, `ChatPresence`
- [ ] **Sync:** `HistorySync`, `AppState`, `AppStateSyncComplete/Error`, `OfflineSync*`
- [ ] **Newsletters:** `NewsletterJoin/Leave/LiveUpdate/MessageMeta/MuteChange`, `MexNotificationData`
- [ ] **Chamadas:** `Call*`
- [x] **Outros — `CATRefreshError`** *(002)*
- [ ] **Outros — `PrivacySettings`, `NotifyAccountReachoutTimelock`**
- [ ] **Entrega por webhook** dos eventos acima (URL por instância, filtro de tipos, segredo HMAC)

## 14. Persistência (store)

- [x] **Interfaces plugáveis** em `store/`: `IdentityStore`, `SessionStore`, `PreKeyStore`, `SenderKeyStore`, `AppStateSyncKeyStore`, `AppStateStore`, `ContactStore`, `ContactUsernameStore` (+ batch), `ChatSettingsStore`, `MsgSecretStore`, `PrivacyTokenStore`, `EventBuffer`, `LIDStore`, `LIDBatchReverseStore`, `DeviceContainer` *(002 — usadas via implementação SQL pronta, sem implementação própria)*
- [x] **Implementação SQL pronta** (`store/sqlstore`) com migrações automáticas — a ZapperMeow usa **PostgreSQL via pgx**, no mesmo pool da API *(002)*
- [x] **Multi-device:** um `Container` gerencia vários `Device` — base da API multi-instância *(002)*

Otimizações PostgreSQL do fork (herdadas, nada a implementar): operações Signal/metadata em lote, índices para lookup de alias, transações vazias evitadas, caches limitados (bounded) com o banco como fonte autoritativa.

## 15. Características operacionais relevantes para a API

Contexto da biblioteca — não é backlog.

- **Multi-sessão em escala:** ~4,2 KB de heap por cliente desconectado (94,6% menos que upstream); transporte HTTP compartilhado entre clientes
- **Performance medida:** p95 de envio em grupo de 128 membros 79,3% menor; 97,9% menos chamadas SQL; 1.700 pares/s de ping-pong criptografado
- **Confiabilidade:** filas de handlers limitadas com reconexão controlada em overflow, decodificação robusta de nós binários malformados, save/delete de device serializado, erros retornados em vez de panics no Signal store
- **Observação de protocolo:** hooks de raw-node para integrações de baixo nível; `DangerousInternals` para acesso interno (usar com cautela)
- **Aviso:** projeto não afiliado ao WhatsApp; uso sujeito aos Termos de Serviço do WhatsApp

---

## Sugestão de agrupamento de recursos REST

Com base na superfície acima e no modelo de contas (plataforma → tenant → N instâncias, ver [ARCHITECTURE.md](ARCHITECTURE.md)), a API se divide em dois planos:

### Rotas de gestão (JWT)

- [x] `/auth/*` — login de super-admin e de admin de tenant, troca da própria senha, reset de senha pelo super-admin *(001)*
- [x] `/admin/tenants` — JWT de **plataforma** (super-admin): CRUD de tenants, suspensão e reativação *(001; a suspensão passou a derrubar as sessões do tenant na 002)*. Limites de uso por tenant continuam pendentes
- [x] `/instances` — JWT de **tenant**: CRUD das próprias instâncias *(001)*, pareamento por QR e por código de telefone, conectar, desconectar, deslogar, estado e trilha de conexão *(002)*, proxy por instância, modo passivo e etapa de passkey *(003)*
- [x] `/instances/{id}/keys` — JWT de tenant: emissão e revogação das API keys da instância *(001)*
- [ ] `/instances/{id}/webhooks` — JWT de tenant: configuração dos webhooks da instância (URL, filtro dos ~75 tipos de evento, segredo HMAC)

### Rotas operacionais (API key da instância)

Aninhadas sob `/instances/{id}/`, autenticadas pela API key da própria instância:

- [x] `/instances/{id}/whoami` — consulta operacional mínima que torna a API key verificável de ponta a ponta *(001)*
- [ ] `/instances/{id}/messages` — enviar (texto, mídia, localização, contato, enquete, botões/listas/flows), editar, revogar, reagir, marcar lido
- [ ] `/instances/{id}/media` — upload, download, retry
- [ ] `/instances/{id}/chats` — arquivar, fixar, silenciar, apagar, labels, presença de digitação, timer efêmero
- [ ] `/instances/{id}/groups` — CRUD, participantes, convites, comunidades, configurações
- [ ] `/instances/{id}/contacts` — verificação, perfil, foto, bloqueio, resolução LID/username
- [ ] `/instances/{id}/newsletters` — CRUD, seguir, mensagens, reações
- [ ] `/instances/{id}/business` — perfil, catálogo, produtos, coleções, carrinho, pedidos, compliance
- [ ] `/instances/{id}/privacy` — ler/alterar configurações
- [x] `/instances/{id}/ws` — WebSocket de eventos em tempo real (QR code, código de pareamento, transições de estado) *(002; eventos de mensagem ainda não trafegam por ele)*
