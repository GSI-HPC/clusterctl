// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/ipmi"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// bmcOutcome says how far the request to one service processor got.
type bmcOutcome int

const (
	// outcomeSent means the request was sent; the row says how it went.
	outcomeSent bmcOutcome = iota
	// outcomeNotTried means the node was left out on purpose, because an
	// earlier batch failed.
	outcomeNotTried
	// outcomeNotSent means the command was interrupted before the request
	// was sent.
	outcomeNotSent
)

// bmcResult is what one processor answered.
type bmcResult struct {
	Node  string `json:"node" yaml:"node"`
	BMC   string `json:"bmc" yaml:"bmc"`
	Via   string `json:"via,omitempty" yaml:"via,omitempty"`
	State string `json:"state,omitempty" yaml:"state,omitempty"`
	Error string `json:"error,omitempty" yaml:"error,omitempty"`

	// err is the error behind Error. It is kept, not only its text, so
	// that the exit code can tell an unreachable processor and an
	// interrupt from a refusal.
	err     error
	outcome bmcOutcome
}

func (r *bmcResult) fail(err error) {
	r.err = err
	r.Error = err.Error()
}

// redfishClientFor builds the client of a node's service processor. It is a
// variable so that the tests can put a fake behind the client.
var redfishClientFor = func(a *app.App, ctx context.Context, node string) (*redfish.Client, error) {
	return a.RedfishClient(ctx, node)
}

// redfishClients builds the client of every node before anything is sent, so
// that a missing credential or processor name stops the command as a usage
// error instead of turning up as one failed row per node.
func redfishClients(ctx context.Context, a *app.App, names []string) ([]*redfish.Client, error) {
	clients := make([]*redfish.Client, len(names))
	for i, node := range names {
		c, err := redfishClientFor(a, ctx, node)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", node, err)
		}
		clients[i] = c
	}
	return clients, nil
}

// redfishCall is one request of a fan-out and what came of it.
type redfishCall[T any] struct {
	node   string
	client *redfish.Client
	value  T
	err    error
	// sent is false for a request the command was interrupted before.
	sent bool
}

// redfishEach sends one request to every processor in parallel, bounded by
// redfishLimit, and reports the fan-out as the step it names, with a target
// for each node, the way fanout.Map reports its work. It is the Redfish
// fan-out of every command but bmc power and bmc status, whose nodes may be
// tried over IPMI too and are reported across both (bmcRun.redfish). Both
// send through the same call, so a failure is classified the same way
// whichever command met it.
//
// Once the command is interrupted, nothing more is sent, and the rest is
// reported as not sent. A request that was already under way when the
// interrupt came may or may not have been carried out, so when it changes
// something (changes) it is reported as an unknown outcome rather than as a
// failure to retry.
//
// A nil client is skipped: nothing is sent, the node is no target of the
// step, and neither a value nor an error is recorded, since the caller has
// said why already.
func redfishEach[T any](ctx context.Context, a *app.App, step string, names []string, clients []*redfish.Client, changes bool,
	do func(context.Context, string, *redfish.Client) (T, error)) []redfishCall[T] {
	calls := newRedfishCalls[T](names, clients)
	var sendable []int
	for i := range calls {
		if calls[i].client != nil {
			sendable = append(sendable, i)
		}
	}
	outcomes := fanout.Map(ctx, sendable, fanout.Options[int]{
		Step:  step,
		Limit: redfishLimit(a),
		Describe: func(i int) (node, host, role string) {
			return calls[i].node, calls[i].client.Host, ""
		},
		PanicLog: a.Diag,
	}, func(ctx context.Context, i int) (struct{}, error) {
		calls[i].send(ctx, a.Diag, changes, do)
		return struct{}{}, calls[i].err
	})
	for k, i := range sendable {
		if calls[i].sent = outcomes[k].Started; !calls[i].sent {
			calls[i].err = errNotSent()
		}
	}
	return calls
}

