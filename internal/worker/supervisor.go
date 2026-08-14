package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/lease"
	"github.com/zapperhub/zappermeow/internal/metrics"
	"github.com/zapperhub/zappermeow/internal/store"
	"github.com/zapperhub/zappermeow/internal/wa"
)

// Errors the gRPC layer maps onto client-visible codes.
var (
	// ErrUnknownInstance means the instance row is gone — deleted while a
	// command was in flight.
	ErrUnknownInstance = errors.New("worker: unknown instance")
	// ErrDraining means the process is shutting down and must not take new work.
	ErrDraining = errors.New("worker: draining")
)

// SessionFactory builds sessions. The worker depends on the interface rather
// than on the HyperMeow container so the supervisor can be exercised against a
// scripted session while Postgres and Redis stay real.
type SessionFactory interface {
	NewSession(ctx context.Context, instanceID domain.ID, cfg wa.SessionConfig) (wa.Session, error)
}

// Supervisor owns every session this process holds a lease for.
type Supervisor struct {
	queries   *store.Queries
	leases    *lease.Manager
	publisher *events.Publisher
	factory   SessionFactory
	logger    *slog.Logger

	pairingWindow time.Duration
	maxSessions   int

	mu       sync.RWMutex
	sessions map[domain.ID]*managedSession
	draining bool
}

// Options configures a Supervisor.
type Options struct {
	Queries       *store.Queries
	Leases        *lease.Manager
	Publisher     *events.Publisher
	Factory       SessionFactory
	Logger        *slog.Logger
	PairingWindow time.Duration
	MaxSessions   int
}

// NewSupervisor builds a Supervisor.
func NewSupervisor(opts Options) *Supervisor {
	return &Supervisor{
		queries:       opts.Queries,
		leases:        opts.Leases,
		publisher:     opts.Publisher,
		factory:       opts.Factory,
		logger:        opts.Logger,
		pairingWindow: opts.PairingWindow,
		maxSessions:   opts.MaxSessions,
		sessions:      make(map[domain.ID]*managedSession),
	}
}

// managedSession is one owned session plus everything needed to stop it.
type managedSession struct {
	instanceID domain.ID
	generation int64
	session    wa.Session

	// ctx lives as long as the session does. The socket is bound to whatever
	// context opens it, so anything shorter — a pairing window, a single
	// command — would take the connection down with it.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// inbox serialises every event of this session into a single handler
	// goroutine. Two independent pumps — one for the main stream, one for the
	// pairing channel — could otherwise apply writes out of order, and a
	// buffered pairing event could resurrect an identity a logout had just
	// deleted.
	inbox chan wa.Event

	mu             sync.Mutex
	pairingCancel  context.CancelFunc
	pairingExpires time.Time
}

// Draining reports whether the process is shutting down.
func (s *Supervisor) Draining() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.draining
}

// Count reports how many sessions this worker currently holds.
func (s *Supervisor) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// HasCapacity reports whether the worker may adopt another session. The limit
// is an operational sizing knob, not a product quota: the platform imposes no
// ceiling on instances (constitution v2.4.0).
func (s *Supervisor) HasCapacity() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.draining && len(s.sessions) < s.maxSessions
}

// ConnectResult is what a connect command produced.
type ConnectResult struct {
	State            domain.InstanceState
	PairingStarted   bool
	PairingExpiresAt time.Time
}

// Adopt takes ownership of an instance and starts its session. It is the entry
// point for both reconciliation and an explicit claim.
func (s *Supervisor) Adopt(ctx context.Context, instanceID domain.ID) error {
	if !s.HasCapacity() {
		return ErrDraining
	}
	if _, held := s.lookup(instanceID); held {
		return nil
	}

	generation, err := s.leases.Acquire(ctx, instanceID)
	if err != nil {
		// Losing the race is the normal outcome when several workers wake up
		// on the same claim; only one of them may own the session.
		return err
	}

	if err := s.start(ctx, instanceID, generation); err != nil {
		// Hand the lease back rather than sitting on a session we failed to
		// start: another worker may well succeed.
		_ = s.leases.Release(ctx, instanceID)
		return err
	}

	metrics.LeaseAcquisitions.WithLabelValues(s.leases.WorkerID()).Inc()
	metrics.SessionsConnected.WithLabelValues(s.leases.WorkerID()).Set(float64(s.Count()))
	s.record(ctx, instanceID, domain.ConnEventLeaseAcquired, domain.ReasonNone, nil)
	return nil
}

