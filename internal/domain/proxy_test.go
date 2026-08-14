package domain_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
)

func TestValidateProxyURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		url      string
		wantCode domain.Code
	}{
		{name: "http without credentials", url: "http://proxy.internal:3128"},
		{name: "https with credentials", url: "https://user:s3cret@proxy.internal:8443"},
		{name: "socks5 by address", url: "socks5://203.0.113.10:1080"},
		{name: "socks5 with credentials", url: "socks5://user:s3cret@203.0.113.10:1080"},
		{name: "surrounding whitespace is tolerated", url: "  http://proxy.internal:3128  "},

		{name: "empty", url: "", wantCode: domain.CodeValidation},
		{name: "only whitespace", url: "   ", wantCode: domain.CodeValidation},
		{name: "too long", url: "http://" + strings.Repeat("a", domain.MaxProxyURLLength) + ".test", wantCode: domain.CodeValidation},

		{name: "no scheme", url: "proxy.internal:3128", wantCode: domain.CodeInvalidProxyURL},
		{name: "no host", url: "http://", wantCode: domain.CodeInvalidProxyURL},
		{name: "port without host", url: "http://:3128", wantCode: domain.CodeInvalidProxyURL},
		{name: "control character", url: "http://proxy\x7f.internal", wantCode: domain.CodeInvalidProxyURL},

		{name: "ftp scheme", url: "ftp://proxy.internal:21", wantCode: domain.CodeUnsupportedProxyScheme},
		{name: "socks4 scheme", url: "socks4://203.0.113.10:1080", wantCode: domain.CodeUnsupportedProxyScheme},
		{name: "socks5h is not accepted by the dialer", url: "socks5h://203.0.113.10:1080", wantCode: domain.CodeUnsupportedProxyScheme},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := domain.ValidateProxyURL("body.url", tc.url)
			if tc.wantCode == "" {
				require.NoError(t, err)
				return
			}

			domainErr, ok := domain.AsError(err)
			require.True(t, ok, "expected a domain error, got %v", err)
			require.Equal(t, tc.wantCode, domainErr.Code)
		})
	}
}

func TestValidateProxyURLIsCaseInsensitiveOnScheme(t *testing.T) {
	t.Parallel()

	require.NoError(t, domain.ValidateProxyURL("body.url", "HTTP://proxy.internal:3128"))
	require.NoError(t, domain.ValidateProxyURL("body.url", "SOCKS5://203.0.113.10:1080"))
}

func TestMaskProxyURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "empty stays empty", url: "", want: ""},
		{name: "no credentials is unchanged", url: "http://proxy.internal:3128", want: "http://proxy.internal:3128"},
		{name: "user without password is kept", url: "http://user@proxy.internal:3128", want: "http://user@proxy.internal:3128"},
		{name: "password is replaced", url: "socks5://user:s3cret@203.0.113.10:1080", want: "socks5://user:***@203.0.113.10:1080"},
		{name: "empty password is still masked", url: "http://user:@proxy.internal:3128", want: "http://user:***@proxy.internal:3128"},
		{name: "unparseable becomes a placeholder", url: "http://proxy\x7f.internal", want: "***"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, domain.MaskProxyURL(tc.url))
		})
	}
}

// The masked form is what every response, event, log line and trail entry
// carries, so the property that matters is not the exact shape — it is that no
// password survives the transformation, whatever the password looks like.
//
// URLs are built through url.UserPassword because that is what a correct client
// sends: a password holding '#', '?', '/' or a space must be percent-encoded to
// be part of a URL at all. Both the plain and the encoded spelling must be
// absent from the result — masking the raw form while leaking the encoded one
// would still hand over the secret.
func TestMaskProxyURLNeverEchoesThePassword(t *testing.T) {
	t.Parallel()

	passwords := []string{
		"s3cret", "hunter2", "p@ss:word", "with/slash", "with?query", "with#hash",
		"with space", "áçèñtëd", "0123456789abcdefghijklmnopqrstuvwxyz",
	}

	for _, password := range passwords {
		t.Run(password, func(t *testing.T) {
			t.Parallel()

			raw := (&url.URL{
				Scheme: "socks5",
				User:   url.UserPassword("user", password),
				Host:   "203.0.113.10:1080",
			}).String()
			require.NoError(t, domain.ValidateProxyURL("body.url", raw))

			masked := domain.MaskProxyURL(raw)

			require.NotContains(t, masked, password)
			require.NotContains(t, masked, url.QueryEscape(password))
			require.NotContains(t, masked, url.PathEscape(password))
			require.Contains(t, masked, "***")
			require.Contains(t, masked, "203.0.113.10:1080", "host must stay readable for diagnosis")
		})
	}
}

// A password that was not percent-encoded makes the whole URL unparseable. The
// safe direction is to report a placeholder rather than echo a string that may
// still hold the secret, even at the cost of losing the host.
func TestMaskProxyURLDegradesSafelyOnMalformedInput(t *testing.T) {
	t.Parallel()

	masked := domain.MaskProxyURL("socks5://user:with space@203.0.113.10:1080")

	require.Equal(t, "***", masked)
	require.NotContains(t, masked, "with space")
}

func TestInstanceMaskedProxyURL(t *testing.T) {
	t.Parallel()

	var noProxy domain.Instance
	require.Empty(t, noProxy.MaskedProxyURL())

	raw := "socks5://user:s3cret@203.0.113.10:1080"
	withProxy := domain.Instance{ProxyURL: &raw}
	require.Equal(t, "socks5://user:***@203.0.113.10:1080", withProxy.MaskedProxyURL())
	require.NotContains(t, withProxy.MaskedProxyURL(), "s3cret")
}
