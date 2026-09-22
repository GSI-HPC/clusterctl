// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func newSlurmCommand(r *root) *cobra.Command {
	return group("slurm", "Administer the workload manager", `
Read and change the state of the Slurm cluster: nodes and their reasons, the
queue, the accounting database and fair share.

The clients are asked for their parsable output with an explicit field list,
rather than having their aligned display output parsed. The display form
moves with the terminal width and truncates, which is how a state once ended
up reported against the wrong node.`,
		newSlurmNodeCommand(r),
		newSlurmJobCommand(r),
		newSlurmAccountCommand(r),
		newSlurmUserCommand(r),
		newSlurmPartitionCommand(r),
	)
}

// slurmClient builds the client from the resolved configuration.
func slurmClient(a *app.App) (*slurm.Client, error) { return a.Slurm() }

// statesFor turns a state group name, or a list of states, into the states to
// ask Slurm for.
func statesFor(arg string) []string { return slurm.States(arg) }

func newSlurmNodeCommand(r *root) *cobra.Command {
	return group("node", "Read and change the state of the nodes", `
List nodes with their state and the reason they carry, take them out of
production and put them back.`,
		newSlurmNodeListCommand(r),
		newSlurmNodeDrainCommand(r),
		newSlurmNodeResumeCommand(r),
		newSlurmNodesetCommand(r),
	)
}

func newSlurmNodeListCommand(r *root) *cobra.Command {
	var state string

	cmd := leaf("list [NODESET]", "List the nodes Slurm knows", `
List the nodes with their state, partition, resources and the reason a drained
node carries.

  clusterctl slurm node list
  clusterctl slurm node list --state defect
  clusterctl slurm node list --state idle -o wide`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			ns, err := a.SelectOptional(strings.Join(args, ","))
			if err != nil {
				return err
			}
			nodes, err := c.Nodes(a.Context(), ns, statesFor(state))
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}

			t := output.NewTable(
				output.Column{Name: "NODE"},
				output.Column{Name: "STATE"},
				output.Column{Name: "PARTITION"},
				output.Column{Name: "CPUS", Right: true},
				output.Column{Name: "MEMORY", Right: true, Wide: true},
				output.Column{Name: "FEATURES", Wide: true},
				output.Column{Name: "GRES", Wide: true},
				output.Column{Name: "REASON"},
			)
			for _, n := range nodes {
				reason := n.Reason
				if reason == "(null)" {
					reason = ""
				}
				if reason != "" && n.ReasonUser != "" {
					reason = fmt.Sprintf("%s [%s]", reason, n.ReasonUser)
				}
				t.Add(n.Name, n.State, n.Partition, n.CPUs, n.Memory, n.Features, n.GRES, reason)
			}
			t.Caption = fmt.Sprintf("%d nodes", len(nodes))
			return a.Print(output.Result{Table: t, Object: nodes})
		})

	cmd.Flags().StringVar(&state, "state", "", "limit to a state or a state group: "+strings.Join(stateGroupNames(), ", "))
	_ = cmd.RegisterFlagCompletionFunc("state", fixed(stateGroupNames()...))
	return cmd
}

func stateGroupNames() []string {
	groups := slurm.StateGroups()
	out := make([]string, 0, len(groups))
	for name := range groups {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func newSlurmNodeDrainCommand(r *root) *cobra.Command {
	return leaf("drain REASON [NODESET]", "Take nodes out of production", `
Set the nodes to drain so that no new job is scheduled on them. Jobs already
running are left alone.

The reason is mandatory and comes first, because a drained node with no reason
is a node nobody dares resume. Say what is wrong and where it is tracked.

  clusterctl slurm node drain 'ticket 4711: failing DIMM' -n exe0007`,
		cobra.MinimumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			reason := args[0]
			ns, err := selection(a, args[1:])
			if err != nil {
				return err
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "drain",
				Targets: ns,
				Detail:  "reason: " + reason,
			}); err != nil {
				if safety.IsDryRun(err) {
					return nil
				}
				return err
			}
			if err := c.Drain(a.Context(), ns, reason); err != nil {
				return exitcode.Wrap(exitcode.TargetFailed, err)
			}
			a.Printf("drained %s\n", ns)
			return nil
		})
}