// redfishLimit is how many requests go to the processors at once:
// bmc.redfish.maxConcurrent, or --fanout where that is lower.
func redfishLimit(a *app.App) int { return a.Bound(a.Spec.BMC.Redfish.MaxConcurrent) }

// newRedfishCalls pairs each node with its client.
func newRedfishCalls[T any](names []string, clients []*redfish.Client) []redfishCall[T] {
	calls := make([]redfishCall[T], len(names))
	for i, node := range names {
		calls[i] = redfishCall[T]{node: node, client: clients[i]}
	}
	return calls
}

// send sends the request of one call. A panic in it is the call's failure,
// with its stack written to log: the request may have been carried out, as
// after any other failure that does not prove it never reached the
// processor. The client closes its connections once the request is
// answered: a processor has few to give, and a command that talks to it
// again, as a reinstall does in its next step, is a fan-out away.
func (call *redfishCall[T]) send(ctx context.Context, log io.Writer, changes bool, do func(context.Context, string, *redfish.Client) (T, error)) {
	defer call.client.CloseIdleConnections()
	defer func() {
		if err := fanout.Recovered(log, call.node, recover()); err != nil {
			call.err = err
		}
	}()
	value, err := do(ctx, call.node, call.client)
	call.value = value
	if err != nil {
		call.err = bmcError(ctx, err, changes)
	}
}

// interruptedState is the state of a request the interrupt came during. An
// action may or may not have been carried out; a read just has no answer.
func interruptedState(changes bool) string {
	if changes {
		return "outcome unknown"
	}
	return "interrupted"
}

// errRequestNotSent marks a request the command was interrupted before.
var errRequestNotSent = errors.New("not sent: the command was interrupted first")

// errNotSent is the error of a request the command was interrupted before.
func errNotSent() error {
	return &exitcode.Error{Code: exitcode.Interrupted, Err: errRequestNotSent}
}

// bmcError is the error of a request to a processor. One the interrupt came
// during exits 130. The clients have already marked a processor that could not
// be reached, trusted or heard to the end as a transport failure.
func bmcError(ctx context.Context, err error, changes bool) error {
	if ctx.Err() != nil {
		if changes {
			return &exitcode.Error{Code: exitcode.Interrupted, Err: fmt.Errorf(
				"outcome unknown, check the state before trying again: interrupted while the request was under way: %w", err)}
		}
		return &exitcode.Error{Code: exitcode.Interrupted, Err: fmt.Errorf("interrupted: %w", err)}
	}
	return err
}

// neverSent says whether an error proves that a Redfish request never
// reached the processor: its name did not resolve, nothing accepted the
// connection, or it presented another certificate than the one recorded,
// which the handshake refuses before the request is written. A request
// that failed any other way may have been carried out.
func neverSent(err error) bool {
	var (
		dnsErr *net.DNSError
		opErr  *net.OpError
		pin    *redfish.PinMismatchError
	)
	return errors.As(err, &dnsErr) || errors.As(err, &pin) ||
		(errors.As(err, &opErr) && opErr.Op == "dial")
}

// callResults turns a fan-out into result rows, with state giving the state
// of a request that succeeded.
func callResults[T any](calls []redfishCall[T], changes bool, state func(T) string) []bmcResult {
	results := make([]bmcResult, len(calls))
	for i, c := range calls {
		r := bmcResult{Node: c.node, Via: app.TransportRedfish}
		if c.client != nil {
			r.BMC = c.client.Host
		}
		switch {
		case !c.sent:
			r.State = "not sent"
			r.outcome = outcomeNotSent
			r.fail(c.err)
		case c.err != nil:
			if exitcode.From(c.err) == exitcode.Interrupted {
				r.State = interruptedState(changes)
			}
			r.fail(c.err)
		default:
			r.State = state(c.value)
		}
		results[i] = r
	}
	return results
}

