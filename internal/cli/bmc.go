// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
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
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			nodes, bmcs, err := bmcSet(a, args)
			if err != nil {
				return err
			}
			if useIPMI || a.PreferredBMCTransport(firstNode(nodes)) == "ipmi" {
				return ipmiPower(a, nodes, bmcs, ipmi.ActionStatus)
			}
			return redfishStatus(a, nodes)
		})
	cmd.Flags().BoolVar(&useIPMI, "ipmi", false, "ask over IPMI instead of Redfish")
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

Powering many nodes on at once trips rack breakers, so a power-on is sent in
batches with a pause between them; both come from the configuration.

Slurm is asked first, because powering off a running job loses it. A node
that Slurm reports running a job is refused unless --lose-jobs is given;
--force gets past a protected host, not this check.`,
		cobra.MinimumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			action := strings.ToLower(args[0])
			nodes, bmcs, err := bmcSet(a, args[1:])
			if err != nil {
				return err
			}
			if action == ipmi.ActionStatus {
				if useIPMI {
					return ipmiPower(a, nodes, bmcs, action)
				}
				return redfishStatus(a, nodes)
			}

			gated := safety.Action{
				Verb:    "power " + action,
				Targets: nodes,
				Detail:  "through " + transportName(a, useIPMI, firstNode(nodes)),
			}
			// A protected host is named before Slurm is asked, so that a
			// refusal never leaves it out.
			if err := a.Gate.Check(gated); err != nil {
				return err
			}
			if err := checkSlurmIdle(a, nodes, action, loseJobs); err != nil {
				return err
			}
			if err := a.Gate.Confirm(gated); err != nil {
				return dryRunOrError(err)
			}

			if action == ipmi.ActionOn {
				return staggeredPowerOn(a, nodes, bmcs, useIPMI, batch, stagger)
			}
			if useIPMI || a.PreferredBMCTransport(firstNode(nodes)) == "ipmi" {
				return ipmiPower(a, nodes, bmcs, action)
			}
			return redfishPower(a, nodes, action)
		})

	cmd.Flags().BoolVar(&useIPMI, "ipmi", false, "act over IPMI instead of Redfish")
	cmd.Flags().IntVar(&batch, "batch", 0, "how many nodes to power on at once (default: from the configuration)")
	cmd.Flags().DurationVar(&stagger, "stagger", 0, "pause between power-on batches (default: from the configuration)")
	addLoseJobsFlag(cmd, &loseJobs)
	cmd.ValidArgsFunction = fixed(ipmi.Actions()...)
	return cmd
}

func transportName(a *app.App, useIPMI bool, node string) string {
	if useIPMI || a.PreferredBMCTransport(node) == "ipmi" {
		return "IPMI"
	}
	return "Redfish"
}

func firstNode(ns *nodeset.NodeSet) string {
	names := ns.Expand()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// staggeredPowerOn spreads a power-on over batches so that a rack does not
// draw its whole inrush current at once.
func staggeredPowerOn(a *app.App, nodes, bmcs *nodeset.NodeSet, useIPMI bool, batch int, stagger time.Duration) error {
	if batch <= 0 {
		batch = a.Spec.Safety.PowerOnBatch
	}
	if stagger <= 0 {
		stagger = a.Spec.Safety.PowerOnStagger.Get()
	}
	if batch <= 0 || nodes.Len() <= batch {
		if useIPMI || a.PreferredBMCTransport(firstNode(nodes)) == "ipmi" {
			return ipmiPower(a, nodes, bmcs, ipmi.ActionOn)
		}
		return redfishPower(a, nodes, ipmi.ActionOn)
	}

	chunks := nodes.Split((nodes.Len() + batch - 1) / batch)
	for i, chunk := range chunks {
		if i > 0 && stagger > 0 {
			a.Printf("waiting %s before the next batch\n", stagger)
			select {
			case <-a.Context().Done():
				return a.Context().Err()
			case <-time.After(stagger):
			}
		}
		a.Printf("powering on %s (%d of %d)\n", chunk, i+1, len(chunks))
		chunkBMCs, err := a.BMCHosts(chunk)
		if err != nil {
			return err
		}
		var runErr error
		if useIPMI || a.PreferredBMCTransport(firstNode(chunk)) == "ipmi" {
			runErr = ipmiPower(a, chunk, chunkBMCs, ipmi.ActionOn)
		} else {
			runErr = redfishPower(a, chunk, ipmi.ActionOn)
		}
		if runErr != nil {
			return runErr
		}
	}
	return nil
}

// ipmiPower runs a power action through the IPMI backend.
func ipmiPower(a *app.App, nodes, bmcs *nodeset.NodeSet, action string) error {
	backend, err := a.IPMIBackend(a.Context(), firstNode(nodes))
	if err != nil {
		return err
	}
	statuses, err := backend.Power(a.Context(), action, bmcs)
	if err != nil {
		return exitcode.Wrap(exitcode.Transport, err)
	}

	t := output.NewTable(output.Cols("BMC", "STATE", "ERROR")...)
	failed := 0
	for _, s := range statuses {
		t.Add(s.BMC, s.State, s.Err)
		if s.Err != "" {
			failed++
		}
	}
	if err := a.Print(output.Result{Table: t, Object: statuses}); err != nil {
		return err
	}
	if failed > 0 {
		return exitcode.Errorf(exitcode.TargetFailed, "%d of %d service processors failed", failed, len(statuses))
	}
	return nil
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
	case "reboot":
		return redfish.ResetGracefulRestart, nil
	default:
		return "", fmt.Errorf("unknown power action %q; expected one of %s",
			action, strings.Join(ipmi.Actions(), ", "))
	}
}

// redfishPower runs a power action over Redfish, one processor at a time up
// to the configured concurrency.
func redfishPower(a *app.App, nodes *nodeset.NodeSet, action string) error {
	resetType, err := resetTypeFor(action)
	if err != nil {
		return exitcode.Wrap(exitcode.Usage, err)
	}
	results := forEachBMC(a, nodes, func(ctx context.Context, node string, c *redfish.Client) (string, error) {
		if err := c.Reset(ctx, resetType); err != nil {
			return "", err
		}
		return resetType + " sent", nil
	})
	return printBMCResults(a, results)
}

// redfishStatus reads the power state of every processor.
func redfishStatus(a *app.App, nodes *nodeset.NodeSet) error {
	results := forEachBMC(a, nodes, func(ctx context.Context, node string, c *redfish.Client) (string, error) {
		return c.PowerState(ctx)
	})
	return printBMCResults(a, results)
}

// bmcResult is what one processor answered.
type bmcResult struct {
	Node  string `json:"node" yaml:"node"`
	BMC   string `json:"bmc" yaml:"bmc"`
	State string `json:"state,omitempty" yaml:"state,omitempty"`
	Error string `json:"error,omitempty" yaml:"error,omitempty"`
}

// forEachBMC talks to the processors of a node set in parallel, bounded by
// the configured concurrency.
func forEachBMC(a *app.App, nodes *nodeset.NodeSet, do func(context.Context, string, *redfish.Client) (string, error)) []bmcResult {
	names := nodes.Expand()
	results := make([]bmcResult, len(names))

	limit := a.Spec.BMC.Redfish.MaxConcurrent
	if limit < 1 {
		limit = 8
	}
	sem := make(chan struct{}, limit)
	done := make(chan int, len(names))

	for i, node := range names {
		sem <- struct{}{}
		go func(i int, node string) {
			defer func() { <-sem; done <- i }()
			out := bmcResult{Node: node}
			client, err := a.RedfishClient(a.Context(), node)
			if err != nil {
				out.Error = err.Error()
				results[i] = out
				return
			}
			out.BMC = client.Host
			state, err := do(a.Context(), node, client)
			if err != nil {
				out.Error = err.Error()
			}
			out.State = state
			results[i] = out
		}(i, node)
	}
	for range names {
		<-done
	}
	return results
}

func printBMCResults(a *app.App, results []bmcResult) error {
	t := output.NewTable(
		output.Column{Name: "NODE"},
		output.Column{Name: "BMC", Wide: true},
		output.Column{Name: "STATE"},
		output.Column{Name: "ERROR"},
	)
	failed := 0
	for _, r := range results {
		t.Add(r.Node, r.BMC, r.State, r.Error)
		if r.Error != "" {
			failed++
		}
	}
	if err := a.Print(output.Result{Table: t, Object: results}); err != nil {
		return err
	}
	if failed > 0 {
		return exitcode.Errorf(exitcode.TargetFailed, "%d of %d service processors failed", failed, len(results))
	}
	return nil
}

// addLoseJobsFlag declares the override of the Slurm job check. It is a flag
// of its own, not --force, so that getting past a protected host does not
// also lose the jobs of every node in the set.
func addLoseJobsFlag(cmd *cobra.Command, loseJobs *bool) {
	cmd.Flags().BoolVar(loseJobs, "lose-jobs", false,
		"go ahead although Slurm reports jobs on the nodes")
}

// checkSlurmIdle refuses a power action on a node that is running a job,
// unless loseJobs is set. It asks Slurm even in a dry run, so that the dry
// run refuses what the real run would.
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
	// The check is a safeguard, not a dependency: a cluster whose workload
	// manager cannot be reached still has to be able to power a node off.
	target, err := a.Role(a.Spec.Slurm.Role)
	if err != nil {
		a.Printf("the Slurm host role is not usable (%v); continuing without the job check\n", err)
		return nil
	}
	result, err := a.ReadRunner.Run(a.Context(), target, transport.Request{
		Argv:    []string{"sinfo", "-h", "-N", "-o", "%N %T", "-n", nodes.Hostlist()},
		Timeout: 30 * time.Second,
		TTY:     transport.TTYNone,
	})
	if err != nil || result.Failed() {
		a.Printf("could not ask Slurm about these nodes; continuing without the job check\n")
		return nil //nolint:nilerr // the check is a safeguard, not a dependency
	}

	busy := nodeset.New()
	for _, line := range result.Lines() {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		state := strings.TrimRight(strings.ToLower(fields[1]), "*~#$@+")
		switch state {
		case "allocated", "alloc", "mixed", "mix", "completing", "comp":
			_ = busy.Add(fields[0])
		}
	}
	if busy.IsEmpty() {
		return nil
	}
	if loseJobs {
		a.Printf("%s %s running Slurm jobs; going ahead because --lose-jobs was given\n", busy, plural2(busy.Len()))
		return nil
	}
	return exitcode.Errorf(exitcode.Usage,
		"%s %s running Slurm jobs; drain them and wait for their jobs to end, or pass --lose-jobs to lose the jobs",
		busy, plural2(busy.Len()))
}

func plural2(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
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
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			target := args[0]
			nodes, err := selection(a, args[1:])
			if err != nil {
				return err
			}
			mode := "once"
			if persistent {
				mode = "persistently"
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "set the boot source of",
				Targets: nodes,
				Detail:  fmt.Sprintf("to %s, %s", target, mode),
			}); err != nil {
				return dryRunOrError(err)
			}
			results := forEachBMC(a, nodes, func(ctx context.Context, _ string, c *redfish.Client) (string, error) {
				if err := c.SetBootOverride(ctx, target, persistent); err != nil {
					return "", err
				}
				return target + " " + mode, nil
			})
			return printBMCResults(a, results)
		})
	set.Flags().BoolVar(&persistent, "persistent", false, "keep the override until it is removed")

	clear := leaf("unset [NODESET]", "Remove a boot source override", `
