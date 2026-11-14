package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireBearerToken wraps next so that every request must carry
// Authorization: Bearer <token>. An empty token returns next unwrapped, so
// a deployment that configures no token runs with authentication off and
// pays nothing per request.
//
// Wrap the handler NewHandler returns, and do it inside any
// http.StripPrefix, so that the /__health exemption below sees the same
// path NewHandler routes on.
//
// Two kinds of request stay open even with a token set. Closing either one
// breaks a working deployment without protecting anything:
//
//   - GET /__health. Fly's http_service check polls it every 15s
//     (fly.toml). A 401 there marks the machine unhealthy and restarts it
//     in a loop.
//   - OPTIONS. Browsers send a CORS preflight with no Authorization
//     header, so a 401 kills the real request before it is ever sent.
//     internal/ds answers the preflight with Access-Control-Allow-Headers:
//     *, which is what lets the real request carry the token.
func RequireBearerToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || r.URL.Path == healthPath {
			next.ServeHTTP(w, r)
			return
		}
		scheme, presented, split := strings.Cut(r.Header.Get("Authorization"), " ")
		// Only the compare touches the secret, and a wrong token and a
		// missing one return the same 401. The || does skip the compare on
		// a malformed header, which reveals only the shape the caller
		// already sent. Comparing lengths here first would put the token's
		// length back on the clock.
		if !split || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(want, []byte(presented)) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	h := w.Header()
	h.Set("WWW-Authenticate", "Bearer")
	// Matches the Access-Control-Allow-Origin internal/ds sets on every
	// other response. Without it a cross-origin fetch sees an opaque
	// network error instead of the 401.
	h.Set("Access-Control-Allow-Origin", "*")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
