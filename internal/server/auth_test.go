package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireToken(t *testing.T) {
	const secret = "s3cr3t-token"

	tests := []struct {
		name       string
		header     string
		wantStatus int
		wantReach  bool // did the wrapped handler run?
	}{
		{"no auth header", "", http.StatusUnauthorized, false},
		{"wrong token", "Bearer nope", http.StatusUnauthorized, false},
		{"malformed header", "s3cr3t-token", http.StatusUnauthorized, false},
		{"correct token", "Bearer s3cr3t-token", http.StatusOK, true},
		{"scheme is case-insensitive", "bearer s3cr3t-token", http.StatusOK, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})
			protected := RequireToken(secret)(next)

			req := httptest.NewRequest(http.MethodPost, "/artifacts", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()

			protected.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("code = %d, want %d", rec.Code, tt.wantStatus)
			}
			if reached != tt.wantReach {
				t.Errorf("wrapped handler reached = %v, want %v", reached, tt.wantReach)
			}
		})
	}
}

// An empty expected token must never authorize, even against an empty presented
// token - the fail-closed guarantee.
func TestRequireTokenEmptyExpectedDeniesAll(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("wrapped handler ran despite no configured token")
	})
	protected := RequireToken("")(next)

	req := httptest.NewRequest(http.MethodPost, "/artifacts", nil)
	req.Header.Set("Authorization", "Bearer ") // empty token
	rec := httptest.NewRecorder()

	protected.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
