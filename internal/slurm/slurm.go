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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// separator is what --parsable2 puts between fields.
const separator = "|"

// Client runs the Slurm clients on the configured host.
type Client struct {
	// Runner executes the clients that change something, usually over ssh.
	Runner transport.Runner
	// Reader executes the clients that only read. It stays the real
	// transport in a dry run, so that the checks made before a change see
	// the cluster rather than an empty recording. It defaults to Runner.
	Reader transport.Runner
	// Target is the host the clients run on.
	Target transport.Target
	// Spec is the Slurm configuration of the cluster.
	Spec v1alpha1.SlurmSpec
	// Timeout bounds one client invocation.
	Timeout time.Duration

	// cluster is the ClusterName of slurm.conf, once it has been read.
	cluster string
}

// run executes a client that changes something and returns its lines.
func (c *Client) run(ctx context.Context, argv []string) ([]string, error) {
	result, err := c.exec(ctx, c.Runner, argv)
	if err != nil {
		return nil, err
	}
	return result.Lines(), nil
}

// read executes a client that only reads and returns its output.
func (c *Client) read(ctx context.Context, argv []string) (*transport.Result, error) {
	runner := c.Reader
	if runner == nil {
		runner = c.Runner
	}
	return c.exec(ctx, runner, argv)
}

// readLines executes a client that only reads and returns its lines.
func (c *Client) readLines(ctx context.Context, argv []string) ([]string, error) {
	result, err := c.read(ctx, argv)
	if err != nil {
		return nil, err
	}
	return result.Lines(), nil
}

// exec runs one client. Every error it returns carries an exit code: a login
// node that cannot be reached is a transport failure, and a client that ran
// and refused is a failed target, reported with what it said.
func (c *Client) exec(ctx context.Context, runner transport.Runner, argv []string) (*transport.Result, error) {
	result, err := runner.Run(ctx, c.Target, transport.Request{
		Argv:    argv,
		Timeout: c.timeout(),
		TTY:     transport.TTYNone,
	})
	if err != nil {
		return nil, keepCode(exitcode.Transport, err)
	}
	if result.Failed() {
		return result, c.failure(argv[0], result)
	}
	return result, nil
}

// failure describes a client that did not succeed.
//
// The transport reports any non-zero exit as "command exited N", which
// drops what Slurm said. Only a failure the transport classified itself, such
// as ssh's own exit 255, or one with no exit status at all, is passed on as
// it is.
func (c *Client) failure(program string, result *transport.Result) error {
	var coded *exitcode.Error
	if result.Err != nil && (errors.As(result.Err, &coded) || result.ExitCode <= 0) {
		return keepCode(exitcode.Transport, result.Err)
	}
	message := oneLine(result.Stderr)
	if message == "" {
		return exitcode.Errorf(exitcode.TargetFailed, "%s on %s exited %d", program, c.Target, result.ExitCode)
	}
	return exitcode.Errorf(exitcode.TargetFailed, "%s on %s exited %d: %s", program, c.Target, result.ExitCode, message)
}

// keepCode gives an error an exit code unless it carries one already.
func keepCode(code int, err error) error {
	var coded *exitcode.Error
	if errors.As(err, &coded) {
		return err
	}
	return exitcode.Wrap(code, err)
}

// oneLine makes what a client printed safe to put into one line of an error:
// its lines are joined with "; " and escaped with output.EscapeCell.
func oneLine(s string) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return output.EscapeCell(strings.Join(lines, "; "))
}

// isControl reports the C0 and C1 control characters and DEL.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
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

// framed is a sinfo or squeue format whose fields are separated by a token
// drawn at random for each query.
//
// Some fields are written by people: anyone who can drain a node writes its
// reason, and any user writes the working directory and command line of a
// job. Such a field can hold the separator and line breaks, and with a plain
// "|" a reason could add rows that look like other nodes. Nobody can write a
// token they do not know, so every record is read back exactly as Slurm
// printed it.
type framed struct {
	fields []string
	sep    string
}

