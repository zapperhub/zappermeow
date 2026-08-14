// Package worker is the stateful plane: the process that owns WhatsApp
// sessions, serves the private gRPC endpoint and keeps the leases alive.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/lease"
	sessionv1 "github.com/zapperhub/zappermeow/internal/pb/sessionv1"
	"github.com/zapperhub/zappermeow/internal/wa"
)

// Detail strings the API maps back onto HTTP problem codes. They are part of
// the internal contract, so changing one is a coordinated deploy.
const (
	DetailWrongGeneration = "WRONG_GENERATION"
	DetailNotPaired       = "NOT_PAIRED"
	DetailAlreadyPaired   = "ALREADY_PAIRED"
	DetailInvalidPhone    = "INVALID_PHONE_NUMBER"
	DetailPairingRunning  = "PAIRING_IN_PROGRESS"
	DetailDraining        = "DRAINING"
	DetailUpstreamFailure = "UPSTREAM_FAILURE"
	DetailUnknownInstance = "UNKNOWN_INSTANCE"

	// Connection extras (feature 003).
	DetailNoPasskeyChallenge    = "NO_PASSKEY_CHALLENGE"
	DetailNoPasskeyCode         = "NO_PASSKEY_CODE"
	DetailNotConnected          = "INSTANCE_NOT_CONNECTED"
	DetailInvalidContact        = "INVALID_CONTACT"
	DetailIdentityNotResolvable = "IDENTITY_NOT_RESOLVABLE"
	DetailCannotVerifySelf      = "CANNOT_VERIFY_SELF"
	DetailContactUnavailable    = "CONTACT_UNAVAILABLE"
)

// GRPCServer exposes SessionService over the private network.
type GRPCServer struct {
	sessionv1.UnimplementedSessionServiceServer

	supervisor *Supervisor
	leases     *lease.Manager
	logger     *slog.Logger
}

// NewGRPCServer builds the server.
func NewGRPCServer(supervisor *Supervisor, leases *lease.Manager, logger *slog.Logger) *GRPCServer {
	return &GRPCServer{supervisor: supervisor, leases: leases, logger: logger}
}

// fence validates the caller's view of ownership before any command runs.
//
// This is the fencing check of Principle III: a worker that lost its lease — to
// a GC pause, a partition, a slow disk — must not touch the session, even if it
// still believes it owns it. Everything else in this file assumes it passed.
func (s *GRPCServer) fence(ctx context.Context, f *sessionv1.Fence) (domain.ID, error) {
	if f == nil {
		return domain.ID{}, status.Error(codes.InvalidArgument, "fence is required")
	}
	instanceID, err := domain.ParseID("instance_id", f.GetInstanceId())
	if err != nil {
		return domain.ID{}, status.Error(codes.InvalidArgument, "instance_id is not a valid UUID")
	}

	if s.supervisor.Draining() {
		return domain.ID{}, status.Error(codes.Unavailable, DetailDraining)
	}

	switch err := s.leases.CheckGeneration(ctx, instanceID, f.GetGeneration()); {
	case err == nil:
		return instanceID, nil
	case errors.Is(err, lease.ErrWrongGeneration), errors.Is(err, lease.ErrNotAcquired):
		// Not an error worth logging loudly: it is the expected outcome of a
		// failover, and the API retries against the new owner.
		return domain.ID{}, status.Error(codes.FailedPrecondition, DetailWrongGeneration)
	default:
		s.logger.Error("fencing check failed",
			slog.String("instance_id", instanceID.String()),
			slog.String("error", err.Error()))
		return domain.ID{}, status.Error(codes.Internal, "fencing check failed")
	}
}

func (s *GRPCServer) Connect(ctx context.Context, req *sessionv1.ConnectRequest) (*sessionv1.ConnectResponse, error) {
	instanceID, err := s.fence(ctx, req.GetFence())
	if err != nil {
		return nil, err
	}

	result, err := s.supervisor.Connect(ctx, instanceID)
	if err != nil {
		return nil, translate(err)
	}

	resp := &sessionv1.ConnectResponse{
		State:          stateToProto(result.State),
		PairingStarted: result.PairingStarted,
	}
	if !result.PairingExpiresAt.IsZero() {
		resp.PairingExpiresAt = timestamppb.New(result.PairingExpiresAt)
	}
	return resp, nil
}

func (s *GRPCServer) PairPhone(ctx context.Context, req *sessionv1.PairPhoneRequest) (*sessionv1.PairPhoneResponse, error) {
	instanceID, err := s.fence(ctx, req.GetFence())
	if err != nil {
		return nil, err
	}

	code, expiresAt, err := s.supervisor.PairPhone(ctx, instanceID, req.GetPhoneNumber(), req.GetReplaceActive())
	if err != nil {
		return nil, translate(err)
	}

	return &sessionv1.PairPhoneResponse{
		PairingCode: code,
		ExpiresAt:   timestamppb.New(expiresAt),
	}, nil
}