func newSlurmNodeResumeCommand(r *root) *cobra.Command {
	return leaf("resume [NODESET]", "Put nodes back into production", `
Set the nodes to resume so that jobs are scheduled on them again.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			if err := a.Gate.Confirm(safety.Action{Verb: "resume", Targets: ns}); err != nil {
				if safety.IsDryRun(err) {
					return nil
				}
				return err
			}
			if err := c.Resume(a.Context(), ns); err != nil {
				return exitcode.Wrap(exitcode.TargetFailed, err)
			}
			a.Printf("resumed %s\n", ns)
			return nil
		})
}

func newSlurmNodesetCommand(r *root) *cobra.Command {
	cmd := leaf("nodeset [STATE]", "Print the nodes in a state as a node set", `
Print a node set of the machines Slurm knows, optionally limited to a state
group, ready to be passed to another command.

  clusterctl slurm node nodeset idle
  clusterctl exec -n "$(clusterctl slurm node nodeset drain)" -- uptime`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			state := ""
			if len(args) == 1 {
				state = args[0]
			}
			ns, err := c.NodeSet(a.Context(), statesFor(state))
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}
			if a.Format.IsMachine() {
				return a.Print(output.Result{Nodes: ns, Object: ns.Expand()})
			}
			return say(cmd, "%s\n", ns)
		})
	cmd.ValidArgsFunction = fixed(stateGroupNames()...)
	return cmd
}

func newSlurmJobCommand(r *root) *cobra.Command {
	return group("job", "Read the queue and the accounting database", `
List running and pending jobs, and read what finished out of the accounting
database.`,
		newSlurmJobListCommand(r),
		newSlurmJobHistoryCommand(r),
		newSlurmJobSummaryCommand(r),
	)
}

func newSlurmJobListCommand(r *root) *cobra.Command {
	var (
		state string
		user  string
	)

	cmd := leaf("list [NODESET]", "List the jobs in the queue", `
List the jobs in the queue, optionally limited to a state, a user or the
nodes they run on.

  clusterctl slurm job list --state running
  clusterctl slurm job list --state pending -o wide
  clusterctl slurm job list -n exe0007`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			ns, err := a.SelectOptional(strings.Join(args, ","))
			if err != nil {
				return err
			}
			filter := slurm.JobFilter{Nodes: ns}
			if state != "" {
				filter.States = strings.Split(strings.ToUpper(state), ",")
			}
			if user != "" {
				filter.Users = strings.Split(user, ",")
			}
			jobs, err := c.Jobs(a.Context(), filter)
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}

			t := output.NewTable(
				output.Column{Name: "JOB"},
				output.Column{Name: "USER"},
				output.Column{Name: "ACCOUNT"},
				output.Column{Name: "PARTITION"},
				output.Column{Name: "STATE"},
				output.Column{Name: "NODES"},
				output.Column{Name: "CPUS", Right: true, Wide: true},
				output.Column{Name: "TIME LIMIT", Wide: true},
				output.Column{Name: "RUNTIME"},
				output.Column{Name: "REASON", Wide: true},
				output.Column{Name: "WORKDIR", Wide: true},
				output.Column{Name: "COMMAND", Wide: true},
			)
			for _, j := range jobs {
				t.Add(j.ID, j.User, j.Account, j.Partition, j.State, j.Nodes,
					j.CPUs, j.TimeLimit, j.Runtime, j.Reason, j.WorkDir, j.Command)
			}
			t.Caption = fmt.Sprintf("%d jobs", len(jobs))
			return a.Print(output.Result{Table: t, Object: jobs})
		})

	cmd.Flags().StringVar(&state, "state", "", "limit to a job state, for example running or pending")
	cmd.Flags().StringVarP(&user, "user", "u", "", "limit to one or more users")
	_ = cmd.RegisterFlagCompletionFunc("state", fixed("running", "pending", "completing", "suspended"))
	return cmd
}

func newSlurmJobHistoryCommand(r *root) *cobra.Command {
	var (
		since time.Duration
		state string
		user  string
		ids   string
	)

	cmd := leaf("history", "List finished jobs from the accounting database", `
Read what finished out of the accounting database.

  clusterctl slurm job history --state failed --since 24h
  clusterctl slurm job history --jobs 4711,4712`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			filter := slurm.AccountingFilter{Since: since, AllUsers: user == ""}
			if filter.Since == 0 {
				filter.Since = a.Spec.Slurm.LookBack.Or(time.Hour)
			}
			if state != "" {
				filter.States = strings.Split(strings.ToUpper(state), ",")
			}
			if user != "" {
				filter.Users = strings.Split(user, ",")
			}
			if ids != "" {
				filter.JobIDs = strings.Split(ids, ",")
			}
			jobs, err := c.History(a.Context(), filter)
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}

			t := output.NewTable(
				output.Column{Name: "JOB"},
				output.Column{Name: "USER"},
				output.Column{Name: "ACCOUNT"},
				output.Column{Name: "STATE"},
				output.Column{Name: "EXIT"},
				output.Column{Name: "ELAPSED"},
				output.Column{Name: "NODES", Wide: true},
				output.Column{Name: "START", Wide: true},
				output.Column{Name: "END", Wide: true},
			)
			for _, j := range jobs {
				t.Add(j.JobID, j.User, j.Account, j.State, j.ExitCode, j.Elapsed, j.Nodes, j.Start, j.End)
			}
			t.Caption = fmt.Sprintf("%d jobs", len(jobs))
			return a.Print(output.Result{Table: t, Object: jobs})
		})

	cmd.Flags().DurationVar(&since, "since", 0, "how far back to look (default: from the configuration)")
	cmd.Flags().StringVar(&state, "state", "", "limit to job states, for example failed,timeout")
	cmd.Flags().StringVarP(&user, "user", "u", "", "limit to one or more users")
	cmd.Flags().StringVar(&ids, "jobs", "", "look up specific job identifiers instead of a time window")
	_ = cmd.RegisterFlagCompletionFunc("state", fixed("completed", "failed", "timeout", "cancelled", "node_fail"))
	return cmd
}

func newSlurmJobSummaryCommand(r *root) *cobra.Command {
	var state string

	cmd := leaf("summary", "Count the jobs per user", `
Count the jobs in a state per user and account, which is the overview to read
before deciding whose work is filling the queue.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			filter := slurm.JobFilter{States: strings.Split(strings.ToUpper(state), ",")}
			jobs, err := c.Jobs(a.Context(), filter)
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}

			type key struct{ user, account, partition string }
			counts := map[key]int{}
			for _, j := range jobs {
				counts[key{j.User, j.Account, j.Partition}]++
			}
			keys := make([]key, 0, len(counts))
			for k := range counts {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				if counts[keys[i]] != counts[keys[j]] {
					return counts[keys[i]] > counts[keys[j]]
				}
				return keys[i].user < keys[j].user
			})

			t := output.NewTable(
				output.Column{Name: "USER"},
				output.Column{Name: "ACCOUNT"},
				output.Column{Name: "PARTITION"},
				output.Column{Name: "JOBS", Right: true},
			)
			object := make([]map[string]any, 0, len(keys))
			for _, k := range keys {
				t.Add(k.user, k.account, k.partition, fmt.Sprint(counts[k]))
				object = append(object, map[string]any{
					"user": k.user, "account": k.account, "partition": k.partition, "jobs": counts[k],
				})
			}
			t.Caption = fmt.Sprintf("%d jobs in state %s", len(jobs), state)
			return a.Print(output.Result{Table: t, Object: object})
		})

	cmd.Flags().StringVar(&state, "state", "PENDING", "the job state to count")
	_ = cmd.RegisterFlagCompletionFunc("state", fixed("PENDING", "RUNNING"))
	return cmd
}