func newFramed(fields ...string) framed {
	b := make([]byte, 12)
	// crypto/rand does not fail on the platforms Go supports.
	_, _ = rand.Read(b)
	return framed{fields: fields, sep: "|" + hex.EncodeToString(b) + "|"}
}

// format is the --format argument. Each field, the last one too, is
// followed by the separator, so a record ends in it.
func (f framed) format() string {
	return strings.Join(f.fields, f.sep) + f.sep
}

// records splits the output into records of exactly as many fields as the
// format has.
func (f framed) records(program, stdout string) ([][]string, error) {
	parts := strings.Split(stdout, f.sep)
	rest := parts[len(parts)-1]
	parts = parts[:len(parts)-1]
	n := len(f.fields)
	if strings.TrimSpace(rest) != "" || len(parts)%n != 0 {
		return nil, exitcode.Errorf(exitcode.TargetFailed,
			"%s printed output that does not have the %d fields asked for", program, n)
	}
	out := make([][]string, 0, len(parts)/n)
	for i := 0; i < len(parts); i += n {
		record := append([]string(nil), parts[i:i+n]...)
		// Every record but the first starts on a new line.
		if i > 0 {
			first, ok := strings.CutPrefix(record[0], "\n")
			if !ok {
				return nil, exitcode.Errorf(exitcode.TargetFailed,
					"%s printed output that does not have the %d fields asked for", program, n)
			}
			record[0] = first
		}
		out = append(out, record)
	}
	return out, nil
}

// hostName matches a single node name as sinfo prints it with --Node.
var hostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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
	state, _, _ := strings.Cut(strings.ToLower(n.State), "+")
	return strings.TrimRight(state, "*~#!%$@^-")
}

// noReason reports whether sinfo's reason field says there is none. sinfo
// prints "none" for %E; "(null)" and the empty string come from older
// clients and other fields.
func noReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "", "none", "(null)":
		return true
	}
	return false
}

// Nodes lists the nodes, optionally limited to a set or to states.
//
// A state is matched against the state Slurm reports for the node, not only
// against what sinfo selects by: sinfo matches a plain state against the base
// state and the flags, so asking it for idle also returns drained nodes.
func (c *Client) Nodes(ctx context.Context, ns *nodeset.NodeSet, states []string) ([]Node, error) {
	// The reason goes last; it is the one field people write.
	format := newFramed("%N", "%T", "%R", "%c", "%m", "%f", "%G", "%u", "%H", "%E")
	argv := []string{"sinfo", "--noheader", "--Node", "--format", format.format()}
	if ns != nil && !ns.IsEmpty() {
		argv = append(argv, "--nodes", ns.Hostlist())
	}
	if len(states) > 0 {
		argv = append(argv, "--states", strings.Join(states, ","))
	}
	result, err := c.read(ctx, argv)
	if err != nil {
		return nil, err
	}
	records, err := format.records("sinfo", result.Stdout)
	if err != nil {
		return nil, err
	}
	keep := stateFilter(states)

	seen := map[string]bool{}
	out := make([]Node, 0, len(records))
	for _, f := range records {
		if !hostName.MatchString(f[0]) {
			return nil, exitcode.Errorf(exitcode.TargetFailed, "sinfo printed %q where a node name belongs", f[0])
		}
		// A node in several partitions is listed once per partition.
		if seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		n := Node{
			Name: f[0], State: f[1], Partition: f[2], CPUs: f[3], Memory: f[4],
			Features: f[5], GRES: f[6], ReasonUser: f[7], ReasonTime: f[8], Reason: f[9],
		}
		// sinfo names a user and a time even when there is no reason.
		if noReason(n.Reason) {
			n.Reason, n.ReasonUser, n.ReasonTime = "", "", ""
		}
		if keep(n) {
			out = append(out, n)
		}
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
		"down":   {"down", "unk", "unknown"},
		"defect": {"drain", "draining", "drained", "down", "fail", "failing"},
	}
}