// printBMCResults prints the rows of a command once, and returns the error
// the command exits with.
func printBMCResults(a *app.App, results []bmcResult) error {
	t := output.NewTable(output.Cols("NODE", "BMC", "VIA", "STATE", "ERROR").Wide("BMC", "VIA")...)
	for _, r := range results {
		t.Add(r.Node, r.BMC, r.Via, r.State, r.Error)
	}
	if err := a.Print(output.Result{Table: t, Object: results}); err != nil {
		return err
	}
	return bmcExit(results)
}

// bmcExit derives the error a command exits with from its rows, with the
// code exitcode.Worst gives; a request the interrupt kept from being sent
// makes it 130. Nodes left out after a failed batch, or because of the
// interrupt, are counted and named.
func bmcExit(results []bmcResult) error {
	var (
		failed      int
		notTried    = nodeset.New()
		notSent     = nodeset.New()
		interrupted = nodeset.New()
		errs        []error
	)
	for _, r := range results {
		switch r.outcome {
		case outcomeNotTried:
			_ = notTried.Add(r.Node)
			continue
		case outcomeNotSent:
			_ = notSent.Add(r.Node)
			continue
		}
		if r.err == nil {
			continue
		}
		errs = append(errs, r.err)
		if exitcode.Worst(r.err) == exitcode.Interrupted {
			// The interrupt is not the processor's failure, so it is
			// counted apart from the failures.
			_ = interrupted.Add(r.Node)
		} else {
			failed++
		}
	}
	if failed == 0 && notTried.IsEmpty() && notSent.IsEmpty() && interrupted.IsEmpty() {
		return nil
	}

	var parts []string
	if failed > 0 || (notSent.IsEmpty() && interrupted.IsEmpty()) {
		parts = append(parts, fmt.Sprintf("%d of %d service processors failed", failed, len(results)))
	} else {
		parts = append(parts, "interrupted")
	}
	if !notTried.IsEmpty() {
		parts = append(parts, fmt.Sprintf("%d not tried: %s", notTried.Len(), notTried))
	}
	if !interrupted.IsEmpty() {
		parts = append(parts, fmt.Sprintf("%d interrupted while under way: %s", interrupted.Len(), interrupted))
	}
	if !notSent.IsEmpty() {
		parts = append(parts, fmt.Sprintf("%d not sent: %s", notSent.Len(), notSent))
	}
	// Rows left out on purpose carry no error, and still fail the command.
	code := cmp.Or(exitcode.Worst(errs...), exitcode.TargetFailed)
	if !notSent.IsEmpty() {
		code = exitcode.Interrupted
	}
	return &exitcode.Error{Code: code, Err: &bmcFailures{message: strings.Join(parts, ", "), errs: errs}}
}

// bmcFailures is the summary of a command that did not succeed on every
// processor, with the error of each one that failed underneath it.
type bmcFailures struct {
	message string
	errs    []error
}

func (e *bmcFailures) Error() string { return e.message }

func (e *bmcFailures) Unwrap() []error { return e.errs }

// bmcPlan is how each node of a set is reached: its processor, and the
// transports to try in order.
type bmcPlan struct {
	nodes *nodeset.NodeSet
	bmc   map[string]string
	order map[string][]string

	clients  map[string]*redfish.Client
	backends map[string]*ipmi.Backend
}

// planBMC works out how every node of a set is reached. It reads the
// configuration only, so it can run before anything is asked or sent.
func planBMC(a *app.App, nodes *nodeset.NodeSet, useIPMI bool) (*bmcPlan, error) {
	p := &bmcPlan{
		nodes:    nodes,
		bmc:      map[string]string{},
		order:    map[string][]string{},
		clients:  map[string]*redfish.Client{},
		backends: map[string]*ipmi.Backend{},
	}
	for _, node := range nodes.Expand() {
		host, err := a.BMCHost(node)
		if err != nil {
			return nil, err
		}
		p.bmc[node] = host
		if useIPMI {
			p.order[node] = []string{app.TransportIPMI}
			continue
		}
		order, err := a.BMCTransports(node)
		if err != nil {
			return nil, err
		}
		p.order[node] = order
	}
	return p, nil
}

