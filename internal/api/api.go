// Package api holds the wire types shared by the server and the CLI.
//
// One definition, imported by both sides, so the contract cannot drift. Nothing
// here does any work; it is the shape of the bytes on the wire.
package api

import "time"

// ContentTypeNDJSON is the media type for a newline-delimited JSON stream. A
// client sends it in Accept to ask for streamed progress.
const ContentTypeNDJSON = "application/x-ndjson"

// Kinds of line in a streamed response.
const (
	KindEvent  = "event"
	KindResult = "result"
)

// Event is one step of a rollout's progress.
type Event struct {
	Kind    string    `json:"kind"` // always KindEvent
	Phase   string    `json:"phase"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
	Error   string    `json:"error,omitempty"`
}

// Result is the outcome of a deploy. In a stream it is the final line; in a
// non-streamed response it is the whole body, with Events filled in.
type Result struct {
	Kind   string `json:"kind,omitempty"`   // KindResult when streamed
	Target string `json:"target,omitempty"` // which workload this deployed to
	Digest string `json:"digest"`           // the artifact's sha256
	Image  string `json:"image"`            // the pushed image reference

	// BaseImage is the base it was appended onto, pinned by digest even when the
	// target names a moving tag - so "which world was this built on?" stays
	// answerable after the tag moves.
	BaseImage string `json:"baseImage,omitempty"`

	Replicas *int32  `json:"replicas,omitempty"`
	Deployed bool    `json:"deployed"` // did the rollout succeed
	Error    string  `json:"error,omitempty"`
	Events   []Event `json:"events,omitempty"` // non-streamed responses only
}

// Artifact is what an upload reports back.
type Artifact struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// LedgerEntry is one line of the ledger as served over HTTP.
type LedgerEntry struct {
	// Seq names this entry. Two entries can share every other field - the same jar
	// republished onto a newer base - so this is the only way a client can say
	// which one it means.
	Seq uint64 `json:"seq"`

	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Target    string    `json:"target,omitempty"`
	Version   string    `json:"version,omitempty"`
	By        string    `json:"by,omitempty"`
	At        time.Time `json:"at"`
	Replicas  *int32    `json:"replicas,omitempty"`
	Deployed  bool      `json:"deployed"`
	Image     string    `json:"image,omitempty"`
	BaseImage string    `json:"baseImage,omitempty"`
}

// Status is the body of GET /status.
type Status struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Uptime  string `json:"uptime"`

	// Targets names the configured workloads, sorted, so a client can discover
	// what exists without a separate call.
	Targets []string `json:"targets,omitempty"`
}
