package middleware

import (
	"net/http"
	"slices"
	"strings"
)

// CORS answers browser preflights for the origins the deployment allows.
//
// It is deliberately not permissive by default: with an empty allowlist the
// middleware adds nothing, so a server-to-server deployment stays exactly as it
// was. The list exists because the platform already commits to browser clients
// — the WebSocket authenticates through a subprotocol precisely so a page can
// connect — and a browser that can open the channel but cannot call the routes
// is a promise half kept.
//
// Credentials are never allowed: this API authenticates with an explicit
// Authorization or X-Api-Key header, never with cookies, so there is no ambient
// authority for another site to borrow. That is what keeps an allowed origin
// from becoming a cross-site request forgery vector.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	if len(allowedOrigins) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// Not a browser request; nothing to negotiate.
				next.ServeHTTP(w, r)
				return
			}

			if !originAllowed(origin, allowedOrigins) {
				// An origin outside the list gets no CORS headers, and the
				// browser blocks the response on its own. Preflights are
				// answered plainly so the failure is a clean CORS error rather
				// than a confusing 404 or 405.
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			header := w.Header()
			// Echoing the origin rather than "*" keeps the response accurate
			// for caches, which must vary on the request origin.
			header.Set("Access-Control-Allow-Origin", origin)
			header.Add("Vary", "Origin")

			if r.Method == http.MethodOptions {
				header.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, "+APIKeyHeader)
				header.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// originAllowed matches an origin against the allowlist, accepting a bare host
// as shorthand for both schemes on that host — "localhost:8090" covers the
// http and https forms, which is what an operator means when they write it.
func originAllowed(origin string, allowed []string) bool {
	if slices.Contains(allowed, origin) {
		return true
	}

	host := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	return slices.Contains(allowed, host)
}
