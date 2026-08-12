package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/config"
)

// failLogin submits a deliberately wrong password.
func (f *fixture) failLogin(email string) *response {
	f.t.Helper()
	return f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": email, "password": "definitely-not-the-password",
	}})
}

// US6 scenario 1 / SC-005: after the configured number of consecutive failures
// the account is locked, and the next attempt is refused even though the
// password is correct.
func TestAccountLocksAfterConsecutiveFailures(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	_ = acme

	for attempt := 1; attempt <= 5; attempt++ {
		f.failLogin("alice@acme.com").problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")
	}

	// The sixth attempt carries the right password and is still refused.
	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": "senhaAlice1",
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")

	assert.Equal(t, int64(1), f.countEvents("account_locked"))
}

// A locked account answers exactly like a wrong password, so the lock cannot be
// used to tell which accounts exist (FR-019).
func TestLockedAccountIsIndistinguishableFromAWrongPassword(t *testing.T) {
	f := newFixture(t)
	f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	for range 5 {
		f.failLogin("alice@acme.com")
	}

	locked := f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": "senhaAlice1",
	}})
	unknown := f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "nobody@example.com", "password": "whatever-password",
	}})

	lockedDoc := locked.problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")
	unknownDoc := unknown.problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")

	assert.Equal(t, unknown.Status, locked.Status)
	assert.Equal(t, unknownDoc.Detail, lockedDoc.Detail)
	assert.Equal(t, unknownDoc.Title, lockedDoc.Title)
	assert.Equal(t, unknownDoc.Type, lockedDoc.Type)
}

// US6 scenarios 2 and 3: the lock expires on its own, and a successful login
// then resets the counter. No unlock job exists — the timestamp simply passes.
func TestLockoutExpiresOnItsOwnAndResetsTheCounter(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.LockoutWindow = time.Second })
	f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	for range 5 {
		f.failLogin("alice@acme.com")
	}
	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": "senhaAlice1",
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")

	time.Sleep(1200 * time.Millisecond)

	// Scenario 2: the login works again with no intervention.
	assert.NotEmpty(t, f.login("alice@acme.com", "senhaAlice1"))
	// The expiry is reported on that first successful login.
	assert.Equal(t, int64(1), f.countEvents("account_unlocked"))

	// Scenario 3: the counter was zeroed, so four more failures do not relock.
	for range 4 {
		f.failLogin("alice@acme.com")
	}
	assert.NotEmpty(t, f.login("alice@acme.com", "senhaAlice1"),
		"a successful login must reset the failure counter")
	assert.Equal(t, int64(1), f.countEvents("account_locked"), "the account must not have locked a second time")
}

// FR-020: the lock lives in Postgres, so it survives a restart of the service.
// A lockout kept only in Redis would evaporate on failover.
func TestLockoutSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	for range 5 {
		f.failLogin("alice@acme.com")
	}

	// A brand new application against the same database, as after a deploy.
	restarted := bootApplication(t, f.infra)

	restarted.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": "senhaAlice1",
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")

	locked, err := f.infra.Queries.GetUserCredentialByEmail(context.Background(), "alice@acme.com")
	require.NoError(t, err)
	require.NotNil(t, locked.LockedUntil, "the lock must be persisted, not held in memory")
	assert.True(t, locked.LockedUntil.After(time.Now()))
}

// A successful login before the threshold clears the tally, so scattered typos
// never accumulate into a lock.
func TestSuccessfulLoginResetsPartialFailures(t *testing.T) {
	f := newFixture(t)
	f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	for range 4 {
		f.failLogin("alice@acme.com")
	}
	assert.NotEmpty(t, f.login("alice@acme.com", "senhaAlice1"))

	for range 4 {
		f.failLogin("alice@acme.com")
	}
	assert.NotEmpty(t, f.login("alice@acme.com", "senhaAlice1"),
		"the counter must have restarted after the successful login")
	assert.Zero(t, f.countEvents("account_locked"))
}

// Locking one account must not affect another.
func TestLockoutIsScopedToOneAccount(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")
	f.newTenant(platformToken, "Globex", "bob@globex.com", "senhaBob123")

	for range 5 {
		f.failLogin("alice@acme.com")
	}

	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": "senhaAlice1",
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")
	assert.NotEmpty(t, f.login("bob@globex.com", "senhaBob123"))
}

// US6 scenario 4 / FR-018: a burst from one origin against varied accounts is
// refused, which is what contains a mass sweep the per-account lock cannot see.
func TestLoginRateLimitPerOrigin(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.LoginRateLimit = 5 })

	var limited bool
	for attempt := range 30 {
		response := f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
			// A different account each time: only the origin limiter can catch this.
			"email": "victim" + string(rune('a'+attempt%26)) + "@example.com", "password": "guess",
		}})
		if response.Status == http.StatusTooManyRequests {
			response.problem(http.StatusTooManyRequests, "RATE_LIMIT_EXCEEDED")
			limited = true
			break
		}
	}

	require.True(t, limited, "a sweep from one origin must eventually be refused")
}

// Every failure, lock and unlock has to be findable in the trail (SC-007).
func TestLockoutEventsAreRecorded(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.LockoutWindow = time.Second })
	f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	for range 5 {
		f.failLogin("alice@acme.com")
	}
	assert.Equal(t, int64(5), f.countEvents("login_failed"))
	assert.Equal(t, int64(1), f.countEvents("account_locked"))

	time.Sleep(1200 * time.Millisecond)
	f.login("alice@acme.com", "senhaAlice1")
	assert.Equal(t, int64(1), f.countEvents("account_unlocked"))

	logs := f.logs.String()
	assert.Contains(t, logs, "account_locked")
	assert.Contains(t, logs, "account_unlocked")
	assert.NotContains(t, logs, "definitely-not-the-password", "an attempted password must never be logged")
}
