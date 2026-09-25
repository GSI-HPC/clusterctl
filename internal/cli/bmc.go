// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/ipmi"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func newBMCCommand(r *root) *cobra.Command {
	return group("bmc", "Operate the service processors of the nodes", `
Reach the service processors out of band: read power state, change it, set
what a machine boots next time, and talk to the Redfish interface directly.

Redfish is tried first by default, because IPMI over LAN ships disabled on
current iLO, XCC and iDRAC firmware. Where IPMI is used, the tools run on the
host that has a route into the management network and the password travels
over standard input, never in an argument vector.

Power actions are destructive: they preview what they are about to do and ask
first, they refuse protected hosts, and above a configured host count they ask
for the count to be typed back.`,
		newBMCStatusCommand(r),
		newBMCPowerCommand(r),
		newBMCBootCommand(r),
		newBMCRedfishCommand(r),
		newBMCWebCommand(r),
		newBMCPingCommand(r),
		newBMCForgetCommand(r),
	)
}

// bmcSet resolves the node set and the matching service processors: the
// inventory's bmcAddress where it records one, else the name the naming
// rules give. A node whose service processor has neither is refused before
// anything is contacted.
func bmcSet(a *app.App, args []string) (nodes, bmcs *nodeset.NodeSet, err error) {
	nodes, err = selection(a, args)
	if err != nil {
		return nil, nil, err
	}
	bmcs, err = a.BMCHosts(nodes)
	if err != nil {
		return nil, nil, err
	}
	return nodes, bmcs, nil
}

func newBMCStatusCommand(r *root) *cobra.Command {
	var useIPMI bool

	cmd := leaf("status [NODESET]", "Show the power state of the service processors", `
Read the power state of each node's service processor.

  clusterctl bmc status -n exe[1-10]
  clusterctl bmc status -n @rack:R02 -o json`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			nodes, _, err := bmcSet(a, args)
			if err != nil {
				return err
			}
			return bmcPowerState(a, nodes, useIPMI)
		}))
	cmd.Flags().BoolVar(&useIPMI, "ipmi", false, "ask over IPMI only, whatever bmc.order says")
	return cmd
}

func newBMCPowerCommand(r *root) *cobra.Command {
	var (
		useIPMI  bool
		batch    int
		stagger  time.Duration
		loseJobs bool
	)

	cmd := leaf("power ACTION [NODESET]", "Change the power state of nodes", `
Change the power state of the nodes through their service processors.

  status  report the current state, changing nothing
  on      power on
  off     cut the power without asking the operating system
  soft    ask the operating system to shut down
  cycle   power off and on again
  reset   reset without asking the operating system

Powering many nodes on at once trips rack breakers, so a power-on and a power
cycle are sent in batches with a pause between them; both come from the
configuration. A batch with a failure stops the run, and the nodes of the
later batches are reported as not tried.

Each node is reached over the transports of bmc.order, or its vendor's, in
turn. An action falls back to the next transport only when the first provably
never reached the service processor, because an action is never sent twice.
Nodes with different accounts are sent to the IPMI backend separately.

Slurm is asked first, because powering off a running job loses it. A node
that Slurm reports running a job, or cannot say about, is refused unless
--lose-jobs is given; --force gets past a protected host, not this check.`,
		cobra.MinimumNArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			// The action is checked before anything is asked of Slurm or
			// the credential store, so that a typo costs nothing.
			action := strings.ToLower(args[0])
			if err := ipmi.CheckAction(action); err != nil {
				return err
			}
			batch, stagger, err := powerBatching(cmd, a, batch, stagger)
			if err != nil {
				return err
			}
			nodes, _, err := bmcSet(a, args[1:])
			if err != nil {
				return err
			}
			if action == ipmi.ActionStatus {
				return bmcPowerState(a, nodes, useIPMI)
			}
			plan, err := planBMC(a, nodes, useIPMI)
			if err != nil {
				return err
			}

			gated := safety.Action{
				Verb:    "power " + action,
				Targets: nodes,
				Detail:  plan.describe(a),
			}
			// A protected host is named before Slurm is asked, so that a
			// refusal never leaves it out.
			if err := a.Gate.Check(gated); err != nil {
				return err
			}
			if err := checkSlurmIdle(a, nodes, action, loseJobs); err != nil {
				return err
			}
			if err := confirmResolved(a, gated, func() error { return plan.resolve(a) }); err != nil {
				return err
			}
			return printBMCResults(a, runPower(a, plan, action, batch, stagger))
		}))

	cmd.Flags().BoolVar(&useIPMI, "ipmi", false, "act over IPMI only, whatever bmc.order says")
	cmd.Flags().IntVar(&batch, "batch", 0, "how many nodes to power on or cycle at once, at least 1 (default: from the configuration)")
	cmd.Flags().DurationVar(&stagger, "stagger", 0, "pause between batches (default: from the configuration)")
	addLoseJobsFlag(cmd, &loseJobs)
	cmd.ValidArgsFunction = fixed(ipmi.Actions()...)
	return cmd
}

