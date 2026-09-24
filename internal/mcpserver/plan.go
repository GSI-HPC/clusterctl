// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// confirmKey names the one question apply_plan asks.
const confirmKey = "confirm"

// change is an action a plan can carry out.
type change struct {
	// verb is what the gate calls it.
	verb string
	// needsReason says the action is refused without a reason.
	needsReason bool
	// run carries the action out through a Slurm client. A plan runs it
	// against a recorder to show what would be sent.
	run func(ctx context.Context, c *slurm.Client, ns *nodeset.NodeSet, reason string) error
	// argv is the command line that does the same from a terminal.
	argv func(ns *nodeset.NodeSet, reason string) []string
	// warn looks at the current state of the nodes and says what the
	// administrator should know before agreeing.
	warn func(nodes []slurm.Node, jobs []slurm.Job, ns *nodeset.NodeSet) []string
}

// changes are the actions plan_change offers. Each is one the command line
// has, with the same gate, the same argument rules and the same effect.
var changes = map[string]change{
	"drain": {
		verb:        "drain",
		needsReason: true,
		run: func(ctx context.Context, c *slurm.Client, ns *nodeset.NodeSet, reason string) error {
			return c.Drain(ctx, ns, reason)
		},
		argv: func(ns *nodeset.NodeSet, reason string) []string {
			return []string{"clusterctl", "slurm", "node", "drain", reason, "-n", ns.String()}
		},
		warn: func(nodes []slurm.Node, jobs []slurm.Job, ns *nodeset.NodeSet) []string {
			var out []string
			if busy := nodesWithJobs(jobs, ns); !busy.IsEmpty() {
				out = append(out, fmt.Sprintf(
					"%s run jobs; they keep running and the nodes stay draining until the jobs end", busy))
			}
			already := nodeset.New()
			for _, n := range nodes {
				if strings.HasPrefix(n.BaseState(), "drain") {
					_ = already.Add(n.Name)
				}
			}
			if !already.IsEmpty() {
				out = append(out, fmt.Sprintf("%s are already draining or drained; their reason is replaced", already))
			}
			return out
		},
	},
	"resume": {
		verb: "resume",
		run: func(ctx context.Context, c *slurm.Client, ns *nodeset.NodeSet, _ string) error {
			return c.Resume(ctx, ns)
		},
		argv: func(ns *nodeset.NodeSet, _ string) []string {
			return []string{"clusterctl", "slurm", "node", "resume", "-n", ns.String()}
		},
		warn: func(nodes []slurm.Node, _ []slurm.Job, _ *nodeset.NodeSet) []string {
			var out []string
			for _, n := range nodes {
				if n.Reason != "" {
					out = append(out, fmt.Sprintf("%s was taken out with the reason %q", n.Name, n.Reason))
				}
			}
			return out
		},
	},
}

// planReason checks the reason of a plan. The agent writes it, and it reaches
// the question put to the user and the node record in Slurm, so it is held
// to the rules of slurm node drain. An action that takes no reason refuses
// one rather than carrying text into the question that nothing uses.
func planReason(ch change, reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if !ch.needsReason {
		if reason != "" {
			return "", exitcode.Errorf(exitcode.Usage, "%s takes no reason; leave it out", ch.verb)
		}
		return "", nil
	}
	if reason == "" {
		return "", exitcode.Errorf(exitcode.Usage,
			"%s needs a reason: say what is wrong and where it is tracked", ch.verb)
	}
	if err := slurm.ValidateReason(reason); err != nil {
		return "", err
	}
	return reason, nil
}

func changeNames() []string {
	out := make([]string, 0, len(changes))
	for name := range changes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// plan is an action resolved, previewed and waiting to be applied.
type plan struct {
	id      string
	action  string
	nodes   *nodeset.NodeSet
	reason  string
	preview safety.Preview
	expires time.Time
	// asked is the state of the question put to the administrator, which
	// the answer has to carry back. It is empty until the question is asked
	// and cleared once it is answered, so one answer confirms one attempt.
	asked string
	used  bool
}

// planStore keeps the plans of one server. A plan lives in memory only: a
// restarted server has forgotten every plan, which is the safe direction.
type planStore struct {
	ttl   time.Duration
	now   func() time.Time
	mu    sync.Mutex
	plans map[string]*plan
}

func newPlanStore(ttl time.Duration, now func() time.Time) *planStore {
	return &planStore{ttl: ttl, now: now, plans: map[string]*plan{}}
}

func (ps *planStore) add(p *plan) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.prune()
	p.expires = ps.now().Add(ps.ttl)
	ps.plans[p.id] = p
}

// get returns a plan that may still be applied.
func (ps *planStore) get(id string) (*plan, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.prune()
	p, ok := ps.plans[id]
	if !ok {
		return nil, exitcode.Errorf(exitcode.Usage,
			"there is no plan %q; it expired, was applied already or never existed; call plan_change again", id)
	}
	return p, nil
}