// reported maps the state names sinfo accepts to the base states %T prints
// for them.
var reported = map[string][]string{
	"alloc": {"allocated"}, "allocated": {"allocated"},
	"mix": {"mixed"}, "mixed": {"mixed"},
	"comp": {"completing"}, "completing": {"completing"},
	"idle":  {"idle"},
	"drain": {"draining", "drained"}, "draining": {"draining"}, "drained": {"drained"},
	"down": {"down"},
	"unk":  {"unknown"}, "unknown": {"unknown"},
	"fail": {"fail"}, "failing": {"failing"},
	"resv": {"reserved"}, "reserved": {"reserved"},
	"futr": {"future"}, "future": {"future"},
	"planned": {"planned"},
	"inval":   {"inval"},
}

// stateFilter returns what keeps a node sinfo returned for the states.
//
// A state that stands for a base state keeps only nodes that report it. A
// state sinfo matches by a flag, such as no_respond or power_down, cannot be
// told apart in the reported state, so a list that holds one keeps what sinfo
// returned; the state groups hold none.
func stateFilter(states []string) func(Node) bool {
	want := map[string]bool{}
	for _, s := range states {
		bases, ok := reported[strings.ToLower(strings.TrimSpace(s))]
		if !ok {
			return func(Node) bool { return true }
		}
		for _, b := range bases {
			want[b] = true
		}
	}
	if len(want) == 0 {
		return func(Node) bool { return true }
	}
	return func(n Node) bool { return want[n.BaseState()] }
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

// CheckNodes makes sure Slurm reads a node set as exactly those nodes.
//
// scontrol update does not take its node list as plain host names: slurmctld
// expands ALL, in any case, to every node and a NodeSet name of slurm.conf to
// its members. Such a word looks like one host to clusterctl, so the gate
// would preview, count and check one host while Slurm changed many. sinfo
// reads the list the same way, so the set is refused unless sinfo returns
// exactly the nodes named, and a node Slurm does not know is refused before
// scontrol changes the ones it reached and then stops. A node that belongs to
// no partition is not listed by sinfo and is refused too, which errs on the
// safe side.
func (c *Client) CheckNodes(ctx context.Context, ns *nodeset.NodeSet) error {
	if err := refuseAll(ns); err != nil {
		return err
	}
	// --all includes the nodes of hidden partitions, which scontrol changes
	// all the same.
	lines, err := c.readLines(ctx, []string{"sinfo", "--all", "--noheader", "--Node", "--format", "%N",
		"--nodes", ns.Hostlist()})
	if err != nil {
		return err
	}
	got := nodeset.New()
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if !hostName.MatchString(name) {
			return exitcode.Errorf(exitcode.TargetFailed, "sinfo printed %q where a node name belongs", name)
		}
		if err := got.Add(name); err != nil {
			return exitcode.Wrap(exitcode.TargetFailed, err)
		}
	}
	if extra := got.Difference(ns); !extra.IsEmpty() {
		return exitcode.Errorf(exitcode.Usage,
			"Slurm reads %s as more nodes than were named, among them %s; name the nodes themselves",
			ns, extra)
	}
	if missing := ns.Difference(got); !missing.IsEmpty() {
		return exitcode.Errorf(exitcode.Usage, "Slurm does not know %s; leave these out", missing)
	}
	return nil
}

// refuseAll refuses a name slurmctld expands to every node.
func refuseAll(ns *nodeset.NodeSet) error {
	for _, name := range ns.Expand() {
		if strings.EqualFold(name, "all") {
			return exitcode.Errorf(exitcode.Usage,
				"%q means every node to Slurm, not a host; name the nodes, for example with @slurm:PARTITION", name)
		}
	}
	return nil
}