// confirmResolved runs the gate and resolves what the command needs to send,
// the accounts above all, before anything is sent. A real run resolves after
// the gate, so that a protected host or a missing confirmation is reported
// before a password is asked for; a dry run resolves before its preview,
// because it has to fail where the real run would.
func confirmResolved(a *app.App, action safety.Action, resolve func() error) error {
	if a.DryRun() {
		if err := resolve(); err != nil {
			return err
		}
		return a.Gate.Confirm(action)
	}
	if err := a.Gate.Confirm(action); err != nil {
		return err
	}
	return resolve()
}

// powerBatching returns the batch size and the pause of a power-on or cycle:
// the flags where they were given, zero included, else the configuration.
func powerBatching(cmd *cobra.Command, a *app.App, batch int, stagger time.Duration) (int, time.Duration, error) {
	if !cmd.Flags().Changed("batch") {
		batch = a.Spec.Safety.PowerOnBatch
	}
	if !cmd.Flags().Changed("stagger") {
		stagger = a.Spec.Safety.PowerOnStagger.Get()
	}
	// A batch below one would power the whole set on at once, which is
	// what the batching is there to prevent.
	if batch < 1 {
		return 0, 0, exitcode.Errorf(exitcode.Usage,
			"--batch is %d; it must be at least 1, or a whole rack powers on at once", batch)
	}
	if stagger < 0 {
		return 0, 0, exitcode.Errorf(exitcode.Usage, "--stagger cannot be negative")
	}
	return batch, stagger, nil
}

// bmcPowerState reads the power state of every node, over the transports of
// its order, falling back to the next when one fails.
func bmcPowerState(a *app.App, nodes *nodeset.NodeSet, useIPMI bool) error {
	plan, err := planBMC(a, nodes, useIPMI)
	if err != nil {
		return err
	}
	if err := plan.resolve(a); err != nil {
		return err
	}
	return printBMCResults(a, plan.run(a, nodes.Expand(), ipmi.ActionStatus))
}

// resetTypeFor maps a power action to the Redfish reset type.
func resetTypeFor(action string) (string, error) {
	switch action {
	case ipmi.ActionOn:
		return redfish.ResetOn, nil
	case ipmi.ActionOff:
		return redfish.ResetForceOff, nil
	case ipmi.ActionSoft:
		return redfish.ResetGracefulShutdown, nil
	case ipmi.ActionReset:
		return redfish.ResetForceRestart, nil
	case ipmi.ActionCycle:
		return redfish.ResetPowerCycle, nil
	default:
		return "", exitcode.Errorf(exitcode.Usage, "the power action %q has no Redfish reset type", action)
	}
}

// addLoseJobsFlag declares the override of the Slurm job check. It is a flag
// of its own, not --force, so that getting past a protected host does not
// also lose the jobs of every node in the set.
func addLoseJobsFlag(cmd *cobra.Command, loseJobs *bool) {
	cmd.Flags().BoolVar(loseJobs, "lose-jobs", false,
		"go ahead although Slurm reports jobs on the nodes, or cannot say")
}

// sinfoHostlistLimit is the longest host list handed to sinfo. A longer one
// would come close to the limit on one argument, so sinfo is asked about
// every node instead and the answer is narrowed here.
const sinfoHostlistLimit = 16 << 10