// start builds the session and begins pumping its events.
func (s *Supervisor) start(ctx context.Context, instanceID domain.ID, generation int64) error {
	instance, err := s.queries.GetInstanceConnectionByID(ctx, instanceID)
	if err != nil {
		if store.IsNoRows(err) {
			return ErrUnknownInstance
		}
		return fmt.Errorf("load instance: %w", err)
	}

	storedJID := ""
	if instance.WaJid != nil {
		storedJID = *instance.WaJid
	}
	proxyURL := ""
	if instance.ProxyUrl != nil {
		proxyURL = *instance.ProxyUrl
	}

	// The proxy is fixed when the client is built, so it is read here on every
	// start — including the ones nobody commanded: a reconnect after a failover
	// must come up through the same proxy as the session it replaces.
	session, err := s.factory.NewSession(ctx, instanceID, wa.SessionConfig{
		StoredJID: storedJID,
		ProxyURL:  proxyURL,
	})
	if err != nil {
		return fmt.Errorf("build session: %w", err)
	}

	sessionCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	managed := &managedSession{
		instanceID: instanceID,
		generation: generation,
		session:    session,
		ctx:        sessionCtx,
		cancel:     cancel,
		done:       make(chan struct{}),
		inbox:      make(chan wa.Event, 64),
	}

	s.mu.Lock()
	s.sessions[instanceID] = managed
	s.mu.Unlock()

	go s.pump(sessionCtx, managed)
	go forward(sessionCtx, session.Events(), managed.inbox)

	// Adopting a lease is not the same as bringing the number online. A paired
	// instance the tenant wants running has to be dialled here, or a restart
	// would leave it owned, silent and reported as connected on the strength of
	// a state nobody refreshed — including missing a logout done from the
	// handset, which only arrives over a live socket (FR-019, US4 scenario 5).
	if storedJID != "" && domain.ConnectionIntent(instance.ConnectionIntent) == domain.IntentActive {
		go s.reconnect(sessionCtx, managed)
	}
	return nil
}

// reconnect dials a session that was adopted with device material already in
// place. Failure is not fatal: the client retries on its own, and a permanent
// refusal arrives as an event like any other.
func (s *Supervisor) reconnect(ctx context.Context, managed *managedSession) {
	if err := s.setState(ctx, managed.instanceID, domain.InstanceConnecting); err != nil {
		s.logger.Error("marking session as connecting failed",
			slog.String("instance_id", managed.instanceID.String()),
			slog.String("error", err.Error()))
	}

	if err := managed.session.Connect(ctx); err != nil {
		s.logger.Warn("reconnecting an adopted session failed",
			slog.String("instance_id", managed.instanceID.String()),
			slog.String("error", err.Error()))
	}
}

// forward copies one source into the session inbox. Copying rather than
// consuming directly is what lets several sources share a single handler.
func forward(ctx context.Context, src <-chan wa.Event, dst chan<- wa.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-src:
			if !ok {
				return
			}
			select {
			case dst <- evt:
			case <-ctx.Done():
				return
			}
		}
	}
}

// stop tears a session down without touching the lease.
func (s *Supervisor) stop(instanceID domain.ID) {
	s.mu.Lock()
	managed, ok := s.sessions[instanceID]
	delete(s.sessions, instanceID)
	s.mu.Unlock()

	if !ok {
		return
	}
	managed.cancelPairing()
	managed.cancel()
	managed.session.Close()
	<-managed.done
}

// stopFromPump tears a session down without waiting for its pump to finish,
// because the caller is the pump itself: waiting would block on a goroutine
// that cannot exit until this call returns.
func (s *Supervisor) stopFromPump(instanceID domain.ID) {
	s.mu.Lock()
	managed, ok := s.sessions[instanceID]
	delete(s.sessions, instanceID)
	s.mu.Unlock()

	if !ok {
		return
	}
	managed.cancelPairing()
	managed.cancel()
	managed.session.Close()
}

