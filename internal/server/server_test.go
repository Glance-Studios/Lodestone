package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatusOK(t *testing.T) {
	srv := New("test-1.2.3", "", newTestStore(t))

	// httptest builds a request and records the response in memory - no socket
	// is opened, so the test is fast and needs no free port
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusOK)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got Status
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}

	if got.Status != "ok" {
		t.Errorf("status field = %q, want ok", got.Status)
	}
	if got.Version != "test-1.2.3" {
		t.Errorf("version field = %q, want test-1.2.3", got.Version)
	}
}

func TestStatusRejectsPost(t *testing.T) {
	srv := New("test", "", newTestStore(t))

	req := httptest.NewRequest(http.MethodPost, "/status", nil)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// The "GET /status" pattern makes the mux answer 405 for other methods -
	// we never wrote that check ourselves
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /status code = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
