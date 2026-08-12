package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// freePort asks the OS for an unused port, so tests never collide.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// serveWith runs serve() in a goroutine and returns its address plus a function
// that triggers the shutdown path and waits for serve to return.
func serveWith(t *testing.T, h http.Handler) (addr string, shutdown func() error) {
	t.Helper()

	port := freePort(t)
	addr = fmt.Sprintf("127.0.0.1:%d", port)

	srv := &http.Server{Addr: addr, Handler: h}
	done := make(chan error, 1)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			done <- err
			return
		}
		done <- nil
	}()

	// Wait for it to actually accept connections before returning.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return addr, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			return err
		}
		return <-done
	}
}

// The point of graceful shutdown: a request already being handled must finish,
// not be severed. A deploy holds its request open for the whole rollout.
func TestShutdownWaitsForInFlightRequest(t *testing.T) {
	const workFor = 400 * time.Millisecond

	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(workFor) // stands in for a rollout
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "finished")
	})

	addr, shutdown := serveWith(t, handler)

	// Fire a slow request, then shut down while it is still running.
	type result struct {
		code int
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		res <- result{code: resp.StatusCode}
	}()

	<-started // the handler is definitely running now

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- shutdown() }()

	got := <-res
	if got.err != nil {
		t.Fatalf("in-flight request failed: %v - shutdown severed it", got.err)
	}
	if got.code != http.StatusOK {
		t.Errorf("in-flight request got %d, want 200", got.code)
	}

	if err := <-shutdownDone; err != nil {
		t.Errorf("shutdown returned %v, want nil", err)
	}
}

// After shutdown starts, new connections must be refused rather than queued.
func TestShutdownStopsAcceptingNewRequests(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr, shutdown := serveWith(t, handler)

	// It serves before shutdown.
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("pre-shutdown request failed: %v", err)
	}
	resp.Body.Close()

	if err := shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// And refuses afterwards.
	if _, err := http.Get("http://" + addr + "/"); err == nil {
		t.Error("a request succeeded after shutdown; the listener is still open")
	}
}

// A port already in use must be reported, not swallowed - serve returns the
// listen error rather than waiting forever for a signal.
func TestServeReportsListenFailure(t *testing.T) {
	// Hold a port so the server cannot bind it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer l.Close()

	errs := make(chan error, 1)
	go func() {
		errs <- serve(l.Addr().String(), http.NewServeMux())
	}()

	select {
	case err := <-errs:
		if err == nil {
			t.Error("serve() = nil for an occupied port, want an error")
		}
	case <-time.After(5 * time.Second):
		t.Error("serve() hung on an occupied port instead of returning the error")
	}
}