// Drop releases a session this worker no longer owns. Called when a heartbeat
// reveals the lease was taken over: the new owner may already be connecting, so
// this process must let go immediately rather than finish anything in flight.
func (s *Supervisor) Drop(ctx context.Context, instanceID domain.ID) {
	s.logger.Warn("dropping session: lease no longer held",
		slog.String("instance_id", instanceID.String()))
	s.stop(instanceID)
	metrics.LeaseLosses.WithLabelValues(s.leases.WorkerID()).Inc()
	metrics.SessionsConnected.WithLabelValues(s.leases.WorkerID()).Set(float64(s.Count()))
	metrics.SessionReconnects.WithLabelValues("lease").Inc()
	s.record(ctx, instanceID, domain.ConnEventLeaseLost, domain.ReasonWorkerLost, nil)
}

// SettingsResult reports what applying the instance settings actually did.
type SettingsResult struct {
	State          domain.InstanceState
	Reconnecting   bool
	PassiveApplied bool
}

// ApplySettings reapplies the instance's persisted settings on the live session.
//
// The values are reread from Postgres rather than carried in the request: the
// database is what every later start reads too, so taking them from anywhere
// else would let a command and a failover disagree about the configuration
// (research R2).
func (s *Supervisor) ApplySettings(ctx context.Context, instanceID domain.ID, proxyChanged, passiveChanged bool) (SettingsResult, error) {
	managed, ok := s.lookup(instanceID)
	if !ok {
		return SettingsResult{}, ErrUnknownInstance
	}

	settings, err := s.queries.GetInstanceSettings(ctx, instanceID)
	if err != nil {
		if store.IsNoRows(err) {
			return SettingsResult{}, ErrUnknownInstance
		}
		return SettingsResult{}, fmt.Errorf("load instance settings: %w", err)
	}

	result := SettingsResult{State: domain.InstanceConnecting}

	// The proxy is bound to the socket at construction, so there is no way to
	// change it on a live client: the session is rebuilt. Doing it here, in the
	// process that holds the lease, is what keeps exclusive ownership intact
	// (Principle III) — no second process ever touches this session.
	if proxyChanged {
		if err := s.relink(ctx, managed); err != nil {
			return SettingsResult{}, err
		}
		result.Reconnecting = true
		// A rebuilt session is a new one: any further command in this call would
		// be acting on the object that was just closed.
		return result, nil
	}

	if passiveChanged {
		applied, err := s.applyPassiveMode(ctx, managed, settings.PassiveMode)
		if err != nil {
			return SettingsResult{}, err
		}
		result.PassiveApplied = applied
	}

	if snapshot, err := s.Status(ctx, instanceID); err == nil {
		result.State = snapshot.State
	}
	return result, nil
}

// relink rebuilds a session so it comes up through the configuration currently
// stored, and dials it again under the same lease and generation.
func (s *Supervisor) relink(ctx context.Context, managed *managedSession) error {
	instanceID := managed.instanceID
	generation := managed.generation

	// A pairing attempt cannot survive this: the QR on screen was minted by the
	// socket about to be dropped, and the server will not honour it afterwards.
	// Ending it explicitly tells the tenant to start a new attempt instead of
	// leaving them staring at a code that silently stopped working.
	if managed.pairingActive() {
		s.publishPairingEnded(ctx, managed, wa.ExpiryCancelled)
	}

	if err := s.recordDisconnect(ctx, instanceID, domain.InstanceConnecting, domain.ReasonProxyUpdated, nil); err != nil {
		s.logger.Error("recording the relink disconnect failed",
			slog.String("instance_id", instanceID.String()),
			slog.String("error", err.Error()))
	}
	// The instance row and the trail are separate writes: updating the state
	// says nothing about the trail, and a transition the tenant cannot see
	// afterwards is a gap in the audit, not a detail.
	s.record(ctx, instanceID, domain.ConnEventDisconnected, domain.ReasonProxyUpdated, nil)
	s.publish(ctx, managed, events.TypeDisconnected, map[string]any{
		"reason": string(domain.ReasonProxyUpdated),
	})

	s.stop(instanceID)

	if err := s.start(ctx, instanceID, generation); err != nil {
		// The session is down and not replaced. Reconciliation adopts it again
		// from the persisted intent, this time reading the new configuration,
		// so the failure costs a delay rather than the instance.
		return fmt.Errorf("rebuild session with new settings: %w", err)
	}

	rebuilt, ok := s.lookup(instanceID)
	if !ok {
		return ErrUnknownInstance
	}
	// start only dials on its own when the instance is paired and meant to be
	// running; a session that was connected a moment ago always is.
	go s.reconnect(rebuilt.ctx, rebuilt)
	return nil
}

