# gRPC Contract — Complementos de Conexão (003-connection-extras)

**Date**: 2026-08-14 | **Proto**: `proto/session/v1/session.proto` | **Base**: [002 session-grpc.md](../../002-instance-connection/contracts/session-grpc.md)

Extensão **aditiva** do `SessionService` da 002 — mesmo pacote `zappermeow.session.v1`, mesma
regra de fencing (todo request carrega `Fence{instance_id, generation}`; generation velha →
`FAILED_PRECONDITION`), mesmo transporte (rede privada, discagem no `grpc_addr` do lease).
Nenhuma RPC ou mensagem existente muda: compatibilidade total com binários da 002 durante rolling
update.

## RPCs novas

```proto
service SessionService {
  // ... RPCs da 002 inalteradas ...

  // Reaplica as configurações persistidas da instância na sessão viva.
  // O worker relê proxy_url/passive_mode do Postgres — o request sinaliza o
  // que mudou, nunca carrega os valores (research R2).
  rpc ApplySettings(ApplySettingsRequest) returns (ApplySettingsResponse);

  // Encaminha a resposta do autenticador WebAuthn para o desafio de passkey
  // pendente da tentativa de pareamento em curso.
  rpc SubmitPasskeyResponse(SubmitPasskeyResponseRequest) returns (SubmitPasskeyResponseResponse);

  // Confirma o código de conferência exibido ao dono do número.
  rpc ConfirmPasskey(ConfirmPasskeyRequest) returns (ConfirmPasskeyResponse);

  // Gera os códigos de verificação de identidade da conversa com um contato.
  rpc GetIdentityVerificationCodes(GetIdentityVerificationCodesRequest)
      returns (GetIdentityVerificationCodesResponse);
}

message ApplySettingsRequest {
  Fence fence = 1;
  bool proxy_changed = 2;    // true => religar a sessão com o proxy relido do banco
  bool passive_changed = 3;  // true => aplicar SetPassive na sessão conectada
}

message ApplySettingsResponse {
  SessionState state = 1;
  bool reconnecting = 2;     // proxy_changed com sessão ativa => true
  bool passive_applied = 3;  // passive_changed com sessão conectada => true
}

message SubmitPasskeyResponseRequest {
  Fence fence = 1;
  // JSON da asserção WebAuthn, opaco para a plataforma; a fronteira wa
  // converte para o tipo da biblioteca.
  bytes webauthn_response_json = 2;
}

message SubmitPasskeyResponseResponse {
  // Estado da tentativa após o envio; a continuação (código ou confirmação
  // automática) chega pelo fluxo de eventos.
  SessionState state = 1;
}

message ConfirmPasskeyRequest {
  Fence fence = 1;
}

message ConfirmPasskeyResponse {
  SessionState state = 1;
}

message GetIdentityVerificationCodesRequest {
  Fence fence = 1;
  // LID já resolvido pela API (a resolução telefone->LID acontece no worker,
  // que tem o store; a API manda o contato como recebido).
  string contact = 2;  // "<user>@lid" ou telefone E.164 sem '+'
}

message GetIdentityVerificationCodesResponse {
  string lid = 1;
  string phone_number = 2;  // vazio quando desconhecido
  string username = 3;      // vazio quando desconhecido
  string numeric_code = 4;  // 60 dígitos
  bytes display_qr = 5;
  bytes verification_qr = 6;
}
```

## Semântica por RPC

| RPC | Pré-condições no worker | Erros gRPC |
| --- | --- | --- |
| `ApplySettings` | fence válido; sessão adotada pelo worker | `FAILED_PRECONDITION` (fence), `NOT_FOUND` (sessão não adotada) |
| `SubmitPasskeyResponse` | tentativa de pareamento em curso **com** desafio pendente não respondido | `FAILED_PRECONDITION` (fence ou sem desafio → detalhe `no_passkey_challenge`), `INTERNAL` (falha da biblioteca — vira `pairing.failed` no WS) |
| `ConfirmPasskey` | código de conferência pendente (resposta aceita, `SkipHandoffUX=false`, ainda não confirmado) | `FAILED_PRECONDITION` (detalhe `no_passkey_code` — cobre antes-do-código, já-consumido e confirmação automática já feita; a biblioteca não é reentrante aqui, research R7) |
| `GetIdentityVerificationCodes` | sessão `connected`; contato resolve para LID ≠ o próprio | `FAILED_PRECONDITION` (`not_connected`), `INVALID_ARGUMENT` (`invalid_contact`, `cannot_verify_self`), `NOT_FOUND` (`identity_not_resolvable`, `contact_unavailable`), `DEADLINE_EXCEEDED` (IQs à rede do WhatsApp — deadline do cliente: 5s, SC-006) |

O mapeamento gRPC → HTTP (RFC 9457 + `code`) segue o padrão da 002, centralizado no
`sessionclient` + `httperr`; os detalhes entre parênteses viajam em `google.rpc.ErrorInfo.reason`
e viram os códigos HTTP novos do [http-api.md](./http-api.md).

## Fluxos

**Mudança de proxy a quente** (research R2):

```
API: valida → UPDATE instances.proxy_url → lease running?
  ├── sim → ApplySettings{proxy_changed} ao dono
  │         worker: encerra tentativa de pareamento (se houver, expiry=cancelled)
  │                 → para sessão → reconstrói cliente (proxy relido) → Connect
  │                 → eventos disconnected(proxy_updated) ... connected no WS
  └── não → nada; próxima conexão lê a configuração
```

Se o RPC falhar (dono morreu entre a leitura do lease e a chamada), a API **não** falha a
requisição: a configuração está persistida e o failover/reconciliação religa com ela — a resposta
volta `reconnecting: false`. Mesma filosofia para `passive_changed`.

**Etapa de passkey** (research R7):

```
worker (canal de QR): passkey-request → publica pairing.passkey_challenge (+ snapshot fase)
API: POST .../passkey/response → SubmitPasskeyResponse → biblioteca envia prólogo
worker: passkey-confirmation (SkipHandoffUX=false) → publica pairing.passkey_code (+ snapshot fase)
        (SkipHandoffUX=true → biblioteca confirma sozinha; nenhum frame de código)
API: POST .../passkey/confirm → ConfirmPasskey → biblioteca envia pairing request cifrado
worker: pair-success → pairing.succeeded → ... → connection.connected
qualquer falha → item error do canal → pairing.failed{failure: passkey_error}
```

## Evolução

Campos e RPCs novos são aditivos; números de campo reservados nunca são reutilizados. A regra da
002 permanece: mudanças incompatíveis neste contrato exigem coordenação explícita de deploy
api ↔ workers (Princípio IV).
