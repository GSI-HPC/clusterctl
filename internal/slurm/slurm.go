// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package slurm reads and changes the state of a Slurm cluster through its
// command line clients, which run on a host that has them.
//
// Output is asked for in the parsable form with an explicit field list and
// split on the separator, rather than parsed out of the aligned display form.
// The display form changes with the terminal width and truncates, which is
// how a power state ended up reported against the wrong node.
package slurm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// separator is what --parsable2 puts between fields.
const separator = "|"

// Client runs the Slurm clients on the configured host.
type Client struct {
	// Runner executes the client, usually over ssh.
	Runner transport.Runner
	// Target is the host the clients run on.
	Target transport.Target
	// Spec is the Slurm configuration of the cluster.
	Spec v1alpha1.SlurmSpec
	// Timeout bounds one client invocation.
	Timeout time.Duration
}

// run executes one client and returns its lines.
func (c *Client) run(ctx context.Context, argv []string) ([]string, error) {
	result, err := c.Runner.Run(ctx, c.Target, transport.Request{
		Argv:    argv,
		Timeout: c.timeout(),
		TTY:     transport.TTYNone,
	})
	if err != nil {
		return nil, err
	}
	if result.Failed() {
		if result.Err != nil {
			return nil, result.Err
		}
		return nil, fmt.Errorf("%s exited %d: %s", argv[0], result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return result.Lines(), nil
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 2 * time.Minute
}

// fields splits a parsable line into exactly n fields, padding a short line
// so that a missing trailing value does not shift the others.
func fields(line string, n int) []string {
	out := strings.Split(line, separator)
	for len(out) < n {
		out = append(out, "")
	}
	return out[:n]
}

// Node is one node as Slurm sees it.
type Node struct {
	Name       string `json:"name" yaml:"name"`
	State      string `json:"state" yaml:"state"`
	Partition  string `json:"partition,omitempty" yaml:"partition,omitempty"`
	CPUs       string `json:"cpus,omitempty" yaml:"cpus,omitempty"`
	Memory     string `json:"memory,omitempty" yaml:"memory,omitempty"`
	Features   string `json:"features,omitempty" yaml:"features,omitempty"`
	GRES       string `json:"gres,omitempty" yaml:"gres,omitempty"`
	Reason     string `json:"reason,omitempty" yaml:"reason,omitempty"`
	ReasonUser string `json:"reasonUser,omitempty" yaml:"reasonUser,omitempty"`
	ReasonTime string `json:"reasonTime,omitempty" yaml:"reasonTime,omitempty"`
}

// BaseState strips the flag characters Slurm appends to a state, so that
// "idle*" and "idle" compare equal.
func (n Node) BaseState() string {
	return strings.TrimRight(strings.ToLower(n.State), "*~#$@+")
}

// Nodes lists the nodes, optionally limited to a set or to states.
func (c *Client) Nodes(ctx context.Context, ns *nodeset.NodeSet, states []string) ([]Node, error) {
	argv := []string{"sinfo", "--noheader", "--Node", "--format",
		"%N|%T|%R|%c|%m|%f|%G|%E|%u|%H"}
	if ns != nil && !ns.IsEmpty() {
		argv = append(argv, "--nodes", ns.Hostlist())
	}
	if len(states) > 0 {
		argv = append(argv, "--states", strings.Join(states, ","))
	}
	lines, err := c.run(ctx, argv)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	out := make([]Node, 0, len(lines))
	for _, line := range lines {
		f := fields(line, 10)
		// A node in several partitions is listed once per partition.
		if seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		out = append(out, Node{
			Name: f[0], State: f[1], Partition: f[2], CPUs: f[3], Memory: f[4],
			Features: f[5], GRES: f[6], Reason: f[7], ReasonUser: f[8], ReasonTime: f[9],
		})
	}
	return out, nil
}

// NodeSet returns the nodes in the given states as a set.
func (c *Client) NodeSet(ctx context.Context, states []string) (*nodeset.NodeSet, error) {
	nodes, err := c.Nodes(ctx, nil, states)
	if err != nil {
		return nil, err
	}
	ns := nodeset.New()
	for _, n := range nodes {
		if err := ns.Add(n.Name); err != nil {
			return nil, err
		}
	}
	return ns, nil
}

// StateGroups maps the short names the commands accept to the Slurm states
// they stand for.
func StateGroups() map[string][]string {
	return map[string][]string{
		"alloc":  {"alloc", "allocated", "mix", "mixed", "comp", "completing"},
		"idle":   {"idle"},
		"drain":  {"drain", "draining", "drained"},
		"down":   {"down", "no_respond", "power_down", "unk", "unknown"},
		"defect": {"drain", "draining", "drained", "down", "fail", "failing"},
	}
}

// States turns a state group name, or a comma separated list of states, into
// the states to ask Slurm for. An empty argument asks for every state.
func States(arg string) []string {
	if arg == "" {
		return nil
	}
	if group, ok := StateGroups()[strings.ToLower(arg)]; ok {
		return group
	}
	return strings.Split(arg, ",")
}

// Drain removes nodes from production with a reason.
//
// The reason is mandatory: a drained node with no reason is a node nobody
// dares resume.
func (c *Client) Drain(ctx context.Context, ns *nodeset.NodeSet, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("draining a node needs a reason")
	}
	_, err := c.run(ctx, []string{"scontrol", "update",
		"nodename=" + ns.Hostlist(), "state=drain", "reason=" + reason})
	return err
}

// Resume puts nodes back into production.
func (c *Client) Resume(ctx context.Context, ns *nodeset.NodeSet) error {
	_, err := c.run(ctx, []string{"scontrol", "update",
		"nodename=" + ns.Hostlist(), "state=resume"})
	return err
}

// Partition is one Slurm partition.
type Partition struct {
	Name        string `json:"name" yaml:"name"`
	Available   string `json:"available,omitempty" yaml:"available,omitempty"`
	Groups      string `json:"groups,omitempty" yaml:"groups,omitempty"`
	Nodes       string `json:"nodes,omitempty" yaml:"nodes,omitempty"`
	NodeCount   string `json:"nodeCount,omitempty" yaml:"nodeCount,omitempty"`
	DefaultTime string `json:"defaultTime,omitempty" yaml:"defaultTime,omitempty"`
	MaxTime     string `json:"maxTime,omitempty" yaml:"maxTime,omitempty"`
	Memory      string `json:"memory,omitempty" yaml:"memory,omitempty"`
	CPUs        string `json:"cpus,omitempty" yaml:"cpus,omitempty"`
	CPUState    string `json:"cpuState,omitempty" yaml:"cpuState,omitempty"`
}

// Partitions lists the partitions.
func (c *Client) Partitions(ctx context.Context, name string) ([]Partition, error) {
	argv := []string{"sinfo", "--noheader", "--format", "%P|%a|%g|%D|%l|%L|%m|%c|%C|%N"}
	if name != "" {
		argv = append(argv, "--partition", name)
	}
	lines, err := c.run(ctx, argv)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]Partition, 0, len(lines))
	for _, line := range lines {
		f := fields(line, 10)
		if seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		out = append(out, Partition{
			Name: f[0], Available: f[1], Groups: f[2], NodeCount: f[3],
			MaxTime: f[4], DefaultTime: f[5], Memory: f[6], CPUs: f[7],
			CPUState: f[8], Nodes: f[9],
		})
	}
	return out, nil
}