// applyPassiveMode pushes the desired mode onto a live session.
//
// It reports false when there is no connection to push it onto, which is not a
// failure: the setting is stored, and the next Connected applies it through the
// very same path.
func (s *Supervisor) applyPassiveMode(ctx context.Context, managed *managedSession, passive bool) (bool, error) {
	if !managed.session.Status().Connected {
		return false, nil
	}
	if err := managed.session.SetPassive(ctx, passive); err != nil {
		if errors.Is(err, wa.ErrNotConnected) {
			// The socket dropped between the check and the call. Same answer as
			// above: the stored value wins at the next Connected.
			return false, nil
		}
		return false, fmt.Errorf("apply passive mode: %w", err)
	}
	return true, nil
}

// Connect brings a session up, pairing when there is no device material.
func (s *Supervisor) Connect(ctx context.Context, instanceID domain.ID) (ConnectResult, error) {
	managed, ok := s.lookup(instanceID)
	if !ok {
		return ConnectResult{}, ErrUnknownInstance
	}

	status := managed.session.Status()
	if status.Connected {
		// Idempotent: repeating the command in the state already reached is
		// accepted with no side effect (FR-008).
		return ConnectResult{State: domain.InstanceConnected}, nil
	}

	if status.Device != nil && status.Device.JID != "" {
		if err := managed.session.Connect(ctx); err != nil {
			return ConnectResult{}, fmt.Errorf("connect session: %w", err)
		}
		return ConnectResult{State: domain.InstanceConnecting}, nil
	}

	expiresAt, err := s.startPairing(ctx, managed)
	if err != nil {
		return ConnectResult{}, err
	}
	return ConnectResult{
		State:            domain.InstancePairing,
		PairingStarted:   true,
		PairingExpiresAt: expiresAt,
	}, nil
}

// startPairing opens the QR stream and connects, which is the order the library
// requires: GetQRChannel must run before Connect.
func (s *Supervisor) startPairing(ctx context.Context, managed *managedSession) (time.Time, error) {
	pairingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.pairingWindow)

	codes, err := managed.session.QRChannel(pairingCtx)
	if err != nil {
		cancel()
		return time.Time{}, err
	}

	expiresAt := time.Now().Add(s.pairingWindow)
	managed.setPairing(cancel, expiresAt)

	// The socket is opened on the session context, never on the pairing one.
	// HyperMeow derives the connection's lifetime from the context given here,
	// and pairing ends by cancelling its own window — which would drop the
	// socket at the exact moment the handshake succeeds, leaving the handset
	// waiting forever for a sync that never comes.
	if err := managed.session.Connect(managed.ctx); err != nil {
		managed.cancelPairing()
		return time.Time{}, fmt.Errorf("connect for pairing: %w", err)
	}

	if err := s.setState(ctx, managed.instanceID, domain.InstancePairing); err != nil {
		return time.Time{}, err
	}
	s.record(ctx, managed.instanceID, domain.ConnEventPairingStarted, domain.ReasonNone, nil)

	// A dedicated goroutine drains the codes — HyperMeow closes the channel and
	// disconnects the client if the consumer stalls (research R2) — but it only
	// forwards: handling stays in the single pump, so pairing writes cannot
	// interleave with connection writes.
	go forward(pairingCtx, codes, managed.inbox)

	return expiresAt, nil
}