Remove the boot source override, so the nodes boot their usual way again.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			nodes, err := selection(a, args)
			if err != nil {
				return err
			}
			if err := a.Gate.Confirm(safety.Action{Verb: "clear the boot source override of", Targets: nodes}); err != nil {
				return dryRunOrError(err)
			}
			results := forEachBMC(a, nodes, func(ctx context.Context, _ string, c *redfish.Client) (string, error) {
				if err := c.ClearBootOverride(ctx); err != nil {
					return "", err
				}
				return "cleared", nil
			})
			return printBMCResults(a, results)
		})

	show := leaf("show [NODESET]", "Show the current boot source override", `
Report what each machine is set to boot next time, and what its firmware
accepts.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			nodes, err := selection(a, args)
			if err != nil {
				return err
			}
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
			var object []row
			failed := 0
			for _, node := range nodes.Expand() {
				client, err := a.RedfishClient(a.Context(), node)
				if err != nil {
					t.Add(node, "", "", "", err.Error())
					object = append(object, row{Node: node, Error: err.Error()})
					failed++
					continue
				}
				sys, err := client.System(a.Context())
				if err != nil {
					t.Add(node, "", "", "", err.Error())
					object = append(object, row{Node: node, Error: err.Error()})
					failed++
					continue
				}
				t.Add(node, sys.BootSource, sys.BootEnabled, strings.Join(sys.BootTargets, ","), "")
				object = append(object, row{Node: node, Source: sys.BootSource, Mode: sys.BootEnabled, Accepts: sys.BootTargets})
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d of %d nodes failed", failed, nodes.Len())
			}
			return nil
		})

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
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			nodes, err := selection(a, args[1:])
			if err != nil {
				return err
			}
			return redfishRequest(a, nodes, "GET", args[0], nil)
		})

	post := leaf("post PATH BODY [NODESET]", "Send an action to a Redfish resource", `
Send a POST with a JSON body to the Redfish interface.

This can power off a machine, so it goes through the confirmation gate like
any other destructive command, and it is never retried.`,
		cobra.MinimumNArgs(2),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			var body any
			if err := json.Unmarshal([]byte(args[1]), &body); err != nil {
				return exitcode.Errorf(exitcode.Usage, "the body is not JSON: %v", err)
			}
			nodes, err := selection(a, args[2:])
			if err != nil {
				return err
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "POST " + args[0] + " to",
				Targets: nodes,
				Detail:  args[1],
			}); err != nil {
				return dryRunOrError(err)
			}
			return redfishRequest(a, nodes, "POST", args[0], body)
		})

	info := leaf("info [NODESET]", "Summarise what the service processors report", `
Read the computer system resource of each node and show the identification,
the power state and the reset types the firmware accepts.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			nodes, err := selection(a, args)
			if err != nil {
				return err
			}
			t := output.NewTable(
				output.Column{Name: "NODE"},
				output.Column{Name: "POWER"},
				output.Column{Name: "HEALTH"},
				output.Column{Name: "MODEL"},
				output.Column{Name: "BIOS", Wide: true},
				output.Column{Name: "RESET TYPES", Wide: true},
				output.Column{Name: "ERROR"},
			)
			object := map[string]any{}
			failed := 0
			for _, node := range nodes.Expand() {
				client, err := a.RedfishClient(a.Context(), node)
				if err != nil {
					t.Add(node, "", "", "", "", "", err.Error())
					failed++
					continue
				}
				sys, err := client.System(a.Context())
				if err != nil {
					t.Add(node, "", "", "", "", "", err.Error())
					object[node] = map[string]string{"error": err.Error()}
					failed++
					continue
				}
				t.Add(node, sys.PowerState, sys.Health, sys.Manufacturer+" "+sys.Model,
					sys.BIOSVersion, strings.Join(sys.ResetTypes, ","), "")
				object[node] = sys
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d of %d nodes failed", failed, nodes.Len())
			}
			return nil
		})

	return group("redfish", "Talk to the Redfish interface directly", `
Send requests to the Redfish interface of the service processors.

The certificate of each processor is pinned the first time it is seen and a
change is refused, because a self signed certificate cannot be verified any
other way.`, get, post, info)
}

