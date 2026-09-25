// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/GSI-HPC/clusterctl/nodeset"
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
	// Responses are returned in the order the calls arrive; once they run
	// out, an empty successful result is returned. A fan-out makes its
	// calls in any order, so Responses suit calls made one after another,
	// and ByTarget the calls of a fan-out.
	Responses []*Result
	// ByTarget holds the result for every call to a target, by the
	// target's name, whatever order the calls arrive in; nil is an empty
	// successful result. The calls to a target it does not name take
	// Responses.
	ByTarget map[string]*Result
	// Reply returns the result for a call. It wins over ByTarget and
	// Responses. It is called from as many goroutines at once as the
	// caller runs requests, so under a fan-out it must be safe for
	// concurrent use, and lock whatever it keeps between calls.
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
	// behaves the same as it would against a real host. Reply is handed a
	// copy, so a test can see what arrived; the call does not keep it.
	var payload []byte
	if req.Stdin != nil {
		if payload, err = io.ReadAll(req.Stdin); err != nil {
			return nil, err
		}
	}

	prepared, keyed := r.ByTarget[target.Name]
	r.mu.Lock()
	r.calls = append(r.calls, Call{Target: target, Request: req, Command: command})
	index := r.next
	if !keyed {
		r.next++
	}
	r.mu.Unlock()

	if r.Reply != nil {
		if req.Stdin != nil {
			req.Stdin = bytes.NewReader(payload)
		}
		return r.Reply(target, req)
	}
	if keyed {
		var result Result
		if prepared != nil {
			result = *prepared
		}
		result.Target = target
		return &result, nil
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

// Sorted returns the recorded requests ordered by target, in node set order,
// so that exe2 comes before exe10, and for each target in the order they
// were made. A fan-out makes its calls in any order; this is the order to
// compare or print them in.
func (r *Recorder) Sorted() []Call {
	calls := r.Calls()
	names := nodeset.New()
	for _, c := range calls {
		_ = names.Add(c.Target.Name)
	}
	rank := map[string]int{}
	for i, name := range names.Expand() {
		rank[name] = i
	}
	// A name the node set language does not read, which no host name is,
	// sorts after the others, in string order.
	position := func(name string) (int, string) {
		if canonical, ok := names.Canonical(name); ok {
			return rank[canonical], ""
		}
		return len(rank), name
	}
	sort.SliceStable(calls, func(i, j int) bool {
		ri, si := position(calls[i].Target.Name)
		rj, sj := position(calls[j].Target.Name)
		if ri != rj {
			return ri < rj
		}
		return si < sj
	})
	return calls
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
