// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/hostkeys"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/secrets"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func newSecretsCommand(r *root) *cobra.Command {
	return group("secrets", "Distribute the encrypted files the nodes need", `
Decrypt the files a site keeps beside its configuration, or the values of its
sops encrypted Secret documents, and write them onto the nodes.

The plaintext never touches this workstation's disk: it is decrypted into
memory and streamed to each node over standard input, which is what keeps a
cluster key off a laptop.`,
		newSecretsListCommand(r),
		newSecretsPushCommand(r),
		newSecretsCheckCommand(r),
	)
}

func newSecretsListCommand(r *root) *cobra.Command {
	return leaf("list", "List the secrets the configuration carries", `
List the encrypted files and where each one lands on a node.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("SOURCE", "TARGET", "MODE", "OWNER")...)
			for _, s := range a.Spec.Services.Cinc.Secrets {
				owner := s.Owner
				if s.Group != "" {
					owner += ":" + s.Group
				}
				source := a.Path(s.Source)
				if s.SecretRef != nil {
					source = "secret " + s.SecretRef.String()
				}
				t.Add(source, s.Target, s.Mode, owner)
			}
			t.Caption = fmt.Sprintf("%d secrets", t.Len())
			return a.Print(output.Result{Table: t, Object: a.Spec.Services.Cinc.Secrets})
		})
}

func newSecretsPushCommand(r *root) *cobra.Command {
	return leaf("push [NODESET]", "Write the secrets onto a node set", `
Decrypt each configured secret and write it onto the nodes with the owner and
mode the configuration asks for.

Every secret is decrypted before anything is asked or written, so a key this
workstation lacks stops the push, and its dry run, before a node is touched.
Each file is written beside its target and moved into place only once all of
it has arrived, so a lost connection leaves the old file as it was. A node
that cannot be reached is not tried again for the next secret; when no node
that failed could be reached, the command exits 3.

This overwrites files on the nodes, so it asks first.

  clusterctl secrets push -n exe[1-4]`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			files := a.Spec.Services.Cinc.Secrets
			if len(files) == 0 {
				return exitcode.Errorf(exitcode.Usage,
					"no secrets are configured; add services.cinc.secrets to the site document")
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}

			// Everything the push needs is worked out before it asks: the
			// addresses of the nodes, and every secret, decrypted. A key
			// that is missing for one of them stops the push, and the dry
			// run, before anything is written rather than half way.
			targets, err := a.NodeTargets(ns)
			if err != nil {
				return err
			}
			contents := make([][]byte, len(files))
			for i, file := range files {
				if contents[i], err = a.SecretContent(file); err != nil {
					return err
				}
			}

			names := make([]string, 0, len(files))
			for _, f := range files {
				names = append(names, f.Target)
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "write secrets onto",
				Targets: ns,
				Detail:  strings.Join(names, ", "),
			}); err != nil {
				return dryRunOrError(err)
			}

			t := output.NewTable(output.Cols("NODE", "SECRET", "STATUS")...)
			// failed holds the first failure of each node. A node that
			// could not be reached is not tried again for the next
			// secret, which would only wait out the connect timeout again.
			failed := map[string]error{}
			gone := map[string]bool{}
			for i, file := range files {
				var live []transport.Target
				for _, tg := range targets {
					if !gone[tg.Name] {
						live = append(live, tg)
					}
				}
				script := secretScript(file, len(contents[i]))
				results := a.Executor().RunEach(a.Context(), live, func(transport.Target) transport.Request {
					return transport.Request{
						Script:  script,
						Stdin:   bytes.NewReader(contents[i]),
						Timeout: a.Timeout().Get(),
						TTY:     transport.TTYNone,
					}
				})
				byName := map[string]*transport.Result{}
				for _, res := range results {
					byName[res.Target.Name] = res
				}
				for _, tg := range targets {
					if gone[tg.Name] && byName[tg.Name] == nil {
						t.Add(tg.Name, file.Target, "skipped: the node could not be reached")
						continue
					}
					res := byName[tg.Name]
					if res == nil {
						res = &transport.Result{Target: tg, ExitCode: -1, Err: fmt.Errorf("%s: no result", tg.Name)}
					}
					t.Add(tg.Name, file.Target, pushStatus(res))
					if !res.Failed() {
						continue
					}
					if _, seen := failed[tg.Name]; !seen {
						failed[tg.Name] = pushError(res)
					}
					if nodeUnreachable(failed[tg.Name]) {
						gone[tg.Name] = true
					}
				}
			}
			if err := a.Print(output.Result{Table: t}); err != nil {
				return err
			}
			return pushFailures(targets, failed)
		})
}

// secretScript writes the payload on standard input to a temporary file
// beside the target, checks that all of it arrived and only then moves it
// into place, so that a connection lost half way leaves the old file, not
// an empty one. A missing directory is created readable by root alone; one
// that exists is left as it is. The payload is never an argument and never
// a file on this machine.
func secretScript(file v1alpha1.SecretFile, size int) string {
	mode := file.Mode
	if mode == "" {
		mode = "0600"
	}
	dir := shellquote.Quote(path.Dir(file.Target))
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\numask 077\nmkdir -p -m 0700 %s\n", dir)
	fmt.Fprintf(&b, "tmp=$(mktemp %s/.clusterctl.XXXXXX)\n", dir)
	b.WriteString("trap 'rm -f \"$tmp\"' EXIT\ntrap 'rm -f \"$tmp\"; exit 1' HUP INT TERM PIPE\n")
	b.WriteString("cat > \"$tmp\"\n")
	b.WriteString("size=$(wc -c < \"$tmp\")\n")
	fmt.Fprintf(&b, "if [ \"$size\" -ne %d ]; then echo \"received $size of %d bytes; the file was left as it was\" >&2; exit 1; fi\n", size, size)
	if file.Owner != "" {
		owner := file.Owner
		if file.Group != "" {
			owner += ":" + file.Group
		}
		fmt.Fprintf(&b, "chown %s \"$tmp\"\n", shellquote.Quote(owner))
	}
	fmt.Fprintf(&b, "chmod %s \"$tmp\"\n", shellquote.Quote(mode))
	fmt.Fprintf(&b, "mv -f \"$tmp\" %s\n", shellquote.Quote(file.Target))
	b.WriteString("trap - EXIT\n")
	return b.String()
}

// pushStatus says what became of one secret on one node, with the reason
// when it failed, from the node or else from the transport.
func pushStatus(res *transport.Result) string {
	if !res.Failed() {
		return "written"
	}
	reason := strings.TrimSpace(lastNonEmpty(res.Stderr))
	if reason == "" && res.Err != nil {
		reason = res.Err.Error()
	}
	if reason == "" {
		reason = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return "failed: " + reason
}

// pushError is the error of a failed write, which keeps what the transport
// said, so that an unreachable node and an interrupt keep their exit code.
func pushError(res *transport.Result) error {
	if res.Err != nil {
		return res.Err
	}
	return fmt.Errorf("%s: exited %d", res.Target.Name, res.ExitCode)
}

// nodeUnreachable reports whether a node that failed was never there to say no.
func nodeUnreachable(err error) bool {
	return exitcode.From(err) == exitcode.Transport || errors.Is(err, context.Canceled)
}

// pushFailures names the nodes a push failed on. It exits 3 when none of
// them could be reached and 1 when one of them answered, and keeps the
// first failure wrapped, an interrupt before anything else, so that it
// still reads as one.
func pushFailures(targets []transport.Target, failed map[string]error) error {
	if len(failed) == 0 {
		return nil
	}
	names := nodeset.New()
	code := exitcode.Transport
	var cause error
	for _, tg := range targets {
		err, ok := failed[tg.Name]
		if !ok {
			continue
		}
		_ = names.Add(tg.Name)
		if !nodeUnreachable(err) {
			code = exitcode.TargetFailed
		}
		if cause == nil || (errors.Is(err, context.Canceled) && !errors.Is(cause, context.Canceled)) {
			cause = err
		}
	}
	return exitcode.Wrap(code, fmt.Errorf("%d of %d nodes failed: %s: %w", len(failed), len(targets), names, cause))
}

func newSecretsCheckCommand(r *root) *cobra.Command {
	var decrypt bool
	cmd := leaf("check", "Check the sops encrypted Secret documents", `
List every Secret document with its keys, the master keys sops encrypted it
to and how many references use it. None of this needs a key: the names, the
keys and the sops metadata are readable, and a reference to a key that does
not exist is already refused when the configuration loads.

With --decrypt each document is also decrypted, in memory, and the result
is thrown away. It proves this workstation can read every secret before a
reinstall needs one, and prints nothing of the plaintext.

  clusterctl secrets check --decrypt`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			used := map[string]int{}
			for _, ref := range secretRefsOf(a.Spec) {
				used[ref.Name]++
			}

			t := output.NewTable(output.Cols("SECRET", "FILE", "KEYS", "ENCRYPTED TO", "USED", "STATUS")...)
			objects := []map[string]any{}
			failed := 0
			bundle := a.Resolved.Bundle
			names := make([]string, 0, len(bundle.Secrets))
			for name := range bundle.Secrets {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				doc := bundle.Secrets[name]
				keys := config.SecretKeys(doc)
				status := "ok"
				var info secrets.SopsInfo
				raw, err := os.ReadFile(doc.File)
				if err == nil {
					info, err = secrets.InspectSops(raw)
				}
				if err == nil {
					err = info.CheckKeyTypes(a.Spec.Workstation.SopsKeyTypes)
				}
				if err == nil && decrypt {
					status = "decrypts"
					_, err = a.SecretValues(name)
				}
				if err != nil {
					status = "fails: " + err.Error()
					failed++
				}
				t.Add(name, doc.File, strconv.Itoa(len(keys)), info.Summary(), strconv.Itoa(used[name]), status)
				recipients := make([]string, 0, len(info.Keys))
				for _, k := range info.Keys {
					recipients = append(recipients, k.Type+":"+k.ID)
				}
				objects = append(objects, map[string]any{
					"name": name, "file": doc.File, "keys": keys,
					"encryptedTo": recipients, "used": used[name], "status": status,
				})
			}
			t.Caption = fmt.Sprintf("%d Secret documents", t.Len())
			if err := a.Print(output.Result{Table: t, Object: objects}); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d of %d Secret documents failed the check", failed, t.Len())
			}
			return nil
		})
	cmd.Flags().BoolVar(&decrypt, "decrypt", false, "also decrypt each document in memory")
	return cmd
}

// secretRefsOf lists the secretRefs of the merged configuration.
func secretRefsOf(spec v1alpha1.EffectiveSpec) []v1alpha1.SecretKeyRef {
	var out []v1alpha1.SecretKeyRef
	for _, c := range spec.Credentials {
		if c.Password.SecretRef != nil {
			out = append(out, *c.Password.SecretRef)
		}
	}
	for _, f := range spec.Services.Cinc.Secrets {
		if f.SecretRef != nil {
			out = append(out, *f.SecretRef)
		}
	}
	return out
}

func newCincCommand(r *root) *cobra.Command {
	return group("cinc", "Drive the configuration management on the nodes", `
Point a node at a configuration archive and run the configuration management
client on it.`,
		newCincConfigCommand(r),
		newCincRunCommand(r),
		newCincShowCommand(r),
		newCincShellCommand(r),
	)
}

func cincConfigPath(a *app.App) string {
	if p := a.Spec.Services.Cinc.SoloConfigPath; p != "" {
		return p
	}
	return "/etc/cinc/solo"
}

func newCincConfigCommand(r *root) *cobra.Command {
	var runList string

	cmd := leaf("config URL [NODESET]", "Point a node set at a configuration archive", `
Write the archive URL, and optionally the run list, into the configuration
file the client reads on each node.

  clusterctl cinc config http://installer/cinc/latest.tgz -n exe[1-4]`,
		cobra.MinimumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			ns, err := selection(a, args[1:])
			if err != nil {
				return err
			}
			path := cincConfigPath(a)
			detail := args[0]
			if runList != "" {
				detail += " with run list " + runList
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb: "set the configuration source of", Targets: ns, Detail: detail,
			}); err != nil {
				return dryRunOrError(err)
			}

			body := fmt.Sprintf("CHEF_RECIPE_URL=%s\n", args[0])
			if runList != "" {
				body += fmt.Sprintf("CHEF_RUN_LIST=%s\n", runList)
			}
			script := fmt.Sprintf("set -eu\ninstall -D -m 0644 /dev/null %s\ncat > %s\n",
				shellquote.Quote(path), shellquote.Quote(path))

			targets, err := a.NodeTargets(ns)
			if err != nil {
				return err
			}
			results := a.Executor().RunEach(a.Context(), targets, func(transport.Target) transport.Request {
				return transport.Request{
					Script:  script,
					Stdin:   strings.NewReader(body),
					Timeout: a.Timeout().Get(),
					TTY:     transport.TTYNone,
				}
			})
			if err := a.Print(output.Result{Table: resultsTable(results), Object: results}); err != nil {
				return err
			}
			return failureError(results)
		})
	cmd.Flags().StringVarP(&runList, "run-list", "R", "", "run list to write alongside the archive URL")
	return cmd
}

