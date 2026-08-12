package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Compile-time proof that each type satisfies Check. Nothing in the types
// themselves refers to the interface, so this assertion is how you pin the
// contract - it fails to build if a method is renamed or its signature drifts.
var (
	_ Check = HTTPCheck{}
	_ Check = TCPCheck{}
	_ Check = ExecCheck{}
)

// -- HTTP ---------------------------------------------------------------------

func TestHTTPCheck(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{
			name:    "200 is healthy",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
		},
		{
			name:    "204 is healthy",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
		},
		{
			name:    "500 is not",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
			wantErr: true,
		},
		{
			name:    "404 is not",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// httptest.NewServer is a real HTTP server on a real port, torn down
			// by Close. No mocking of the transport.
			ts := httptest.NewServer(tt.handler)
			defer ts.Close()

			err := HTTPCheck{URL: ts.URL}.Check(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Check() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHTTPCheckCustomAcceptStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer ts.Close()

	c := HTTPCheck{
		URL:          ts.URL,
		AcceptStatus: func(code int) bool { return code == http.StatusTeapot },
	}
	if err := c.Check(context.Background()); err != nil {
		t.Errorf("Check() error = %v, want nil for the accepted status", err)
	}
}

func TestHTTPCheckUnreachable(t *testing.T) {
	// Start a server then close it, so the port is certainly not listening.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()

	if err := (HTTPCheck{URL: url}).Check(context.Background()); err == nil {
		t.Error("Check() error = nil, want a connection failure")
	}
}

// -- TCP ----------------------------------------------------------------------

func TestTCPCheck(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()

	// ts.Listener.Addr() is the real host:port the test server bound.
	addr := ts.Listener.Addr().String()

	if err := (TCPCheck{Addr: addr}).Check(context.Background()); err != nil {
		t.Errorf("Check() on a listening port error = %v, want nil", err)
	}
}

func TestTCPCheckNothingListening(t *testing.T) {
	// Port 1 on loopback: reserved and never listening.
	err := TCPCheck{Addr: "127.0.0.1:1", Timeout: 2 * time.Second}.Check(context.Background())
	if err == nil {
		t.Error("Check() error = nil, want a dial failure")
	}
}

// -- Exec ---------------------------------------------------------------------

func TestExecCheck(t *testing.T) {
	// Pick a command that exists on the host running the test.
	ok := ExecCheck{Name: "cmd", Args: []string{"/c", "exit 0"}}
	bad := ExecCheck{Name: "cmd", Args: []string{"/c", "exit 1"}}
	if runtime.GOOS != "windows" {
		ok = ExecCheck{Name: "true"}
		bad = ExecCheck{Name: "false"}
	}

	if err := ok.Check(context.Background()); err != nil {
		t.Errorf("exit 0: Check() error = %v, want nil", err)
	}
	if err := bad.Check(context.Background()); err == nil {
		t.Error("exit 1: Check() error = nil, want a failure")
	}
}

func TestExecCheckMissingBinary(t *testing.T) {
	c := ExecCheck{Name: "definitely-not-a-real-binary-xyz"}
	if err := c.Check(context.Background()); err == nil {
		t.Error("Check() error = nil, want a not-found failure")
	}
}

// -- fakes, to test CheckAll without any I/O ---------------------------------

// fakeCheck satisfies Check without inheriting from anything. This is the point
// of implicit satisfaction: a test double is just a struct with the methods.
type fakeCheck struct {
	name  string
	delay time.Duration
	err   error
	calls *atomic.Int32
}

func (f fakeCheck) Describe() string { return f.name }

func (f fakeCheck) Check(ctx context.Context) error {
	if f.calls != nil {
		f.calls.Add(1)
	}
	select {
	case <-time.After(f.delay):
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCheckAllPasses(t *testing.T) {
	checks := []Check{fakeCheck{name: "a"}, fakeCheck{name: "b"}, fakeCheck{name: "c"}}

	if err := CheckAll(context.Background(), checks); err != nil {
		t.Errorf("CheckAll() = %v, want nil", err)
	}
}

func TestCheckAllNoChecks(t *testing.T) {
	if err := CheckAll(context.Background(), nil); err != nil {
		t.Errorf("CheckAll(nil) = %v, want nil", err)
	}
}

func TestCheckAllReportsEveryFailure(t *testing.T) {
	boom := errors.New("connection refused")
	nope := errors.New("bad status")

	checks := []Check{
		fakeCheck{name: "healthy"},
		fakeCheck{name: "broken-1", err: boom},
		fakeCheck{name: "broken-2", err: nope},
	}

	err := CheckAll(context.Background(), checks)
	if err == nil {
		t.Fatal("CheckAll() = nil, want an error")
	}

	// errors.Join keeps every cause reachable, so both are still findable.
	if !errors.Is(err, boom) {
		t.Errorf("error does not wrap boom: %v", err)
	}
	if !errors.Is(err, nope) {
		t.Errorf("error does not wrap nope: %v", err)
	}
}

func TestCheckAllRunsConcurrently(t *testing.T) {
	var calls atomic.Int32
	const delay = 100 * time.Millisecond

	checks := []Check{
		fakeCheck{name: "a", delay: delay, calls: &calls},
		fakeCheck{name: "b", delay: delay, calls: &calls},
		fakeCheck{name: "c", delay: delay, calls: &calls},
	}

	start := time.Now()
	if err := CheckAll(context.Background(), checks); err != nil {
		t.Fatalf("CheckAll() = %v, want nil", err)
	}
	elapsed := time.Since(start)

	if n := calls.Load(); n != 3 {
		t.Fatalf("%d checks ran, want 3", n)
	}
	// Sequentially this is 300ms; concurrently ~100ms.
	if elapsed > 250*time.Millisecond {
		t.Errorf("took %v, want under 250ms - checks should run concurrently", elapsed)
	}
}

func TestCheckAllRespectsContext(t *testing.T) {
	checks := []Check{fakeCheck{name: "slow", delay: 5 * time.Second}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := CheckAll(ctx, checks)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("CheckAll() = %v, want context.DeadlineExceeded in the chain", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v - should return when ctx is done, not wait it out", elapsed)
	}
}
