// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/hostkeys"
	"github.com/GSI-HPC/clusterctl/internal/ipmi"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// check is one thing doctor looked at.
type check struct {
	Name   string `json:"name" yaml:"name"`
	Status string `json:"status" yaml:"status"`
	Detail string `json:"detail,omitempty" yaml:"detail,omitempty"`
}

// The statuses a check can report.
const (
	statusOK   = "ok"
	statusWarn = "warning"
	statusFail = "failed"
	statusSkip = "skipped"
)

func newDoctorCommand(r *root) *cobra.Command {
	var remote bool

	cmd := leaf("doctor", "Check that this installation can do its job", `
Check the things that go wrong quietly: a configuration that does not resolve,
a missing ssh client, a host key file that is not there, an unreadable age
identity.

With --remote the infrastructure hosts are contacted as well and asked whether
the tools the commands rely on are installed. Under --dry-run nothing is
contacted, and each host is reported as skipped rather than as reachable.

  clusterctl doctor
  clusterctl doctor --remote`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			streams := r.streams
			var checks []check

			a, err := r.App()
			if err != nil {
				checks = append(checks, check{"configuration", statusFail, err.Error()})
				// Without a configuration there is no App to print through,
				// but -o was still asked for: a caller parsing JSON must not
				// be handed a table.
				format, ferr := output.ParseFormat(r.format)
				if ferr != nil {
					format, _ = output.ParseFormat(output.FormatTable)
				}
				return printChecks(nil, format, streams, checks)
			}
			checks = append(checks,
				check{"configuration", statusOK, fmt.Sprintf("context %s, cluster %s, site %s",
					a.Resolved.Context.Name, a.Resolved.ClusterName, a.Resolved.SiteName)},
			)
			checks = append(checks, localChecks(a)...)
			if remote {
				checks = append(checks, remoteChecks(a)...)
			}
			return printChecks(a, a.Format, streams, checks)
		})

	cmd.Flags().BoolVar(&remote, "remote", false, "also contact the infrastructure hosts")
	return cmd
}

func localChecks(a *app.App) []check {
	var checks []check

	// The ssh client is what everything runs through.
	if path, err := exec.LookPath(a.SSH.Binary()); err != nil {
		checks = append(checks, check{"ssh client", statusFail, a.SSH.Binary() + " is not in PATH"})
	} else {
		checks = append(checks, check{"ssh client", statusOK, path + " " + sshVersion(path)})
	}
	if _, err := exec.LookPath(a.SSH.ScpBinary()); err != nil {
		checks = append(checks, check{"scp client", statusWarn, a.SSH.ScpBinary() + " is not in PATH; copy will not work"})
	} else {
		checks = append(checks, check{"scp client", statusOK, ""})
	}

	// The generated configuration has to be writable, and the host key file
	// has to exist for strict checking to mean anything.
	if path, err := a.SSH.ConfigPath(); err != nil {
		checks = append(checks, check{"generated ssh configuration", statusFail, err.Error()})
	} else {
		checks = append(checks, sshConfigCheck(a, path))
	}

	known := a.Path(a.Spec.SSH.KnownHostsFile)
	switch known {
	case "":
		checks = append(checks, check{"host key file", statusFail,
			"none is configured; set ssh.knownHostsFile"})
	default:
		file, err := hostkeys.Load(known)
		switch {
		case err != nil:
			checks = append(checks, check{"host key file", statusFail, err.Error()})
		case len(file.Entries) == 0:
			checks = append(checks, check{"host key file", statusWarn,
				known + " is empty; collect keys with \"clusterctl hostkey refresh\""})
		default:
			checks = append(checks, check{"host key file", statusOK,
				fmt.Sprintf("%d entries in %s", len(file.Entries), known)})
		}
	}

	for _, dir := range []struct{ name, path string }{
		{"state directory", a.StateDir},
		{"cache directory", a.CacheDir},
	} {
		if err := fileutil.EnsureDir(dir.path); err != nil {
			checks = append(checks, check{dir.name, statusFail, err.Error()})
			continue
		}
		checks = append(checks, check{dir.name, statusOK, dir.path})
	}

	// The inventory is what the naming and the groups are built on.
	if a.Inventory.Len() == 0 {
		checks = append(checks, check{"node inventory", statusWarn,
			"no nodes are described; groups by attribute and rack lookups will be empty"})
	} else {
		checks = append(checks, check{"node inventory", statusOK,
			fmt.Sprintf("%d nodes", a.Inventory.Len())})
	}

	// An age identity that cannot be read only shows up when a secret is
	// needed, which is the worst moment to find out.
	if paths := a.IdentityPaths(); len(paths) > 0 {
		var unreadable []string
		for _, p := range paths {
			if err := readableFile(p); err != nil {
				unreadable = append(unreadable, err.Error())
			}
		}
		switch len(unreadable) {
		case 0:
			checks = append(checks, check{"age identities", statusOK, fmt.Sprintf("%d readable", len(paths))})
		default:
			checks = append(checks, check{"age identities", statusFail,
				fmt.Sprintf("%d of %d cannot be read: %s", len(unreadable), len(paths), strings.Join(unreadable, "; "))})
		}
	}

	// A sshuttle that is not installed means the tunnels cannot come up.
	if len(a.Spec.Tunnels) > 0 {
		binary := a.Spec.Workstation.SshuttleBinary
		if binary == "" {
			binary = "sshuttle"
		}
		if _, err := exec.LookPath(binary); err != nil {
			checks = append(checks, check{"sshuttle", statusWarn,
				binary + " is not in PATH; the tunnels cannot be started"})
		} else {
			checks = append(checks, check{"sshuttle", statusOK, ""})
		}
	}

	return checks
}

