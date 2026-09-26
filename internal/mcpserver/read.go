// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

const (
	// maxExpanded is the largest set select_nodes also lists name by name.
	maxExpanded = 256
	// maxDescribed is the largest set describe_nodes describes node by node.
	// Beyond it the answer is too long to be read, and a query is the better
	// question.
	maxDescribed = 64
	// maxGroupsOf bounds the groups facet, which can cost a remote call per
	// node.
	maxGroupsOf = 16
	// defaultRows and maxRows bound what query_slurm returns.
	defaultRows = 200
	maxRows     = 2000
)

func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
}

func (s *Server) addReadTools() {
	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:  "select_nodes",
		Title: "Resolve a node set",
		Description: "Resolve a node set expression against the inventory and the group sources. " +
			"Returns the folded set, its size, the names when there are few, the nodes the inventory " +
			"does not know and the protected hosts the set contains.",
		Annotations: readOnly(),
	}, limited(s, s.selectNodes))

	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:  "describe_nodes",
		Title: "Describe nodes",
		Description: fmt.Sprintf("Describe up to %d nodes in one call: host and service processor names, "+
			"the inventory entry, the Slurm state with its reason, and the jobs running on each node. "+
			"The groups facet is off by default and limited to %d nodes, because it can cost a remote call "+
			"per node. A facet that cannot be read is reported under errors and the rest is still returned.",
			maxDescribed, maxGroupsOf),
		Annotations: readOnly(),
	}, limited(s, s.describeNodes))

	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:  "query_slurm",
		Title: "Query Slurm",
		Description: "Read the workload manager. kind is one of: nodes (state and drain reason, filter by " +
			"nodes and state), jobs (the queue, filter by nodes, state and users), history (finished jobs " +
			"from the accounting database, filter by since, state, users and jobIds), partitions, or summary " +
			"(queued jobs counted per user, account and partition). Node states may be given as a group: " +
			"alloc, idle, drain, down or defect.",
		Annotations: readOnly(),
	}, limited(s, s.querySlurm))
}

// selectInput is the argument of select_nodes.
type selectInput struct {
	Expression string `json:"expression" jsonschema:"node set expression, for example exe[1-10], @rack:R02 or @exe&@rack:R02"`
}

// selectOutput is what select_nodes returns.
type selectOutput struct {
	Context   string   `json:"context"`
	Nodes     string   `json:"nodes" jsonschema:"the folded node set"`
	Count     int      `json:"count"`
	Expanded  []string `json:"expanded,omitempty" jsonschema:"every name, when the set is small enough to list"`
	Unknown   string   `json:"unknown,omitempty" jsonschema:"nodes the inventory does not know"`
	Protected string   `json:"protected,omitempty" jsonschema:"protected hosts in the set, which no change may touch"`
}

func (s *Server) selectNodes(ctx context.Context, _ *mcp.CallToolRequest, in selectInput) (*mcp.CallToolResult, *selectOutput, error) {
	a, err := s.app(ctx)
	if err != nil {
		return nil, nil, callError(err)
	}
	ns, err := a.Select(in.Expression)
	if err != nil {
		return nil, nil, callError(err)
	}
	out := &selectOutput{Context: s.context, Nodes: ns.String(), Count: ns.Len()}
	if ns.Len() <= maxExpanded {
		out.Expanded = ns.Expand()
	}
	if a.Inventory.Len() > 0 {
		out.Unknown = ns.Difference(a.Inventory.NodeSet()).String()
	}
	// The gate's own comparison, so that a set the gate would refuse is
	// never reported as free of protected hosts.
	protected, err := a.Gate.ProtectedIn(ns)
	if err != nil {
		return nil, nil, callError(err)
	}
	out.Protected = protected.String()
	return nil, out, nil
}

// describeInput is the argument of describe_nodes.
type describeInput struct {
	Nodes  string   `json:"nodes" jsonschema:"node set expression"`
	Facets []string `json:"facets,omitempty" jsonschema:"what to include: inventory, slurm, jobs, groups; default inventory, slurm and jobs"`
}

// nodeView is everything known about one node.
type nodeView struct {
	Name        string              `json:"name"`
	Host        string              `json:"host,omitempty"`
	BMC         string              `json:"bmc,omitempty"`
	BMCError    string              `json:"bmcError,omitempty" jsonschema:"why the node has no service processor host"`
	InInventory bool                `json:"inInventory"`
	Inventory   *inventory.Node     `json:"inventory,omitempty"`
	Groups      map[string][]string `json:"groups,omitempty"`
	Slurm       *slurm.Node         `json:"slurm,omitempty"`
	Jobs        []jobView           `json:"jobs,omitempty"`
}

