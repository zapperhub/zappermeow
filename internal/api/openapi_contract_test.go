package api_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The published spec is generated from the handlers, so this asserts what the
// handlers actually declare rather than a document someone maintains by hand
// (principle IV). The security schemes are the part worth locking down: the
// verification route taking only an API key is a requirement (FR-025), and it
// sits in the same huma group as routes that accept both — one wrong line and
// it would silently start accepting tenant tokens.
func TestOpenAPIDeclaresTheConnectionExtras(t *testing.T) {
	f := newFixture(t)

	resp := f.do(request{method: http.MethodGet, path: "/openapi.json"})
	require.Equal(t, http.StatusOK, resp.Status)

	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string                `json:"operationId"`
			Security    []map[string][]string `json:"security"`
		} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &doc))

	both := []string{"apiKeyAuth", "bearerAuth"}
	keyOnly := []string{"apiKeyAuth"}

	tests := []struct {
		path        string
		method      string
		operationID string
		schemes     []string
	}{
		{"/instances/{instanceId}/proxy", "put", "set-instance-proxy", both},
		{"/instances/{instanceId}/proxy", "delete", "clear-instance-proxy", both},
		{"/instances/{instanceId}/passive-mode", "put", "set-instance-passive-mode", both},
		{"/instances/{instanceId}/pairing/passkey/response", "post", "submit-instance-passkey-response", both},
		{"/instances/{instanceId}/pairing/passkey/confirm", "post", "confirm-instance-passkey", both},
		{"/instances/{instanceId}/identity-verification-codes", "get", "get-instance-identity-verification-codes", keyOnly},
	}

	for _, tc := range tests {
		t.Run(tc.operationID, func(t *testing.T) {
			operations, ok := doc.Paths[tc.path]
			require.True(t, ok, "path %s is missing from the generated spec", tc.path)

			operation, ok := operations[tc.method]
			require.True(t, ok, "%s %s is missing from the generated spec", tc.method, tc.path)
			assert.Equal(t, tc.operationID, operation.OperationID)

			var schemes []string
			for _, requirement := range operation.Security {
				for name := range requirement {
					schemes = append(schemes, name)
				}
			}
			sort.Strings(schemes)
			assert.Equal(t, tc.schemes, schemes)
		})
	}
}
