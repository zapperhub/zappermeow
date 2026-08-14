package domain

import (
	"net/url"
	"strings"
)

// MaxProxyURLLength is a sanity bound. No legitimate proxy address comes close;
// the limit exists so a pathological value cannot be parked in the column.
const MaxProxyURLLength = 1024

// proxySchemes are the schemes the library can actually dial. Anything else is
// rejected at write time rather than at connect time: the tenant finds out when
// they configure the proxy, not when a session silently fails to come up.
//
// The set mirrors what SetProxyAddress accepts (research R1) — http and https
// go through an HTTP proxy, socks5 through a SOCKS dialer.
var proxySchemes = map[string]struct{}{
	"http":   {},
	"https":  {},
	"socks5": {},
}

// NormalizeProxyURL trims surrounding whitespace. An empty result means "no
// proxy": that is how a tenant clears the setting through the same field.
func NormalizeProxyURL(raw string) string { return strings.TrimSpace(raw) }

// ValidateProxyURL checks an egress proxy address, reporting the offending
// request member. An empty value is not accepted here — clearing the proxy is a
// separate operation, so an empty string in a set request is a mistake worth
// naming rather than a silent removal.
func ValidateProxyURL(location, raw string) error {
	trimmed := NormalizeProxyURL(raw)
	if trimmed == "" {
		return ErrValidation(location, "must not be empty")
	}
	if len(trimmed) > MaxProxyURLLength {
		return ErrValidation(location, "proxy URL is too long")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ErrInvalidProxyURL()
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "" || parsed.Host == "" {
		return ErrInvalidProxyURL()
	}
	if _, ok := proxySchemes[scheme]; !ok {
		return ErrUnsupportedProxyScheme()
	}
	// Hostname() strips the port, so "http://:3128" is caught here rather than
	// producing a dialable-looking URL with no host.
	if parsed.Hostname() == "" {
		return ErrInvalidProxyURL()
	}
	return nil
}

// MaskProxyURL replaces the password with a fixed marker, keeping scheme, user,
// host and port readable. Every path that shows a proxy to a tenant — responses,
// events, the trail, logs — goes through here (FR-007).
//
// A value that cannot be parsed is reported as a placeholder rather than echoed:
// an unparseable string still came from a field that may hold a secret.
func MaskProxyURL(raw string) string {
	trimmed := NormalizeProxyURL(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "***"
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(parsed.User.Username(), "***")
		}
	}
	// url.URL.String() percent-encodes the userinfo, which would turn the
	// marker into %2A%2A%2A and make the masked form harder to read.
	return strings.Replace(parsed.String(), "%2A%2A%2A", "***", 1)
}