// transportTitle is how a transport is named to the administrator.
func transportTitle(transport string) string {
	if transport == app.TransportIPMI {
		return "IPMI"
	}
	return "Redfish"
}

// describe says how the set is reached, for the preview: one line per group
// of nodes that share their transports and account.
func (p *bmcPlan) describe(a *app.App) string {
	var keys []string
	groups := map[string]*nodeset.NodeSet{}
	for _, node := range p.nodes.Expand() {
		titles := make([]string, len(p.order[node]))
		for i, t := range p.order[node] {
			titles[i] = transportTitle(t)
		}
		key := "through " + strings.Join(titles, ", falling back to ")
		if credential := a.BMCCredentialName(node); credential != "" {
			key += " with the credential " + credential
		}
		if groups[key] == nil {
			groups[key] = nodeset.New()
			keys = append(keys, key)
		}
		_ = groups[key].Add(node)
	}
	if len(keys) == 1 {
		return keys[0]
	}
	lines := make([]string, len(keys))
	for i, key := range keys {
		lines[i] = fmt.Sprintf("%s: %s", key, groups[key])
	}
	return strings.Join(lines, "\n  ")
}

// resolve resolves the account of every node, and builds what the first
// transport of each needs, before anything is sent. A missing credential or
// IPMI host role stops the command as a usage error.
func (p *bmcPlan) resolve(ctx context.Context, a *app.App) error {
	for _, node := range p.nodes.Expand() {
		switch p.order[node][0] {
		case app.TransportRedfish:
			if _, err := p.client(ctx, a, node); err != nil {
				return err
			}
		case app.TransportIPMI:
			if _, err := p.backend(ctx, a, node); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *bmcPlan) client(ctx context.Context, a *app.App, node string) (*redfish.Client, error) {
	if c, ok := p.clients[node]; ok {
		return c, nil
	}
	c, err := redfishClientFor(a, ctx, node)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", node, err)
	}
	p.clients[node] = c
	return c, nil
}

// backend returns the IPMI backend for the account of a node. Nodes with the
// same account share one, so a set is sent to the backend once per account.
func (p *bmcPlan) backend(ctx context.Context, a *app.App, node string) (*ipmi.Backend, error) {
	credential := a.BMCCredentialName(node)
	if b, ok := p.backends[credential]; ok {
		return b, nil
	}
	b, err := a.IPMIBackend(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", node, err)
	}
	p.backends[credential] = b
	return b, nil
}

// runStep runs the plan on some of its nodes as a step of its own, "power
// <action>", whose targets are the nodes.
func (p *bmcPlan) runStep(ctx context.Context, a *app.App, names []string, action string) []bmcResult {
	ctx, step := progress.Start(ctx, progress.KindStep, "power "+action,
		progress.WithFlags(progress.Fold), progress.Total(len(names)))
	rows := p.run(ctx, a, names, action)
	step.End(bmcExit(rows))
	return rows
}

// run carries out a power action, or reads the power state, on some nodes of
// the plan. Each node is tried over its first transport. A read that failed
// is tried again over the next; an action only when the request provably
// never reached the processor, because an action that may have been carried
// out is never sent twice.
//
// Each node is reported as one target under the span ctx carries, however
// many transports it is tried over: every target is queued before anything
// is sent, marked running when the first request for its node goes out,
// and ended with the node's last answer. The span above shows no limit,
// since a node that falls back keeps its target from one transport to the
// next, and IPMI asks for every processor of an account in one run.
func (p *bmcPlan) run(ctx context.Context, a *app.App, names []string, action string) []bmcResult {
	r := &bmcRun{plan: p, action: action, targets: map[string]bmcTarget{}, results: map[string]bmcResult{}}
	for _, node := range names {
		target, span := progress.Start(ctx, progress.KindTarget, node, progress.Queued(),
			progress.Node(node), progress.Host(p.bmc[node]))
		r.targets[node] = bmcTarget{ctx: target, span: span}
	}
	pending := names
	for ; len(pending) > 0; r.step++ {
		byTransport := map[string][]string{}
		for _, node := range pending {
			transport := p.order[node][r.step]
			byTransport[transport] = append(byTransport[transport], node)
		}
		var next []string
		for _, transport := range []string{app.TransportRedfish, app.TransportIPMI} {
			nodes := byTransport[transport]
			if len(nodes) == 0 {
				continue
			}
			var rows []bmcResult
			if transport == app.TransportIPMI {
				rows = r.runIPMI(ctx, a, nodes)
			} else {
				rows = r.runRedfish(ctx, a, nodes)
			}
			for _, row := range rows {
				row, again := r.settle(ctx, transport, row)
				r.results[row.Node] = row
				if !again {
					r.targets[row.Node].span.End(row.err)
					continue
				}
				// The error may carry what the processor said, so it is
				// quoted to keep control characters off the terminal.
				a.Printf("%s: %s failed (%s); trying %s\n", row.Node, transportTitle(transport),
					strconv.Quote(row.Error), transportTitle(p.order[row.Node][r.step+1]))
				next = append(next, row.Node)
			}
		}
		pending = next
	}

	out := make([]bmcResult, len(names))
	for i, node := range names {
		out[i] = r.results[node]
	}
	return out
}

// bmcRun is one run of a plan over some of its nodes.
type bmcRun struct {
	plan   *bmcPlan
	action string
	// targets are what the nodes are reported by, one each across every
	// transport it is tried over.
	targets map[string]bmcTarget
	// results holds the latest answer for each node. It is written between
	// the runs of two transports, and only read while one is under way.
	results map[string]bmcResult
	// step is how many transports the nodes still pending were tried over.
	step int
}

// bmcTarget is the target a node is reported by, and the context its
// requests are sent under.
type bmcTarget struct {
	ctx  context.Context
	span *progress.Span
}

// settle takes a node's answer over a transport: a failure carries the
// failure over the transport before, where there was one, and settle says
// whether the node is to be tried over the next.
func (r *bmcRun) settle(ctx context.Context, transport string, row bmcResult) (bmcResult, bool) {
	if earlier, ok := r.results[row.Node]; ok && row.err != nil {
		row.fail(fmt.Errorf("%w; before that, %s failed: %s", row.err, transportTitle(earlier.Via), earlier.Error))
	}
	order := r.plan.order[row.Node]
	again := row.err != nil && r.step+1 < len(order) && mayFallBack(r.action, transport, row.err) && ctx.Err() == nil
	return row, again
}

// answered ends the target of a node as soon as its answer over a transport
// is in, when that answer is the node's last, so that a display counts the
// node done then rather than once the slowest processor has answered. run
// settles every answer again when the transport's run is over, the same
// way, and ends the targets still open; a span ends only once.
func (r *bmcRun) answered(ctx context.Context, transport, node string, err error) {
	if row, again := r.settle(ctx, transport, bmcResult{Node: node, err: err}); !again {
		r.targets[node].span.End(row.err)
	}
}

// mayFallBack says whether a failure over one transport may be tried again
// over the next.
func mayFallBack(action, transport string, err error) bool {
	var pin *redfish.PinMismatchError
	switch code := exitcode.Worst(err); {
	case code == exitcode.Interrupted, code == exitcode.Usage:
		return false
	case errors.As(err, &pin):
		// The request never left, but a processor presenting another
		// certificate may not be the processor; its account goes to it
		// over no other protocol either.
		return false
	case action == ipmi.ActionStatus:
		return true
	default:
		return transport == app.TransportRedfish && neverSent(err)
	}
}

// runRedfish carries out a power action over Redfish.
func (r *bmcRun) runRedfish(ctx context.Context, a *app.App, names []string) []bmcResult {
	p := r.plan
	var (
		sendable []string
		clients  []*redfish.Client
		rows     []bmcResult
	)
	for _, node := range names {
		c, err := p.client(ctx, a, node)
		if err != nil {
			row := bmcResult{Node: node, BMC: p.bmc[node], Via: app.TransportRedfish}
			row.fail(err)
			rows = append(rows, row)
			continue
		}
		sendable = append(sendable, node)
		clients = append(clients, c)
	}

	if r.action == ipmi.ActionStatus {
		calls := r.redfish(ctx, a, sendable, clients, false, func(ctx context.Context, _ string, c *redfish.Client) (string, error) {
			return c.PowerState(ctx)
		})
		return append(rows, callResults(calls, false, func(s string) string { return s })...)
	}
	resetType, err := resetTypeFor(r.action)
	if err != nil {
		for _, node := range sendable {
			row := bmcResult{Node: node, BMC: p.bmc[node], Via: app.TransportRedfish}
			row.fail(err)
			rows = append(rows, row)
		}
		return rows
	}
	calls := r.redfish(ctx, a, sendable, clients, true, func(ctx context.Context, _ string, c *redfish.Client) (string, error) {
		return resetType + " sent", c.Reset(ctx, resetType)
	})
	return append(rows, callResults(calls, true, func(s string) string { return s })...)
}

// redfish sends a request to the processor of every node, at most
// redfishLimit at a time and through the same call as redfishEach, but
// under the run's targets rather than a step of its own: a node's target
// is marked running as its request takes its place, and ended as the
// answer comes in when that is the node's last. Every node has a client.
func (r *bmcRun) redfish(ctx context.Context, a *app.App, names []string, clients []*redfish.Client, changes bool,
	do func(context.Context, string, *redfish.Client) (string, error)) []redfishCall[string] {
	calls := newRedfishCalls[string](names, clients)
	fanout.Each(ctx, len(calls), redfishLimit(a), func(i int) {
		t := r.targets[calls[i].node]
		t.span.Run()
		calls[i].sent = true
		calls[i].send(t.ctx, a.Diag, changes, do)
		r.answered(ctx, app.TransportRedfish, calls[i].node, calls[i].err)
	})
	for i := range calls {
		if !calls[i].sent {
			calls[i].err = errNotSent()
		}
	}
	return calls
}

// runIPMI carries out a power action over IPMI, one backend run per account,
// which marks the targets of all its nodes running at once.
func (r *bmcRun) runIPMI(ctx context.Context, a *app.App, names []string) []bmcResult {
	p, action := r.plan, r.action
	var (
		accounts []string
		groups   = map[string][]string{}
		rows     []bmcResult
	)
	for _, node := range names {
		credential := a.BMCCredentialName(node)
		if groups[credential] == nil {
			accounts = append(accounts, credential)
		}
		groups[credential] = append(groups[credential], node)
	}

	for _, credential := range accounts {
		nodes := groups[credential]
		fail := func(err error, outcome bmcOutcome, state string) {
			for _, node := range nodes {
				row := bmcResult{Node: node, BMC: p.bmc[node], Via: app.TransportIPMI, State: state, outcome: outcome}
				row.fail(err)
				rows = append(rows, row)
			}
		}
		if ctx.Err() != nil {
			fail(errNotSent(), outcomeNotSent, "not sent")
			continue
		}
		backend, err := p.backend(ctx, a, nodes[0])
		if err != nil {
			fail(err, outcomeSent, "")
			continue
		}
		bmcs := nodeset.New()
		byBMC := map[string][]string{}
		for _, node := range nodes {
			_ = bmcs.Add(p.bmc[node])
			byBMC[p.bmc[node]] = append(byBMC[p.bmc[node]], node)
		}
		for _, node := range nodes {
			r.targets[node].span.Run()
		}
		statuses, err := backend.Power(ctx, action, bmcs)
		if err != nil {
			err = bmcError(ctx, err, action != ipmi.ActionStatus)
			state := ""
			if exitcode.From(err) == exitcode.Interrupted {
				state = interruptedState(action != ipmi.ActionStatus)
			}
			fail(err, outcomeSent, state)
			continue
		}
		answered := map[string]bool{}
		for _, s := range statuses {
			answered[s.BMC] = true
			for _, node := range byBMC[s.BMC] {
				row := bmcResult{Node: node, BMC: s.BMC, Via: app.TransportIPMI, State: s.State}
				if s.Err != "" {
					cause := s.Cause
					if cause == nil {
						cause = errors.New(s.Err)
					}
					cause = bmcError(ctx, cause, action != ipmi.ActionStatus)
					if exitcode.From(cause) == exitcode.Interrupted {
						row.State = interruptedState(action != ipmi.ActionStatus)
					}
					row.fail(cause)
				}
				rows = append(rows, row)
			}
		}
		// Every node gets a row, even one whose processor the backend
		// answered for under another spelling.
		for _, node := range nodes {
			if !answered[p.bmc[node]] {
				row := bmcResult{Node: node, BMC: p.bmc[node], Via: app.TransportIPMI, State: "unknown"}
				row.fail(fmt.Errorf("the IPMI backend returned no answer for %s", p.bmc[node]))
				rows = append(rows, row)
			}
		}
	}
	return rows
}

// batched says whether an action powers machines on, and so has to be
// spread over batches: a power-on and a power cycle both draw the inrush
// current of every machine at once.
func batched(action string) bool {
	return action == ipmi.ActionOn || action == ipmi.ActionCycle
}

// staggerAfter waits out the pause between two batches; the tests replace
// it.
var staggerAfter = time.After

// runPower carries out a power action on the whole plan, in batches with a
// pause between them where the action powers machines on.
//
// A batch with a failure stops the run, because the failure may be the
// breaker the batching is there to protect; the nodes of the later batches
// are reported as not tried. An interrupt stops it too, and what was not
// sent is reported as such, after a batch that failed as well, since the
// interrupt is what left it out then: a batch it cut short fails too. The
// batches are those of fanout.Batches, reported under its step, "power
// <action>", each with its nodes as its targets, and showing no limit, for
// the reason run gives.
func runPower(ctx context.Context, a *app.App, p *bmcPlan, action string, batch int, stagger time.Duration) []bmcResult {
	if !batched(action) || batch <= 0 || p.nodes.Len() <= batch {
		return p.runStep(ctx, a, p.nodes.Expand(), action)
	}

	verb := "powering on"
	if action == ipmi.ActionCycle {
		verb = "power cycling"
	}
	var results []bmcResult
	batches := fanout.Batches(ctx, p.nodes, fanout.BatchOptions{
		Step:  "power " + action,
		Size:  batch,
		Pause: stagger,
		After: staggerAfter,
		BeforePause: func(pause time.Duration) {
			a.Printf("waiting %s before the next batch\n", pause)
		},
		Before: func(i, n int, chunk *nodeset.NodeSet) {
			a.Printf("%s %s (%d of %d)\n", verb, chunk, i+1, n)
		},
	}, func(ctx context.Context, chunk *nodeset.NodeSet) error {
		rows := p.run(ctx, a, chunk.Expand(), action)
		results = append(results, rows...)
		return bmcExit(rows)
	})
	for _, b := range batches {
		if b.Ran {
			continue
		}
		outcome, state, err := outcomeNotSent, "not sent", errNotSent()
		if errors.Is(b.Err, fanout.ErrNotTried) {
			outcome, state, err = outcomeNotTried, "not tried", b.Err
		}
		for _, node := range b.Nodes.Expand() {
			row := bmcResult{Node: node, BMC: p.bmc[node], State: state, outcome: outcome}
			row.fail(err)
			results = append(results, row)
		}
	}
	return results
}