// MaxReasonLength is the longest drain reason accepted, in characters.
const MaxReasonLength = 200

// ValidateReason checks a drain reason before anything is shown or sent.
//
// The reason is mandatory: a drained node with no reason is a node nobody
// dares resume. It is shown in the confirmation, stored by Slurm and printed
// back by sinfo, so it may not hold control characters, which could rewrite
// what the confirmation shows, nor the separator of the parsable output.
func ValidateReason(reason string) error {
	trimmed := strings.TrimSpace(reason)
	switch {
	case trimmed == "":
		return exitcode.Errorf(exitcode.Usage,
			"draining a node needs a reason: say what is wrong and where it is tracked")
	case !utf8.ValidString(reason):
		return exitcode.Errorf(exitcode.Usage, "the reason is not valid UTF-8")
	case utf8.RuneCountInString(trimmed) > MaxReasonLength:
		return exitcode.Errorf(exitcode.Usage,
			"the reason is longer than %d characters; say what is wrong and where it is tracked", MaxReasonLength)
	case noReason(trimmed):
		return exitcode.Errorf(exitcode.Usage,
			"%q is what Slurm prints for no reason; say what is wrong and where it is tracked", trimmed)
	}
	for _, r := range reason {
		if isControl(r) {
			return exitcode.Errorf(exitcode.Usage,
				"the reason contains the control character %s; write it on one line of plain text",
				strings.Trim(strconv.QuoteRune(r), "'"))
		}
		if r == '|' {
			return exitcode.Errorf(exitcode.Usage, "the reason contains |, which Slurm's parsable output uses between fields")
		}
	}
	return nil
}

// Drain removes nodes from production with a reason.
//
// scontrol update is not atomic: slurmctld changes the nodes it reaches and
// stops at the first it cannot change. When the update fails, the nodes are
// read back, and the error says which of them are drained with this reason.
func (c *Client) Drain(ctx context.Context, ns *nodeset.NodeSet, reason string) error {
	if err := ValidateReason(reason); err != nil {
		return err
	}
	if err := refuseAll(ns); err != nil {
		return err
	}
	_, err := c.run(ctx, []string{"scontrol", "update",
		"nodename=" + ns.Hostlist(), "state=drain", "reason=" + reason})
	if err != nil {
		return c.readBack(ctx, ns, err, "drained with this reason", func(n Node) bool {
			return strings.HasPrefix(n.BaseState(), "drain") && n.Reason == reason
		})
	}
	return nil
}

// Resume puts nodes back into production. A failed update is read back the
// way Drain reads it back.
func (c *Client) Resume(ctx context.Context, ns *nodeset.NodeSet) error {
	if err := refuseAll(ns); err != nil {
		return err
	}
	_, err := c.run(ctx, []string{"scontrol", "update",
		"nodename=" + ns.Hostlist(), "state=resume"})
	if err != nil {
		return c.readBack(ctx, ns, err, "back in production", func(n Node) bool {
			switch base := n.BaseState(); {
			case strings.HasPrefix(base, "drain"), base == "down", strings.HasPrefix(base, "fail"):
				return false
			}
			return true
		})
	}
	return nil
}

// readBack reads the nodes after a failed update and adds to the error which
// of them are now in the state the update was for. The error keeps its exit
// code.
func (c *Client) readBack(ctx context.Context, ns *nodeset.NodeSet, failed error, state string, changed func(Node) bool) error {
	nodes, err := c.Nodes(ctx, ns, nil)
	if err != nil {
		return fmt.Errorf("%w; the nodes could not be read back, so which of them are %s is not known: %w",
			failed, state, err)
	}
	done := nodeset.New()
	for _, n := range nodes {
		if changed(n) {
			_ = done.Add(n.Name)
		}
	}
	rest := ns.Difference(done)
	switch {
	case done.IsEmpty():
		return fmt.Errorf("%w; afterwards none of %s is %s", failed, ns, state)
	case rest.IsEmpty():
		return fmt.Errorf("%w; afterwards all of %s are %s", failed, ns, state)
	}
	return fmt.Errorf("%w; afterwards %s %s %s, %s %s not", failed, done, isAre(done), state, rest, isAre(rest))
}