// remoteTools are the programs each host role has to carry for the commands
// that use it to work.
func remoteTools(a *app.App) map[string][]string {
	tools := map[string][]string{}
	add := func(role string, names ...string) {
		if role == "" {
			return
		}
		tools[role] = append(tools[role], names...)
	}
	add(a.Spec.Slurm.Role, "sinfo", "squeue", "sacct", "sacctmgr", "scontrol", "getent")
	add(a.Spec.BMC.IPMI.Via, ipmiBinary(a.Spec.BMC.IPMI), "fping")
	add(a.Spec.Services.DHCP.Role, "dhcpd")
	add(a.Spec.Services.PXESrv.Role, "git")
	add(a.Spec.Services.Fabric.Role, "ibportstate", "ibqueryerrors", "ibaddr", "iblinkinfo", "perfquery")
	return tools
}

// ipmiBinary is the program the configured IPMI back end runs, at the path
// it runs it from. The defaults are the ones internal/ipmi uses.
func ipmiBinary(spec v1alpha1.IPMISpec) string {
	if spec.Backend == ipmi.BackendIpmitool {
		if spec.IpmitoolPath != "" {
			return spec.IpmitoolPath
		}
		return "/usr/bin/ipmitool"
	}
	if spec.IpmipowerPath != "" {
		return spec.IpmipowerPath
	}
	return "/usr/sbin/ipmipower"
}

// toolCheck is the line of the remote script that prints a tool when it is
// missing. A tool given as a path has to be that executable; a bare name is
// looked up in PATH. The name comes from the configuration, so it is quoted.
func toolCheck(tool string) string {
	q := shellQuote(tool)
	if strings.Contains(tool, "/") {
		return fmt.Sprintf("test -x %s || echo %s\n", q, q)
	}
	return fmt.Sprintf("command -v %s >/dev/null 2>&1 || echo %s\n", q, q)
}

// readableFile opens a file the way reading it would, so that a directory,
// a dangling link or a file without read permission is caught; a stat alone
// passes all three.
func readableFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	return nil
}

