// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
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
	}, s.selectNodes)

	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:  "describe_nodes",
		Title: "Describe nodes",
		Description: fmt.Sprintf("Describe up to %d nodes in one call: host and service processor names, "+
			"the inventory entry, the Slurm state with its reason, and the jobs running on each node. "+
			"The groups facet is off by default and limited to %d nodes, because it can cost a remote call "+
			"per node. A facet that cannot be read is reported under errors and the rest is still returned.",
			maxDescribed, maxGroupsOf),
		Annotations: readOnly(),
	}, s.describeNodes)

	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:  "query_slurm",
		Title: "Query Slurm",
		Description: "Read the workload manager. kind is one of: nodes (state and drain reason, filter by " +
			"nodes and state), jobs (the queue, filter by nodes, state and users), history (finished jobs " +
			"from the accounting database, filter by since, state, users and jobIds), partitions, or summary " +
			"(queued jobs counted per user, account and partition). Node states may be given as a group: " +
			"alloc, idle, drain, down or defect.",
		Annotations: readOnly(),
	}, s.querySlurm)
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
	a, _, err := s.app(ctx)
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

	a, _, err := s.app(ctx)
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
		view.BMC, _ = a.Namer.BMC(name)
		if node, ok := a.Inventory.Lookup(name); ok {
			view.InInventory = true
			if want("inventory") {
				view.Inventory = node
			}
		}
		byName[name] = view
	}

	if want("groups") {
		if len(names) > maxGroupsOf {
			out.Errors["groups"] = fmt.Sprintf("asked for %d nodes; the groups facet answers for at most %d", len(names), maxGroupsOf)
		} else {
			for _, name := range names {
				// A failing source does not hide what the others found.
				memberships, err := a.Groups.GroupsOf(name)
				byName[name].Groups = memberships
				if err != nil {
					out.Errors["groups"] = err.Error()
					break
				}
			}
		}
	}

	if want("slurm") || want("jobs") {
		s.describeSlurm(ctx, a, described, byName, want, out.Errors)
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	return nil, out, nil
}

// describeSlurm fills in the Slurm facets. A workload manager that cannot be
// reached is reported rather than failing the description, which is often
// asked for precisely because something is down.
func (s *Server) describeSlurm(ctx context.Context, a *app.App, ns *nodeset.NodeSet, byName map[string]*nodeView, want func(string) bool, errs map[string]string) {
	c, err := a.Slurm()
	if err != nil {
		errs["slurm"] = err.Error()
		return
	}
	if want("slurm") {
		nodes, err := c.Nodes(ctx, ns, nil)
		if err != nil {
			errs["slurm"] = err.Error()
		}
		for i := range nodes {
			if view, ok := byName[nodes[i].Name]; ok {
				view.Slurm = &nodes[i]
			}
		}
	}
	if want("jobs") {
		jobs, err := c.Jobs(ctx, slurm.JobFilter{Nodes: ns})
		if err != nil {
			errs["jobs"] = err.Error()
		}
		for _, j := range jobs {
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

	a, _, err := s.app(ctx)
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
		state := in.State
		if state == "" {
			state = "PENDING"
		}
		jobs, err := c.Jobs(ctx, slurm.JobFilter{States: upper(state)})
		if err != nil {
			return nil, nil, callError(exitcode.Wrap(exitcode.Transport, err))
		}
		rows := summarise(jobs)
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

// summaryRow counts the jobs of one user in one account and partition.
type summaryRow struct {
	User      string `json:"user"`
	Account   string `json:"account"`
	Partition string `json:"partition"`
	Jobs      int    `json:"jobs"`
}

// summarise counts jobs per user, account and partition, most jobs first.
func summarise(jobs []slurm.Job) []summaryRow {
	type key struct{ user, account, partition string }
	counts := map[key]int{}
	for _, j := range jobs {
		counts[key{j.User, j.Account, j.Partition}]++
	}
	rows := make([]summaryRow, 0, len(counts))
	for k, n := range counts {
		rows = append(rows, summaryRow{User: k.user, Account: k.account, Partition: k.partition, Jobs: n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Jobs != rows[j].Jobs {
			return rows[i].Jobs > rows[j].Jobs
		}
		if rows[i].User != rows[j].User {
			return rows[i].User < rows[j].User
		}
		return rows[i].Account+rows[i].Partition < rows[j].Account+rows[j].Partition
	})
	return rows
}
