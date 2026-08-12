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
	var res Result

	for e := range events {
		res.Events = append(res.Events, e)

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
