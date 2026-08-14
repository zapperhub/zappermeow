package wa_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store/testutil"
	"github.com/zapperhub/zappermeow/internal/wa"
)

// The container runs against the real HyperMeow store on real Postgres: its
// whole job is bridging the two, and a stubbed store would test nothing.
func newContainer(t *testing.T) *wa.Container {
	t.Helper()

	infra := testutil.Shared(t)
	infra.Reset(t)

	container, err := wa.NewContainer(context.Background(), infra.Pool, "ZapperMeow Test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Close() })
	return container
}

func TestNewSessionCreatesAFreshDeviceWhenUnpaired(t *testing.T) {
	container := newContainer(t)

	session, err := container.NewSession(context.Background(), domain.NewID(), wa.SessionConfig{})
	require.NoError(t, err)
	defer session.Close()

	status := session.Status()
	assert.False(t, status.LoggedIn)
	assert.Nil(t, status.Device, "an unpaired instance has no device yet")
}

// A remote logout destroys the device on both sides. If the instance row still
// points at it — because the platform missed the event, or the material was
// removed out of band — refusing to build a session would strand the instance
// forever: it could never be adopted, so it could never be paired again.
func TestNewSessionRecoversFromMissingDeviceMaterial(t *testing.T) {
	container := newContainer(t)

	session, err := container.NewSession(context.Background(), domain.NewID(),
		wa.SessionConfig{StoredJID: "5511999999999:99@s.whatsapp.net"})
	require.NoError(t, err, "a dangling JID must not strand the instance")
	defer session.Close()

	// It starts unpaired, which is exactly the state a new pairing needs.
	assert.Nil(t, session.Status().Device)
}

// A stored value that is not a real JID lands on the same recovery path.
//
// The library parses loosely — anything without a server becomes user@server —
// so a corrupted value resolves to a device that does not exist. Recovering
// rather than refusing is the deliberate choice: stranding an instance is worse
// than re-pairing one, and the fallback logs a warning so the corruption is
// still visible to whoever operates the platform.
func TestNewSessionRecoversFromACorruptedJID(t *testing.T) {
	container := newContainer(t)

	session, err := container.NewSession(context.Background(), domain.NewID(), wa.SessionConfig{StoredJID: "not-a-jid"})
	require.NoError(t, err)
	defer session.Close()

	assert.Nil(t, session.Status().Device)
}