// ask records that the question for a plan is being put and returns the
// state the answer has to carry.
func (ps *planStore) ask(p *plan) string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p.asked = p.id + "." + randomID()
	return p.asked
}

// answered checks that an answer belongs to the question last asked for a
// plan, and forgets the question so the answer cannot be used twice.
func (ps *planStore) answered(p *plan, state string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ok := p.asked != "" && state == p.asked
	p.asked = ""
	return ok
}

// take marks a plan used. Only one caller wins, so a plan is applied at most
// once even when two applies race.
func (ps *planStore) take(p *plan) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p.used {
		return false
	}
	p.used = true
	delete(ps.plans, p.id)
	return true
}

func (ps *planStore) prune() {
	now := ps.now()
	for id, p := range ps.plans {
		if now.After(p.expires) {
			delete(ps.plans, id)
		}
	}
}

func randomID() string {
	b := make([]byte, 8)
	// crypto/rand does not fail on the platforms Go supports.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) addPlanTools() {
	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:  "plan_change",
		Title: "Plan a change",
		Description: "Prepare a change without making it. action is one of: " + strings.Join(changeNames(), ", ") +
			" (drain needs a reason saying what is wrong and where it is tracked). The node set is resolved " +
			"once and pinned, protected hosts are refused, and the plan shows the current state of the nodes, " +
			"warnings and the exact commands that would be sent. Show the plan to the user, then call " +
			"apply_plan. A plan expires after a few minutes and can be applied once.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.planChange)

	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:  "apply_plan",
		Title: "Apply a planned change",
		Description: "Carry out a plan from plan_change. Repeat the plan's nodes and count exactly as the plan " +
			"gave them. The user is asked to confirm, and above the site's threshold to type the number of " +
			"hosts; a declined confirmation changes nothing.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, s.applyPlan)
}

func ptr[T any](v T) *T { return &v }

// planInput is the argument of plan_change.
type planInput struct {
	Action string `json:"action" jsonschema:"what to do: drain or resume"`
	Nodes  string `json:"nodes" jsonschema:"node set expression; it is resolved now and the plan keeps the result"`
	Reason string `json:"reason,omitempty" jsonschema:"why; required to drain, say what is wrong and where it is tracked"`
}

// planOutput is what plan_change returns.
type planOutput struct {
	PlanID        string            `json:"planId"`
	Context       string            `json:"context"`
	Action        string            `json:"action"`
	Nodes         string            `json:"nodes" jsonschema:"the pinned node set; apply_plan must repeat it"`
	Count         int               `json:"count" jsonschema:"how many hosts; apply_plan must repeat it"`
	Summary       string            `json:"summary"`
	Detail        string            `json:"detail,omitempty"`
	CurrentState  map[string]string `json:"currentState,omitempty" jsonschema:"the Slurm state of the nodes now, as state to node set"`
	Warnings      []string          `json:"warnings,omitempty"`
	Commands      []string          `json:"commands" jsonschema:"what would be sent, host by host"`
	CommandLine   string            `json:"commandLine" jsonschema:"the clusterctl command that does the same from a terminal"`
	CountRequired bool              `json:"countRequired" jsonschema:"the user will have to type the number of hosts"`
	ExpiresAt     time.Time         `json:"expiresAt"`
}