func newSlurmAccountCommand(r *root) *cobra.Command {
	list := leaf("list [ACCOUNT]", "List the accounts and their coordinators", `
List the accounts of the accounting database.`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			accounts, err := c.Accounts(a.Context(), first(args))
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}
			t := output.NewTable(
				output.Column{Name: "ACCOUNT"},
				output.Column{Name: "DESCRIPTION"},
				output.Column{Name: "ORGANIZATION", Wide: true},
				output.Column{Name: "COORDINATORS"},
			)
			for _, acc := range accounts {
				t.Add(acc.Account, acc.Description, acc.Organization, acc.Coordinators)
			}
			return a.Print(output.Result{Table: t, Object: accounts})
		})

	limits := leaf("limits [ACCOUNT]", "List the limits of an account's associations", `
List the associations of an account with the limits they carry.`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			assoc, err := c.AccountLimits(a.Context(), first(args))
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}
			t := output.NewTable(output.Cols(
				"ACCOUNT", "USER", "MAX SUBMIT", "MAX JOBS", "MAX NODES", "MAX CPUS", "MAX WALL", "FAIRSHARE")...)
			for _, x := range assoc {
				t.Add(x.Account, x.User, x.MaxSubmit, x.MaxJobs, x.MaxNodes, x.MaxCPUs, x.MaxWall, x.FairShare)
			}
			return a.Print(output.Result{Table: t, Object: assoc})
		})

	add := leaf("add ACCOUNT [ORGANIZATION] [DESCRIPTION]", "Create an account", `
Add an account to the accounting database. The organisation and the
description default to what the cluster configuration says and to the account
name.`,
		cobra.RangeArgs(1, 3),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			org, desc := "", ""
			if len(args) > 1 {
				org = args[1]
			}
			if len(args) > 2 {
				desc = args[2]
			}
			if err := confirmChange(a, "create the Slurm account "+args[0]); err != nil {
				return err
			}
			if err := c.AddAccount(a.Context(), args[0], org, desc); err != nil {
				return exitcode.Wrap(exitcode.TargetFailed, err)
			}
			a.Printf("account %s created\n", args[0])
			return nil
		})

	coordinator := leaf("coordinator ACCOUNT USER...", "Make users coordinators of an account", `
Add coordinators to an account.`,
		cobra.MinimumNArgs(2),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			if err := confirmChange(a, fmt.Sprintf("make %s coordinators of %s",
				strings.Join(args[1:], ", "), args[0])); err != nil {
				return err
			}
			if err := c.SetCoordinators(a.Context(), args[0], args[1:]); err != nil {
				return exitcode.Wrap(exitcode.TargetFailed, err)
			}
			a.Printf("coordinators of %s set\n", args[0])
			return nil
		})

	shares := leaf("shares [ACCOUNT] [VALUE]", "Read or set fair share", `
List the fair-share values of the accounts, or set the value of one.`,
		cobra.MaximumNArgs(2),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			if len(args) == 2 {
				if err := confirmChange(a, fmt.Sprintf("set the fair share of %s to %s", args[0], args[1])); err != nil {
					return err
				}
				if err := c.SetFairShare(a.Context(), args[0], args[1]); err != nil {
					return exitcode.Wrap(exitcode.TargetFailed, err)
				}
				a.Printf("fair share of %s set to %s\n", args[0], args[1])
				return nil
			}
			assoc, err := c.Shares(a.Context(), first(args))
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}
			t := output.NewTable(output.Cols("ACCOUNT", "USER", "FAIRSHARE")...)
			for _, x := range assoc {
				t.Add(x.Account, x.User, x.FairShare)
			}
			return a.Print(output.Result{Table: t, Object: assoc})
		})

	return group("account", "Administer the accounting database accounts", `
List, create and configure the accounts jobs are charged to.`,
		list, limits, add, coordinator, shares)
}