func newCincShowCommand(r *root) *cobra.Command {
	return leaf("show [NODESET]", "Show what each node is configured from", `
Print the configuration source and run list each node is set to use.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			path := cincConfigPath(a)
			results, err := runOnNodes(a, ns, func(string) transport.Request {
				return transport.Request{
					Argv:    []string{"cat", path},
					Timeout: a.Timeout().Get(),
					TTY:     transport.TTYNone,
				}
			})
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("NODE", "ARCHIVE", "RUN LIST")...)
			for _, res := range results {
				if res.Failed() {
					t.Add(res.Target.Name, "not configured", "")
					continue
				}
				url, runList := "", ""
				for _, line := range res.Lines() {
					key, value, ok := strings.Cut(line, "=")
					if !ok {
						continue
					}
					switch strings.TrimSpace(key) {
					case "CHEF_RECIPE_URL":
						url = value
					case "CHEF_RUN_LIST":
						runList = value
					}
				}
				t.Add(res.Target.Name, url, runList)
			}
			return a.Print(output.Result{Table: t})
		})
}

func newCincRunCommand(r *root) *cobra.Command {
	var runList string

	cmd := leaf("run [NODESET]", "Run the configuration management client on a node set", `
Run the configuration management client on each node, using the archive and
run list it is configured with.

This changes the nodes, so it asks first.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			binary := a.Spec.Services.Cinc.Binary
			if binary == "" {
				binary = "cinc-solo"
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb: "run the configuration management on", Targets: ns, Detail: binary,
			}); err != nil {
				return dryRunOrError(err)
			}

			override := ""
			if runList != "" {
				override = " --override-runlist " + shellquote.Quote(runList)
			}
			script := fmt.Sprintf(
				"set -eu\n. %s\nexec %s --minimal-ohai --recipe-url \"$CHEF_RECIPE_URL\"%s\n",
				shellquote.Quote(cincConfigPath(a)), shellquote.Quote(binary), override)
			if runList == "" {
				script = fmt.Sprintf(
					"set -eu\n. %s\nexec %s --minimal-ohai --recipe-url \"$CHEF_RECIPE_URL\" --override-runlist \"$CHEF_RUN_LIST\"\n",
					shellquote.Quote(cincConfigPath(a)), shellquote.Quote(binary))
			}

			results, err := runOnNodes(a, ns, func(string) transport.Request {
				return transport.Request{
					Script:  script,
					Timeout: a.Timeout().Or(30 * time.Minute),
					TTY:     transport.TTYNone,
				}
			})
			if err != nil {
				return err
			}
			if err := a.Print(output.Result{Table: resultsTable(results), Object: results}); err != nil {
				return err
			}
			return failureError(results)
		})
	cmd.Flags().StringVarP(&runList, "run-list", "R", "", "run list to use for this run only")
	return cmd
}

