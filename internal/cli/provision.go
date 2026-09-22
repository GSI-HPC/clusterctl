// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
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
Decrypt the files a site keeps beside its configuration and write them onto
the nodes, and encrypt the values the configuration carries inline.

The plaintext never touches this workstation's disk: it is decrypted into
memory and streamed to each node over standard input, which is what keeps a
cluster key off a laptop.`,
		newSecretsListCommand(r),
		newSecretsPushCommand(r),
		newSecretsEncryptCommand(r),
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
				if s.Age != "" {
					source = "(inline)"
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

			identities, err := a.Identities()
			if err != nil {
				return err
			}
			targets, err := a.NodeTargets(ns)
			if err != nil {
				return err
			}

			t := output.NewTable(output.Cols("NODE", "SECRET", "STATUS")...)
			failed := 0
			for _, file := range files {
				plaintext, err := a.SecretContent(file, identities)
				if err != nil {
					return err
				}
				mode := file.Mode
				if mode == "" {
					mode = "0600"
				}
				// The payload arrives on stdin, so it is never an argument
				// and never a file on this machine.
				script := fmt.Sprintf(
					"set -eu\numask 077\ninstall -D -m %s /dev/null %s\ncat > %s\n",
					shellquote.Quote(mode), shellquote.Quote(file.Target), shellquote.Quote(file.Target))
				if file.Owner != "" {
					owner := file.Owner
					if file.Group != "" {
						owner += ":" + file.Group
					}
					script += fmt.Sprintf("chown %s %s\n", shellquote.Quote(owner), shellquote.Quote(file.Target))
				}

				results := a.Executor().RunEach(a.Context(), targets, func(transport.Target) transport.Request {
					return transport.Request{
						Script:  script,
						Stdin:   strings.NewReader(string(plaintext)),
						Timeout: a.Timeout().Get(),
						TTY:     transport.TTYNone,
					}
				})
				for _, res := range results {
					status := "written"
					if res.Failed() {
						status = "failed: " + strings.TrimSpace(lastNonEmpty(res.Stderr))
						failed++
					}
					t.Add(res.Target.Name, file.Target, status)
				}
			}
			if err := a.Print(output.Result{Table: t}); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d writes failed", failed)
			}
			return nil
		})
}

func newSecretsEncryptCommand(r *root) *cobra.Command {
	var (
		file       string
		recipients []string
		armorOnly  bool
		indent     int
	)
	cmd := leaf("encrypt", "Encrypt a value to write inline in the configuration", `
Encrypt a password or a file to the site's recipients and print it as an age
field, ready to be pasted into a document:

  $ clusterctl secrets encrypt
  Secret: 
  Again: 
  age: |
    -----BEGIN AGE ENCRYPTED FILE-----
    ...
    -----END AGE ENCRYPTED FILE-----

On a terminal the value is asked for twice without echo. Otherwise it is read
from standard input as it is, so a trailing newline is kept; a password
resolved from an age field has its trailing newline removed either way.