func isAre(ns *nodeset.NodeSet) string {
	if ns.Len() == 1 {
		return "is"
	}
	return "are"
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
	lines, err := c.readLines(ctx, argv)
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
	// The working directory and the command line go last; any user writes
	// them.
	format := newFramed("%i", "%u", "%a", "%P", "%T", "%N", "%D", "%C", "%l", "%M", "%Q", "%r", "%Z", "%o")
	argv := []string{"squeue", "--noheader", "--format", format.format()}
	if len(f.States) > 0 {
		argv = append(argv, "--states", strings.Join(f.States, ","))
	}
	if len(f.Users) > 0 {
		argv = append(argv, "--user", strings.Join(f.Users, ","))
	}
	if f.Nodes != nil && !f.Nodes.IsEmpty() {
		argv = append(argv, "--nodelist", f.Nodes.Hostlist())
	}
	result, err := c.read(ctx, argv)
	if err != nil {
		return nil, err
	}
	records, err := format.records("squeue", result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(records))
	for _, f := range records {
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
		// The window is given relative to the clock of the host sacct runs
		// on. A time of day would be read in that host's zone, which need
		// not be the zone of the workstation that computed it.
		since := f.Since
		if since <= 0 {
			since = time.Hour
		}
		seconds := int64(math.Ceil(since.Seconds()))
		argv = append(argv, "--starttime", fmt.Sprintf("now-%d", seconds))
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

	lines, err := c.readLines(ctx, argv)
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
	lines, err := c.readLines(ctx, argv)
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
	lines, err := c.readLines(ctx, argv)
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
	lines, err := c.readLines(ctx, argv)
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
	lines, err := c.readLines(ctx, argv)
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

// validName matches an account or user name sacctmgr takes as one name.
// sacctmgr splits a value at commas and expands a bracketed range, so
// "proj[1-100]" would create a hundred accounts under a preview that names
// one.
var validName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]*$`)

// ValidateName checks an account or user name before it is shown or sent.
// kind says what the name is, for the error.
func ValidateName(kind, value string) error {
	if !validName.MatchString(value) {
		return exitcode.Errorf(exitcode.Usage,
			"%q is not a %s name: use letters, digits and . _ @ - only, since sacctmgr reads , and [ ] as lists",
			value, kind)
	}
	return nil
}

// ValidateUserName is ValidateName for a user, which also may not be a
// number: getent resolves a number as a user ID.
func ValidateUserName(value string) error {
	if err := ValidateName("user", value); err != nil {
		return err
	}
	if strings.Trim(value, "0123456789") == "" {
		return exitcode.Errorf(exitcode.Usage, "%q is a number, not a user name", value)
	}
	return nil
}

// ValidateText checks a free text value, such as an account description,
// which may not hold control characters.
func ValidateText(kind, value string) error {
	for _, r := range value {
		if isControl(r) {
			return exitcode.Errorf(exitcode.Usage, "the %s contains the control character %s",
				kind, strings.Trim(strconv.QuoteRune(r), "'"))
		}
	}
	return nil
}

// fairShare matches what sacctmgr takes as a fair-share value.
var fairShare = regexp.MustCompile(`^([0-9]+|parent)$`)

// ValidateFairShare checks a fair-share value.
func ValidateFairShare(value string) error {
	if !fairShare.MatchString(value) {
		return exitcode.Errorf(exitcode.Usage, "%q is not a fair-share value: give a whole number or parent", value)
	}
	return nil
}

// Cluster returns the name slurm.conf gives the cluster.
//
// One slurmdbd can serve several clusters, and sacctmgr applies a change
// that names no cluster to all of them. Every accounting change clusterctl
// makes therefore names the cluster its login node belongs to.
func (c *Client) Cluster(ctx context.Context) (string, error) {
	if c.cluster != "" {
		return c.cluster, nil
	}
	lines, err := c.readLines(ctx, []string{"scontrol", "show", "config"})
	if err != nil {
		return "", err
	}
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "ClusterName") {
			continue
		}
		value = strings.TrimSpace(value)
		if err := ValidateName("cluster", value); err != nil {
			return "", exitcode.Errorf(exitcode.TargetFailed, "scontrol reports the cluster name %q", value)
		}
		c.cluster = value
		return value, nil
	}
	return "", exitcode.Errorf(exitcode.TargetFailed,
		"scontrol show config on %s names no ClusterName, so a change could reach every cluster of the accounting database", c.Target)
}

// Organization returns the organisation a new account gets when none is
// given.
func (c *Client) Organization(organization string) string {
	if organization == "" {
		organization = c.Spec.Organization
	}
	if organization == "" {
		organization = "default"
	}
	return organization
}

// AddAccount creates an account in the cluster.
func (c *Client) AddAccount(ctx context.Context, name, organization, description string) error {
	cluster, err := c.Cluster(ctx)
	if err != nil {
		return err
	}
	if description == "" {
		description = name
	}
	_, err = c.run(ctx, []string{"sacctmgr", "--immediate", "add", "account", name, "cluster=" + cluster,
		"description=" + description, "organization=" + c.Organization(organization)})
	return err
}

// SetCoordinators makes users coordinators of an account.
func (c *Client) SetCoordinators(ctx context.Context, account string, users []string) error {
	_, err := c.run(ctx, []string{"sacctmgr", "--immediate", "add", "coordinator",
		"account=" + account, "names=" + strings.Join(users, ",")})
	return err
}

// SetFairShare sets the fair-share value of an account in the cluster.
func (c *Client) SetFairShare(ctx context.Context, account, value string) error {
	cluster, err := c.Cluster(ctx)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, []string{"sacctmgr", "--immediate", "modify", "account",
		"where", "name=" + account, "cluster=" + cluster, "set", "fairshare=" + value})
	return err
}

// HasPosixUser reports whether the cluster knows a user account at all.
//
// getent resolves the name through the name service switch of the login
// node, which includes its local files. It exits 2 for a name it does not
// know.
func (c *Client) HasPosixUser(ctx context.Context, user string) (bool, error) {
	result, err := c.read(ctx, []string{"getent", "passwd", user})
	if result != nil && result.ExitCode == 2 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// The entry must be the one asked for, not one getent found another way.
	entry, _, _ := strings.Cut(strings.TrimSpace(result.Stdout), ":")
	return entry == user, nil
}

// UserAddition is what adding a user to an account does, resolved before it
// is shown.
type UserAddition struct {
	User    string
	Cluster string
	// Account is the account the user is associated with.
	Account string
	// DefaultAccount is the default account the user ends up with. For a
	// user who exists already it is empty when the default stays as it is.
	DefaultAccount string
	// Exists says the user has associations in the cluster already.
	Exists bool
	// Current is the default account of a user who exists already.
	Current string
	// associate are the accounts the user is newly associated with.
	associate []string
}

// PlanUserAdd resolves what adding a user to an account does: which account
// an omitted one stands for, whether the user exists, and which associations
// and default account result.
func (c *Client) PlanUserAdd(ctx context.Context, user, account, defaultAccount string) (UserAddition, error) {
	cluster, err := c.Cluster(ctx)
	if err != nil {
		return UserAddition{}, err
	}
	if account == "" {
		account = c.Spec.DefaultAccount
	}
	if account == "" {
		account = "default"
	}
	lines, err := c.readLines(ctx, []string{"sacctmgr", "--noheader", "--parsable2", "show", "user", "withassoc",
		"format=User,Account,DefaultAccount", "where", "name=" + user, "cluster=" + cluster})
	if err != nil {
		return UserAddition{}, err
	}
	u := UserAddition{User: user, Cluster: cluster, Account: account}
	associated := map[string]bool{}
	for _, line := range lines {
		f := fields(line, 3)
		if f[0] != user {
			continue
		}
		u.Exists = true
		associated[f[1]] = true
		if f[2] != "" {
			u.Current = f[2]
		}
	}

	if !u.Exists {
		// A new user gets a default account, and an association with it.
		u.DefaultAccount = defaultAccount
		if u.DefaultAccount == "" {
			u.DefaultAccount = account
		}
		u.associate = []string{account}
		if u.DefaultAccount != account {
			u.associate = append(u.associate, u.DefaultAccount)
		}
		return u, nil
	}
	if !associated[account] {
		u.associate = append(u.associate, account)
	}
	if defaultAccount != "" && defaultAccount != u.Current {
		u.DefaultAccount = defaultAccount
		if defaultAccount != account && !associated[defaultAccount] {
			u.associate = append(u.associate, defaultAccount)
		}
	}
	return u, nil
}

// String says what the addition does, for the confirmation.
func (u UserAddition) String() string {
	var b strings.Builder
	if u.Exists {
		if len(u.associate) > 0 {
			fmt.Fprintf(&b, "associate %s with the %s %s", u.User, plural(len(u.associate), "account"), and(u.associate))
		} else {
			fmt.Fprintf(&b, "leave %s associated with %s", u.User, u.Account)
		}
		if u.DefaultAccount != "" {
			fmt.Fprintf(&b, " and make %s the default account of %s instead of %s", u.DefaultAccount, u.User, u.Current)
		}
	} else {
		fmt.Fprintf(&b, "create the Slurm user %s with the %s %s", u.User, plural(len(u.associate), "account"), and(u.associate))
		if len(u.associate) > 1 {
			fmt.Fprintf(&b, ", %s the default", u.DefaultAccount)
		}
	}
	fmt.Fprintf(&b, " in the Slurm cluster %s", u.Cluster)
	return b.String()
}

// Changes reports whether the addition changes anything.
func (u UserAddition) Changes() bool {
	return len(u.associate) > 0 || u.DefaultAccount != ""
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func and(words []string) string {
	if len(words) <= 1 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// AddUser carries out an addition PlanUserAdd resolved.
func (c *Client) AddUser(ctx context.Context, u UserAddition) error {
	if !u.Exists {
		_, err := c.run(ctx, []string{"sacctmgr", "--immediate", "add", "user", "names=" + u.User,
			"account=" + strings.Join(u.associate, ","), "defaultaccount=" + u.DefaultAccount, "cluster=" + u.Cluster})
		return err
	}
	if len(u.associate) > 0 {
		if _, err := c.run(ctx, []string{"sacctmgr", "--immediate", "add", "user", "names=" + u.User,
			"account=" + strings.Join(u.associate, ","), "cluster=" + u.Cluster}); err != nil {
			return err
		}
	}
	if u.DefaultAccount != "" {
		return c.setDefaultAccount(ctx, u.User, u.DefaultAccount, u.Cluster)
	}
	return nil
}

// SetDefaultAccount changes the default account of a user in the cluster.
func (c *Client) SetDefaultAccount(ctx context.Context, user, account string) error {
	cluster, err := c.Cluster(ctx)
	if err != nil {
		return err
	}
	return c.setDefaultAccount(ctx, user, account, cluster)
}

func (c *Client) setDefaultAccount(ctx context.Context, user, account, cluster string) error {
	_, err := c.run(ctx, []string{"sacctmgr", "--immediate", "modify", "user",
		"where", "name=" + user, "cluster=" + cluster, "set", "defaultaccount=" + account})
	return err
}