func newSlurmUserCommand(r *root) *cobra.Command {
	list := leaf("list [USER]", "List the user associations", `
List the users of the accounting database with their accounts.`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			users, err := c.Users(a.Context(), first(args))
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}
			t := output.NewTable(output.Cols("USER", "ACCOUNT", "DEFAULT", "FAIRSHARE")...)
			for _, u := range users {
				t.Add(u.User, u.Account, u.DefaultAccount, u.FairShare)
			}
			return a.Print(output.Result{Table: t, Object: users})
		})

	add := leaf("add USER [ACCOUNT] [DEFAULT_ACCOUNT]", "Associate a user with an account", `
Associate a user with an account, creating the association when the user has
none yet.

The cluster is asked whether the user exists at all before anything is
written, with getent rather than id, so that a local account on the login node
is not mistaken for a directory user.`,
		cobra.RangeArgs(1, 3),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			user := args[0]
			exists, err := c.HasPosixUser(a.Context(), user)
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}
			if !exists {
				return exitcode.Errorf(exitcode.Usage,
					"the cluster does not know a user account %q; check the directory before adding it to Slurm", user)
			}
			account, defaultAccount := "", ""
			if len(args) > 1 {
				account = args[1]
			}
			if len(args) > 2 {
				defaultAccount = args[2]
			}
			if err := confirmChange(a, fmt.Sprintf("associate %s with the Slurm account %s", user, account)); err != nil {
				return err
			}
			if err := c.AddUser(a.Context(), user, account, defaultAccount); err != nil {
				return exitcode.Wrap(exitcode.TargetFailed, err)
			}
			a.Printf("user %s associated\n", user)
			return nil
		})

	setDefault := leaf("default USER ACCOUNT", "Set the default account of a user", `
Change which account a user's jobs are charged to by default.`,
		cobra.ExactArgs(2),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			if err := confirmChange(a, fmt.Sprintf("set the default account of %s to %s", args[0], args[1])); err != nil {
				return err
			}
			if err := c.SetDefaultAccount(a.Context(), args[0], args[1]); err != nil {
				return exitcode.Wrap(exitcode.TargetFailed, err)
			}
			a.Printf("default account of %s set to %s\n", args[0], args[1])
			return nil
		})

	return group("user", "Administer the accounting database users", `
List users and their accounts, and change what their jobs are charged to.`,
		list, add, setDefault)
}