The recipients are secrets.recipients of the site document, or the
--recipient flags when given. Nothing is decrypted, so no identity is needed.

  clusterctl secrets encrypt --file munge.key --indent 10 >> site.yaml`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			if indent < 0 {
				return exitcode.Errorf(exitcode.Usage, "--indent must not be negative")
			}
			if len(recipients) == 0 {
				recipients = a.Spec.Secrets.Recipients
			}
			recs, err := secrets.ParseRecipients(recipients)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}

			var plaintext []byte
			if file != "" {
				if plaintext, err = os.ReadFile(file); err != nil {
					return exitcode.Wrap(exitcode.Usage, err)
				}
			} else if plaintext, err = a.ReadSecret("Secret"); err != nil {
				return err
			}
			if len(plaintext) == 0 {
				return exitcode.Errorf(exitcode.Usage, "there is nothing to encrypt")
			}

			armored, err := secrets.EncryptArmored(plaintext, recs)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			return say(cmd, "%s", ageField(armored, indent, armorOnly))
		})
	cmd.Flags().StringVar(&file, "file", "", "encrypt this file instead of reading the value")
	cmd.Flags().StringArrayVar(&recipients, "recipient", nil, "encrypt to this age or ssh public key instead of secrets.recipients (repeatable)")
	cmd.Flags().BoolVar(&armorOnly, "armor", false, "print the armored ciphertext alone, without the age key")
	cmd.Flags().IntVar(&indent, "indent", 0, "indent the output by this many spaces, to paste it at its depth")
	return cmd
}

// ageField renders armored ciphertext as an age field of a document, or as
// the bare armor, indented by the given number of spaces.
func ageField(armored string, indent int, armorOnly bool) string {
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	body := pad
	if !armorOnly {
		b.WriteString(pad + "age: |\n")
		body += "  "
	}
	for _, line := range strings.Split(strings.TrimRight(armored, "\n"), "\n") {
		b.WriteString(body + line + "\n")
	}
	return b.String()
}

// sealed is one encrypted value of the configuration.
type sealed struct {
	name   string
	form   string
	inline string
	file   string
}

// sealedValues lists every encrypted value the configuration carries, in a
// stable order: the credentials by name, then the secret files.
func sealedValues(a *app.App) []sealed {
	var out []sealed
	names := make([]string, 0, len(a.Spec.Credentials))
	for name := range a.Spec.Credentials {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pw := a.Spec.Credentials[name].Password
		switch {
		case pw.Age != "":
			out = append(out, sealed{name: "credential " + name, form: "inline", inline: pw.Age})
		case pw.AgeFile != "":
			out = append(out, sealed{name: "credential " + name, form: a.Path(pw.AgeFile), file: a.Path(pw.AgeFile)})
		}
	}
	for _, f := range a.Spec.Services.Cinc.Secrets {
		switch {
		case f.Age != "":
			out = append(out, sealed{name: "file " + f.Target, form: "inline", inline: f.Age})
		case f.Source != "":
			out = append(out, sealed{name: "file " + f.Target, form: a.Path(f.Source), file: a.Path(f.Source)})
		}
	}
	return out
}

func newSecretsCheckCommand(r *root) *cobra.Command {
	var decrypt bool
	cmd := leaf("check", "Check every encrypted value of the configuration", `
List every encrypted value the configuration carries, inline or in a file,
check that it is a complete age file and name the kinds of recipient it is
encrypted to. No identity is needed for this.

With --decrypt each value is also decrypted with the configured identities,
in memory, and the result is thrown away: it proves this workstation can
read every secret before a reinstall needs one. Nothing is printed of the
plaintext.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			var identities []age.Identity
			if decrypt {
				if identities, err = a.Identities(); err != nil {
					return err
				}
			}

			t := output.NewTable(output.Cols("SECRET", "SOURCE", "RECIPIENTS", "STATUS")...)
			failed := 0
			for _, s := range sealedValues(a) {
				var stanzas []string
				if s.inline != "" {
					stanzas, err = secrets.Inspect(s.inline)
				} else {
					stanzas, err = secrets.InspectFile(s.file)
				}
				status := "ok"
				switch {
				case err != nil:
					status = "invalid: " + err.Error()
				case decrypt:
					if s.inline != "" {
						_, err = secrets.DecryptArmored(s.inline, identities)
					} else {
						_, err = secrets.Decrypt(s.file, identities)
					}
					status = "decrypts"
					if err != nil {
						status = "fails: " + err.Error()
					}
				}
				if err != nil {
					failed++
				}
				t.Add(s.name, s.form, summarizeStanzas(stanzas), status)
			}
			t.Caption = fmt.Sprintf("%d encrypted values", t.Len())
			if err := a.Print(output.Result{Table: t}); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d of %d encrypted values failed the check", failed, t.Len())
			}
			return nil
		})
	cmd.Flags().BoolVar(&decrypt, "decrypt", false, "also decrypt each value in memory with the configured identities")
	return cmd
}

// summarizeStanzas renders the recipient stanza types of an age file as a
// count per type, for example "2 X25519, 1 ssh-ed25519".
func summarizeStanzas(types []string) string {
	if len(types) == 0 {
		return "-"
	}
	counts := map[string]int{}
	var order []string
	for _, t := range types {
		if counts[t] == 0 {
			order = append(order, t)
		}
		counts[t]++
	}
	parts := make([]string, 0, len(order))
	for _, t := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[t], t))
	}
	return strings.Join(parts, ", ")
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
