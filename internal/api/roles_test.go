package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoleMatrix(t *testing.T) {
	srv := NewServer(Config{
		APIKey: "admin-key-000000000000000", AgentAPIKey: "agent-key-000000000000000", ExecutorAPIKey: "executor-key-000000000000",
		RateLimitPerMinute: 1000,
	}).Handler()
	cases := []struct {
		method, path, key string
		want              int
	}{
		{"POST", "/api/v1/routes/execute", "agent-key-000000000000000", http.StatusForbidden},
		{"POST", "/api/v1/routes/execute", "admin-key-000000000000000", http.StatusForbidden},
		{"POST", "/api/v1/routes/execute", "", http.StatusUnauthorized},
		{"POST", "/api/v1/routes/execute", "executor-key-000000000000", http.StatusServiceUnavailable}, // allowed, routing not configured
		{"POST", "/api/v1/routes/quote", "agent-key-000000000000000", http.StatusServiceUnavailable},
		{"GET", "/api/v1/provider-accounts", "agent-key-000000000000000", http.StatusUnauthorized},
		{"GET", "/api/v1/provider-accounts", "executor-key-000000000000", http.StatusUnauthorized},
		{"POST", "/api/v1/reconcile/midtrans/WP-1", "agent-key-000000000000000", http.StatusForbidden},
		{"GET", "/api/v1/outbound-events", "executor-key-000000000000", http.StatusForbidden},
		{"GET", "/api/v1/outbound-events", "admin-key-000000000000000", http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		if c.key != "" {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s with %q = %d, want %d (%s)", c.method, c.path, c.key, rec.Code, c.want, rec.Body.String())
		}
	}
}
