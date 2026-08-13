# gRPC Contract — api ↔ session-worker (002-instance-connection)

**Date**: 2026-08-13 | **Arquivo**: `proto/session/v1/session.proto` | **Gerado em**: `internal/pb/sessionv1/`

Contrato interno entre o plano stateless (`api`) e o plano stateful (`session-worker`), exigido em
Protobuf pelo Princípio IV. Trafega **apenas na rede privada** (overlay no Swarm, bridge no
Compose); a api disca o `grpc_addr` registrado no lease, nunca um VIP com balanceamento — o destino
precisa ser o dono exato daquela sessão.

## Regra de fencing (não negociável)

Toda RPC carrega `instance_id` **e** `generation`. O worker compara com a geração do lease que
detém:

- geração igual → executa;
- geração diferente ou lease não detido → `FAILED_PRECONDITION` com detail `WRONG_GENERATION`,
  **sem** tocar na sessão.

A api, ao receber `WRONG_GENERATION` ou falha de conexão, invalida o cache `wa:lease:{id}`, relê o
lease e tenta **uma única vez** no novo endereço antes de devolver erro (FR-025, R6).

## Serviço

```protobuf
syntax = "proto3";

package zappermeow.session.v1;
option go_package = "github.com/polymorfa/zappermeow/internal/pb/sessionv1;sessionv1";

import "google/protobuf/timestamp.proto";

service SessionService {
  // Liga a sessão: pareia quando não há device salvo, reconecta quando há.
  rpc Connect(ConnectRequest) returns (ConnectResponse);

  // Inicia pareamento por código de telefone e devolve o código a digitar no aparelho.
  rpc PairPhone(PairPhoneRequest) returns (PairPhoneResponse);

  // Coloca offline preservando o material da sessão.
  rpc Disconnect(DisconnectRequest) returns (DisconnectResponse);

  // Encerra a sessão no WhatsApp, remove o dispositivo e apaga o material local.
  rpc Logout(LogoutRequest) returns (LogoutResponse);

  // Estado em memória do dono (complementa o estado persistido; usado em diagnóstico).
  rpc GetStatus(GetStatusRequest) returns (GetStatusResponse);
}

message Fence {
  string instance_id = 1;  // UUID
  int64  generation  = 2;  // geração do lease conhecida pela api
}

enum SessionState {
  SESSION_STATE_UNSPECIFIED = 0;
  SESSION_STATE_REGISTERED  = 1;  // registrada
  SESSION_STATE_PAIRING     = 2;  // pareando
  SESSION_STATE_CONNECTING  = 3;  // conectando
  SESSION_STATE_CONNECTED   = 4;  // conectada
  SESSION_STATE_DISCONNECTED = 5; // desconectada
  SESSION_STATE_LOGGED_OUT  = 6;  // deslogada
  SESSION_STATE_BANNED      = 7;  // banida
}

enum PairingMethod {
  PAIRING_METHOD_UNSPECIFIED = 0;
  PAIRING_METHOD_QR          = 1;
  PAIRING_METHOD_PHONE       = 2;
}

message DeviceIdentity {
  string jid           = 1;  // JID completo, com sufixo de dispositivo
  string lid           = 2;
  string phone_number  = 3;
  string push_name     = 4;
  string platform      = 5;
  string business_name = 6;
}

message ConnectRequest {
  Fence fence = 1;
}

message ConnectResponse {
  SessionState state = 1;
  // Preenchido quando o comando iniciou uma tentativa de pareamento por QR.
  google.protobuf.Timestamp pairing_expires_at = 2;
  bool pairing_started = 3;
}

message PairPhoneRequest {
  Fence  fence          = 1;
  string phone_number   = 2;  // E.164 sem '+'
  bool   replace_active = 3;  // encerra tentativa em curso (default do chamador: true)
}

message PairPhoneResponse {
  string pairing_code = 1;  // 8 caracteres
  google.protobuf.Timestamp expires_at = 2;
}

message DisconnectRequest {
  Fence fence = 1;
}

message DisconnectResponse {
  SessionState state = 1;
}

message LogoutRequest {
  Fence fence = 1;
  // Quando a sessão está offline, autoriza conectar temporariamente para
  // remover o dispositivo no servidor antes de apagar o material local.
  bool allow_temporary_connect = 2;
}

message LogoutResponse {
  SessionState state = 1;
  // true = dispositivo removido no servidor; false = só o material local foi apagado (R10).
  bool remote_removed = 2;
}

message GetStatusRequest {
  Fence fence = 1;
}

message GetStatusResponse {
  SessionState   state          = 1;
  DeviceIdentity device         = 2;
  bool           connected      = 3;
  bool           logged_in      = 4;
  PairingMethod  pairing_method = 5;
  google.protobuf.Timestamp connected_at = 6;
}
```

## Mapeamento de erros

| Situação no worker | Código gRPC | Detail | Tradução na api |
| --- | --- | --- | --- |
| Geração diferente / lease perdido | `FAILED_PRECONDITION` | `WRONG_GENERATION` | Relê lease, tenta 1×; persistindo → `503 SESSION_UNAVAILABLE` |
| Instância sem device salvo em operação que exige sessão | `FAILED_PRECONDITION` | `NOT_PAIRED` | `409 INSTANCE_NOT_PAIRED` |
| Número inválido | `INVALID_ARGUMENT` | `INVALID_PHONE_NUMBER` | `422 INVALID_PHONE_NUMBER` |
| Tentativa de pareamento já ativa e `replace_active = false` | `ABORTED` | `PAIRING_IN_PROGRESS` | `409 PAIRING_IN_PROGRESS` |
| Worker em draining | `UNAVAILABLE` | `DRAINING` | Relê lease, tenta 1× no novo dono |
| Falha do WhatsApp no comando | `UNAVAILABLE` | `UPSTREAM_FAILURE` | `502` com `code: WHATSAPP_UNAVAILABLE` |

## O que **não** é gRPC

- **Eventos worker → api** (QR, transições): Redis pub/sub `events:{instance_id}` — serve todas as
  réplicas da api de uma vez; um stream gRPC amarraria o evento à réplica que originou o comando.
- **Acordar workers para um lease livre**: Redis pub/sub `sessions:claim`, porque nesse momento
  ainda não existe dono a quem discar.
- **Parar sessões de tenant suspenso**: `sessions:stop` — broadcast, não comando ponto a ponto.

## Evolução

Mudança incompatível exige nova versão de pacote (`session/v2`) e coordenação explícita de deploy
(Princípio IV). Campos novos opcionais e RPCs novas são compatíveis e não exigem versionamento;
remover ou renumerar campo, não.