// Job is one job in the queue.
type Job struct {
	ID        string `json:"id" yaml:"id"`
	User      string `json:"user,omitempty" yaml:"user,omitempty"`
	Account   string `json:"account,omitempty" yaml:"account,omitempty"`
	Partition string `json:"partition,omitempty" yaml:"partition,omitempty"`
	State     string `json:"state,omitempty" yaml:"state,omitempty"`
	Nodes     string `json:"nodes,omitempty" yaml:"nodes,omitempty"`
	NodeCount string `json:"nodeCount,omitempty" yaml:"nodeCount,omitempty"`
	CPUs      string `json:"cpus,omitempty" yaml:"cpus,omitempty"`
	TimeLimit string `json:"timeLimit,omitempty" yaml:"timeLimit,omitempty"`
	Runtime   string `json:"runtime,omitempty" yaml:"runtime,omitempty"`
	Priority  string `json:"priority,omitempty" yaml:"priority,omitempty"`
	Reason    string `json:"reason,omitempty" yaml:"reason,omitempty"`
	WorkDir   string `json:"workDir,omitempty" yaml:"workDir,omitempty"`
	Command   string `json:"command,omitempty" yaml:"command,omitempty"`
}

// JobFilter selects which jobs to list.
type JobFilter struct {
	States []string
	Users  []string
	Nodes  *nodeset.NodeSet
}

