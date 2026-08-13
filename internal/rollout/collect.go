package rollout

import (
	"errors"
	"fmt"
)

// Result is the outcome of a deployment plus every event it emitted.
type Result struct {
	Events []Event
	Err    error
}

// Succeeded reports whether the deployment finished healthy.
func (r Result) Succeeded() bool { return r.Err == nil }

// Collect drains an event channel to completion and reports the outcome. Use it
// when you want the whole story rather than live progress.
func Collect(events <-chan Event) Result {
	return CollectFunc(events, nil)
}

// CollectFunc drains events, calling fn for each one as it arrives, and reports
// the outcome. It is how a caller streams progress without reimplementing the
// success-or-failure rules.
//
// If fn returns an error - a client that hung up mid-stream, say - it is not
// called again, but the channel is still drained to completion so the rollout is
// never left with an unread event.
func CollectFunc(events <-chan Event, fn func(Event) error) Result {
	var res Result

	for e := range events {
		res.Events = append(res.Events, e)

		if fn != nil {
			if err := fn(e); err != nil {
				fn = nil // stop emitting, keep draining
			}
		}

		switch e.Phase {
		case PhaseFailed:
			err := errors.New(e.Message)
			if e.Err != "" {
				err = fmt.Errorf("%s: %s", e.Message, e.Err)
			}
			res.Err = err
		case PhaseSucceeded:
			res.Err = nil
		}
	}

	return res
}