// checkSlurmIdle refuses a power action on nodes that may be running a job.
//
// It fails closed: a node counts as idle only when Slurm reports it in a
// state known to run no job. A busy state, a state the check does not know,
// a node Slurm did not report and a Slurm that cannot be asked all refuse
// the action, unless loseJobs is set. It asks Slurm even in a dry run, so
// that the dry run refuses what the real run would.
func checkSlurmIdle(a *app.App, nodes *nodeset.NodeSet, action string, loseJobs bool) error {
	if action == ipmi.ActionStatus || action == ipmi.ActionOn {
		return nil
	}
	if a.Spec.Safety.SlurmAware != nil && !*a.Spec.Safety.SlurmAware {
		a.Printf("the Slurm job check is off (safety.slurmAware is false); nothing checks whether %s run jobs\n", nodes)
		return nil
	}
	if a.Spec.Slurm.Role == "" {
		a.Printf("slurm.role names no host, so the Slurm job check is skipped; nothing checks whether %s run jobs\n", nodes)
		return nil
	}

	jobs, err := slurmJobs(a, nodes)
	if err != nil {
		if loseJobs {
			a.Printf("could not ask Slurm whether %s run jobs (%s); going ahead because --lose-jobs was given\n", nodes, output.EscapeCell(err.Error()))
			return nil
		}
		var coded *exitcode.Error
		if !errors.As(err, &coded) {
			err = exitcode.Wrap(exitcode.Transport, err)
		}
		return fmt.Errorf("could not ask Slurm whether %s run jobs, so nothing was done; "+
			"pass --lose-jobs to go ahead without the check: %w", nodes, err)
	}
	if jobs.idle() {
		return nil
	}
	if loseJobs {
		a.Printf("%s; going ahead because --lose-jobs was given\n", jobs)
		return nil
	}
	advice := "pass --lose-jobs to go ahead and lose any jobs on them"
	if !jobs.busy.IsEmpty() {
		advice = "drain them and wait for their jobs to end, or pass --lose-jobs to lose the jobs"
	}
	return exitcode.Errorf(exitcode.Usage, "%s; %s", jobs, advice)
}