func (s *GRPCServer) Disconnect(ctx context.Context, req *sessionv1.DisconnectRequest) (*sessionv1.DisconnectResponse, error) {
	instanceID, err := s.fence(ctx, req.GetFence())
	if err != nil {
		return nil, err
	}

	state, err := s.supervisor.Disconnect(ctx, instanceID)
	if err != nil {
		return nil, translate(err)
	}
	return &sessionv1.DisconnectResponse{State: stateToProto(state)}, nil
}

func (s *GRPCServer) Logout(ctx context.Context, req *sessionv1.LogoutRequest) (*sessionv1.LogoutResponse, error) {
	instanceID, err := s.fence(ctx, req.GetFence())
	if err != nil {
		return nil, err
	}

	remoteRemoved, err := s.supervisor.Logout(ctx, instanceID, req.GetAllowTemporaryConnect())
	if err != nil {
		return nil, translate(err)
	}

	return &sessionv1.LogoutResponse{
		State:         sessionv1.SessionState_SESSION_STATE_REGISTERED,
		RemoteRemoved: remoteRemoved,
	}, nil
}

func (s *GRPCServer) GetStatus(ctx context.Context, req *sessionv1.GetStatusRequest) (*sessionv1.GetStatusResponse, error) {
	instanceID, err := s.fence(ctx, req.GetFence())
	if err != nil {
		return nil, err
	}

	snapshot, err := s.supervisor.Status(ctx, instanceID)
	if err != nil {
		return nil, translate(err)
	}

	resp := &sessionv1.GetStatusResponse{
		State:     stateToProto(snapshot.State),
		Connected: snapshot.Status.Connected,
		LoggedIn:  snapshot.Status.LoggedIn,
	}
	if snapshot.Status.Device != nil {
		d := snapshot.Status.Device
		resp.Device = &sessionv1.DeviceIdentity{
			Jid:          d.JID,
			Lid:          d.LID,
			PhoneNumber:  d.PhoneNumber,
			PushName:     d.PushName,
			Platform:     d.Platform,
			BusinessName: d.BusinessName,
		}
	}
	if !snapshot.ConnectedAt.IsZero() {
		resp.ConnectedAt = timestamppb.New(snapshot.ConnectedAt)
	}
	return resp, nil
}

func (s *GRPCServer) ApplySettings(ctx context.Context, req *sessionv1.ApplySettingsRequest) (*sessionv1.ApplySettingsResponse, error) {
	instanceID, err := s.fence(ctx, req.GetFence())
	if err != nil {
		return nil, err
	}

	result, err := s.supervisor.ApplySettings(ctx, instanceID, req.GetProxyChanged(), req.GetPassiveChanged())
	if err != nil {
		return nil, translate(err)
	}

	return &sessionv1.ApplySettingsResponse{
		State:          stateToProto(result.State),
		Reconnecting:   result.Reconnecting,
		PassiveApplied: result.PassiveApplied,
	}, nil
}

func (s *GRPCServer) SubmitPasskeyResponse(ctx context.Context, req *sessionv1.SubmitPasskeyResponseRequest) (*sessionv1.SubmitPasskeyResponseResponse, error) {
	instanceID, err := s.fence(ctx, req.GetFence())
	if err != nil {
		return nil, err
	}

	state, err := s.supervisor.SubmitPasskeyResponse(ctx, instanceID, req.GetWebauthnResponseJson())
	if err != nil {
		return nil, translate(err)
	}
	return &sessionv1.SubmitPasskeyResponseResponse{State: stateToProto(state)}, nil
}

func (s *GRPCServer) ConfirmPasskey(ctx context.Context, req *sessionv1.ConfirmPasskeyRequest) (*sessionv1.ConfirmPasskeyResponse, error) {
	instanceID, err := s.fence(ctx, req.GetFence())
	if err != nil {
		return nil, err
	}

	state, err := s.supervisor.ConfirmPasskey(ctx, instanceID)
	if err != nil {
		return nil, translate(err)
	}
	return &sessionv1.ConfirmPasskeyResponse{State: stateToProto(state)}, nil
}