func (s *Server) planChange(ctx context.Context, _ *mcp.CallToolRequest, in planInput) (*mcp.CallToolResult, *planOutput, error) {
	ch, ok := changes[in.Action]
	if !ok {
		return nil, nil, callError(exitcode.Errorf(exitcode.Usage,
			"unknown action %q; plan_change offers %s", in.Action, strings.Join(changeNames(), ", ")))
	}
	reason, err := planReason(ch, in.Reason)
	if err != nil {
		return nil, nil, callError(err)
	}

	a, _, err := s.app(ctx)
	if err != nil {
		return nil, nil, callError(err)
	}
	ns, err := a.Select(in.Nodes)
	if err != nil {
		return nil, nil, callError(err)
	}
	action := safety.Action{Verb: ch.verb, Targets: ns}
	if reason != "" {
		// Quoted, so that what the agent wrote reads as one value in the
		// question put to the user.
		action.Detail = fmt.Sprintf("reason: %q", reason)
	}
	refused := func(err error) error {
		_ = s.audit.record(auditEntry{Event: "plan", Context: s.context, Action: in.Action,
			Nodes: ns.String(), Count: ns.Len(), Detail: action.Detail, Outcome: "refused: " + err.Error()})
		return callError(err)
	}
	preview, err := a.Gate.Preview(action)
	if err != nil {
		return nil, nil, refused(err)
	}

	c, err := a.Slurm()
	if err != nil {
		return nil, nil, callError(err)
	}
	// slurmctld expands ALL and NodeSet names, which the preview counts as
	// one host each; the plan stands only for a set Slurm reads as itself.
	if err := c.CheckNodes(ctx, ns); err != nil {
		return nil, nil, refused(err)
	}
	// The action is run against a recorder: what it records is exactly what
	// apply_plan will send, rendered the way --dry-run prints it.
	recorder := &transport.Recorder{}
	dry := *c
	dry.Runner = recorder
	if err := ch.run(ctx, &dry, ns, reason); err != nil {
		return nil, nil, callError(exitcode.Wrap(exitcode.Usage, err))
	}

	p := &plan{id: randomID(), action: in.Action, nodes: ns, reason: reason, preview: preview}
	s.plans.add(p)

	out := &planOutput{
		PlanID:        p.id,
		Context:       s.context,
		Action:        in.Action,
		Nodes:         preview.Targets,
		Count:         preview.Count,
		Summary:       preview.Summary(),
		Detail:        preview.Detail,
		CommandLine:   shellquote.Join(ch.argv(ns, reason)),
		CountRequired: preview.CountRequired,
		ExpiresAt:     p.expires.UTC(),
	}
	for _, call := range recorder.Calls() {
		out.Commands = append(out.Commands, call.Describe())
	}

	// The current state helps the administrator judge the plan. When the
	// workload manager does not answer the plan still stands, and says so.
	nodes, nodesErr := c.Nodes(ctx, ns, nil)
	jobs, jobsErr := c.Jobs(ctx, slurm.JobFilter{Nodes: ns})
	if err := errors.Join(nodesErr, jobsErr); err != nil {
		out.Warnings = append(out.Warnings, "the current state could not be read: "+err.Error())
	} else {
		out.CurrentState = statesOf(nodes)
		out.Warnings = append(out.Warnings, ch.warn(nodes, jobs, ns)...)
	}

	_ = s.audit.record(auditEntry{Event: "plan", Context: s.context, Plan: p.id, Action: in.Action,
		Nodes: preview.Targets, Count: preview.Count, Detail: preview.Detail, Outcome: "planned"})
	return nil, out, nil
}

// applyInput is the argument of apply_plan.
type applyInput struct {
	PlanID string `json:"planId"`
	Nodes  string `json:"nodes" jsonschema:"the plan's nodes, exactly as plan_change returned them"`
	Count  int    `json:"count" jsonschema:"the plan's count, exactly as plan_change returned it"`
}

// applyOutput is what apply_plan returns.
type applyOutput struct {
	PlanID  string `json:"planId"`
	Context string `json:"context"`
	Action  string `json:"action"`
	Nodes   string `json:"nodes"`
	Count   int    `json:"count"`
	Applied bool   `json:"applied"`
	Message string `json:"message"`
}

func (s *Server) applyPlan(ctx context.Context, req *mcp.CallToolRequest, in applyInput) (*mcp.CallToolResult, *applyOutput, error) {
	p, err := s.plans.get(in.PlanID)
	if err != nil {
		return nil, nil, callError(err)
	}
	// Repeating the plan puts the hosts into the call itself, so that a
	// client asking for approval of the call shows what it touches.
	if in.Nodes != p.preview.Targets || in.Count != p.preview.Count {
		return nil, nil, callError(exitcode.Errorf(exitcode.Usage,
			"plan %s is for %d hosts %s, not %d hosts %s; repeat the plan exactly",
			p.id, p.preview.Count, p.preview.Targets, in.Count, in.Nodes))
	}
	ch := changes[p.action]
	entry := auditEntry{Event: "apply", Context: s.context, Plan: p.id, Action: p.action,
		Nodes: p.preview.Targets, Count: p.preview.Count, Detail: p.preview.Detail}

	// The configuration is read again: a host protected since the plan was
	// made is refused now.
	a, _, err := s.app(ctx)
	if err != nil {
		return nil, nil, callError(err)
	}
	if _, err := a.Gate.Preview(safety.Action{Verb: ch.verb, Targets: p.nodes, Detail: p.preview.Detail}); err != nil {
		entry.Outcome = "refused: " + err.Error()
		_ = s.audit.record(entry)
		return nil, nil, callError(err)
	}

	if s.opts.Confirm == ConfirmElicit {
		result, err := s.confirm(req, p)
		if result != nil || err != nil {
			if err != nil {
				entry.Outcome = "declined: " + err.Error()
				_ = s.audit.record(entry)
			}
			return result, nil, callError(err)
		}
	}

	if !s.plans.take(p) {
		return nil, nil, callError(exitcode.Errorf(exitcode.Usage, "plan %s was applied already", p.id))
	}
	if err := s.execute(ctx, a, ch, p); err != nil {
		entry.Outcome = "failed: " + err.Error()
		_ = s.audit.record(entry)
		return nil, nil, callError(err)
	}
	entry.Outcome = "applied"
	_ = s.audit.record(entry)
	return nil, &applyOutput{
		PlanID: p.id, Context: s.context, Action: p.action, Nodes: p.preview.Targets, Count: p.preview.Count,
		Applied: true, Message: fmt.Sprintf("%s done: %s", ch.verb, p.preview.Summary()),
	}, nil
}