func plural2(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// slurmJobState is what Slurm said about the nodes of a power action.
type slurmJobState struct {
	// busy are the nodes in a state that runs jobs.
	busy *nodeset.NodeSet
	// strange are the nodes in a state the check does not know, with the
	// states as Slurm wrote them.
	strange *nodeset.NodeSet
	states  []string
	// missing are the nodes Slurm did not report at all.
	missing *nodeset.NodeSet
}

func (s slurmJobState) idle() bool {
	return s.busy.IsEmpty() && s.strange.IsEmpty() && s.missing.IsEmpty()
}

// String names every node that is not known to be idle, and why.
func (s slurmJobState) String() string {
	var parts []string
	if !s.busy.IsEmpty() {
		parts = append(parts, fmt.Sprintf("%s %s running Slurm jobs", s.busy, plural2(s.busy.Len())))
	}
	if !s.strange.IsEmpty() {
		quoted := make([]string, len(s.states))
		for i, state := range s.states {
			// The state comes from the Slurm host; quoting it keeps control
			// characters off the terminal.
			quoted[i] = strconv.Quote(state)
		}
		parts = append(parts, fmt.Sprintf("Slurm reports %s in a state not known to be free of jobs (%s)",
			s.strange, strings.Join(quoted, ", ")))
	}
	if !s.missing.IsEmpty() {
		pronoun := "they run"
		if s.missing.Len() == 1 {
			pronoun = "it runs"
		}
		parts = append(parts, fmt.Sprintf("Slurm did not report %s, so whether %s jobs is not known "+
			"(check that the Slurm and inventory names agree)", s.missing, pronoun))
	}
	return strings.Join(parts, "; ")
}

// slurmJobs asks Slurm about the state of every node in the set.
func slurmJobs(a *app.App, nodes *nodeset.NodeSet) (slurmJobState, error) {
	target, err := a.Role(a.Spec.Slurm.Role)
	if err != nil {
		return slurmJobState{}, err
	}
	// --all includes the nodes of hidden partitions, which run jobs too.
	argv := []string{"sinfo", "--all", "-h", "-N", "-o", "%N %T"}
	if hostlist := nodes.Hostlist(); len(hostlist) <= sinfoHostlistLimit {
		argv = append(argv, "-n", hostlist)
	}
	result, err := a.ReadRunner.Run(a.Context(), target, transport.Request{
		Argv:    argv,
		Timeout: 30 * time.Second,
		TTY:     transport.TTYNone,
	})
	if err != nil {
		return slurmJobState{}, err
	}
	if result.Failed() {
		if result.Err != nil {
			return slurmJobState{}, result.Err
		}
		return slurmJobState{}, fmt.Errorf("sinfo exited %d", result.ExitCode)
	}

	// sinfo lists a node once per partition; the busiest answer counts.
	verdict := map[string]slurmVerdict{}
	stateOf := map[string]string{}
	for _, line := range result.Lines() {
		fields := strings.Fields(line)
		if len(fields) == 0 || !nodes.Contains(fields[0]) {
			continue
		}
		name, state := fields[0], ""
		if len(fields) > 1 {
			state = fields[1]
		}
		if v := classifySlurmState(state); v > verdict[name] {
			verdict[name] = v
			stateOf[name] = state
		}
	}

	out := slurmJobState{busy: nodeset.New(), strange: nodeset.New(), missing: nodeset.New()}
	seen := map[string]bool{}
	for _, name := range nodes.Expand() {
		switch verdict[name] {
		case slurmUnreported:
			_ = out.missing.Add(name)
		case slurmStrange:
			_ = out.strange.Add(name)
			if state := stateOf[name]; !seen[state] {
				seen[state] = true
				out.states = append(out.states, state)
			}
		case slurmBusy:
			_ = out.busy.Add(name)
		}
	}
	return out, nil
}

// slurmVerdict is what a state says about jobs, ordered so that the more
// cautious verdict is the larger one.
type slurmVerdict int

const (
	slurmUnreported slurmVerdict = iota
	slurmIdle
	slurmStrange
	slurmBusy
)

// slurmStateSuffixes are the characters sinfo appends to a state to flag
// it: not responding, powered down, rebooting, maintenance and so on.
const slurmStateSuffixes = "*~#!%$@^-+"

// classifySlurmState reads a %T state. A compound state such as
// "mixed+drain" is busy when any part is, and idle only when every part is
// known to run no job.
//
// Only the states Slurm prints for a node without jobs count as idle. Slurm
// prints "fail" for a mixed node with the fail flag, and "maint" and
// "reboot" for a node whose jobs are still completing, so those are not.
func classifySlurmState(state string) slurmVerdict {
	base := strings.TrimRight(strings.ToLower(state), slurmStateSuffixes)
	if base == "" {
		return slurmStrange
	}
	verdict := slurmIdle
	for part := range strings.SplitSeq(base, "+") {
		switch part {
		case "allocated", "alloc", "mixed", "mix", "completing", "comp",
			"draining", "drng", "failing", "failg":
			return slurmBusy
		case "idle", "drained", "drain", "down", "future", "futr",
			"planned", "plnd", "reserved", "resv",
			"power_down", "powering_down", "powered_down", "pow_dn":
		default:
			verdict = slurmStrange
		}
	}
	return verdict
}

func newBMCBootCommand(r *root) *cobra.Command {
	var persistent bool

	set := leaf("set TARGET [NODESET]", "Set what the nodes boot next time", `
Ask the service processors to boot from a given source next time.

The override applies once by default. A persistent override is what leaves a
machine reinstalling in a loop, so --persistent has to be asked for.

  clusterctl bmc boot set Pxe -n exe[1-4]
  clusterctl bmc boot set Hdd -n exe1 --persistent`,
		cobra.MinimumNArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			target := args[0]
			nodes, _, err := bmcSet(a, args[1:])
			if err != nil {
				return err
			}
			mode := "once"
			if persistent {
				mode = "persistently"
			}
			var clients []*redfish.Client
			if err := confirmResolved(a, safety.Action{
				Verb:    "set the boot source of",
				Targets: nodes,
				Detail:  fmt.Sprintf("to %s, %s", target, mode),
			}, func() (err error) {
				clients, err = redfishClients(a, nodes.Expand())
				return err
			}); err != nil {
				return err
			}
			calls := redfishEach(a, nodes.Expand(), clients, true, func(ctx context.Context, _ string, c *redfish.Client) (string, error) {
				return target + " " + mode, c.SetBootOverride(ctx, target, persistent)
			})
			return printBMCResults(a, callResults(calls, true, func(s string) string { return s }))
		}))
	set.Flags().BoolVar(&persistent, "persistent", false, "keep the override until it is removed")

	clear := leaf("unset [NODESET]", "Remove a boot source override", `
Remove the boot source override, so the nodes boot their usual way again.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			nodes, _, err := bmcSet(a, args)
			if err != nil {
				return err
			}
			var clients []*redfish.Client
			if err := confirmResolved(a, safety.Action{Verb: "clear the boot source override of", Targets: nodes},
				func() (err error) {
					clients, err = redfishClients(a, nodes.Expand())
					return err
				}); err != nil {
				return err
			}
			calls := redfishEach(a, nodes.Expand(), clients, true, func(ctx context.Context, _ string, c *redfish.Client) (string, error) {
				return "cleared", c.ClearBootOverride(ctx)
			})
			return printBMCResults(a, callResults(calls, true, func(s string) string { return s }))
		}))

	show := leaf("show [NODESET]", "Show the current boot source override", `
Report what each machine is set to boot next time, and what its firmware
accepts.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			nodes, _, err := bmcSet(a, args)
			if err != nil {
				return err
			}
			clients, err := redfishClients(a, nodes.Expand())
			if err != nil {
				return err
			}
			calls := redfishEach(a, nodes.Expand(), clients, false, func(ctx context.Context, _ string, c *redfish.Client) (*redfish.System, error) {
				return c.System(ctx)
			})
			t := output.NewTable(
				output.Column{Name: "NODE"},
				output.Column{Name: "SOURCE"},
				output.Column{Name: "MODE"},
				output.Column{Name: "ACCEPTS", Wide: true},
				output.Column{Name: "ERROR"},
			)
			type row struct {
				Node    string   `json:"node"`
				Source  string   `json:"source,omitempty"`
				Mode    string   `json:"mode,omitempty"`
				Accepts []string `json:"accepts,omitempty"`
				Error   string   `json:"error,omitempty"`
			}
			object := make([]row, 0, len(calls))
			for _, c := range calls {
				if c.err != nil {
					t.Add(c.node, "", "", "", c.err.Error())
					object = append(object, row{Node: c.node, Error: c.err.Error()})
					continue
				}
				sys := c.value
				t.Add(c.node, sys.BootSource, sys.BootEnabled, strings.Join(sys.BootTargets, ","), "")
				object = append(object, row{Node: c.node, Source: sys.BootSource, Mode: sys.BootEnabled, Accepts: sys.BootTargets})
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			return bmcExit(callResults(calls, false, func(*redfish.System) string { return "" }))
		}))

	return group("boot", "Read and set what a node boots next time", `
Read and change the Redfish boot source override.`, set, clear, show)
}