// jobView is a job as it concerns one node.
type jobView struct {
	ID      string `json:"id"`
	User    string `json:"user,omitempty"`
	State   string `json:"state,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	Nodes   string `json:"nodes,omitempty"`
}

// describeOutput is what describe_nodes returns.
type describeOutput struct {
	Context   string            `json:"context"`
	Nodes     string            `json:"nodes"`
	Count     int               `json:"count"`
	Truncated bool              `json:"truncated,omitempty" jsonschema:"only the first nodes were described"`
	Items     []nodeView        `json:"items"`
	Errors    map[string]string `json:"errors,omitempty" jsonschema:"facets that could not be read, with the reason"`
}

var allFacets = []string{"inventory", "slurm", "jobs", "groups"}

func (s *Server) describeNodes(ctx context.Context, _ *mcp.CallToolRequest, in describeInput) (*mcp.CallToolResult, *describeOutput, error) {
	facets := in.Facets
	if len(facets) == 0 {
		facets = []string{"inventory", "slurm", "jobs"}
	}
	for _, f := range facets {
		if !slices.Contains(allFacets, f) {
			return nil, nil, callError(exitcode.Errorf(exitcode.Usage,
				"unknown facet %q; use %s", f, strings.Join(allFacets, ", ")))
		}
	}
	want := func(f string) bool { return slices.Contains(facets, f) }

	a, err := s.app(ctx)
	if err != nil {
		return nil, nil, callError(err)
	}
	ns, err := a.Select(in.Nodes)
	if err != nil {
		return nil, nil, callError(err)
	}
	out := &describeOutput{Context: s.context, Nodes: ns.String(), Count: ns.Len(), Errors: map[string]string{}}
	names := ns.Expand()
	if len(names) > maxDescribed {
		names, out.Truncated = names[:maxDescribed], true
	}
	described := nodeset.New()
	for _, name := range names {
		_ = described.Add(name)
	}

	out.Items = make([]nodeView, len(names))
	byName := map[string]*nodeView{}
	for i, name := range names {
		view := &out.Items[i]
		view.Name = name
		view.Host, _ = a.Namer.FQDN(name)
		// The host the bmc commands reach, or why there is none: an empty
		// field alone reads as a node that simply has no BMC.
		if bmc, err := a.BMCHost(name); err != nil {
			view.BMCError = err.Error()
		} else {
			view.BMC = bmc
		}
		if node, ok := a.Inventory.Lookup(name); ok {
			view.InInventory = true
			if want("inventory") {
				view.Inventory = node
			}
		}
		byName[name] = view
	}

	// The groups, the Slurm state and the jobs are read side by side, and
	// merged into the views once all of them are in.
	r := newReads(ctx, a)
	var groups *branch[[]map[string][]string]
	if want("groups") {
		if len(names) > maxGroupsOf {
			out.Errors["groups"] = fmt.Sprintf("asked for %d nodes; the groups facet answers for at most %d", len(names), maxGroupsOf)
		} else {
			groups = read(r, "the groups facet", nil, func() ([]map[string][]string, error) {
				return readGroups(r, names)
			})
		}
	}
	var sl slurmReads
	if want("slurm") || want("jobs") {
		sl = readSlurm(r, described, want)
	}
	r.wait()

	if groups != nil {
		for i, name := range names {
			// A failing source does not hide what the others found, for
			// this node or any other.
			if i < len(groups.value) {
				byName[name].Groups = groups.value[i]
			}
		}
		if groups.err != nil {
			out.Errors["groups"] = groups.err.Error()
		}
	}
	sl.merge(described, byName, out.Errors)
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	return nil, out, nil
}

// readGroups reads the groups of every node, fanout.PerHost nodes at a
// time, each holding a place on every host a group source sends its
// commands to, and returns them in the order of the names, with the first
// error in that order.
func readGroups(r *reads, names []string) ([]map[string][]string, error) {
	var on []string
	for _, src := range r.a.Spec.Groups.Sources {
		if src.Exec == nil {
			continue
		}
		// A role that cannot be resolved fails the lookup, which says so.
		if target, err := r.a.Role(src.Exec.Role); err == nil {
			on = append(on, r.on(target)...)
		}
	}
	outcomes := fanout.Map(r.ctx, names, fanout.Options[string]{
		Step:     "read the groups",
		Limit:    fanout.PerHost,
		Describe: func(name string) (node, host, role string) { return name, "", "" },
		PanicLog: r.a.Diag,
		Acquire: func(ctx context.Context, _ string) (func(), error) {
			return r.hosts.Acquire(ctx, on...)
		},
	}, func(ctx context.Context, name string) (map[string][]string, error) {
		return r.a.Groups.GroupsOfContext(ctx, name)
	})
	memberships := make([]map[string][]string, len(names))
	var first error
	for i, o := range outcomes {
		memberships[i] = o.Value
		if first == nil {
			first = o.Err
		}
	}
	return memberships, first
}

// slurmReads are the Slurm facets of describe_nodes as they were read.
type slurmReads struct {
	// err is why the workload manager could not be asked at all.
	err   error
	nodes *branch[[]slurm.Node]
	jobs  *branch[[]slurm.Job]
}

// readSlurm starts reading the Slurm facets asked for, sinfo and squeue side
// by side. A workload manager that cannot be reached is reported rather
// than failing the description, which is often asked for precisely because
// something is down.
func readSlurm(r *reads, ns *nodeset.NodeSet, want func(string) bool) slurmReads {
	c, err := r.a.Slurm()
	if err != nil {
		return slurmReads{err: err}
	}
	var sl slurmReads
	on := r.on(c.Target)
	if want("slurm") {
		sl.nodes = read(r, "sinfo", on, func() ([]slurm.Node, error) { return c.Nodes(r.ctx, ns, nil) })
	}
	if want("jobs") {
		sl.jobs = read(r, "squeue", on, func() ([]slurm.Job, error) {
			return c.Jobs(r.ctx, slurm.JobFilter{Nodes: ns})
		})
	}
	return sl
}

// merge fills the Slurm facets into the views, and what could not be read
// into errs.
func (sl slurmReads) merge(ns *nodeset.NodeSet, byName map[string]*nodeView, errs map[string]string) {
	if sl.err != nil {
		errs["slurm"] = sl.err.Error()
		return
	}
	if sl.nodes != nil {
		if sl.nodes.err != nil {
			errs["slurm"] = sl.nodes.err.Error()
		}
		for i := range sl.nodes.value {
			if view, ok := byName[sl.nodes.value[i].Name]; ok {
				view.Slurm = &sl.nodes.value[i]
			}
		}
	}
	if sl.jobs != nil {
		if sl.jobs.err != nil {
			errs["jobs"] = sl.jobs.err.Error()
		}
		for _, j := range sl.jobs.value {
			on, err := nodeset.Parse(j.Nodes)
			if err != nil {
				continue
			}
			for _, name := range on.Intersection(ns).Expand() {
				if view, ok := byName[name]; ok {
					view.Jobs = append(view.Jobs, jobView{
						ID: j.ID, User: j.User, State: j.State, Runtime: j.Runtime, Nodes: j.Nodes,
					})
				}
			}
		}
	}
}

// reads are the reads one call makes side by side, each in a goroutine of
// its own, and the places they hold on the hosts they go to: at most
// fanout.PerHost sessions at a time to any one host, a jump host on the way
// counted too, so that reads that go to one host, such as a group source
// and the Slurm clients both on the login node, share its places.
type reads struct {
	ctx   context.Context
	a     *app.App
	hosts fanout.Hosts
	wg    sync.WaitGroup
}

func newReads(ctx context.Context, a *app.App) *reads { return &reads{ctx: ctx, a: a} }

// on returns the hosts a session to target opens a connection to.
func (r *reads) on(target transport.Target) []string {
	return append(r.a.SSH.JumpHosts(target.Host), target.Host)
}

// wait returns once every read has.
func (r *reads) wait() { r.wg.Wait() }

// branch is what one read came to, once reads.wait has returned.
type branch[T any] struct {
	value T
	err   error
}

// read starts fn in a goroutine of r, once it holds a place on each of the
// hosts in on. A panic in it becomes its error, with the stack in the
// server's log: recover reaches only its own goroutine, and one bad call
// must not end the server and the plans it holds.
func read[T any](r *reads, what string, on []string, fn func() (T, error)) *branch[T] {
	b := &branch[T]{}
	r.wg.Go(func() {
		defer func() {
			if err := fanout.Recovered(r.a.Diag, what, recover()); err != nil {
				b.err = err
			}
		}()
		release, err := r.hosts.Acquire(r.ctx, on...)
		if err != nil {
			b.err = err
			return
		}
		defer release()
		b.value, b.err = fn()
	})
	return b
}

// slurmInput is the argument of query_slurm.
type slurmInput struct {
	Kind      string   `json:"kind" jsonschema:"nodes, jobs, history, partitions or summary"`
	Nodes     string   `json:"nodes,omitempty" jsonschema:"node set expression, for nodes and jobs"`
	State     string   `json:"state,omitempty" jsonschema:"state or comma separated states; for nodes also a group: alloc, idle, drain, down, defect"`
	Users     []string `json:"users,omitempty" jsonschema:"limit to these users, for jobs and history"`
	Since     string   `json:"since,omitempty" jsonschema:"how far back history looks, as a duration such as 24h; default from the configuration"`
	JobIDs    []string `json:"jobIds,omitempty" jsonschema:"look up these jobs in history instead of a time window"`
	Partition string   `json:"partition,omitempty" jsonschema:"limit partitions to one"`
	Limit     int      `json:"limit,omitempty" jsonschema:"most rows to return, default 200"`
}

// slurmOutput is what query_slurm returns.
type slurmOutput struct {
	Context   string `json:"context"`
	Kind      string `json:"kind"`
	Count     int    `json:"count" jsonschema:"how many rows matched"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"only the first rows are returned; narrow the query or raise limit"`
	Items     any    `json:"items"`
}

func (s *Server) querySlurm(ctx context.Context, _ *mcp.CallToolRequest, in slurmInput) (*mcp.CallToolResult, *slurmOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = defaultRows
	}
	limit = min(limit, maxRows)

	a, err := s.app(ctx)
	if err != nil {
		return nil, nil, callError(err)
	}
	c, err := a.Slurm()
	if err != nil {
		return nil, nil, callError(err)
	}
	var ns *nodeset.NodeSet
	if strings.TrimSpace(in.Nodes) != "" {
		if ns, err = a.Select(in.Nodes); err != nil {
			return nil, nil, callError(err)
		}
	}
	upper := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.ToUpper(s), ",")
	}

	var items any
	var count int
	switch in.Kind {
	case "nodes":
		nodes, err := c.Nodes(ctx, ns, slurm.States(in.State))
		if err != nil {
			return nil, nil, callError(exitcode.Wrap(exitcode.Transport, err))
		}
		count = len(nodes)
		items = nodes[:min(limit, count)]
	case "jobs":
		jobs, err := c.Jobs(ctx, slurm.JobFilter{Nodes: ns, States: upper(in.State), Users: in.Users})
		if err != nil {
			return nil, nil, callError(exitcode.Wrap(exitcode.Transport, err))
		}
		count = len(jobs)
		items = jobs[:min(limit, count)]
	case "history":
		filter := slurm.AccountingFilter{
			States:   upper(in.State),
			Users:    in.Users,
			JobIDs:   in.JobIDs,
			AllUsers: len(in.Users) == 0,
			Since:    a.Spec.Slurm.LookBack.Or(time.Hour),
		}
		if in.Since != "" {
			if filter.Since, err = time.ParseDuration(in.Since); err != nil {
				return nil, nil, callError(exitcode.Errorf(exitcode.Usage, "since: %v", err))
			}
		}
		jobs, err := c.History(ctx, filter)
		if err != nil {
			return nil, nil, callError(exitcode.Wrap(exitcode.Transport, err))
		}
		count = len(jobs)
		items = jobs[:min(limit, count)]
	case "partitions":
		partitions, err := c.Partitions(ctx, in.Partition)
		if err != nil {
			return nil, nil, callError(exitcode.Wrap(exitcode.Transport, err))
		}
		count = len(partitions)
		items = partitions[:min(limit, count)]
	case "summary":
		state := cmp.Or(in.State, "PENDING")
		jobs, err := c.Jobs(ctx, slurm.JobFilter{States: upper(state)})
		if err != nil {
			return nil, nil, callError(exitcode.Wrap(exitcode.Transport, err))
		}
		rows := slurm.CountJobs(jobs)
		count = len(rows)
		items = rows[:min(limit, count)]
	default:
		return nil, nil, callError(exitcode.Errorf(exitcode.Usage,
			"unknown kind %q; use nodes, jobs, history, partitions or summary", in.Kind))
	}
	return nil, &slurmOutput{
		Context: s.context, Kind: in.Kind, Count: count, Truncated: count > limit, Items: items,
	}, nil
}
