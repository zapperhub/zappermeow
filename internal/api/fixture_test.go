package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/api"
	"github.com/zapperhub/zappermeow/internal/config"
	"github.com/zapperhub/zappermeow/internal/store/testutil"
)

const (
	bootstrapEmail    = "root@example.com"
	bootstrapPassword = "bootstrap-secret-1"
	signingKey        = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// syncBuffer collects log output for the tests that assert on what was logged.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fixture is one API instance wired to the shared containers.
type fixture struct {
	t       *testing.T
	infra   *testutil.Infra
	handler http.Handler
	logs    *syncBuffer
	cfg     *config.Config
}

// newFixture resets the database and boots a fresh application, which also runs
// the super-admin bootstrap. Pass mutators to change the configuration a test
// needs (a shorter lockout window, a tighter rate limit, no bootstrap).
func newFixture(t *testing.T, mutators ...func(*config.Config)) *fixture {
	t.Helper()

	infra := testutil.Shared(t)
	infra.Reset(t)

	return bootApplication(t, infra, mutators...)
}

// bootApplication starts an application against the current database contents,
// without resetting it. Used to simulate a restart.
func bootApplication(t *testing.T, infra *testutil.Infra, mutators ...func(*config.Config)) *fixture {
	t.Helper()

	cfg := &config.Config{
		ListenAddr:           ":0",
		DatabaseURL:          infra.DatabaseURL,
		RedisAddr:            infra.RedisAddr,
		JWTSigningKey:        signingKey,
		JWTTTL:               time.Hour,
		BootstrapEmail:       bootstrapEmail,
		BootstrapPassword:    bootstrapPassword,
		LockoutMaxFailures:   5,
		LockoutWindow:        15 * time.Minute,
		LoginRateLimit:       1000,
		OperationalRateLimit: 1000,

		// Session-worker settings. The API never owns a session, but Validate()
		// covers the whole configuration, so the fixture mirrors the documented
		// defaults instead of leaving them at zero.
		WorkerGRPCListenAddr:      ":9090",
		MaxSessionsPerWorker:      200,
		PairingWindow:             180 * time.Second,
		LeaseHeartbeatInterval:    10 * time.Second,
		LeaseExpiry:               30 * time.Second,
		ReconcileInterval:         15 * time.Second,
		ConnectionEventsRetention: 720 * time.Hour,
		// No worker fleet runs in these tests, so commands that need one give
		// up quickly instead of paying the production grace period.
		ClaimWait: 150 * time.Millisecond,
	}
	for _, mutate := range mutators {
		mutate(cfg)
	}
	require.NoError(t, cfg.Validate())

	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(logs), &slog.HandlerOptions{Level: slog.LevelDebug}))

	app, err := api.NewApplication(context.Background(), api.Options{
		Config: cfg,
		Logger: logger,
		Pool:   infra.Pool,
		Redis:  infra.Redis,
	})
	require.NoError(t, err)

	return &fixture{t: t, infra: infra, handler: app.Handler(), logs: logs, cfg: cfg}
}

// request describes one HTTP call in a test.
type request struct {
	method string
	path   string
	body   any
	token  string
	apiKey string
	header map[string]string
}

// response is the decoded result of a call.
type response struct {
	t      *testing.T
	Status int
	Body   []byte
}

func (f *fixture) do(req request) *response {
	f.t.Helper()

	var reader io.Reader
	if req.body != nil {
		encoded, err := json.Marshal(req.body)
		require.NoError(f.t, err)
		reader = bytes.NewReader(encoded)
	}

	httpReq := httptest.NewRequestWithContext(f.t.Context(), req.method, req.path, reader)
	httpReq.Header.Set("Content-Type", "application/json")
	if req.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.token)
	}
	if req.apiKey != "" {
		httpReq.Header.Set("X-Api-Key", req.apiKey)
	}
	for key, value := range req.header {
		httpReq.Header.Set(key, value)
	}

	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, httpReq)

	return &response{t: f.t, Status: recorder.Code, Body: recorder.Body.Bytes()}
}

// envelope is the standard success shape.
type envelope struct {
	Status    int             `json:"status"`
	Data      json.RawMessage `json:"data"`
	Timestamp time.Time       `json:"timestamp"`
}

// problem is the RFC 9457 error shape extended with `code`.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
	Errors []struct {
		Message  string `json:"message"`
		Location string `json:"location"`
		Value    any    `json:"value"`
	} `json:"errors"`
	Timestamp time.Time `json:"timestamp"`
}

// data asserts the response is a success envelope with the expected status and
// decodes its payload into out.
func (r *response) data(wantStatus int, out any) {
	r.t.Helper()
	require.Equal(r.t, wantStatus, r.Status, "unexpected status; body: %s", r.Body)

	var env envelope
	require.NoError(r.t, json.Unmarshal(r.Body, &env), "body: %s", r.Body)
	require.Equal(r.t, wantStatus, env.Status, "the envelope status must mirror the HTTP status")
	require.False(r.t, env.Timestamp.IsZero(), "the envelope must carry a timestamp")

	if out != nil {
		require.NoError(r.t, json.Unmarshal(env.Data, out), "data: %s", env.Data)
	}
}

// problem asserts the response is a problem document with the expected status
// and stable code, and returns it for further assertions.
func (r *response) problem(wantStatus int, wantCode string) problem {
	r.t.Helper()
	require.Equal(r.t, wantStatus, r.Status, "unexpected status; body: %s", r.Body)

	var doc problem
	require.NoError(r.t, json.Unmarshal(r.Body, &doc), "body: %s", r.Body)
	require.Equal(r.t, wantCode, doc.Code, "unexpected error code; body: %s", r.Body)
	require.Equal(r.t, wantStatus, doc.Status, "the problem status must mirror the HTTP status")
	require.False(r.t, doc.Timestamp.IsZero(), "the problem must carry a timestamp")
	return doc
}

// login authenticates and returns the access token.
func (f *fixture) login(email, password string) string {
	f.t.Helper()

	var data struct {
		AccessToken        string `json:"access_token"`
		TokenType          string `json:"token_type"`
		ExpiresIn          int    `json:"expires_in"`
		Audience           string `json:"audience"`
		MustChangePassword bool   `json:"must_change_password"`
	}
	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": email, "password": password,
	}}).data(http.StatusOK, &data)

	require.NotEmpty(f.t, data.AccessToken)
	return data.AccessToken
}

// platformToken logs the bootstrap super-admin in.
func (f *fixture) platformToken() string {
	f.t.Helper()
	return f.login(bootstrapEmail, bootstrapPassword)
}

// countEvents returns how many security events of a type were recorded.
func (f *fixture) countEvents(eventType string) int64 {
	f.t.Helper()
	count, err := f.infra.Queries.CountSecurityEventsByType(context.Background(), eventType)
	require.NoError(f.t, err)
	return count
}

// The envelope is a rigid contract of exactly three members: a client parsing
// it must never meet a fourth it did not expect.
func assertEnvelopeMembers(t *testing.T, body []byte) {
	t.Helper()

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw))

	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	require.Equal(t, []string{"data", "status", "timestamp"}, keys,
		"unexpected envelope members: %s", body)
}