func (s *Server) execute(ctx context.Context, a *app.App, ch change, p *plan) error {
	c, err := a.Slurm()
	if err != nil {
		return err
	}
	if err := ch.run(ctx, c, p.nodes, p.reason); err != nil {
		return exitcode.Wrap(exitcode.TargetFailed, err)
	}
	return nil
}

// confirm puts the gate's question to the administrator through the client
// and judges the answer by the gate's rule. It returns a result when the
// question still has to be asked, an error when it was not answered yes, and
// neither when the plan may go ahead.
//
// The question travels as an input request of the tool call: the client
// shows it to the administrator and calls the tool again with the answer,
// which the model never sees or writes.
func (s *Server) confirm(req *mcp.CallToolRequest, p *plan) (*mcp.CallToolResult, error) {
	caps := req.ClientCapabilities()
	if caps == nil || caps.Elicitation == nil {
		return nil, exitcode.Errorf(exitcode.Usage,
			"this client cannot ask the user to confirm (it does not offer MCP elicitation), so nothing was done; "+
				"the user can run %s themselves, or restart the server with --confirm=%s",
			shellquote.Join(changes[p.action].argv(p.nodes, p.reason)), ConfirmApproval)
	}

	answer, ok := req.Params.InputResponses[confirmKey]
	if !ok {
		return &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{confirmKey: s.question(p)},
			RequestState:  s.plans.ask(p),
		}, nil
	}
	if !s.plans.answered(p, req.Params.RequestState) {
		return nil, exitcode.Errorf(exitcode.Usage,
			"the confirmation does not belong to this plan's question; call apply_plan again to be asked")
	}
	result, ok := answer.(*mcp.ElicitResult)
	if !ok || result.Action != "accept" {
		return nil, exitcode.Errorf(exitcode.Interrupted, "the user did not confirm, nothing was done")
	}
	return nil, p.preview.Accept(answerText(p.preview, result.Content))
}

// question is the elicitation that stands in for the gate's prompt.
func (s *Server) question(p *plan) *mcp.ElicitParams {
	message := fmt.Sprintf("About to %s\n", p.preview.Summary())
	if p.preview.Detail != "" {
		message += "  " + p.preview.Detail + "\n"
	}
	message += fmt.Sprintf("Context %s, cluster %s.\n", s.context, s.cluster)

	if p.preview.CountRequired {
		message += p.preview.Question()
		return &mcp.ElicitParams{
			Message: message,
			RequestedSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count": map[string]any{
						"type":        "integer",
						"title":       "Number of hosts",
						"description": fmt.Sprintf("Type the number of hosts to %s", p.preview.Verb),
					},
				},
				"required": []string{"count"},
			},
		}
	}
	message += "Continue?"
	return &mcp.ElicitParams{
		Message: message,
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{
					"type":    "boolean",
					"title":   fmt.Sprintf("Yes, %s %d hosts", p.preview.Verb, p.preview.Count),
					"default": false,
				},
			},
			"required": []string{"confirm"},
		},
	}
}

// answerText turns a form answer into what the gate's prompt would have
// read from the terminal.
func answerText(p safety.Preview, content map[string]any) string {
	if p.CountRequired {
		switch n := content["count"].(type) {
		case float64:
			return strconv.FormatFloat(n, 'f', -1, 64)
		case int:
			return strconv.Itoa(n)
		case string:
			return n
		}
		return ""
	}
	if yes, _ := content["confirm"].(bool); yes {
		return "yes"
	}
	return "no"
}

// nodesWithJobs returns the nodes of a set that run one of the jobs.
func nodesWithJobs(jobs []slurm.Job, ns *nodeset.NodeSet) *nodeset.NodeSet {
	out := nodeset.New()
	for _, j := range jobs {
		on, err := nodeset.Parse(j.Nodes)
		if err != nil {
			continue
		}
		out = out.Union(on.Intersection(ns))
	}
	return out
}

// statesOf groups nodes by their Slurm state.
func statesOf(nodes []slurm.Node) map[string]string {
	sets := map[string]*nodeset.NodeSet{}
	for _, n := range nodes {
		state := n.BaseState()
		if sets[state] == nil {
			sets[state] = nodeset.New()
		}
		_ = sets[state].Add(n.Name)
	}
	out := make(map[string]string, len(sets))
	for state, ns := range sets {
		out[state] = ns.String()
	}
	return out
}