// Jobs lists the jobs in the queue.
func (c *Client) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	argv := []string{"squeue", "--noheader", "--format",
		"%i|%u|%a|%P|%T|%N|%D|%C|%l|%M|%Q|%r|%Z|%o"}
	if len(f.States) > 0 {
		argv = append(argv, "--states", strings.Join(f.States, ","))
	}
	if len(f.Users) > 0 {
		argv = append(argv, "--user", strings.Join(f.Users, ","))
	}
	if f.Nodes != nil && !f.Nodes.IsEmpty() {
		argv = append(argv, "--nodelist", f.Nodes.Hostlist())
	}
	lines, err := c.run(ctx, argv)
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(lines))
	for _, line := range lines {
		f := fields(line, 14)
		out = append(out, Job{
			ID: f[0], User: f[1], Account: f[2], Partition: f[3], State: f[4],
			Nodes: f[5], NodeCount: f[6], CPUs: f[7], TimeLimit: f[8], Runtime: f[9],
			Priority: f[10], Reason: f[11], WorkDir: f[12], Command: f[13],
		})
	}
	return out, nil
}

// Accounting is one finished job from the accounting database.
type Accounting struct {
	JobID     string `json:"jobId" yaml:"jobId"`
	User      string `json:"user,omitempty" yaml:"user,omitempty"`
	Account   string `json:"account,omitempty" yaml:"account,omitempty"`
	State     string `json:"state,omitempty" yaml:"state,omitempty"`
	ExitCode  string `json:"exitCode,omitempty" yaml:"exitCode,omitempty"`
	Nodes     string `json:"nodes,omitempty" yaml:"nodes,omitempty"`
	Elapsed   string `json:"elapsed,omitempty" yaml:"elapsed,omitempty"`
	Start     string `json:"start,omitempty" yaml:"start,omitempty"`
	End       string `json:"end,omitempty" yaml:"end,omitempty"`
	Submit    string `json:"submit,omitempty" yaml:"submit,omitempty"`
	Partition string `json:"partition,omitempty" yaml:"partition,omitempty"`
}

// AccountingFilter selects which finished jobs to list.
type AccountingFilter struct {
	States   []string
	Users    []string
	JobIDs   []string
	Since    time.Duration
	AllUsers bool
}

