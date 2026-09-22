// SPDX-License-Identifier: LGPL-3.0-or-later

package transport

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Call is one request a Recorder was asked to run.
type Call struct {
	Target  Target
	Request Request
	// Command is the shell command line the request renders to, which is
	// what --dry-run prints and what the tests assert on.
	Command string
}

// Recorder is a Runner that records requests instead of running them.
//
// It backs --dry-run, where the point is to show exactly what would be sent,
// and the tests, where a real ssh is neither available nor wanted.
type Recorder struct {
	// Responses are returned in order; once they run out, an empty
	// successful result is returned. A response may be keyed to a target
	// name through Reply instead.
	Responses []*Result
	// Reply returns the result for a call. It wins over Responses.
	Reply func(Target, Request) (*Result, error)

	mu    sync.Mutex
	calls []Call
	next  int
}

// Run implements Runner.
func (r *Recorder) Run(_ context.Context, target Target, req Request) (*Result, error) {
	command, err := RemoteCommand(req)
	if err != nil {
		return nil, err
	}
	// The payload is read so that a caller streaming a secret over stdin
	// behaves the same as it would against a real host.
	if req.Stdin != nil {
		if _, err := io.Copy(io.Discard, req.Stdin); err != nil {
			return nil, err
		}
	}

	r.mu.Lock()
	r.calls = append(r.calls, Call{Target: target, Request: req, Command: command})
	index := r.next
	r.next++
	r.mu.Unlock()

	if r.Reply != nil {
		return r.Reply(target, req)
	}
	if index < len(r.Responses) {
		result := *r.Responses[index]
		result.Target = target
		return &result, nil
	}
	return &Result{Target: target}, nil
}

// Calls returns the recorded requests.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// Commands returns the rendered command line of each recorded request.
func (r *Recorder) Commands() []string {
	out := make([]string, 0, len(r.Calls()))
	for _, c := range r.Calls() {
		out = append(out, c.Command)
	}
	return out
}

// Describe renders a call the way --dry-run prints it.
func (c Call) Describe() string {
	if c.Command == "" {
		return fmt.Sprintf("%s: interactive login", c.Target)
	}
	return fmt.Sprintf("%s: %s", c.Target, c.Command)
}