func redfishRequest(a *app.App, nodes *nodeset.NodeSet, method, path string, body any) error {
	object := map[string]any{}
	failed := 0
	for _, node := range nodes.Expand() {
		client, err := a.RedfishClient(a.Context(), node)
		if err != nil {
			object[node] = map[string]string{"error": err.Error()}
			failed++
			continue
		}
		answer, err := client.Do(a.Context(), method, path, body)
		if err != nil {
			object[node] = map[string]string{"error": err.Error()}
			failed++
			continue
		}
		object[node] = answer
	}
	// A Redfish body is a tree, so the table formats show it as JSON too.
	if err := jsonOut(a, object); err != nil {
		return err
	}
	if failed > 0 {
		return exitcode.Errorf(exitcode.TargetFailed, "%d of %d nodes failed", failed, nodes.Len())
	}
	return nil
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
	_, err = fmt.Fprintln(a.Out, string(encoded))
	return err
}

func newBMCWebCommand(r *root) *cobra.Command {
	return leaf("web NODE", "Open the web interface of a service processor", `
Print the URL of a node's service processor, and open it in the browser when
one is configured.`,
		cobra.ExactArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			host, err := a.BMCHost(args[0])
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
		})
}

func newBMCPingCommand(r *root) *cobra.Command {
	return leaf("ping [NODESET]", "Check which service processors answer", `
Sweep the service processors of a node set from the host that can reach the
management network, and report which of them answer.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
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

			// fping answers for a whole list in one run, and prints one line
			// per host whether it answered or not.
			argv := append([]string{"fping", "-a", "-q", "-r", "1"}, bmcs.Expand()...)
			result, err := a.Runner.Run(a.Context(), target, transport.Request{
				Argv:    argv,
				Timeout: 2 * time.Minute,
				TTY:     transport.TTYNone,
			})
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}

			alive := nodeset.New()
			for _, line := range result.Lines() {
				_ = alive.Add(strings.Fields(line)[0])
			}
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
			if alive.Len() < bmcs.Len() {
				return exitcode.Errorf(exitcode.TargetFailed,
					"%d service processors did not answer", bmcs.Len()-alive.Len())
			}
			return nil
		})
}

func newBMCForgetCommand(r *root) *cobra.Command {
	return leaf("forget NODE...", "Forget the recorded certificate of a service processor", `
Remove the recorded certificate fingerprint of a service processor, so that
the next connection records whatever it now presents.

Run this after a certificate was replaced on purpose. If it changed without
anyone replacing it, find out why first.`,
		cobra.MinimumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			path := a.Path(a.Spec.BMC.Redfish.PinStore)
			if path == "" {
				path = a.StatePath("bmc-pins")
			}
			store := &redfish.PinStore{Path: path}
			for _, node := range args {
				// The pin is kept under the host the client talked to.
				host, err := a.BMCHost(node)
				if err != nil {
					return err
				}
				if err := store.Remove(a.Context(), host); err != nil {
					return err
				}
				a.Printf("forgot the certificate of %s\n", host)
			}
			return nil
		})
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
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
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
			req := transport.Request{Argv: argv}
			if a.DryRun() {
				line, err := a.SSH.Args(target, req)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), strings.Join(line, " "))
				return err
			}
			return a.SSH.Interactive(a.Context(), target, req)
		})
}

func newPDUListCommand(r *root) *cobra.Command {
	return leaf("list", "List the power distribution units of the known racks", `
Derive the power distribution unit name of every rack in the inventory.

The row is taken from the rack name where it carries one; racks whose name
does not say which row they are in are listed without one.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
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
		})
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