func newSlurmPartitionCommand(r *root) *cobra.Command {
	return leaf("partition [NAME]", "List the partitions and their resources", `
List the partitions with their node counts, run-time limits and the resources
they offer.`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			c, err := slurmClient(a)
			if err != nil {
				return err
			}
			partitions, err := c.Partitions(a.Context(), first(args))
			if err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}
			t := output.NewTable(
				output.Column{Name: "PARTITION"},
				output.Column{Name: "AVAIL"},
				output.Column{Name: "NODES", Right: true},
				output.Column{Name: "MAX TIME"},
				output.Column{Name: "DEFAULT TIME", Wide: true},
				output.Column{Name: "MEMORY", Right: true, Wide: true},
				output.Column{Name: "CPUS", Right: true, Wide: true},
				output.Column{Name: "CPU A/I/O/T", Wide: true},
				output.Column{Name: "GROUPS", Wide: true},
				output.Column{Name: "NODELIST", Wide: true},
			)
			for _, p := range partitions {
				t.Add(p.Name, p.Available, p.NodeCount, p.MaxTime, p.DefaultTime,
					p.Memory, p.CPUs, p.CPUState, p.Groups, p.Nodes)
			}
			return a.Print(output.Result{Table: t, Object: partitions})
		})
}

// confirmChange asks before a change to the accounting database, which has no
// undo and no node set to preview.
func confirmChange(a *app.App, what string) error {
	ns := nodeset.New()
	_ = ns.Add("accounting")
	err := a.Gate.Confirm(safety.Action{Verb: what + " on", Targets: ns})
	if safety.IsDryRun(err) {
		return safety.ErrDryRun
	}
	return err
}

func first(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}
