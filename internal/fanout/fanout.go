// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package fanout works on many targets at once.
//
// The degree of parallelism is bounded and conservative by default: a
// connection through a tunnel or a jump host is far more fragile than a local
// one, and a wide fan-out is what makes sshd refuse connections under its
// MaxStartups limit.
package fanout

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/hostname"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// DefaultMax is used when nothing configures the fan-out.
const DefaultMax = 16

// defaultStep names the step of an executor that names none.
const defaultStep = "run"

// Executor runs requests on many targets.
type Executor struct {
	// Runner performs one request; a Recorder stands in for a dry run.
	Runner transport.Runner
	// Max is how many targets are worked on at once.
	Max int
	// Step names the step a display shows the targets under; empty is
	// "run".
	Step string
	// Flags are given to that step, such as progress.ShowLines for a
	// command whose output is the product.
	Flags progress.Flags
	// PanicLog receives the stack of a panic in a worker; nil is the
	// process's standard error.
	PanicLog io.Writer
	// Answers says that a result that failed is an answer all the same,
	// for a command that reports it, as provision status reports which
	// nodes ssh reaches: every target ends well, but for one the context
	// ended. The results are the same either way.
	Answers bool
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
// the others: the point of a fan-out is to learn about every node. It runs
// on Map, and is reported as Map reports its work. A panic in the runner or
// in build becomes the target's failure rather than the end of the process.
func (e *Executor) RunEach(ctx context.Context, targets []transport.Target, build func(transport.Target) transport.Request) []*transport.Result {
	outcomes := Map(ctx, targets, Options[transport.Target]{
		Step:     cmp.Or(e.Step, defaultStep),
		Flags:    e.Flags,
		Limit:    e.Max,
		Describe: describeTarget,
		PanicLog: e.PanicLog,
	}, func(ctx context.Context, target transport.Target) (*transport.Result, error) {
		result, err := e.Runner.Run(ctx, target, build(target))
		if result == nil {
			result = &transport.Result{Target: target, ExitCode: -1}
		}
		if err != nil && result.Err == nil {
			result.Err = err
		}
		if e.Answers && ctx.Err() == nil {
			return result, nil
		}
		return result, resultError(result)
	})
	results := make([]*transport.Result, len(targets))
	for i, o := range outcomes {
		results[i] = o.Value
		if results[i] == nil {
			// A target the runner panicked on, or one the context ended
			// before, is reported with its error rather than left as a
			// nil result.
			results[i] = &transport.Result{Target: targets[i], ExitCode: -1, Err: o.Err}
		}
	}
	return results
}

// describeTarget says what a display names a target by.
func describeTarget(t transport.Target) (node, host, role string) {
	return cmp.Or(t.Name, t.Host), t.Host, t.Role
}

// resultError is the error a target is reported with: the result's own, or
// its exit status when it failed without one.
func resultError(r *transport.Result) error {
	if r.Err != nil || !r.Failed() {
		return r.Err
	}
	return fmt.Errorf("%s: command exited %d", cmp.Or(r.Target.Name, r.Target.Host), r.ExitCode)
}

// Each calls work with every index below n, at most limit at a time, and
// returns once every call has returned. When ctx ends, no further call is
// started, so an interrupt stops a fan-out the same way wherever it is; the
// caller tells what was left out by what work did not record.
func Each(ctx context.Context, n, limit int, work func(i int)) {
	sem := make(chan struct{}, max(1, min(limit, n)))
	var wg sync.WaitGroup
	for i := range n {
		select {
		case <-ctx.Done():
		case sem <- struct{}{}:
			// When a slot and the cancellation are both ready, select
			// picks either, so the context is asked again.
			if ctx.Err() != nil {
				<-sem
			}
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			work(i)
		})
	}
	wg.Wait()
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
//
// A group holds its targets as a node set, so a target whose name is not one
// host name cannot be put into one: it would be read as an expression, or
// lost. GroupByOutput then returns an error naming it rather than groups
// that drop or miscount it.
func GroupByOutput(results []*transport.Result) ([]Group, error) {
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
		name := cmp.Or(r.Target.Name, r.Target.Host)
		if err := hostname.Check(name); err != nil {
			return nil, fmt.Errorf("the output of %q cannot be grouped: %w", name, err)
		}
		if err := g.Nodes.Add(name); err != nil {
			return nil, fmt.Errorf("the output of %q cannot be grouped: %w", name, err)
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
	return out, nil
}