// PairPhone starts a phone-code attempt.
func (s *Supervisor) PairPhone(ctx context.Context, instanceID domain.ID, phoneNumber string, replaceActive bool) (string, time.Time, error) {
	managed, ok := s.lookup(instanceID)
	if !ok {
		return "", time.Time{}, ErrUnknownInstance
	}

	if managed.pairingActive() {
		if !replaceActive {
			return "", time.Time{}, wa.ErrPairingRunning
		}
		managed.cancelPairing()
		s.publishPairingEnded(ctx, managed, wa.ExpiryReplaced)
	}

	code, expiresAt, err := managed.session.PairPhone(ctx, phoneNumber)
	if err != nil {
		// WhatsApp rejects pairing for reasons the platform cannot infer — an
		// unknown number, a rate limit, an account restriction. The reason only
		// exists here, so it is logged here or it is lost.
		s.logger.Error("pairing by phone was refused",
			slog.String("instance_id", instanceID.String()),
			slog.String("error", err.Error()))
		return "", time.Time{}, err
	}

	managed.setPairing(func() {}, expiresAt)
	if err := s.setState(ctx, instanceID, domain.InstancePairing); err != nil {
		return "", time.Time{}, err
	}
	s.record(ctx, instanceID, domain.ConnEventPairingStarted, domain.ReasonNone,
		map[string]any{"method": string(wa.MethodPhone)})

	return code, expiresAt, nil
}

// Disconnect takes a session offline while keeping its device material.
func (s *Supervisor) Disconnect(ctx context.Context, instanceID domain.ID) (domain.InstanceState, error) {
	managed, ok := s.lookup(instanceID)
	if !ok {
		// Nothing running here is already the requested outcome; reporting an
		// error would make a harmless repeat look like a failure (FR-008).
		return domain.InstanceDisconnected, nil
	}

	managed.cancelPairing()
	managed.session.Disconnect()

	if err := s.recordDisconnect(ctx, instanceID, domain.InstanceDisconnected, domain.ReasonUserRequested, nil); err != nil {
		return "", err
	}
	s.publish(ctx, managed, events.TypeDisconnected, map[string]any{
		"reason":    string(domain.ReasonUserRequested),
		"permanent": true,
	})

	s.stop(instanceID)
	// Handing the lease back matters as much as stopping: a lease still held by
	// a worker that no longer runs the session blocks everyone — including this
	// process — from adopting it until the heartbeat goes stale.
	_ = s.leases.Release(ctx, instanceID)
	return domain.InstanceDisconnected, nil
}

// Logout ends the session on WhatsApp and deletes the local material.
func (s *Supervisor) Logout(ctx context.Context, instanceID domain.ID, allowTemporaryConnect bool) (bool, error) {
	managed, ok := s.lookup(instanceID)
	if !ok {
		return false, ErrUnknownInstance
	}

	managed.cancelPairing()

	// Client.Logout needs a live socket: it sends an IQ and aborts without
	// deleting anything when the connection is down (research R10).
	if allowTemporaryConnect && !managed.session.Status().Connected {
		connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := managed.session.Connect(connectCtx); err != nil {
			s.logger.Warn("temporary connect for logout failed, falling back to local wipe",
				slog.String("instance_id", instanceID.String()),
				slog.String("error", err.Error()))
		}
		cancel()
	}

	remoteRemoved, err := managed.session.Logout(ctx)
	if err != nil {
		return false, err
	}

	reason := domain.ReasonUserRequested
	if !remoteRemoved {
		reason = domain.ReasonLogoutLocalOnly
	}

	if err := s.queries.ClearDeviceIdentity(ctx, instanceID); err != nil {
		return remoteRemoved, fmt.Errorf("clear device identity: %w", err)
	}
	s.record(ctx, instanceID, domain.ConnEventLoggedOut, reason, nil)
	s.publish(ctx, managed, events.TypeLoggedOut, map[string]any{
		"reason":     string(reason),
		"from_phone": false,
	})

	s.stop(instanceID)
	_ = s.leases.Release(ctx, instanceID)
	return remoteRemoved, nil
}

// StatusSnapshot is the live view of a session plus its persisted state.
type StatusSnapshot struct {
	State       domain.InstanceState
	Status      wa.Status
	ConnectedAt time.Time
}