func (s *GRPCServer) GetIdentityVerificationCodes(
	ctx context.Context,
	req *sessionv1.GetIdentityVerificationCodesRequest,
) (*sessionv1.GetIdentityVerificationCodesResponse, error) {
	instanceID, err := s.fence(ctx, req.GetFence())
	if err != nil {
		return nil, err
	}

	codes, err := s.supervisor.IdentityVerificationCodes(ctx, instanceID, req.GetContact())
	if err != nil {
		return nil, translate(err)
	}

	return &sessionv1.GetIdentityVerificationCodesResponse{
		Lid:            codes.LID,
		PhoneNumber:    codes.PhoneNumber,
		Username:       codes.Username,
		NumericCode:    codes.NumericCode,
		DisplayQr:      codes.DisplayQR,
		VerificationQr: codes.VerificationQR,
	}, nil
}

// translate maps supervisor errors onto the gRPC codes the API knows how to
// turn into problem details.
func translate(err error) error {
	switch {
	case errors.Is(err, ErrUnknownInstance):
		return status.Error(codes.FailedPrecondition, DetailUnknownInstance)
	case errors.Is(err, wa.ErrNotPaired):
		return status.Error(codes.FailedPrecondition, DetailNotPaired)
	case errors.Is(err, wa.ErrAlreadyPaired):
		return status.Error(codes.FailedPrecondition, DetailAlreadyPaired)
	case errors.Is(err, wa.ErrInvalidPhoneNumber):
		return status.Error(codes.InvalidArgument, DetailInvalidPhone)
	case errors.Is(err, wa.ErrPairingRunning):
		return status.Error(codes.Aborted, DetailPairingRunning)
	case errors.Is(err, ErrDraining):
		return status.Error(codes.Unavailable, DetailDraining)
	case errors.Is(err, wa.ErrNoPasskeyChallenge):
		return status.Error(codes.FailedPrecondition, DetailNoPasskeyChallenge)
	case errors.Is(err, wa.ErrNoPasskeyCode):
		return status.Error(codes.FailedPrecondition, DetailNoPasskeyCode)
	case errors.Is(err, wa.ErrNotConnected):
		return status.Error(codes.FailedPrecondition, DetailNotConnected)
	case errors.Is(err, wa.ErrInvalidContact), errors.Is(err, wa.ErrInvalidPasskeyResponse):
		return status.Error(codes.InvalidArgument, DetailInvalidContact)
	case errors.Is(err, wa.ErrIdentityNotResolvable):
		return status.Error(codes.NotFound, DetailIdentityNotResolvable)
	case errors.Is(err, wa.ErrCannotVerifySelf):
		return status.Error(codes.InvalidArgument, DetailCannotVerifySelf)
	case errors.Is(err, wa.ErrContactUnavailable):
		return status.Error(codes.NotFound, DetailContactUnavailable)
	default:
		// Anything reaching here is a failure talking to WhatsApp or to our own
		// storage. Unavailable tells the API to surface it as an upstream
		// problem rather than a client mistake.
		return status.Error(codes.Unavailable, fmt.Sprintf("%s: %v", DetailUpstreamFailure, err))
	}
}

func stateToProto(state domain.InstanceState) sessionv1.SessionState {
	switch state {
	case domain.InstanceRegistered:
		return sessionv1.SessionState_SESSION_STATE_REGISTERED
	case domain.InstancePairing:
		return sessionv1.SessionState_SESSION_STATE_PAIRING
	case domain.InstanceConnecting:
		return sessionv1.SessionState_SESSION_STATE_CONNECTING
	case domain.InstanceConnected:
		return sessionv1.SessionState_SESSION_STATE_CONNECTED
	case domain.InstanceDisconnected:
		return sessionv1.SessionState_SESSION_STATE_DISCONNECTED
	case domain.InstanceLoggedOut:
		return sessionv1.SessionState_SESSION_STATE_LOGGED_OUT
	case domain.InstanceBanned:
		return sessionv1.SessionState_SESSION_STATE_BANNED
	default:
		return sessionv1.SessionState_SESSION_STATE_UNSPECIFIED
	}
}

// StateFromProto is the inverse, used by the API when reading a response.
func StateFromProto(state sessionv1.SessionState) domain.InstanceState {
	switch state {
	case sessionv1.SessionState_SESSION_STATE_REGISTERED:
		return domain.InstanceRegistered
	case sessionv1.SessionState_SESSION_STATE_PAIRING:
		return domain.InstancePairing
	case sessionv1.SessionState_SESSION_STATE_CONNECTING:
		return domain.InstanceConnecting
	case sessionv1.SessionState_SESSION_STATE_CONNECTED:
		return domain.InstanceConnected
	case sessionv1.SessionState_SESSION_STATE_DISCONNECTED:
		return domain.InstanceDisconnected
	case sessionv1.SessionState_SESSION_STATE_LOGGED_OUT:
		return domain.InstanceLoggedOut
	case sessionv1.SessionState_SESSION_STATE_BANNED:
		return domain.InstanceBanned
	default:
		return ""
	}
}
