package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireBearer(t *testing.T) {
	const token = "fixed-test-bearer-token"
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	tests := []struct {
		name   string
		header string
		token  string
		want   int
	}{
		{"no header", "", token, http.StatusUnauthorized},
		{"wrong scheme", "Basic " + token, token, http.StatusUnauthorized},
		{"wrong token", "Bearer nope", token, http.StatusUnauthorized},
		{"prefix of the token", "Bearer fixed-test", token, http.StatusUnauthorized},
		{"token plus suffix", "Bearer fixed-test-bearer-token-extra", token, http.StatusUnauthorized},
		{"empty configured token rejects everything", "Bearer ", "", http.StatusUnauthorized},
		{"correct", "Bearer " + token, token, http.StatusNoContent},
		{"correct, extra spaces", "Bearer   " + token + "  ", token, http.StatusNoContent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/punches/desired", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			RequireBearer(tc.token, ok).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("a 401 must tell the caller the scheme")
			}
		})
	}
}
