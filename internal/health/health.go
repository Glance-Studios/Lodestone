// Package health probes whether a deployed workload is actually working.
//
// One contract, three implementations. None of HTTPCheck, TCPCheck or ExecCheck
// mentions the Check interface - having the method is what satisfies it.
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Check is the whole contract: probe something, report why it is unhealthy.
type Check interface {
	// Check returns nil if healthy. It must respect ctx cancellation.
	Check(ctx context.Context) error

	// Describe names the check for logs and errors.
	Describe() string
}

// DefaultTimeout bounds a single probe when a caller sets none.
const DefaultTimeout = 5 * time.Second

// -- HTTP ---------------------------------------------------------------------

// HTTPCheck probes an HTTP endpoint and requires an acceptable status code.
type HTTPCheck struct {
	URL string

	// AcceptStatus reports whether a status code counts as healthy. Nil means
	// any 2xx is healthy.
	AcceptStatus func(code int) bool

	// Client is optional; nil uses a client bounded by DefaultTimeout.
	Client *http.Client
}

func (c HTTPCheck) Describe() string { return "http " + c.URL }

func (c HTTPCheck) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", c.URL, err)
	}
	defer resp.Body.Close()

	accept := c.AcceptStatus
	if accept == nil {
		accept = func(code int) bool { return code >= 200 && code < 300 }
	}
	if !accept(resp.StatusCode) {
		return fmt.Errorf("get %s: status %d", c.URL, resp.StatusCode)
	}
	return nil
}

// -- TCP ----------------------------------------------------------------------

// TCPCheck probes that something is accepting connections at Addr ("host:port").
type TCPCheck struct {
	Addr    string
	Timeout time.Duration // zero means DefaultTimeout
}

func (c TCPCheck) Describe() string { return "tcp " + c.Addr }

func (c TCPCheck) Check(ctx context.Context) error {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	// A Dialer with a context, so a cancelled ctx abandons the dial.
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.Addr, err)
	}
	return conn.Close()
}

// -- Exec ---------------------------------------------------------------------

// ExecCheck runs a command and treats a zero exit status as healthy.
type ExecCheck struct {
	Name string
	Args []string
}

func (c ExecCheck) Describe() string {
	return "exec " + strings.Join(append([]string{c.Name}, c.Args...), " ")
}

func (c ExecCheck) Check(ctx context.Context) error {
	// CommandContext kills the process if ctx is cancelled - without it a hung
	// command would outlive the check.
	out, err := exec.CommandContext(ctx, c.Name, c.Args...).CombinedOutput()
	if err != nil {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			return fmt.Errorf("run %s: %w: %s", c.Name, err, trimmed)
		}
		return fmt.Errorf("run %s: %w", c.Name, err)
	}
	return nil
}

// -- Running them -------------------------------------------------------------

// CheckAll runs every check concurrently and returns all failures joined, or nil
// if they all pass. It abandons the remaining work if ctx is cancelled.
func CheckAll(ctx context.Context, checks []Check) error {
	if len(checks) == 0 {
		return nil
	}

	errs := make([]error, len(checks))

	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine writes its own index, so no lock is needed.
			if err := c.Check(ctx); err != nil {
				errs[i] = fmt.Errorf("%s: %w", c.Describe(), err)
			}
		}()
	}
	wg.Wait()

	// errors.Join drops nils and returns nil if every entry is nil. The result
	// still unwraps to each cause, so errors.Is works through it.
	return errors.Join(errs...)
}
