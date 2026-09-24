// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/internal/transport"

	"github.com/spf13/cobra"
)

// exampleDir is the configuration shipped with the documentation, so the
// commands are exercised against the same files an administrator copies.
const exampleDir = "../../examples/site"

type harness struct {
	out, errOut *bytes.Buffer
	recorder    *transport.Recorder
	root        *root
}

// run builds the command tree over the example configuration and runs one
// command line against it.
func run(t *testing.T, opts harnessOptions, args ...string) (*harness, error) {
	t.Helper()

	h := &harness{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	h.recorder = opts.recorder
	if h.recorder == nil {
		h.recorder = &transport.Recorder{}
	}

	dir := t.TempDir()
	var in io.Reader = strings.NewReader(opts.stdin)
	if opts.in != nil {
		in = opts.in
	}
	cacheDir := opts.cacheDir
	if cacheDir == "" {
		cacheDir = filepath.Join(dir, "cache")
	}
	streams := app.Streams{
		In:       in,
		Out:      h.out,
		Err:      h.errOut,
		IsTTY:    opts.tty,
		StateDir: filepath.Join(dir, "state"),
		CacheDir: cacheDir,
	}
	if opts.stateDir != "" {
		streams.StateDir = opts.stateDir
	}
	if opts.streams != nil {
		opts.streams(&streams)
	}

	ctx := opts.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := NewRootCommand(ctx, streams)
	cmd.SetOut(h.out)
	cmd.SetErr(h.errOut)
	var args0 []string
	if !opts.bare {
		for _, file := range exampleFiles(t, opts.config) {
			args0 = append(args0, "--config", file)
		}
	}
	for _, extra := range opts.config {
		args0 = append(args0, "--config", extra)
	}
	cmd.SetArgs(append(args0, args...))

	// The root holds the flags; reach it to install the fake transport.
	h.root = builtRoots[cmd]
	h.root.runner = h.recorder

	return h, cmd.Execute()
}

// exampleFiles lists the files of the example configuration, leaving out
// those that a directory in extra holds a file of the same name for: that
// file takes the example's place, since a second document of the same kind
// and name would be refused rather than replace the first.
func exampleFiles(t *testing.T, extra []string) []string {
	t.Helper()
	files, err := config.ExpandEntries([]string{exampleDir})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, file := range files {
		replaced := false
		for _, dir := range extra {
			if _, err := os.Stat(filepath.Join(dir, filepath.Base(file))); err == nil {
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, file)
		}
	}
	return out
}

type harnessOptions struct {
	recorder *transport.Recorder
	stdin    string
	// in replaces stdin, for an input that fails to be read.
	in  io.Reader
	tty bool
	// config are read after the example configuration. A file in one of
	// them takes the place of the example's file of the same name; any
	// other document of a kind and name the example has already is
	// refused, except that a Config document may redefine a context.
	config []string
	// bare leaves the example configuration out, so that only config is
	// read, or the search path when config is empty too.
	bare bool
	// stateDir is shared between runs that stand for processes of one user.
	stateDir string
	// streams changes the streams before the command tree is built.
	streams func(*app.Streams)
	// cacheDir replaces the fresh cache directory of each run, so that
	// several runs can share one the way the commands of one user do.
	cacheDir string
	// ctx is the context the command runs under; cancelling it is what an
	// interrupt does.
	ctx context.Context
}

// rootOf digs the root state out of a built command tree.

func TestVersionPrintsProvenance(t *testing.T) {
	h, err := run(t, harnessOptions{}, "version")
	if err != nil {
		t.Fatalf("version failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "linux/") && !strings.Contains(h.out.String(), "darwin/") {
		t.Errorf("version output has no platform:\n%s", h.out)
	}
}

func TestConfigValidateAcceptsTheExample(t *testing.T) {
	h, err := run(t, harnessOptions{}, "config", "validate")
	if err != nil {
		t.Fatalf("config validate failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "documents are valid") {
		t.Errorf("output does not confirm the documents:\n%s", h.out)
	}
}

func TestConfigExplainNamesTheLayerAndLine(t *testing.T) {
	h, err := run(t, harnessOptions{}, "config", "explain", "fanout.max")
	if err != nil {
		t.Fatalf("config explain failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"fanout.max", "site", "site.yaml:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}

	_, err = run(t, harnessOptions{}, "config", "explain", "nope.nothing")
	if err == nil {
		t.Fatal("an unset path should be reported")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
}

func TestNodeSelectEvaluatesGroupsAndOperators(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		// The inventory wrote these nodes as exe0001 and so on, so the set
		// comes back under the names the site gave them.
		{[]string{"node", "select", "exe[1-5]!exe3"}, "exe[0001-0002,0004-0005]"},
		{[]string{"node", "select", "ghost[1-2]"}, "ghost[1-2]"},
		{[]string{"node", "select", "@inventory:wlm"}, "wlm01"},
		{[]string{"node", "select", "@rack:R03"}, "sub[0001-0002]"},
		{[]string{"node", "select", "@inventory:exe&@rack:R02", "--count"}, "10"},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{}, tc.args...)
			if err != nil {
				t.Fatalf("command failed: %v", err)
			}
			if got := strings.TrimSpace(h.out.String()); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNodeFQDNAppliesTheNamingRules(t *testing.T) {
	h, err := run(t, harnessOptions{}, "node", "fqdn", "-n", "exe[1-3]")
	if err != nil {
		t.Fatalf("node fqdn failed: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "exe[0001-0003].hpc.example.org"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}

	h, err = run(t, harnessOptions{}, "node", "fqdn", "-n", "exe[1-3]", "--bmc")
	if err != nil {
		t.Fatalf("node fqdn --bmc failed: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "exe[0001-0003].mgmt.hpc.example.org"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestExecSendsTheArgumentVectorUnchanged(t *testing.T) {
	h, err := run(t, harnessOptions{}, "exec", "-n", "exe[1-2]", "--", "echo", "*.log", "it's")
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	calls := h.recorder.Calls()
	if got, want := len(calls), 2; got != want {
		t.Fatalf("got %d calls, want %d", got, want)
	}
	// The glob must not be expanded on the workstation and the apostrophe
	// must survive, which is what the shell toolkit got wrong.
	if got, want := calls[0].Command, `echo '*.log' 'it'\''s'`; !strings.Contains(got, want) {
		t.Errorf("command = %q, want it to contain %q", got, want)
	}
	if !strings.HasPrefix(calls[0].Command, "timeout -k") {
		t.Errorf("command = %q, want a timeout enforced on the node", calls[0].Command)
	}
}

func TestExecReportsFailingNodes(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		if tg.Name == "exe0002" {
			return &transport.Result{Target: tg, ExitCode: 3, Stderr: "no such file\n"}, nil
		}
		return &transport.Result{Target: tg, Stdout: "ok\n"}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "exec", "-n", "exe[1-3]", "--", "true")
	if err == nil {
		t.Fatal("a failing node should make the command fail")
	}
	if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if !strings.Contains(h.out.String(), "exe0001: ok") {
		t.Errorf("the successful nodes are not reported:\n%s", h.out)
	}
}

func TestExecDedupCollapsesIdenticalAnswers(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		out := "5.14.0\n"
		if tg.Name == "exe0004" {
			out = "4.18.0\n"
		}
		return &transport.Result{Target: tg, Stdout: out}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "exec", "-n", "exe[1-4]", "--dedup", "--", "uname", "-r")
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	out := h.out.String()
	if !strings.Contains(out, "exe[0001-0003] (3)") {
		t.Errorf("identical answers were not collapsed:\n%s", out)
	}
	if !strings.Contains(out, "exe0004 (1)") {
		t.Errorf("the odd node out is missing:\n%s", out)
	}
}

func TestExecRequiresACommand(t *testing.T) {
	_, err := run(t, harnessOptions{}, "exec", "-n", "exe1")
	if err == nil {
		t.Fatal("exec with nothing to run should be refused")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
}

func TestSelectionIsRequired(t *testing.T) {
	_, err := run(t, harnessOptions{}, "exec", "--", "true")
	if err == nil {
		t.Fatal("exec without a node set should be refused")
	}
	if !strings.Contains(err.Error(), "-n") {
		t.Errorf("error = %v, want it to say how to select nodes", err)
	}
}

func TestDestructiveCommandRefusesWithoutATerminal(t *testing.T) {
	// A power action in a script must not go ahead unasked, even on a node
	// Slurm reports idle.
	rec := &transport.Recorder{Responses: []*transport.Result{{Stdout: "exe0001 idle\n"}}}
	_, err := run(t, harnessOptions{tty: false, recorder: rec}, "bmc", "power", "off", "-n", "exe1")
	if err == nil {
		t.Fatal("a power action with no terminal should be refused")
	}
	if !strings.Contains(err.Error(), "-y") {
		t.Errorf("error = %v, want it to mention -y", err)
	}
}

func TestDestructiveCommandRefusesProtectedHosts(t *testing.T) {
	_, err := run(t, harnessOptions{tty: true, stdin: "y\n"}, "bmc", "power", "off", "-n", "wlm01", "-y")
	if err == nil {
		t.Fatal("a protected host should be refused")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error = %v, want it to name the way out", err)
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "secret")
	h, err := run(t, harnessOptions{recorder: slurmAllIdle(t)}, "provision", "reinstall", "-n", "exe0001", "--dry-run")
	if err != nil {
		t.Fatalf("a dry run should succeed: %v", err)
	}
	// Slurm is asked, as the real run would; nothing else is sent.
	if sinfo, other := sinfoCalls(h.recorder); sinfo != 1 || other != 0 {
		t.Errorf("a dry run sent sinfo %d times and %d other commands, want sinfo once and nothing else", sinfo, other)
	}
	if !strings.Contains(h.errOut.String(), "Would reinstall") {
		t.Errorf("the dry run says nothing:\n%s", h.errOut)
	}
}

func TestSlurmNodeListParsesTheClientOutput(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: slurm.Render(req,
			slurm.Row{"N": "exe0001", "T": "idle", "R": "main", "c": "128", "E": "none", "u": "Unknown"},
			slurm.Row{"N": "exe0002", "T": "drained", "R": "main", "c": "128", "E": "ticket 4711", "u": "alice"},
		)}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "slurm", "node", "list")
	if err != nil {
		t.Fatalf("slurm node list failed: %v", err)
	}
	out := h.out.String()
	if !strings.Contains(out, "ticket 4711 [alice]") {
		t.Errorf("the drain reason is missing:\n%s", out)
	}
	if !strings.Contains(rec.Commands()[0], "sinfo") {
		t.Errorf("command = %q, want sinfo", rec.Commands()[0])
	}
}

func TestSlurmDrainNeedsAReasonAndConfirmation(t *testing.T) {
	h, err := run(t, harnessOptions{tty: true, stdin: "y\n", recorder: newSlurmCluster().recorder()},
		"slurm", "node", "drain", "ticket 4711: failing DIMM", "-n", "exe0007")
	if err != nil {
		t.Fatalf("slurm node drain failed: %v", err)
	}
	var command string
	for _, c := range h.recorder.Commands() {
		if strings.Contains(c, "scontrol update") {
			command = c
		}
	}
	if !strings.Contains(command, "'reason=ticket 4711: failing DIMM'") {
		t.Errorf("command = %q, want the reason as one argument", command)
	}

	// A declined confirmation must leave the cluster alone.
	cluster := newSlurmCluster()
	_, err = run(t, harnessOptions{tty: true, stdin: "n\n", recorder: cluster.recorder()},
		"slurm", "node", "drain", "reason", "-n", "exe0007")
	if err == nil {
		t.Fatal("a declined action should not proceed")
	}
	if got, want := exitcode.From(err), exitcode.Interrupted; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if changes := cluster.changes(); len(changes) != 0 {
		t.Errorf("a declined action sent %v", changes)
	}
}

func TestOutputFormats(t *testing.T) {
	for _, format := range []string{"json", "yaml", "nodeset", "name"} {
		t.Run(format, func(t *testing.T) {
			h, err := run(t, harnessOptions{}, "node", "list", "-o", format, "exe[1-2]")
			if err != nil {
				t.Fatalf("node list -o %s failed: %v", format, err)
			}
			if strings.TrimSpace(h.out.String()) == "" {
				t.Errorf("-o %s produced nothing", format)
			}
		})
	}

	_, err := run(t, harnessOptions{}, "node", "list", "-o", "xml")
	if err == nil {
		t.Fatal("an unknown output format should be refused")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
}

func TestUnknownContextIsReported(t *testing.T) {
	_, err := run(t, harnessOptions{}, "--context", "nope", "config", "view")
	if err == nil {
		t.Fatal("an unknown context should be reported")
	}
	if !strings.Contains(err.Error(), "cluster1") {
		t.Errorf("error = %v, want it to list the known contexts", err)
	}
}

func TestSetOverridesConfiguration(t *testing.T) {
	h, err := run(t, harnessOptions{}, "--set", "fanout.max=3", "config", "explain", "fanout.max")
	if err != nil {
		t.Fatalf("config explain failed: %v", err)
	}
	out := h.out.String()
	if !strings.Contains(out, "flags") || !strings.Contains(out, "3") {
		t.Errorf("--set did not win:\n%s", out)
	}

	_, err = run(t, harnessOptions{}, "--set", "nonsense", "config", "view")
	if err == nil {
		t.Fatal("a malformed --set should be refused")
	}
}

func TestHostkeyListReadsTheConfiguredFile(t *testing.T) {
	h, err := run(t, harnessOptions{}, "hostkey", "list")
	if err != nil {
		t.Fatalf("hostkey list failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "entries in") {
		t.Errorf("output does not name the file:\n%s", h.out)
	}
}

func TestTunnelStartDryRunShowsTheCommand(t *testing.T) {
	h, err := run(t, harnessOptions{}, "tunnel", "start", "ipmi", "--dry-run")
	if err != nil {
		t.Fatalf("tunnel start --dry-run failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"sshuttle", "--remote mgmt-gw.example.org", "10.0.0.0/8", "--exclude desk01.example.org"} {
		if !strings.Contains(out, want) {
			t.Errorf("command is missing %q:\n%s", want, out)
		}
	}
}

func TestDHCPHostsUsesTheParsedConfiguration(t *testing.T) {
	rec := &transport.Recorder{Responses: []*transport.Result{{
		Stdout: "host exe0001 {\n  hardware ethernet aa:bb:cc:11:22:33;\n  fixed-address 10.0.1.1;\n" +
			"  filename \"/srv/pxesrv/boot/exe/ipxe.net2\";\n}\n",
	}}}
	h, err := run(t, harnessOptions{recorder: rec}, "dhcp", "hosts", "-n", "exe0001")
	if err != nil {
		t.Fatalf("dhcp hosts failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"10.0.1.1", "aa:bb:cc:11:22:33", "ipxe.net2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorReportsWhatItChecked(t *testing.T) {
	h, _ := run(t, harnessOptions{}, "doctor")
	out := h.out.String()
	for _, want := range []string{"configuration", "ssh client", "checks"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor did not check %q:\n%s", want, out)
		}
	}
}

func TestHelpForEveryCommand(t *testing.T) {
	// Every command has to describe itself, because the help text is the
	// only documentation at the terminal.
	cmd := NewRootCommand(context.Background(), app.Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Name() != "help" && c.Name() != "completion" {
			if c.Short == "" {
				t.Errorf("command %q has no short description", c.CommandPath())
			}
			if c.Long == "" && c.HasSubCommands() {
				t.Errorf("command group %q has no long description", c.CommandPath())
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cmd)
}
