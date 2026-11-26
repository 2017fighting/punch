package controller

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireBearer wraps a handler with the bearer-token check the HTTPRoute's design
// depends on: the route has no edge authentication (the caller is a shell script), so
// the token is the whole credential and it is validated on EVERY request — including
// the desired list, which ADR-0044 treats as semi-public but not anonymous.
//
// The compare is constant-time and length-safe: crypto/subtle returns 0 for differing
// lengths without an early exit, so neither the token nor its length leaks through
// timing. An empty configured token rejects everything — `cmd/punch-controller` refuses
// to start in that case; this is the second line.
func RequireBearer(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearerFromHeader(r.Header.Get("Authorization"))
		if !ok || len(want) == 0 {
			unauthorized(w)
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerFromHeader(h string) (string, bool) {
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="punch"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
