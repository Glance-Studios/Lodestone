package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/api"
)

// Client talks to one lodestoned.
type Client struct {
	api   string
	token string
	http  *http.Client
}

// NewClient returns a Client for ctx.
//
// No overall timeout on the http.Client: a deploy legitimately runs for minutes,
// and a blanket timeout would sever it mid-rollout. Per-request deadlines come
// from the context instead, which lets short calls stay short.
func NewClient(ctx Context) *Client {
	return &Client{
		api:   strings.TrimSuffix(ctx.API, "/"),
		token: ctx.Token,
		http:  &http.Client{},
	}
}

// APIError is a non-2xx response from the server.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("server returned %d", e.Status)
	}
	return fmt.Sprintf("server returned %d: %s", e.Status, body)
}

// Unauthorized reports whether the server rejected the token.
func (e *APIError) Unauthorized() bool { return e.Status == http.StatusUnauthorized }

// Busy reports whether a deploy was already running.
func (e *APIError) Busy() bool { return e.Status == http.StatusLocked }

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// do sends req and returns the response only when it is a 2xx.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}

	if resp.StatusCode/100 != 2 {
		// Read a bounded amount: an error body should be a sentence, and a
		// misbehaving server should not be able to exhaust our memory.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode, Body: string(msg)}
	}
	return resp, nil
}

// Status fetches GET /status.
func (c *Client) Status(ctx context.Context) (api.Status, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/status", nil)
	if err != nil {
		return api.Status{}, err
	}

	resp, err := c.do(req)
	if err != nil {
		return api.Status{}, err
	}
	defer resp.Body.Close()

	var out api.Status
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return api.Status{}, fmt.Errorf("decode status: %w", err)
	}
	return out, nil
}

// Ledger fetches GET /artifacts.
func (c *Client) Ledger(ctx context.Context) ([]api.LedgerEntry, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/artifacts", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out []api.LedgerEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode ledger: %w", err)
	}
	return out, nil
}

// UploadOptions carries the metadata stamped onto a ledger entry.
type UploadOptions struct {
	Version string
	By      string
}

func (o UploadOptions) query() string {
	q := url.Values{}
	if o.Version != "" {
		q.Set("version", o.Version)
	}
	if o.By != "" {
		q.Set("by", o.By)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// Push uploads an artifact without deploying it.
func (c *Client) Push(ctx context.Context, r io.Reader, opts UploadOptions) (api.Artifact, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/artifacts"+opts.query(), r)
	if err != nil {
		return api.Artifact{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.do(req)
	if err != nil {
		return api.Artifact{}, err
	}
	defer resp.Body.Close()

	var out api.Artifact
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return api.Artifact{}, fmt.Errorf("decode upload response: %w", err)
	}
	return out, nil
}

// ErrNoResult reports a deploy stream that ended without a result line, which
// means the connection dropped mid-rollout - the deploy may still be running.
var ErrNoResult = errors.New("stream ended without a result")

// Deploy uploads an artifact and streams the rollout, calling onEvent for each
// event as it arrives. The returned Result is the stream's final line.
//
// Note the outcome comes from that line, not from the HTTP status: the server
// commits to 200 when it writes the first byte, long before it knows whether the
// rollout worked.
func (c *Client) Deploy(ctx context.Context, r io.Reader, opts UploadOptions, onEvent func(api.Event)) (api.Result, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/deploy"+opts.query(), r)
	if err != nil {
		return api.Result{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Accept", api.ContentTypeNDJSON)

	resp, err := c.do(req)
	if err != nil {
		return api.Result{}, err
	}
	defer resp.Body.Close()

	// A server that ignored the Accept header sent one object, not a stream.
	if !strings.Contains(resp.Header.Get("Content-Type"), api.ContentTypeNDJSON) {
		var out api.Result
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return api.Result{}, fmt.Errorf("decode deploy response: %w", err)
		}
		for _, e := range out.Events {
			if onEvent != nil {
				onEvent(e)
			}
		}
		return out, nil
	}

	return readStream(resp.Body, onEvent)
}

// readStream consumes NDJSON lines until the result line or EOF.
func readStream(body io.Reader, onEvent func(api.Event)) (api.Result, error) {
	sc := bufio.NewScanner(body)
	// Raise the line limit: a rollout message carrying a Kubernetes condition can
	// exceed bufio's 64 KiB default, and the failure mode would be a truncated
	// deploy report rather than an obvious error.
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	var (
		result api.Result
		seen   bool
	)

	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		// Read the discriminator before committing to a type.
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			return result, fmt.Errorf("malformed stream line: %w", err)
		}

		switch probe.Kind {
		case api.KindEvent:
			var e api.Event
			if err := json.Unmarshal(line, &e); err != nil {
				return result, fmt.Errorf("decode event: %w", err)
			}
			if onEvent != nil {
				onEvent(e)
			}
		case api.KindResult:
			if err := json.Unmarshal(line, &result); err != nil {
				return result, fmt.Errorf("decode result: %w", err)
			}
			seen = true
		default:
			// Forward compatibility: a newer server may add line kinds, and an
			// older client should ignore them rather than fail.
			continue
		}
	}

	if err := sc.Err(); err != nil {
		return result, fmt.Errorf("read stream: %w", err)
	}
	if !seen {
		return result, ErrNoResult
	}
	return result, nil
}

// shortTimeout bounds the quick read-only calls. Deploys deliberately have none.
const shortTimeout = 30 * time.Second

// WithShortTimeout returns a context for a quick call.
func WithShortTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, shortTimeout)
}
