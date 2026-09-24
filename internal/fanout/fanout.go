// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package fanout runs one request on many targets at once.
//
// The degree of parallelism is bounded and conservative by default: a
// connection through a tunnel or a jump host is far more fragile than a local
// one, and a wide fan-out is what makes sshd refuse connections under its
// MaxStartups limit.
package fanout

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// DefaultMax is used when nothing configures the fan-out.
const DefaultMax = 16

// Executor runs requests on many targets.
type Executor struct {
	// Runner performs one request; a Recorder stands in for a dry run.
	Runner transport.Runner
	// Max is how many targets are worked on at once.
	Max int
	// OnResult is called as each target finishes, in completion order, for
	// progress output. It must be safe to call from several goroutines.
	OnResult func(*transport.Result)
}

// Run executes the same request on every target.
func (e *Executor) Run(ctx context.Context, targets []transport.Target, req transport.Request) []*transport.Result {
	return e.RunEach(ctx, targets, func(transport.Target) transport.Request { return req })
}

// RunEach executes a request built per target, which is how a command sends
// each node its own arguments.
//
// Results come back in the order the targets were given, whatever order they
// finished in, so output is reproducible. A target that fails does not stop
// the others: the point of a fan-out is to learn about every node.
func (e *Executor) RunEach(ctx context.Context, targets []transport.Target, build func(transport.Target) transport.Request) []*transport.Result {
	results := make([]*transport.Result, len(targets))
	if len(targets) == 0 {
		return results
	}

	limit := e.Max
	if limit < 1 {
		limit = DefaultMax
	}
	if limit > len(targets) {
		limit = len(targets)
	}

	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, target := range targets {
		select {
		case <-ctx.Done():
			// Whatever has not started is reported as cancelled rather than
			// left as a nil result.
			results[i] = &transport.Result{Target: target, ExitCode: -1, Err: ctx.Err()}
			continue
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(i int, target transport.Target) {
			defer wg.Done()
			defer func() { <-sem }()

			result, err := e.Runner.Run(ctx, target, build(target))
			if result == nil {
				result = &transport.Result{Target: target, ExitCode: -1}
			}
			if err != nil && result.Err == nil {
				result.Err = err
			}
			results[i] = result
			if e.OnResult != nil {
				e.OnResult(result)
			}
		}(i, target)
	}
	wg.Wait()
	return results
}

// Failures returns the results that did not succeed.
func Failures(results []*transport.Result) []*transport.Result {
	var out []*transport.Result
	for _, r := range results {
		if r.Failed() {
			out = append(out, r)
		}
	}
	return out
}

// Succeeded returns the node set whose targets succeeded.
func Succeeded(results []*transport.Result) *nodeset.NodeSet {
	ns := nodeset.New()
	for _, r := range results {
		if !r.Failed() {
			_ = ns.Add(r.Target.Name)
		}
	}
	return ns
}

// Status says in a word how a target ended: "ok", "exit N", "unreachable"
// when the transport could not reach it, or "interrupted" when it was
// cancelled.
func Status(r *transport.Result) string {
	switch {
	case r == nil:
		return "no result"
	case errors.Is(r.Err, context.Canceled):
		return "interrupted"
	case exitcode.From(r.Err) == exitcode.Transport:
		return "unreachable"
	case !r.Failed():
		return "ok"
	case r.ExitCode > 0:
		return fmt.Sprintf("exit %d", r.ExitCode)
	default:
		return "failed"
	}
}

// Group is a set of targets that produced identical output.
type Group struct {
	// Nodes are the targets that produced this output.
	Nodes *nodeset.NodeSet `json:"nodes" yaml:"nodes"`
	// Output is the standard output they shared, exactly as it came.
	Output string `json:"output" yaml:"output"`
	// ExitCode is the status they shared.
	ExitCode int `json:"exitCode" yaml:"exitCode"`
	// Status says how they ended, as Status reports it.
	Status string `json:"status" yaml:"status"`
}

// GroupByOutput collects targets that answered the same thing, which turns
// the output of a thousand nodes into the handful of answers worth reading.
//
// Targets are grouped only when both their output and the way they ended are
// the same: for grep -q or test -e the status is the whole answer, and a
// node that could not be reached said nothing, which is not the same as a
// node that answered with nothing. Output that differs only in blank lines
// at the end is not the same output either.
//
// Groups come back largest first, and equally sized groups in node set order,
// so two runs of the same command print the same thing.
func GroupByOutput(results []*transport.Result) []Group {
	type key struct {
		output string
		status string
		code   int
	}
	index := map[key]*Group{}
	for _, r := range results {
		k := key{output: r.Stdout, status: Status(r), code: r.ExitCode}
		g, ok := index[k]
		if !ok {
			g = &Group{Nodes: nodeset.New(), Output: k.output, ExitCode: k.code, Status: k.status}
			index[k] = g
		}
		name := r.Target.Name
		if name == "" {
			name = r.Target.Host
		}
		if err := g.Nodes.Add(name); err != nil {
			// A name that is not a host name still has to appear somewhere,
			// so it becomes its own group rather than being dropped.
			g.Output = k.output
		}
	}

	out := make([]Group, 0, len(index))
	for _, g := range index {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Nodes.Len() != out[j].Nodes.Len() {
			return out[i].Nodes.Len() > out[j].Nodes.Len()
		}
		return out[i].Nodes.String() < out[j].Nodes.String()
	})
	return out
}
