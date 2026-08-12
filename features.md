# ZapperMeow — Levantamento de Funcionalidades do HyperMeow

Levantamento das funcionalidades disponíveis na biblioteca [polymorfa/hypermeow](https://github.com/polymorfa/hypermeow) (fork production-focused do `tulir/whatsmeow`, escrito em Go) para servir de base à construção de uma API RESTful completa.

- **Import:** `import whatsmeow "github.com/polymorfa/hypermeow"` (o pacote raiz mantém o nome `whatsmeow`)
- **Instalação:** `go get github.com/polymorfa/hypermeow@main` (versões semânticas foram retraídas; usar sempre `@main` e pinar a pseudo-versão no `go.mod`)
- **Licença:** MPL-2.0 (código upstream) + MIT (código Polymorfa)
- **Levantado em:** 2026-08-12, commit mais recente de 2026-08-11

---

## 1. Sessão, autenticação e conexão

Base para endpoints de gerenciamento de instâncias/sessões (multi-sessão é o caso de uso alvo do fork).

| Funcionalidade | API da biblioteca |
| --- | --- |
| Criar cliente a partir de um device store | `NewClient` |
| Conectar / conectar com contexto | `Connect`, `ConnectContext`, `WaitForConnection` |
| Login via QR Code (stream de códigos p/ exibir no front) | `GetQRChannel` (evento `QR`) |
| Pareamento por código de telefone (sem QR) | `PairPhone` |
| Pareamento por passkey | `SendPasskeyResponse`, `SendPasskeyConfirmation` (eventos `PairPasskeyRequest/Confirmation/Error`) |
| Status de sessão | `IsConnected`, `IsLoggedIn` |
| Desconectar / reconectar / logout | `Disconnect`, `ResetConnection`, `Logout` |
| Modo passivo (receber sem marcar online) | `SetPassive` |
| Proxy e HTTP clients customizados | `SetProxy`, `SetProxyAddress`, `SetSOCKSProxy`, `SetMediaHTTPClient`, `SetWebsocketHTTPClient`, `SetPreLoginHTTPClient` |
| Handlers de eventos (base p/ webhooks) | `AddEventHandler`, `AddEventHandlerWithSuccessStatus`, `RemoveEventHandler(s)` |
| Códigos de verificação de identidade (LID) | `GetIdentityVerificationCodes` |

Eventos de ciclo de vida: `Connected`, `Disconnected`, `LoggedOut`, `StreamError`, `StreamReplaced`, `TemporaryBan`, `ClientOutdated`, `ConnectFailure`, `KeepAliveTimeout`, `KeepAliveRestored`, `ManualLoginReconnect`, `PairSuccess`, `PairError`, `QRScannedWithoutMultidevice`.

## 2. Envio de mensagens

| Funcionalidade | API da biblioteca |
| --- | --- |
| Enviar mensagem (DM, grupo, status/broadcast) | `SendMessage` (+ `SendRequestExtra`: ID custom, timeout, media handle, bot inline, peer message) |
| Tipos de conteúdo (proto `waE2E.Message`) | texto/extended text, imagem, vídeo, áudio/PTT, documento, sticker (+ packs via `FetchStickerPack`), localização (+ live location), contato (vCard), botões, listas, templates, view-once |
| Gerar ID de mensagem | `GenerateMessageID`, `GenerateFacebookMessageID` |
| Editar mensagem | `BuildEdit` + `SendMessage` |
| Revogar/apagar p/ todos | `RevokeMessage`, `BuildRevoke`, `BuildMessageKey` |
| Reações | `BuildReaction`; criptografia: `EncryptReaction`, `DecryptReaction` |
| Enquetes (polls) | `BuildPollCreation`, `BuildPollVote`, `EncryptPollVote`, `DecryptPollVote`, `HashPollOptions` |
| Comentários (channels) | `EncryptComment`, `DecryptComment` |
| Mensagens efêmeras (timer de desaparecimento) | `SetDisappearingTimer`, `SetDefaultDisappearingTimer`, `ParseDisappearingTimerString` |
| Mensagens de consentimento de número | `BuildRequestPhoneNumberMessage`, `BuildSharePhoneNumberMessage` |
| Mensagens FB/Messenger (armadillo) | `SendFBMessage` |
| Solicitar mensagem indisponível / history sync sob demanda | `BuildUnavailableMessageRequest`, `BuildHistorySyncRequest` |
| Parse de mensagens da web | `ParseWebMessage` |

## 3. Mídia (upload/download)

| Funcionalidade | API da biblioteca |
| --- | --- |
| Upload criptografado (imagem, vídeo, áudio, doc, sticker) | `Upload`, `UploadReader` |
| Upload para newsletters/canais (plaintext) | `UploadNewsletter`, `UploadNewsletterReader` |
| Download de qualquer mídia de mensagem | `Download`, `DownloadAny`, `DownloadThumbnail`, `DownloadFB` |
| Download direto para arquivo (streaming, memória constante) | `DownloadToFile`, `DownloadFBToFile`, `DownloadMediaWithPathToFile`, `DownloadMediaWithOnlyPathToFile` |
| Download por path direto | `DownloadMediaWithPath`, `DownloadMediaWithOnlyPath` |
| Deletar mídia do servidor | `DeleteMedia` |
| Retry de mídia expirada | `SendMediaRetryReceipt`, `DecryptMediaRetryNotification` (eventos `MediaRetry`, `MediaRetryError`) |

## 4. Grupos

| Funcionalidade | API da biblioteca |
| --- | --- |
| Criar grupo | `CreateGroup` |
| Info do grupo / grupos que participo | `GetGroupInfo`, `GetJoinedGroups` |
| Comunidades (subgrupos, vincular/desvincular) | `GetSubGroups`, `GetLinkedGroupsParticipants`, `LinkGroup`, `UnlinkGroup` |
| Sair do grupo | `LeaveGroup` |
| Participantes (adicionar, remover, promover, rebaixar) | `UpdateGroupParticipants` |
| Solicitações de entrada (aprovar/rejeitar) | `GetGroupRequestParticipants`, `UpdateGroupRequestParticipants`, `SetGroupJoinApprovalMode` |
| Modo de adição de membros | `SetGroupMemberAddMode` |
| Foto, nome, tópico/descrição | `SetGroupPhoto`, `SetGroupName`, `SetGroupTopic`, `SetGroupDescription` |
| Travar edição de info / modo somente-admins (announce) | `SetGroupLocked`, `SetGroupAnnounce` |
| Link de convite (obter/resetar, entrar, preview) | `GetGroupInviteLink`, `JoinGroupWithLink`, `GetGroupInfoFromLink`, `JoinGroupWithInvite`, `GetGroupInfoFromInvite` |

Eventos: `GroupInfo`, `JoinedGroup` (+ mismatch de hash de participantes exposto como erro — hardening do fork).

## 5. Contatos e usuários

| Funcionalidade | API da biblioteca |
| --- | --- |
| Verificar se números estão no WhatsApp | `IsOnWhatsApp` |
| Info de usuários (status, devices, business) | `GetUserInfo`, `GetUserDevices`, `GetUserDevicesContext` |
| Foto de perfil (usuário/grupo, preview ou full) | `GetProfilePictureInfo` |
| Status/recado próprio | `SetStatusMessage` |
| Bloqueio (listar, bloquear, desbloquear) | `GetBlocklist`, `UpdateBlocklist` (eventos `Blocklist`, `BlocklistChange`) |
| QR link de contato | `GetContactQRLink`, `ResolveContactQRLink` |
| Resolver username / LID | `ResolveUsername`, `ResolveLID`, `StoreLIDPNMapping` |
| Bots (Meta AI etc.) | `GetBotListV2`, `GetBotProfiles` |
| Store de contatos (nomes, push names, usernames) | `ContactStore`, `ContactUsernameStore` (+ leitura em lote) |

**Modelo de identidade LID-first (diferencial do fork):** LIDs são a identidade Signal estável; números de telefone e usernames são aliases persistidos, com resolução em lote (`LIDBatchReverseStore`) e cache negativo. Relevante para o design de rotas: aceitar/retornar JID de telefone, LID e username.

## 6. Presença, recibos e estado de chat

| Funcionalidade | API da biblioteca |
| --- | --- |
| Presença global (online/offline) | `SendPresence` |
| Presença em chat (digitando/gravando/pausado) | `SendChatPresence` (`composing`, `paused`, mídia `audio`) |
| Assinar presença de contatos | `SubscribePresence` (evento `Presence`) |
| Marcar como lido / recibos | `MarkRead`, `SetForceActiveDeliveryReceipts` (evento `Receipt` — entregue/lido/tocado) |
| Arquivar, fixar, silenciar, favoritar, marcar lido | App state: `BuildArchive`, `BuildPin`, `BuildMute`/`BuildMuteAbs`, `BuildStar`, `BuildMarkChatAsRead` via `SendAppState` |
| Apagar chat / apagar p/ mim | `BuildDeleteChat` (+ eventos `ClearChat`, `DeleteChat`, `DeleteForMe`) |
| Labels/etiquetas (criar/editar, associar a chat/mensagem, substituição atômica) | `BuildLabelEdit`, `BuildLabelChat`, `BuildLabelChatChanges`, `BuildLabelMessage` (eventos `LabelEdit`, `LabelAssociationChat`, `LabelAssociationMessage`) |
| Respostas rápidas (quick replies) | `BuildQuickReply`, `BuildQuickReplyDelete` (evento `QuickReply`) |
| Push name | `BuildSettingPushName` |

## 7. Sincronização de estado e histórico

| Funcionalidade | API da biblioteca |
| --- | --- |
| Sincronizar app state (contatos, chats, configurações) | `FetchAppState`, `SendAppState`, `MarkNotDirty` |
| History sync (histórico de conversas no pareamento) | `DownloadHistorySync`, evento `HistorySync` |
| Políticas independentes de history sync (fork): recibo, persistência e deleção de mídia configuráveis separadamente | Config do client |
| Controle de eventos de full-sync e de labels (fork) | Config do client |
| Eventos de sync | `AppState`, `AppStateSyncComplete`, `AppStateSyncError`, `OfflineSyncPreview`, `OfflineSyncCompleted` |
| Recuperação de decriptação (retry receipts) | automático + `SetMaxParallelRetryReceiptHandling` (eventos `UndecryptableMessage`, `UndecryptedMessage`) |

## 8. Newsletters / Canais

| Funcionalidade | API da biblioteca |
| --- | --- |
| Criar / deletar canal | `CreateNewsletter`, `DeleteNewsletter` (deleção é exclusiva do fork, via MEX) |
| Seguir / deixar de seguir | `FollowNewsletter`, `UnfollowNewsletter` |
| Info e lista de inscritos | `GetNewsletterInfo`, `GetNewsletterInfoWithInvite`, `GetSubscribedNewsletters` |
| Mensagens e atualizações | `GetNewsletterMessages`, `GetNewsletterMessageUpdates`, `NewsletterSubscribeLiveUpdates` |
| Reagir / marcar visualizado / silenciar | `NewsletterSendReaction`, `NewsletterMarkViewed`, `NewsletterToggleMute` |
| Aceitar termos | `AcceptTOSNotice` |

Eventos: `NewsletterJoin`, `NewsletterLeave`, `NewsletterLiveUpdate`, `NewsletterMessageMeta`, `NewsletterMuteChange`.

## 9. WhatsApp Business (grande diferencial do fork)

### Leituras
- Perfil business de terceiros: `GetBusinessProfile`
- Contas vinculadas (Instagram/Facebook): `GetBusinessLinkedAccounts`
- Elegibilidade de recursos business: `GetBusinessEligibility`
- Catálogo e produtos: `GetCatalog`, `GetCatalogProducts`, `GetCatalogProduct`
- Coleções: `GetProductCollections`, `GetProductCollection`
- Pedidos: `GetOrderDetails`
- Compliance de comerciante: `GetBusinessMerchantCompliance`
- Resolver link de mensagem business: `ResolveBusinessMessageLink`

### Mutações
- Perfil: `UpdateBusinessProfile`, `SetBusinessCoverPhoto`, `DeleteBusinessCoverPhoto`
- Catálogo: `CreateBusinessCatalog`
- Produtos: `CreateBusinessProduct`, `UpdateBusinessProduct`, `DeleteBusinessProducts`, `UploadBusinessProductImage`, `SetBusinessProductVisibility`, `AppealBusinessProduct`
- Coleções: `CreateBusinessCollection`, `UpdateBusinessCollection`, `DeleteBusinessCollections`, `ReorderBusinessCollections`, `AppealBusinessCollection`
- Carrinho: `SetBusinessCartEnabled`
- Compliance: `SetBusinessMerchantCompliance`

### Builders de mensagens interativas (com validação de limites, UTF-8 e preservação exata de números JSON)
- `BuildBusinessProductMessage`, `BuildBusinessProductListMessage`
- `BuildBusinessOrderMessage`, `BuildBusinessAddressMessage`
- `BuildBusinessListMessage`, `BuildBusinessNativeFlowButtonsMessage`, `BuildBusinessFlowMessage` (Native Flows tipados)

## 10. Chamadas

- Rejeitar chamada: `RejectCall`
- Eventos (somente sinalização; não há mídia de voz/vídeo): `CallOffer`, `CallAccept`, `CallPreAccept`, `CallReject`, `CallTerminate`, `CallTransport`, `CallRelayLatency`, `CallOfferNotice`, `UnknownCallEvent`

## 11. Privacidade e segurança

| Funcionalidade | API da biblioteca |
| --- | --- |
| Ler configurações de privacidade | `GetPrivacySettings`, `TryFetchPrivacySettings` |
| Alterar (visto por último, foto, status, recibos, grupos, online, call add) | `SetPrivacySetting` (evento `PrivacySettings`) |
| Timer padrão de mensagens efêmeras | `SetDefaultDisappearingTimer` |
| Mudança de identidade de contato (re-instalação) | evento `IdentityChange` (com limpeza de estado Signal PN+LID) |
| Códigos de segurança/verificação | `GetIdentityVerificationCodes` |
| Redação de payloads sensíveis nos logs (tokens, cookies, perfis business) | automático (fork) |

## 12. Push notifications

- Config do servidor: `GetServerPushNotificationConfig`
- Registro (FCM/APNs/web): `RegisterForPushNotifications`

## 13. Eventos disponíveis (base para sistema de webhooks da API)

Total de ~75 tipos em `types/events`, agrupados:

- **Mensagens:** `Message`, `FBMessage`, `Receipt`, `UndecryptableMessage`, `UndecryptedMessage`, `MediaRetry(Error)`
- **Conexão:** `Connected`, `Disconnected`, `LoggedOut`, `StreamError`, `StreamReplaced`, `TemporaryBan`, `ConnectFailure`, `ClientOutdated`, `KeepAlive*`
- **Pareamento:** `QR`, `PairSuccess`, `PairError`, `PairPasskey*`, `QRScannedWithoutMultidevice`
- **Grupos:** `GroupInfo`, `JoinedGroup`
- **Contatos/perfil:** `Contact`, `LIDContact`, `PushName(Setting)`, `Picture`, `BusinessName`, `UserAbout`, `IdentityChange`, `Blocklist(Change)`
- **Estado de chat:** `Archive`, `Pin`, `Mute`, `Star`, `MarkChatAsRead`, `ClearChat`, `DeleteChat`, `DeleteForMe`, `UnarchiveChatsSetting`, `UserStatusMute`
- **Labels/quick replies (fork):** `LabelEdit`, `LabelAssociationChat`, `LabelAssociationMessage`, `QuickReply`
- **Presença:** `Presence`, `ChatPresence`
- **Sync:** `HistorySync`, `AppState`, `AppStateSyncComplete/Error`, `OfflineSync*`
- **Newsletters:** `NewsletterJoin/Leave/LiveUpdate/MessageMeta/MuteChange`, `MexNotificationData`
- **Chamadas:** `Call*`
- **Outros:** `PrivacySettings`, `NotifyAccountReachoutTimelock`, `CATRefreshError`

## 14. Persistência (store)

- Interfaces plugáveis em `store/`: `IdentityStore`, `SessionStore`, `PreKeyStore`, `SenderKeyStore`, `AppStateSyncKeyStore`, `AppStateStore`, `ContactStore`, `ContactUsernameStore` (+ batch), `ChatSettingsStore`, `MsgSecretStore`, `PrivacyTokenStore`, `EventBuffer`, `LIDStore`, `LIDBatchReverseStore`, `DeviceContainer`
- Implementação SQL pronta (`store/sqlstore`) com dialetos: **PostgreSQL (pgx e lib/pq)** e **SQLite3**, com migrações automáticas
- Otimizações PostgreSQL do fork: operações Signal/metadata em lote, índices para lookup de alias, transações vazias evitadas, caches limitados (bounded) com o banco como fonte autoritativa
- Multi-device: um `Container` gerencia vários `Device` (base natural para API multi-instância)

## 15. Características operacionais relevantes para a API

- **Multi-sessão em escala:** ~4,2 KB de heap por cliente desconectado (94,6% menos que upstream); transporte HTTP compartilhado entre clientes
- **Performance medida:** p95 de envio em grupo de 128 membros 79,3% menor; 97,9% menos chamadas SQL; 1.700 pares/s de ping-pong criptografado
- **Confiabilidade:** filas de handlers limitadas com reconexão controlada em overflow, decodificação robusta de nós binários malformados, save/delete de device serializado, erros retornados em vez de panics no Signal store
- **Observação de protocolo:** hooks de raw-node para integrações de baixo nível; `DangerousInternals` para acesso interno (usar com cautela)
- **Aviso:** projeto não afiliado ao WhatsApp; uso sujeito aos Termos de Serviço do WhatsApp

---

## Sugestão de agrupamento de recursos REST

Com base na superfície acima e no modelo de contas (plataforma → tenant → N instâncias, ver [ARCHITECTURE.md](ARCHITECTURE.md)), a API se divide em dois planos:

### Rotas de gestão (JWT)

- `/admin/tenants` — JWT de **plataforma** (super-admin): CRUD de tenants e seus limites de uso.
- `/instances` — JWT de **tenant**: CRUD das próprias instâncias (números WhatsApp), pareamento (QR code, pair-by-phone), status, logout, proxy.
- `/instances/{id}/keys` — JWT de tenant: emissão e revogação das API keys da instância.
- `/instances/{id}/webhooks` — JWT de tenant: configuração dos webhooks da instância (URL, filtro dos ~75 tipos de evento, segredo HMAC).

### Rotas operacionais (API key da instância)

Aninhadas sob `/instances/{id}/`, autenticadas pela API key da própria instância:

1. `/instances/{id}/messages` — enviar (texto, mídia, localização, contato, enquete, botões/listas/flows), editar, revogar, reagir, marcar lido
2. `/instances/{id}/media` — upload, download, retry
3. `/instances/{id}/chats` — arquivar, fixar, silenciar, apagar, labels, presença de digitação, timer efêmero
4. `/instances/{id}/groups` — CRUD, participantes, convites, comunidades, configurações
5. `/instances/{id}/contacts` — verificação, perfil, foto, bloqueio, resolução LID/username
6. `/instances/{id}/newsletters` — CRUD, seguir, mensagens, reações
7. `/instances/{id}/business` — perfil, catálogo, produtos, coleções, carrinho, pedidos, compliance
8. `/instances/{id}/privacy` — ler/alterar configurações
9. `/instances/{id}/ws` — WebSocket de eventos em tempo real (QR code, mensagens, status de conexão)