func newBMCRedfishCommand(r *root) *cobra.Command {
	get := leaf("get PATH [NODESET]", "Read a Redfish resource", `
Send a GET to the Redfish interface of each service processor and print what
comes back.

  clusterctl bmc redfish get /redfish/v1/Systems/1 -n exe1
  clusterctl bmc redfish get /redfish/v1/Managers -n exe1 -o jsonpath='{.exe1.Members[*].[\"@odata.id\"]}'`,
		cobra.MinimumNArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			nodes, _, err := bmcSet(a, args[1:])
			if err != nil {
				return err
			}
			return redfishRequest(a, nodes, "GET", args[0], nil)
		}))

	var loseJobs bool
	post := leaf("post PATH BODY [NODESET]", "Send an action to a Redfish resource", `
Send a POST with a JSON body to the Redfish interface.

This can power off a machine, so it goes through the confirmation gate like
any other destructive command, and it is never retried. A path that names a
reset asks Slurm first, as bmc power does, and --lose-jobs overrides that.`,
		cobra.MinimumNArgs(2),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			var body any
			if err := json.Unmarshal([]byte(args[1]), &body); err != nil {
				return exitcode.Errorf(exitcode.Usage, "the body is not JSON: %v", err)
			}
			nodes, _, err := bmcSet(a, args[2:])
			if err != nil {
				return err
			}
			gated := safety.Action{
				Verb:    "POST " + args[0] + " to",
				Targets: nodes,
				Detail:  args[1],
			}
			if resetsHost(args[0]) {
				if err := a.Gate.Check(gated); err != nil {
					return err
				}
				if err := checkSlurmIdle(a, nodes, ipmi.ActionReset, loseJobs); err != nil {
					return err
				}
			}
			if err := a.Gate.Confirm(gated); err != nil {
				return err
			}
			return redfishRequest(a, nodes, "POST", args[0], body)
		}))
	addLoseJobsFlag(post, &loseJobs)

	info := leaf("info [NODESET]", "Summarise what the service processors report", `
Read the computer system resource of each node and show the identification,
the power state and the reset types the firmware accepts.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			nodes, _, err := bmcSet(a, args)
			if err != nil {
				return err
			}
			clients, err := redfishClients(a, nodes.Expand())
			if err != nil {
				return err
			}
			calls := redfishEach(a, nodes.Expand(), clients, false, func(ctx context.Context, _ string, c *redfish.Client) (*redfish.System, error) {
				return c.System(ctx)
			})
			t := output.NewTable(
				output.Column{Name: "NODE"},
				output.Column{Name: "POWER"},
				output.Column{Name: "HEALTH"},
				output.Column{Name: "MODEL"},
				output.Column{Name: "BIOS", Wide: true},
				output.Column{Name: "RESET TYPES", Wide: true},
				output.Column{Name: "ERROR"},
			)
			// Every node is in the object, a failed one with its error, so
			// that a reader of the JSON sees what the table shows.
			object := map[string]any{}
			for _, c := range calls {
				if c.err != nil {
					t.Add(c.node, "", "", "", "", "", c.err.Error())
					object[c.node] = map[string]string{"error": c.err.Error()}
					continue
				}
				sys := c.value
				t.Add(c.node, sys.PowerState, sys.Health, sys.Manufacturer+" "+sys.Model,
					sys.BIOSVersion, strings.Join(sys.ResetTypes, ","), "")
				object[c.node] = sys
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			return bmcExit(callResults(calls, false, func(*redfish.System) string { return "" }))
		}))

	return group("redfish", "Talk to the Redfish interface directly", `
Send requests to the Redfish interface of the service processors.

The certificate of each processor is pinned the first time it is seen and a
change is refused, because a self signed certificate cannot be verified any
other way.`, get, post, info)
}

// resetsHost says whether a Redfish POST path may power a machine off. It
// errs on the side of asking Slurm: any path that mentions a reset counts,
// such as ComputerSystem.Reset, Chassis.Reset or a vendor's own.
func resetsHost(path string) bool {
	return strings.Contains(strings.ToLower(path), "reset")
}

func redfishRequest(a *app.App, nodes *nodeset.NodeSet, method, path string, body any) error {
	clients, err := redfishClients(a, nodes.Expand())
	if err != nil {
		return err
	}
	changes := method != "GET"
	calls := redfishEach(a, nodes.Expand(), clients, changes, func(ctx context.Context, _ string, c *redfish.Client) (map[string]any, error) {
		return c.Do(ctx, method, path, body)
	})
	object := map[string]any{}
	for _, c := range calls {
		if c.err != nil {
			object[c.node] = map[string]string{"error": c.err.Error()}
			continue
		}
		object[c.node] = c.value
	}
	// A Redfish body is a tree, so the table formats show it as JSON too.
	if err := jsonOut(a, object); err != nil {
		return err
	}
	return bmcExit(callResults(calls, changes, func(map[string]any) string { return "" }))
}

// jsonOut prints a tree, using the selected format when it is a machine one
// and indented JSON otherwise.
func jsonOut(a *app.App, object any) error {
	if a.Format.IsMachine() {
		return a.Print(output.Result{Object: object})
	}
	encoded, err := json.MarshalIndent(object, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.Out, output.EscapeText(string(encoded)))
	return err
}

func newBMCWebCommand(r *root) *cobra.Command {
	return leaf("web NODE", "Open the web interface of a service processor", `
Print the URL of a node's service processor, and open it in the browser when
one is configured.`,
		cobra.ExactArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			// The URL is opened in a browser, so the name is selected like
			// every other node argument before it is put into one.
			node, err := oneNode(a, args[0])
			if err != nil {
				return err
			}
			host, err := a.BMCHost(node)
			if err != nil {
				return err
			}
			url := "https://" + host
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), url); err != nil {
				return err
			}

			browser := a.Spec.Workstation.Browser
			if browser == "" || a.DryRun() {
				return nil
			}
			if err := exec.CommandContext(a.Context(), browser, url).Start(); err != nil {
				return exitcode.Errorf(exitcode.Usage, "opening %s with %s: %v", url, browser, err)
			}
			return nil
		}))
}

func newBMCPingCommand(r *root) *cobra.Command {
	return leaf("ping [NODESET]", "Check which service processors answer", `
Sweep the service processors of a node set from the host that can reach the
management network, and report which of them answer.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			_, bmcs, err := bmcSet(a, args)
			if err != nil {
				return err
			}
			role := a.Spec.BMC.IPMI.Via
			if role == "" {
				return exitcode.Errorf(exitcode.Usage,
					"no host role can reach the management network; set bmc.ipmi.via")
			}
			target, err := a.Role(role)
			if err != nil {
				return err
			}

			// fping answers for a whole list in one run and prints the hosts
			// that answered. The list follows --, so no name is read as an
			// option.
			argv := append([]string{"fping", "-a", "-q", "-r", "1", "--"}, bmcs.Expand()...)
			result, err := a.Runner.Run(a.Context(), target, transport.Request{
				Argv:    argv,
				Timeout: 2 * time.Minute,
				TTY:     transport.TTYNone,
			})
			if err != nil {
				return bmcError(a.Context(), exitcode.Wrap(exitcode.Transport, err), false)
			}
			// fping exits 1 when a host did not answer and 2 when a name did
			// not resolve; anything else, or a failed connection, means the
			// sweep itself did not run.
			if result.Err != nil || result.ExitCode > 2 || result.ExitCode < 0 {
				detail := strings.TrimSpace(lastNonEmpty(result.Stderr))
				if detail == "" && result.Err != nil {
					detail = result.Err.Error()
				}
				return bmcError(a.Context(), exitcode.Errorf(exitcode.Transport,
					"the sweep with fping on %s failed (exit %d): %s", target, result.ExitCode, detail), false)
			}

			alive := nodeset.New()
			for _, line := range result.Lines() {
				fields := strings.Fields(line)
				if len(fields) == 0 {
					continue
				}
				_ = alive.Add(fields[0])
			}
			alive = alive.Intersection(bmcs)
			t := output.NewTable(output.Cols("BMC", "STATE")...)
			for _, name := range bmcs.Expand() {
				state := "no answer"
				if alive.Contains(name) {
					state = "alive"
				}
				t.Add(name, state)
			}
			t.Caption = fmt.Sprintf("%d of %d answered", alive.Len(), bmcs.Len())
			if err := a.Print(output.Result{Table: t}); err != nil {
				return err
			}
			if result.ExitCode == 2 {
				return exitcode.Errorf(exitcode.Transport,
					"%d service processors did not answer, and fping could not resolve some of them", bmcs.Len()-alive.Len())
			}
			if alive.Len() < bmcs.Len() {
				return exitcode.Errorf(exitcode.TargetFailed,
					"%d service processors did not answer", bmcs.Len()-alive.Len())
			}
			return nil
		}))
}