// sshConfigCheck has the ssh client read the generated configuration for
// every host role, the way a connection would, and reports everything it
// says. One misspelt option breaks every connection, and ssh names the
// option on a line before the last one, which is all a failed connection
// reports.
func sshConfigCheck(a *app.App, path string) check {
	const name = "generated ssh configuration"
	binary, err := exec.LookPath(a.SSH.Binary())
	if err != nil {
		// The ssh client check says so already.
		return check{name, statusWarn, path + " was not read: no ssh client"}
	}
	hosts := []string{}
	for _, role := range a.RoleNames() {
		if target, err := a.Role(role); err == nil {
			hosts = append(hosts, target.Host)
		}
	}
	if len(hosts) == 0 {
		hosts = append(hosts, "localhost")
	}
	for _, host := range hosts {
		ctx, cancel := context.WithTimeout(a.Context(), 10*time.Second)
		var stderr strings.Builder
		cmd := exec.CommandContext(ctx, binary, "-G", "-F", path, "--", host)
		cmd.Stderr = &stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			detail := strings.Join(nonEmptyLines(stderr.String()), "; ")
			if detail == "" {
				detail = err.Error()
			}
			return check{name, statusFail, fmt.Sprintf("ssh cannot read %s for %s: %s", path, host, detail)}
		}
	}
	return check{name, statusOK, path}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func remoteChecks(a *app.App) []check {
	var checks []check
	tools := remoteTools(a)

	for _, role := range a.RoleNames() {
		target, err := a.Role(role)
		if err != nil {
			checks = append(checks, check{"role " + role, statusFail, err.Error()})
			continue
		}
		// A dry run contacts nothing, and the recorder that stands in for
		// the hosts answers every request with success. Reporting that as
		// reachable would be a check that was never made.
		if a.DryRunRecorder != nil {
			checks = append(checks, check{"role " + role, statusSkip, "not contacted under --dry-run"})
			continue
		}
		result, err := a.Runner.Run(a.Context(), target, transport.Request{
			Argv:    []string{"true"},
			Timeout: 20 * time.Second,
			TTY:     transport.TTYNone,
		})
		if err != nil || result.Failed() {
			detail := "unreachable"
			if err != nil {
				detail = err.Error()
			} else if result.Err != nil {
				detail = result.Err.Error()
			}
			checks = append(checks, check{"role " + role, statusFail, detail})
			continue
		}
		checks = append(checks, check{"role " + role, statusOK, target.Host})

		wanted := tools[role]
		if len(wanted) == 0 {
			continue
		}
		var script strings.Builder
		script.WriteString("set -u\n")
		for _, tool := range wanted {
			if tool == "" {
				continue
			}
			script.WriteString(toolCheck(tool))
		}
		missing, err := a.Runner.Run(a.Context(), target, transport.Request{
			Script:  script.String(),
			Timeout: 30 * time.Second,
			TTY:     transport.TTYNone,
		})
		switch {
		case err != nil:
			checks = append(checks, check{"tools on " + role, statusWarn, err.Error()})
		case len(missing.Lines()) > 0:
			checks = append(checks, check{"tools on " + role, statusFail,
				"missing: " + strings.Join(missing.Lines(), ", ")})
		default:
			checks = append(checks, check{"tools on " + role, statusOK,
				fmt.Sprintf("%d programs present", len(wanted))})
		}
	}
	return checks
}

func printChecks(a *app.App, format output.Format, streams app.Streams, checks []check) error {
	t := output.NewTable(output.Cols("CHECK", "STATUS", "DETAIL")...)
	failed, warned, skipped := 0, 0, 0
	for _, c := range checks {
		t.Add(c.Name, c.Status, c.Detail)
		switch c.Status {
		case statusFail:
			failed++
		case statusWarn:
			warned++
		case statusSkip:
			skipped++
		}
	}
	t.Caption = fmt.Sprintf("%d checks, %d failed, %d warnings", len(checks), failed, warned)
	if skipped > 0 {
		t.Caption += fmt.Sprintf(", %d skipped", skipped)
	}

	result := output.Result{Table: t, Object: checks}
	if a != nil {
		if err := a.Print(result); err != nil {
			return err
		}
	} else if err := format.Write(streams.Out, result); err != nil {
		return err
	}
	if failed > 0 {
		return exitcode.Errorf(exitcode.TargetFailed, "%d checks failed", failed)
	}
	return nil
}

// sshVersion reads the version the ssh client reports, which it prints to
// standard error.
func sshVersion(path string) string {
	out, err := exec.Command(path, "-V").CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	return strings.TrimSpace(string(out))
}