// History reads finished jobs from the accounting database.
func (c *Client) History(ctx context.Context, f AccountingFilter) ([]Accounting, error) {
	argv := []string{"sacct", "--noheader", "--parsable2", "--allocations", "--format",
		"jobid,user,account,state,exitcode,nodelist,elapsed,start,end,submit,partition"}
	if len(f.JobIDs) > 0 {
		argv = append(argv, "--jobs", strings.Join(f.JobIDs, ","))
	} else {
		since := f.Since
		if since <= 0 {
			since = time.Hour
		}
		argv = append(argv, "--starttime", time.Now().Add(-since).Format("2006-01-02T15:04:05"))
	}
	if len(f.States) > 0 {
		argv = append(argv, "--state", strings.Join(f.States, ","))
	}
	switch {
	case len(f.Users) > 0:
		argv = append(argv, "--user", strings.Join(f.Users, ","))
	case f.AllUsers:
		argv = append(argv, "--allusers")
	}

	lines, err := c.run(ctx, argv)
	if err != nil {
		return nil, err
	}
	out := make([]Accounting, 0, len(lines))
	for _, line := range lines {
		f := fields(line, 11)
		out = append(out, Accounting{
			JobID: f[0], User: f[1], Account: f[2], State: f[3], ExitCode: f[4],
			Nodes: f[5], Elapsed: f[6], Start: f[7], End: f[8], Submit: f[9], Partition: f[10],
		})
	}
	return out, nil
}

// Association is a user or account entry of the accounting database.
type Association struct {
	Account        string `json:"account" yaml:"account"`
	User           string `json:"user,omitempty" yaml:"user,omitempty"`
	DefaultAccount string `json:"defaultAccount,omitempty" yaml:"defaultAccount,omitempty"`
	Coordinators   string `json:"coordinators,omitempty" yaml:"coordinators,omitempty"`
	Organization   string `json:"organization,omitempty" yaml:"organization,omitempty"`
	Description    string `json:"description,omitempty" yaml:"description,omitempty"`
	FairShare      string `json:"fairShare,omitempty" yaml:"fairShare,omitempty"`
	MaxJobs        string `json:"maxJobs,omitempty" yaml:"maxJobs,omitempty"`
	MaxSubmit      string `json:"maxSubmit,omitempty" yaml:"maxSubmit,omitempty"`
	MaxNodes       string `json:"maxNodes,omitempty" yaml:"maxNodes,omitempty"`
	MaxCPUs        string `json:"maxCpus,omitempty" yaml:"maxCpus,omitempty"`
	MaxWall        string `json:"maxWall,omitempty" yaml:"maxWall,omitempty"`
}

// Accounts lists the accounts with their coordinators.
func (c *Client) Accounts(ctx context.Context, name string) ([]Association, error) {
	argv := []string{"sacctmgr", "--noheader", "--parsable2", "list", "account",
		"withcoordinator", "format=Account,Description,Organization,Coordinators"}
	if name != "" {
		argv = append(argv, "where", "name="+name)
	}
	lines, err := c.run(ctx, argv)
	if err != nil {
		return nil, err
	}
	out := make([]Association, 0, len(lines))
	for _, line := range lines {
		f := fields(line, 4)
		out = append(out, Association{
			Account: f[0], Description: f[1], Organization: f[2], Coordinators: f[3],
		})
	}
	return out, nil
}

// AccountLimits lists the associations of an account with their limits.
func (c *Client) AccountLimits(ctx context.Context, account string) ([]Association, error) {
	argv := []string{"sacctmgr", "--noheader", "--parsable2", "--associations", "list", "account",
		"format=Account,User,MaxSubmit,MaxJobs,MaxNodes,MaxCPUs,MaxWall,Fairshare"}
	if account != "" {
		argv = append(argv, "where", "name="+account)
	}
	lines, err := c.run(ctx, argv)
	if err != nil {
		return nil, err
	}
	out := make([]Association, 0, len(lines))
	for _, line := range lines {
		f := fields(line, 8)
		out = append(out, Association{
			Account: f[0], User: f[1], MaxSubmit: f[2], MaxJobs: f[3],
			MaxNodes: f[4], MaxCPUs: f[5], MaxWall: f[6], FairShare: f[7],
		})
	}
	return out, nil
}