// Status reports what the owner knows about a session.
func (s *Supervisor) Status(ctx context.Context, instanceID domain.ID) (StatusSnapshot, error) {
	managed, ok := s.lookup(instanceID)
	if !ok {
		return StatusSnapshot{}, ErrUnknownInstance
	}

	row, err := s.queries.GetInstanceConnectionByID(ctx, instanceID)
	if err != nil {
		if store.IsNoRows(err) {
			return StatusSnapshot{}, ErrUnknownInstance
		}
		return StatusSnapshot{}, fmt.Errorf("load instance: %w", err)
	}

	snapshot := StatusSnapshot{
		State:  domain.InstanceState(row.ConnectionState),
		Status: managed.session.Status(),
	}
	if row.ConnectedAt != nil {
		snapshot.ConnectedAt = *row.ConnectedAt
	}
	return snapshot, nil
}

// Shutdown drains the worker: no new work, every lease handed back, every
// session closed cleanly. Doing this on SIGTERM is what keeps a rolling deploy
// to seconds of downtime instead of a full lease expiry.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.draining = true
	ids := make([]domain.ID, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	for _, id := range ids {
		managed, ok := s.lookup(id)
		if ok && managed.pairingActive() {
			// A pairing attempt cannot survive the process; telling the client
			// it ended beats leaving a QR hanging with no codes coming.
			s.publishPairingEnded(ctx, managed, wa.ExpiryWorkerShutdown)
		}
		s.stop(id)
	}

	if err := s.leases.ReleaseAll(ctx); err != nil {
		return fmt.Errorf("release leases on shutdown: %w", err)
	}
	s.logger.Info("worker drained", slog.Int("sessions_released", len(ids)))
	return nil
}

// Held lists the sessions this worker currently runs.
func (s *Supervisor) Held() []domain.ID {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := make([]domain.ID, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	return ids
}

// Owns reports whether a session runs in this process.
func (s *Supervisor) Owns(instanceID domain.ID) bool {
	_, ok := s.lookup(instanceID)
	return ok
}

// Capacity reports how many more sessions this worker may take.
func (s *Supervisor) Capacity() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	free := s.maxSessions - len(s.sessions)
	if free < 0 {
		return 0
	}
	return free
}

// StopOnRequest drops a session because something outside asked for it — a
// tenant suspension, an administrative stop — recording the reason so the
// tenant sees why the number went offline.
func (s *Supervisor) StopOnRequest(ctx context.Context, instanceID domain.ID, reason domain.DisconnectReason) {
	managed, ok := s.lookup(instanceID)
	if !ok {
		return
	}

	managed.cancelPairing()
	managed.session.Disconnect()

	if err := s.recordDisconnect(ctx, instanceID, domain.InstanceDisconnected, reason, nil); err != nil {
		s.logger.Error("persisting requested stop failed",
			slog.String("instance_id", instanceID.String()),
			slog.String("error", err.Error()))
	}
	s.record(ctx, instanceID, domain.ConnEventDisconnected, reason, nil)
	s.publish(ctx, managed, events.TypeDisconnected, map[string]any{
		"reason":    string(reason),
		"permanent": reason.Permanent(),
	})

	s.stop(instanceID)
	_ = s.leases.Release(ctx, instanceID)
}

func (s *Supervisor) lookup(instanceID domain.ID) (*managedSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	managed, ok := s.sessions[instanceID]
	return managed, ok
}

// --- managedSession helpers ---

func (m *managedSession) setPairing(cancel context.CancelFunc, expiresAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pairingCancel = cancel
	m.pairingExpires = expiresAt
}

func (m *managedSession) pairingActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pairingCancel != nil && time.Now().Before(m.pairingExpires)
}

// pairingExpiry is when the attempt in flight runs out. The passkey step does
// not get a deadline of its own: the pairing window is shorter than the
// library's handoff validity, so it is always the binding one (research R7).
func (m *managedSession) pairingExpiry() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pairingExpires
}

func (m *managedSession) cancelPairing() {
	m.mu.Lock()
	cancel := m.pairingCancel
	m.pairingCancel = nil
	m.pairingExpires = time.Time{}
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}
