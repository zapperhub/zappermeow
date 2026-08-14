package api_test

import (
	"net/http"
	"testing"
)

// The route takes the instance's own API key and nothing else. A tenant token
// authenticates fine everywhere else in the connection plane, which is exactly
// why this needs its own test: inheriting the group's authenticator would have
// let it through (FR-025).
func TestVerificationCodesRefuseATenantToken(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/identity-verification-codes?contact=123456789@lid",
		token:  setup.tenant.token,
	}).problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
}

// With a key that passes, the request reaches the session layer and gets an
// honest "no worker". A credential that had failed would never get that far.
func TestVerificationCodesAcceptAnInstanceAPIKey(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/identity-verification-codes?contact=123456789@lid",
		apiKey: setup.key,
	}).problem(http.StatusServiceUnavailable, "SESSION_UNAVAILABLE")
}

// A key belongs to one instance and opens nothing else, not even a sibling
// under the same tenant.
func TestVerificationCodesRefuseAKeyFromAnotherInstance(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	other := f.newConnectionSetup(t, "Globex", "bob@globex.com")

	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + other.instanceID + "/identity-verification-codes?contact=123456789@lid",
		apiKey: setup.key,
	}).problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
}

// The contact is required: without it the request cannot name anyone.
func TestVerificationCodesRequireAContact(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	response := f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/identity-verification-codes",
		apiKey: setup.key,
	})
	if response.Status != http.StatusUnprocessableEntity && response.Status != http.StatusBadRequest {
		t.Fatalf("a missing contact must be refused, got %d: %s", response.Status, response.Body)
	}
}

// The passkey routes are connection routes and follow the plane's dual
// authentication, unlike the verification one above.
func TestPasskeyRoutesRefuseOutOfOrderCommands(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	// No worker is running, so the command cannot reach a pending step. The
	// point here is that the route exists, authenticates and validates.
	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + setup.instanceID + "/pairing/passkey/response",
		token:  setup.tenant.token,
		body:   map[string]any{"response": map[string]any{"id": "cred"}},
	}).problem(http.StatusServiceUnavailable, "SESSION_UNAVAILABLE")

	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + setup.instanceID + "/pairing/passkey/confirm",
		apiKey: setup.key,
	}).problem(http.StatusServiceUnavailable, "SESSION_UNAVAILABLE")
}

// An empty assertion is refused before it reaches a worker: there is nothing to
// forward, and the tenant needs to hear which field was wrong.
func TestPasskeyResponseRequiresABody(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + setup.instanceID + "/pairing/passkey/response",
		token:  setup.tenant.token,
		body:   map[string]any{},
	}).problem(http.StatusUnprocessableEntity, "VALIDATION_ERROR")
}