// Users lists the user associations.
func (c *Client) Users(ctx context.Context, user string) ([]Association, error) {
	argv := []string{"sacctmgr", "--noheader", "--parsable2", "show", "user", "withassoc",
		"format=User,Account,DefaultAccount,Fairshare"}
	if user != "" {
		argv = append(argv, "where", "name="+user)
	}
	lines, err := c.run(ctx, argv)
	if err != nil {
		return nil, err
	}
	out := make([]Association, 0, len(lines))
	for _, line := range lines {
		f := fields(line, 4)
		out = append(out, Association{
			User: f[0], Account: f[1], DefaultAccount: f[2], FairShare: f[3],
		})
	}
	return out, nil
}

// Shares lists the fair-share situation.
func (c *Client) Shares(ctx context.Context, account string) ([]Association, error) {
	argv := []string{"sacctmgr", "--noheader", "--parsable2", "list", "account",
		"withassoc", "format=Account,User,Fairshare"}
	if account != "" {
		argv = append(argv, "where", "name="+account)
	}
	lines, err := c.run(ctx, argv)
	if err != nil {
		return nil, err
	}
	out := make([]Association, 0, len(lines))
	for _, line := range lines {
		f := fields(line, 3)
		out = append(out, Association{Account: f[0], User: f[1], FairShare: f[2]})
	}
	return out, nil
}

// AddAccount creates an account.
func (c *Client) AddAccount(ctx context.Context, name, organization, description string) error {
	if organization == "" {
		organization = c.Spec.Organization
	}
	if organization == "" {
		organization = "default"
	}
	if description == "" {
		description = name
	}
	_, err := c.run(ctx, []string{"sacctmgr", "--immediate", "add", "account", name,
		"description=" + description, "organization=" + organization})
	return err
}

// SetCoordinators makes users coordinators of an account.
func (c *Client) SetCoordinators(ctx context.Context, account string, users []string) error {
	_, err := c.run(ctx, []string{"sacctmgr", "--immediate", "add", "coordinator",
		"account=" + account, "names=" + strings.Join(users, ",")})
	return err
}

// SetFairShare sets the fair-share value of an account.
func (c *Client) SetFairShare(ctx context.Context, account, value string) error {
	_, err := c.run(ctx, []string{"sacctmgr", "--immediate", "modify", "account",
		"where", "name=" + account, "set", "fairshare=" + value})
	return err
}

// HasPosixUser reports whether the cluster knows a user account at all.
//
// getent is asked rather than id, because id answers from the local files as
// well and would claim a user exists that the directory does not know.
func (c *Client) HasPosixUser(ctx context.Context, user string) (bool, error) {
	result, err := c.Runner.Run(ctx, c.Target, transport.Request{
		Argv:    []string{"getent", "passwd", user},
		Timeout: c.timeout(),
		TTY:     transport.TTYNone,
	})
	if err != nil {
		return false, err
	}
	if result.Err != nil {
		return false, result.Err
	}
	return result.ExitCode == 0 && strings.TrimSpace(result.Stdout) != "", nil
}

// AddUser associates a user with an account, creating the association when
// the user has none.
func (c *Client) AddUser(ctx context.Context, user, account, defaultAccount string) error {
	if account == "" {
		account = c.Spec.DefaultAccount
	}
	if account == "" {
		account = "default"
	}
	if defaultAccount == "" {
		defaultAccount = account
	}
	existing, err := c.Users(ctx, user)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		_, err = c.run(ctx, []string{"sacctmgr", "--immediate", "create", "user",
			"name=" + user, "account=" + account, "defaultaccount=" + defaultAccount})
		return err
	}
	_, err = c.run(ctx, []string{"sacctmgr", "--immediate", "add", "user",
		"account=" + account, "names=" + user})
	return err
}

// SetDefaultAccount changes the default account of a user.
func (c *Client) SetDefaultAccount(ctx context.Context, user, account string) error {
	_, err := c.run(ctx, []string{"sacctmgr", "--immediate", "modify", "user",
		"where", "name=" + user, "set", "defaultaccount=" + account})
	return err
}