func newCincShellCommand(r *root) *cobra.Command {
	return leaf("shell [-- COMMAND...]", "Open a shell on the archive host", `
Log in to the host that publishes the configuration archives.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			return roleShell(r, cmd, args, func(a *app.App) string { return a.Spec.Services.HTTP.Role })
		})
}

func newProvisionCommand(r *root) *cobra.Command {
	return group("provision", "Reinstall nodes end to end", `
Run the whole reinstallation of a node set: forget its host keys, point the
PXE service at the installation, tell the machine to boot from the network
once, and reset it.

Each step is the command of the same name, so anything that goes wrong can be
picked up and repeated by hand.`,
		newProvisionReinstallCommand(r),
		newProvisionStatusCommand(r),
	)
}

func newProvisionReinstallCommand(r *root) *cobra.Command {
	var (
		bootPath string
		keepKeys bool
		noReset  bool
	)

	cmd := leaf("reinstall [NODESET]", "Reinstall a node set from the network", `
Reinstall nodes: remove their host keys, configure the network boot, set the
machines to boot from the network once, and reset them.

This destroys everything on the nodes. It previews what it will do, refuses
protected hosts, and above the configured host count asks for the count to be
typed back.

  clusterctl provision reinstall -n exe0001
  clusterctl provision reinstall -n @rack:R02 --dry-run`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}

			// Everything that can be resolved is resolved before the first
			// change, so a set with one unknown node stops here rather than
			// halfway through.
			plans := map[string]string{}
			for _, node := range ns.Expand() {
				path := bootPath
				if path == "" {
					resolved, _, err := inventory.BootPath(a.Inventory, a.Spec.BootPaths, node)
					if err != nil {
						return exitcode.Wrap(exitcode.Usage, err)
					}
					path = resolved
				}
				plans[node] = path
				if _, err := nodeAddress(a, node); err != nil {
					return err
				}
			}

			if err := a.Gate.Confirm(safety.Action{
				Verb:    "reinstall",
				Targets: ns,
				Detail:  "everything on these machines is lost",
			}); err != nil {
				return dryRunOrError(err)
			}

			steps := output.NewTable(output.Cols("STEP", "RESULT")...)
			record := func(step string, err error) error {
				if err != nil {
					steps.Add(step, "failed: "+err.Error())
					_ = a.Print(output.Result{Table: steps})
					return err
				}
				steps.Add(step, "ok")
				return nil
			}

			if !keepKeys {
				path := a.Path(a.Spec.SSH.KnownHostsFile)
				err := hostkeys.Modify(a.Context(), path, func(f *hostkeys.File) error {
					for _, node := range ns.Expand() {
						host, err := a.Namer.FQDN(node)
						if err != nil {
							return err
						}
						f.Remove(host)
						f.Remove(node)
					}
					return nil
				})
				if err := record("forget the host keys", err); err != nil {
					return err
				}
			}

			if err := record("configure the network boot", setBootPaths(a, plans)); err != nil {
				return err
			}

			bootErr := forEachBMCError(a, ns, func(node string, c *redfish.Client) error {
				return c.SetBootOverride(a.Context(), "Pxe", false)
			})
			if err := record("boot from the network once", bootErr); err != nil {
				return err
			}

			if !noReset {
				resetErr := forEachBMCError(a, ns, func(node string, c *redfish.Client) error {
					return c.Reset(a.Context(), redfish.ResetForceRestart)
				})
				if err := record("reset the machines", resetErr); err != nil {
					return err
				}
			}

			steps.Caption = fmt.Sprintf("%s is reinstalling; follow it with \"clusterctl boot log\"", ns)
			return a.Print(output.Result{Table: steps})
		})

	cmd.Flags().StringVar(&bootPath, "boot-path", "", "boot configuration to install from (default: from the cluster rules)")
	cmd.Flags().BoolVar(&keepKeys, "keep-host-keys", false, "leave the host key file alone")
	cmd.Flags().BoolVar(&noReset, "no-reset", false, "configure everything but do not reset the machines")
	return cmd
}

// setBootPaths writes the boot path links for a whole set in one call.
func setBootPaths(a *app.App, plans map[string]string) error {
	role, err := pxeRole(a)
	if err != nil {
		return err
	}
	root := a.Spec.Services.PXESrv.Root
	if root == "" {
		root = "/srv/pxesrv"
	}
	var script strings.Builder
	script.WriteString("set -eu\n")
	for node, path := range plans {
		address, err := nodeAddress(a, node)
		if err != nil {
			return err
		}
		fmt.Fprintf(&script, "ln -sfn %s %s\n",
			shellquote.Quote(path), shellquote.Quote(filepath.Join(root, address)))
	}
	_, err = a.RunOnRole(a.Context(), role, transport.Request{
		Script:  script.String(),
		Timeout: a.Timeout().Get(),
		TTY:     transport.TTYNone,
	})
	return err
}

// forEachBMCError runs an action against every service processor and reports
// the first failure, having tried all of them.
func forEachBMCError(a *app.App, ns *nodeset.NodeSet, do func(string, *redfish.Client) error) error {
	var failures []string
	for _, node := range ns.Expand() {
		client, err := a.RedfishClient(a.Context(), node)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", node, err))
			continue
		}
		if err := do(node, client); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", node, err))
		}
	}
	if len(failures) > 0 {
		return exitcode.Errorf(exitcode.TargetFailed, "%s", strings.Join(failures, "; "))
	}
	return nil
}

func newProvisionStatusCommand(r *root) *cobra.Command {
	return leaf("status [NODESET]", "Show where a reinstallation stands", `
Show, for each node, the boot path it is configured with, what its service
processor reports and whether it answers over ssh yet.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			results, err := runOnNodes(a, ns, func(string) transport.Request {
				return transport.Request{
					Argv:    []string{"uptime", "-p"},
					Timeout: 30 * time.Second,
					TTY:     transport.TTYNone,
				}
			})
			if err != nil {
				return err
			}
			power := forEachBMC(a, ns, func(ctx context.Context, _ string, c *redfish.Client) (string, error) {
				return c.PowerState(ctx)
			})
			states := map[string]string{}
			for _, p := range power {
				if p.Error != "" {
					states[p.Node] = "unknown"
					continue
				}
				states[p.Node] = p.State
			}

			t := output.NewTable(output.Cols("NODE", "POWER", "SSH", "UPTIME")...)
			for _, res := range results {
				reachable := "no"
				uptime := ""
				if !res.Failed() {
					reachable = "yes"
					uptime = res.Output()
				}
				t.Add(res.Target.Name, states[res.Target.Name], reachable, uptime)
			}
			return a.Print(output.Result{Table: t})
		})
}
