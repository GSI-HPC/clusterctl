// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/ipmi"
	"github.com/GSI-HPC/clusterctl/internal/output"
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
func redfishClients(a *app.App, names []string) ([]*redfish.Client, error) {
	clients := make([]*redfish.Client, len(names))
	for i, node := range names {
		c, err := redfishClientFor(a, a.Context(), node)
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
// the configured concurrency. It is the one Redfish fan-out: bmc and
// provision both go through it, so a failure is classified the same way
// whichever command met it.
//
// The context is checked before each request starts: once the command is
// interrupted, nothing more is sent, and the rest is reported as not sent.
// A request that was already under way when the interrupt came may or may
// not have been carried out, so when it changes something (changes) it is
// reported as an unknown outcome rather than as a failure to retry.
//
// A nil client is skipped: nothing is sent, and neither a value nor an
// error is recorded, since the caller has said why already. A panic while
// a request is under way becomes that node's failure, and the others are
// still sent.
func redfishEach[T any](a *app.App, names []string, clients []*redfish.Client, changes bool,
	do func(context.Context, string, *redfish.Client) (T, error)) []redfishCall[T] {
	ctx := a.Context()
	limit := a.Spec.BMC.Redfish.MaxConcurrent
	if limit < 1 {
		limit = 8
	}
	calls := make([]redfishCall[T], len(names))
	for i, node := range names {
		calls[i] = redfishCall[T]{node: node, client: clients[i]}
	}
	fanout.Each(ctx, len(calls), limit, func(i int) {
		if calls[i].client != nil {
			calls[i].sent = true
			calls[i].send(ctx, changes, do)
		}
	})
	for i := range calls {
		if calls[i].client != nil && !calls[i].sent {
			calls[i].err = errNotSent()
		}
	}
	return calls
}

// send sends the request of one call. A panic in it is the call's failure:
// the request may have been carried out, as after any other failure that
// does not prove it never reached the processor.
func (call *redfishCall[T]) send(ctx context.Context, changes bool, do func(context.Context, string, *redfish.Client) (T, error)) {
	defer func() {
		if err := fanout.Recovered(call.node, recover()); err != nil {
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
	t := output.NewTable(
		output.Column{Name: "NODE"},
		output.Column{Name: "BMC", Wide: true},
		output.Column{Name: "VIA", Wide: true},
		output.Column{Name: "STATE"},
		output.Column{Name: "ERROR"},
	)
	for _, r := range results {
		t.Add(r.Node, r.BMC, r.Via, r.State, r.Error)
	}
	if err := a.Print(output.Result{Table: t, Object: results}); err != nil {
		return err
	}
	return bmcExit(results)
}

// bmcExit derives the error a command exits with from its rows.
//
// The code says the worst thing that happened, in this order: an interrupt
// exits 130, a processor that could not be resolved, reached or trusted 3, a
// configuration problem 2, and a processor that refused 1. Nodes left out
// after a failed batch, or because of the interrupt, are counted and named.
func bmcExit(results []bmcResult) error {
	var (
		failed      int
		notTried    = nodeset.New()
		notSent     = nodeset.New()
		interrupted = nodeset.New()
		errs        []error
		code        = exitcode.TargetFailed
	)
	for _, r := range results {
		switch r.outcome {
		case outcomeNotTried:
			_ = notTried.Add(r.Node)
			continue
		case outcomeNotSent:
			_ = notSent.Add(r.Node)
			code = exitcode.Interrupted
			continue
		}
		if r.err == nil {
			continue
		}
		errs = append(errs, r.err)
		c := codeOf(r.err)
		if c == exitcode.Interrupted {
			// The interrupt is not the processor's failure, so it is
			// counted apart from the failures.
			_ = interrupted.Add(r.Node)
		} else {
			failed++
		}
		if rank(c) > rank(code) {
			code = c
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
	return &exitcode.Error{Code: code, Err: &bmcFailures{message: strings.Join(parts, ", "), errs: errs}}
}

// codeOf is the exit code an error of one processor stands for.
func codeOf(err error) int {
	if errors.Is(err, context.Canceled) {
		return exitcode.Interrupted
	}
	return exitcode.From(err)
}

// rank orders the exit codes by how much they tell.
func rank(code int) int {
	switch code {
	case exitcode.Interrupted:
		return 4
	case exitcode.Transport:
		return 3
	case exitcode.Usage:
		return 2
	default:
		return 1
	}
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
func (p *bmcPlan) resolve(a *app.App) error {
	for _, node := range p.nodes.Expand() {
		switch p.order[node][0] {
		case app.TransportRedfish:
			if _, err := p.client(a, node); err != nil {
				return err
			}
		case app.TransportIPMI:
			if _, err := p.backend(a, node); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *bmcPlan) client(a *app.App, node string) (*redfish.Client, error) {
	if c, ok := p.clients[node]; ok {
		return c, nil
	}
	c, err := redfishClientFor(a, a.Context(), node)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", node, err)
	}
	p.clients[node] = c
	return c, nil
}

// backend returns the IPMI backend for the account of a node. Nodes with the
// same account share one, so a set is sent to the backend once per account.
func (p *bmcPlan) backend(a *app.App, node string) (*ipmi.Backend, error) {
	credential := a.BMCCredentialName(node)
	if b, ok := p.backends[credential]; ok {
		return b, nil
	}
	b, err := a.IPMIBackend(a.Context(), node)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", node, err)
	}
	p.backends[credential] = b
	return b, nil
}

// run carries out a power action, or reads the power state, on some nodes of
// the plan. Each node is tried over its first transport. A read that failed
// is tried again over the next; an action only when the request provably
// never reached the processor, because an action that may have been carried
// out is never sent twice.
func (p *bmcPlan) run(a *app.App, names []string, action string) []bmcResult {
	results := map[string]bmcResult{}
	pending := names
	for step := 0; len(pending) > 0; step++ {
		byTransport := map[string][]string{}
		for _, node := range pending {
			transport := p.order[node][step]
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
				rows = p.runIPMI(a, nodes, action)
			} else {
				rows = p.runRedfish(a, nodes, action)
			}
			for _, row := range rows {
				if earlier, ok := results[row.Node]; ok && row.err != nil {
					row.fail(fmt.Errorf("%w; before that, %s failed: %s", row.err, transportTitle(earlier.Via), earlier.Error))
				}
				results[row.Node] = row
				order := p.order[row.Node]
				if row.err == nil || step+1 >= len(order) || !mayFallBack(action, transport, row.err) || a.Context().Err() != nil {
					continue
				}
				// The error may carry what the processor said, so it is
				// quoted to keep control characters off the terminal.
				a.Printf("%s: %s failed (%s); trying %s\n", row.Node, transportTitle(transport),
					strconv.Quote(row.Error), transportTitle(order[step+1]))
				next = append(next, row.Node)
			}
		}
		pending = next
	}

	out := make([]bmcResult, len(names))
	for i, node := range names {
		out[i] = results[node]
	}
	return out
}

// mayFallBack says whether a failure over one transport may be tried again
// over the next.
func mayFallBack(action, transport string, err error) bool {
	var pin *redfish.PinMismatchError
	switch code := codeOf(err); {
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
func (p *bmcPlan) runRedfish(a *app.App, names []string, action string) []bmcResult {
	var (
		sendable []string
		clients  []*redfish.Client
		rows     []bmcResult
	)
	for _, node := range names {
		c, err := p.client(a, node)
		if err != nil {
			row := bmcResult{Node: node, BMC: p.bmc[node], Via: app.TransportRedfish}
			row.fail(err)
			rows = append(rows, row)
			continue
		}
		sendable = append(sendable, node)
		clients = append(clients, c)
	}

	if action == ipmi.ActionStatus {
		calls := redfishEach(a, sendable, clients, false, func(ctx context.Context, _ string, c *redfish.Client) (string, error) {
			return c.PowerState(ctx)
		})
		return append(rows, callResults(calls, false, func(s string) string { return s })...)
	}
	resetType, err := resetTypeFor(action)
	if err != nil {
		for _, node := range sendable {
			row := bmcResult{Node: node, BMC: p.bmc[node], Via: app.TransportRedfish}
			row.fail(err)
			rows = append(rows, row)
		}
		return rows
	}
	calls := redfishEach(a, sendable, clients, true, func(ctx context.Context, _ string, c *redfish.Client) (string, error) {
		return resetType + " sent", c.Reset(ctx, resetType)
	})
	return append(rows, callResults(calls, true, func(s string) string { return s })...)
}

// runIPMI carries out a power action over IPMI, one backend run per account.
func (p *bmcPlan) runIPMI(a *app.App, names []string, action string) []bmcResult {
	ctx := a.Context()
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
		backend, err := p.backend(a, nodes[0])
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

// runPower carries out a power action on the whole plan, in batches with a
// pause between them where the action powers machines on.
//
// A batch with a failure stops the run, because the failure may be the
// breaker the batching is there to protect; the nodes of the later batches
// are reported as not tried. An interrupt stops it too, and what was not
// sent is reported as such.
func runPower(a *app.App, p *bmcPlan, action string, batch int, stagger time.Duration) []bmcResult {
	if !batched(action) || batch <= 0 || p.nodes.Len() <= batch {
		return p.run(a, p.nodes.Expand(), action)
	}

	verb := "powering on"
	if action == ipmi.ActionCycle {
		verb = "power cycling"
	}
	ctx := a.Context()
	chunks := p.nodes.Split((p.nodes.Len() + batch - 1) / batch)
	var results []bmcResult
	skip := func(from int, outcome bmcOutcome, state string, err error) {
		for _, chunk := range chunks[from:] {
			for _, node := range chunk.Expand() {
				row := bmcResult{Node: node, BMC: p.bmc[node], State: state, outcome: outcome}
				row.fail(err)
				results = append(results, row)
			}
		}
	}
	for i, chunk := range chunks {
		if i > 0 {
			if bmcExit(results) != nil {
				skip(i, outcomeNotTried, "not tried", errors.New("not tried: an earlier batch failed"))
				break
			}
			if stagger > 0 {
				a.Printf("waiting %s before the next batch\n", stagger)
				select {
				case <-ctx.Done():
				case <-time.After(stagger):
				}
			}
			if ctx.Err() != nil {
				skip(i, outcomeNotSent, "not sent", errNotSent())
				break
			}
		}
		a.Printf("%s %s (%d of %d)\n", verb, chunk, i+1, len(chunks))
		results = append(results, p.run(a, chunk.Expand(), action)...)
	}
	return results
}