func newBMCForgetCommand(r *root) *cobra.Command {
	return leaf("forget [NODESET | BMC...]", "Forget the recorded certificate of a service processor", `
Remove the recorded certificate fingerprint of a service processor, so that
the next connection records whatever it now presents.

Name the nodes, or the service processor as the certificate error names it.
The fingerprints about to be dropped are shown and confirmed like any other
change, and a protected host is refused without --force.

Run this after a certificate was replaced on purpose. If it changed without
anyone replacing it, find out why first: the next connection trusts whatever
it is shown and sends the BMC account to it.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			store := a.PinStore()
			pins, err := store.Load()
			if err != nil {
				return err
			}
			drops, targets, err := pinsToForget(a, pins, args)
			if err != nil {
				return err
			}

			lines := make([]string, len(drops))
			for i, d := range drops {
				lines[i] = d.host + " " + d.fingerprint
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "forget the certificates of",
				Targets: targets,
				Detail:  "drops from " + store.Path + ":\n    " + strings.Join(lines, "\n    "),
			}); err != nil {
				return err
			}
			for _, d := range drops {
				if err := store.Remove(a.Context(), d.host); err != nil {
					return err
				}
				a.Printf("forgot the certificate of %s, %s\n", d.host, d.fingerprint)
			}
			return nil
		}))
}

// pinToForget is a recorded certificate about to be dropped.
type pinToForget struct {
	host, fingerprint string
}

// pinsToForget finds the pins the arguments name, and the nodes they belong
// to, for the gate. An argument is a service processor when a pin is recorded
// under it, which is the name the certificate error gives; anything else is
// a node set, mapped to its service processors the way the other commands
// map it. A node without a recorded pin is named; none at all is an error.
func pinsToForget(a *app.App, pins map[string]string, args []string) ([]pinToForget, *nodeset.NodeSet, error) {
	var (
		drops   []pinToForget
		targets = nodeset.New()
		rest    []string
	)
	add := func(host, fingerprint, node string) error {
		for _, d := range drops {
			if d.host == host {
				return nil
			}
		}
		drops = append(drops, pinToForget{host: host, fingerprint: fingerprint})
		return targets.Add(node)
	}
	for _, arg := range args {
		host, ok := recordedPin(pins, arg)
		if !ok {
			rest = append(rest, arg)
			continue
		}
		// The gate checks nodes, so the processor is mapped back to its
		// node; a protected node is protected under either name.
		if err := add(host, pins[host], nodeOfBMC(a, host)); err != nil {
			return nil, nil, exitcode.Wrap(exitcode.Usage, err)
		}
	}

	unpinned := nodeset.New()
	if len(rest) > 0 || len(args) == 0 {
		nodes, err := selection(a, rest)
		if err != nil {
			return nil, nil, err
		}
		for _, node := range nodes.Expand() {
			bmc, err := a.BMCHost(node)
			if err != nil {
				return nil, nil, err
			}
			host, ok := recordedPin(pins, bmc)
			if !ok {
				_ = unpinned.Add(node)
				continue
			}
			if err := add(host, pins[host], node); err != nil {
				return nil, nil, exitcode.Wrap(exitcode.Usage, err)
			}
		}
	}
	if len(drops) == 0 {
		return nil, nil, exitcode.Errorf(exitcode.TargetFailed,
			"no certificate is recorded for %s; nothing was forgotten", unpinned)
	}
	if !unpinned.IsEmpty() {
		a.Printf("no certificate is recorded for %s\n", unpinned)
	}
	return drops, targets, nil
}

// recordedPin finds the host a pin is recorded under. Host names are not
// case sensitive, and a trailing dot only makes one absolute.
func recordedPin(pins map[string]string, name string) (string, bool) {
	if _, ok := pins[name]; ok {
		return name, true
	}
	want := strings.TrimSuffix(name, ".")
	for host := range pins {
		if strings.EqualFold(host, want) {
			return host, true
		}
	}
	return "", false
}

// nodeOfBMC returns the node whose service processor a host is. For a host
// no node of the inventory has, it returns the host's short name, which is
// what a protected host is usually listed as, or the address itself.
func nodeOfBMC(a *app.App, host string) string {
	if a.Inventory != nil {
		for _, name := range a.Inventory.Names() {
			bmc, err := a.BMCHost(name)
			if err == nil && strings.EqualFold(bmc, host) {
				return name
			}
		}
	}
	if net.ParseIP(host) != nil {
		return host
	}
	return strings.SplitN(host, ".", 2)[0]
}

func newPDUCommand(r *root) *cobra.Command {
	return group("pdu", "Reach the rack power distribution units", `
Open a session on a rack power distribution unit, or run one command on it.
The host name is built from the configured format, the row and the rack.`,
		newPDUShellCommand(r),
		newPDUListCommand(r),
	)
}

func pduHost(a *app.App, row, rack string) (string, error) {
	format := a.Spec.BMC.PDU.NameFormat
	if format == "" {
		return "", exitcode.Errorf(exitcode.Usage,
			"no power distribution unit name format is configured; set bmc.pdu.nameFormat")
	}
	host := fmt.Sprintf(format, row, rack)
	if domain := a.Spec.BMC.PDU.Domain; domain != "" && !strings.Contains(host, ".") {
		host += "." + domain
	}
	return host, nil
}

func newPDUShellCommand(r *root) *cobra.Command {
	return leaf("shell ROW RACK [-- COMMAND...]", "Open a session on a rack power distribution unit", `
Connect to the power distribution unit of a rack, or run one command on it.

  clusterctl pdu shell 1 R02
  clusterctl pdu shell 1 R02 -- show outlets`,
		cobra.MinimumNArgs(2),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			host, err := pduHost(a, args[0], args[1])
			if err != nil {
				return err
			}
			user := a.Spec.BMC.PDU.User
			if user == "" {
				user = "admin"
			}
			var argv []string
			if at := cmd.ArgsLenAtDash(); at >= 0 {
				argv = args[at:]
			}
			target := transport.Target{Name: host, Host: host, User: user}
			// A PDU's command line is its own, not sh.
			return session(a, cmd, target, transport.Request{Argv: argv, NoShell: true})
		}))
}

func newPDUListCommand(r *root) *cobra.Command {
	return leaf("list", "List the power distribution units of the known racks", `
Derive the power distribution unit name of every rack in the inventory.

The row is taken from the rack name where it carries one; racks whose name
does not say which row they are in are listed without one.`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
			racks := a.Inventory.Racks()
			sort.Strings(racks)
			t := output.NewTable(output.Cols("RACK", "PDU", "NODES")...)
			for _, rack := range racks {
				host, err := pduHost(a, rowOf(rack), rack)
				if err != nil {
					return err
				}
				t.Add(rack, host, a.Inventory.InRack(rack).String())
			}
			return a.Print(output.Result{Table: t})
		}))
}

// rowOf reads the row out of a rack name such as "R02", falling back to the
// whole name when it carries no digits.
func rowOf(rack string) string {
	digits := strings.TrimLeftFunc(rack, func(r rune) bool { return r < '0' || r > '9' })
	if digits == "" {
		return rack
	}
	if len(digits) > 1 {
		return digits[:1]
	}
	return digits
}
