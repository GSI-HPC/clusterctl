// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/hostkeys"
	"github.com/GSI-HPC/clusterctl/internal/ipmi"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/progress"
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
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
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
		}))
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
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
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
				if contents[i], err = a.SecretContent(a.Context(), file); err != nil {
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
				return err
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
					return a.Collect(transport.Request{Script: script, Stdin: bytes.NewReader(contents[i])})
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
		}))
}

// secretScript writes the payload on standard input to a temporary file
// beside the target, checks that all of it arrived and only then moves it
// into place, so that a connection lost half way leaves the old file, not
// an empty one. A missing directory is created readable by root alone; one
// that exists is left as it is. The payload is never an argument and never
// a file on this machine.
func secretScript(file v1alpha1.SecretFile, size int) string {
	mode := cmp.Or(file.Mode, "0600")
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

// pushFailures names the nodes a push failed on, with the code
// exitcode.Worst gives, and keeps the first failure wrapped, an interrupt
// before anything else, so that it still reads as one.
func pushFailures(targets []transport.Target, failed map[string]error) error {
	if len(failed) == 0 {
		return nil
	}
	names := nodeset.New()
	var errs []error
	var cause error
	for _, tg := range targets {
		err, ok := failed[tg.Name]
		if !ok {
			continue
		}
		_ = names.Add(tg.Name)
		errs = append(errs, err)
		if cause == nil || (errors.Is(err, context.Canceled) && !errors.Is(cause, context.Canceled)) {
			cause = err
		}
	}
	return exitcode.Wrap(exitcode.Worst(errs...), fmt.Errorf("%d of %d nodes failed: %s: %w", len(failed), len(targets), names, cause))
}

func newSecretsCheckCommand(r *root) *cobra.Command {
	var decrypt bool
	cmd := leaf("check", "Check the sops encrypted Secret documents", `
List every Secret document with its keys, the master keys sops encrypted it
to and how many references use it. None of this needs a key or sops: the
names, the keys and the sops metadata are readable, and a reference to a key
that does not exist is already refused when the configuration loads.

With --decrypt each document is also decrypted by the sops command, into
memory, and the result is thrown away. It proves this workstation can read
every secret before a reinstall needs one, and prints nothing of the
plaintext. The caption of the table names the sops that decrypts.

  clusterctl secrets check --decrypt`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
			used := map[string]int{}
			for _, ref := range secretRefsOf(a.Spec) {
				used[ref.Name]++
			}

			t := output.NewTable(output.Cols("SECRET", "FILE", "KEYS", "ENCRYPTED TO", "USED", "STATUS")...)
			objects := []map[string]any{}
			failed := 0
			bundle := a.Resolved.Bundle
			for _, name := range slices.Sorted(maps.Keys(bundle.Secrets)) {
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
					_, err = a.SecretValues(a.Context(), name)
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
			// Which sops decrypts, and whether it can: it is not needed to
			// list the documents, so only --decrypt fails without it.
			t.Caption = fmt.Sprintf("%d Secret documents", t.Len())
			if t.Len() > 0 {
				if found, err := a.Sops().Find(a.Context()); err == nil {
					t.Caption += ", decrypted with sops " + found.Version + " at " + found.Path
				} else {
					t.Caption += "; " + err.Error()
				}
			}
			if err := a.Print(output.Result{Table: t, Object: objects}); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d of %d Secret documents failed the check", failed, t.Len())
			}
			return nil
		}))
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

// The configuration file on a node holds shell assignments, the format the
// shell toolkit sourced. clusterctl writes every value quoted, so the file
// stays safe for anything that still sources it, and reads it back without
// a shell, so nothing in it is ever run as root.
const (
	cincURLKey     = "CHEF_RECIPE_URL"
	cincRunListKey = "CHEF_RUN_LIST"
)

// cincRunTimeout is how long the client may run on a node when
// fanout.commandTimeout does not allow longer. Converging a freshly
// installed node can take far longer than the commands that setting is
// meant for, whose default is ten minutes.
const cincRunTimeout = 30 * time.Minute

// cincSolo is what the configuration file of a node names.
type cincSolo struct {
	URL     string
	RunList string
}

// render writes the file, one quoted assignment per value. A run list is
// written only when there is one, and the file replaces the old one whole.
func (c cincSolo) render() string {
	body := cincURLKey + "=" + shellquote.Quote(c.URL) + "\n"
	if c.RunList != "" {
		body += cincRunListKey + "=" + shellquote.Quote(c.RunList) + "\n"
	}
	return body
}

// check reports whether the client can be run with what the file names.
func (c cincSolo) check() error {
	if c.URL == "" {
		return fmt.Errorf("%s is not set", cincURLKey)
	}
	if err := checkCincURL(c.URL); err != nil {
		return err
	}
	return checkCincRunList(c.RunList)
}

// checkCincURL accepts the archive locations the client can fetch: an http
// or https URL with a host and nothing a line or a word would end at.
func checkCincURL(s string) error {
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return fmt.Errorf("archive URL %q contains whitespace or a control character", s)
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("archive URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("archive URL %q is not an http or https URL", s)
	}
	if u.Host == "" {
		return fmt.Errorf("archive URL %q names no host", s)
	}
	return nil
}

// checkCincRunList refuses a run list that would end the line it is written
// on, or carry anything else a terminal would act on.
func checkCincRunList(s string) error {
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return fmt.Errorf("run list %q contains a control character", s)
	}
	return nil
}

// parseCincSolo reads a configuration file without a shell. Every line is a
// comment or an assignment, and the two values clusterctl uses must be
// literal words: quoted, escaped or plain, but never anything a shell would
// expand or run. A file that sourcing would make do more than assign is
// refused rather than guessed at.
func parseCincSolo(text string) (cincSolo, error) {
	var c cincSolo
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok || !isShellName(name) {
			return cincSolo{}, fmt.Errorf("line %d is not an assignment: %q", n+1, line)
		}
		var target *string
		switch name {
		case cincURLKey:
			target = &c.URL
		case cincRunListKey:
			target = &c.RunList
		default:
			continue
		}
		word, err := shellLiteral(value)
		if err != nil {
			return cincSolo{}, fmt.Errorf("line %d, %s: %w", n+1, name, err)
		}
		*target = word
	}
	return c, nil
}

func isShellName(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return s != ""
}

// shellLiteral decodes the value of an assignment the way a shell would,
// provided the shell would do nothing but take it literally. It is the
// reverse of shellquote.Quote, and it also reads the unquoted values the
// shell toolkit wrote.
func shellLiteral(s string) (string, error) {
	var b strings.Builder
	i := 0
word:
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c < 0x20 && c != '\t', c == 0x7f:
			return "", fmt.Errorf("contains the control character %q", c)
		case c == ' ', c == '\t':
			break word
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return "", errors.New("has an unterminated ' quote")
			}
			b.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			end := strings.IndexByte(s[i+1:], '"')
			if end < 0 {
				return "", errors.New(`has an unterminated " quote`)
			}
			quoted := s[i+1 : i+1+end]
			if j := strings.IndexAny(quoted, "$`\\"); j >= 0 {
				return "", fmt.Errorf("has %q inside double quotes, which the shell would expand", quoted[j])
			}
			b.WriteString(quoted)
			i += end + 1
		case c == '\\':
			if i+1 == len(s) {
				return "", errors.New("ends in a backslash")
			}
			i++
			b.WriteByte(s[i])
		case strings.IndexByte("$`;&|<>()~", c) >= 0:
			return "", fmt.Errorf("has an unquoted %q, which the shell would interpret", c)
		default:
			b.WriteByte(c)
		}
	}
	if rest := strings.TrimLeft(s[i:], " \t"); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("is followed by %q, which the shell would run as a command", rest)
	}
	return b.String(), nil
}

// cincReadScript prints the configuration file of a node, or nothing when
// there is none, so that a node without one is told apart from a node that
// could not be read.
func cincReadScript(file string) string {
	q := shellquote.Quote(file)
	return fmt.Sprintf("if [ -e %s ]; then exec cat -- %s; fi\n", q, q)
}

// cincWriteScript replaces file with the size bytes that arrive on standard
// input. They go to a temporary file beside it first and are moved into
// place only once all of them are there, so a connection that drops halfway
// leaves the old file rather than an empty or truncated one. Missing
// directories are made for a world readable file, whatever the umask of the
// remote user.
func cincWriteScript(file string, size int) string {
	dir := filepath.Dir(file)
	return fmt.Sprintf(`set -eu
umask 022
mkdir -p -- %[1]s
tmp=$(mktemp %[2]s)
trap 'rm -f -- "$tmp"' EXIT
cat > "$tmp"
size=$(($(wc -c < "$tmp")))
if [ "$size" -ne %[4]d ]; then
	echo "received $size of %[4]d bytes; %[3]s is unchanged" >&2
	exit 1
fi
chmod 0644 "$tmp"
mv -f -- "$tmp" %[3]s
trap - EXIT
`,
		shellquote.Quote(dir),
		shellquote.Quote(filepath.Join(dir, "."+filepath.Base(file)+".XXXXXX")),
		shellquote.Quote(file), size)
}

// cincDetail says why a node failed, with anything the node sent escaped.
func cincDetail(res *transport.Result) string {
	if line := strings.TrimSpace(lastNonEmpty(res.Stderr)); line != "" {
		return output.EscapeCell(line)
	}
	if res.Err != nil {
		return output.EscapeCell(res.Err.Error())
	}
	return fmt.Sprintf("exit %d", res.ExitCode)
}

// cincRefused stands in for a node on which nothing was run because what it
// answered cannot be used.
func cincRefused(target transport.Target, err error) *transport.Result {
	return &transport.Result{Target: target, ExitCode: -1, Err: err}
}

func newCincConfigCommand(r *root) *cobra.Command {
	var runList string

	cmd := leaf("config URL [NODESET]", "Point a node set at a configuration archive", `
Write the archive URL, and optionally the run list, into the configuration
file the client reads on each node. The file is replaced whole, so leaving
out --run-list removes a run list written before, and cinc run then uses the
run list of the archive.

The URL must be an http or https URL. Each value is written quoted, and the
new file is moved into place only once it has arrived complete.

  clusterctl cinc config http://installer/cinc/latest.tgz -n exe[1-4]`,
		cobra.MinimumNArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			solo := cincSolo{URL: args[0], RunList: runList}
			if err := solo.check(); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			ns, err := selection(a, args[1:])
			if err != nil {
				return err
			}
			path := a.Spec.Services.Cinc.SoloConfigPath
			detail := solo.URL
			if solo.RunList != "" {
				detail += " with run list " + solo.RunList
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb: "set the configuration source of", Targets: ns, Detail: detail,
			}); err != nil {
				return err
			}

			body := solo.render()
			script := cincWriteScript(path, len(body))

			targets, err := a.NodeTargets(ns)
			if err != nil {
				return err
			}
			results := a.Executor().RunEach(a.Context(), targets, func(transport.Target) transport.Request {
				return a.Collect(transport.Request{Script: script, Stdin: strings.NewReader(body)})
			})
			if err := a.Print(output.Result{Table: resultsTable(results), Object: results}); err != nil {
				return err
			}
			return failureError(results)
		}))
	cmd.Flags().StringVarP(&runList, "run-list", "R", "", "run list to write alongside the archive URL")
	return cmd
}

// cincShowRow is what cinc show reports for one node.
type cincShowRow struct {
	Node       string `json:"node" yaml:"node"`
	Configured bool   `json:"configured" yaml:"configured"`
	Archive    string `json:"archive,omitempty" yaml:"archive,omitempty"`
	RunList    string `json:"runList,omitempty" yaml:"runList,omitempty"`
	Error      string `json:"error,omitempty" yaml:"error,omitempty"`
}

func newCincShowCommand(r *root) *cobra.Command {
	return leaf("show [NODESET]", "Show what each node is configured from", `
Print the configuration source and run list each node is set to use.

A node without the file is shown as not configured. A node that could not be
read, or whose file holds anything but plain assignments, is shown as failed
and makes the command fail, with exit code 3 when a node could not be
reached.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			path := a.Spec.Services.Cinc.SoloConfigPath
			results, err := runOnNodes(a.Context(), a, ns, transport.Request{
				Script: cincReadScript(path),
			})
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("NODE", "ARCHIVE", "RUN LIST")...)
			rows := make([]cincShowRow, 0, len(results))
			outcomes := make([]*transport.Result, 0, len(results))
			for _, res := range results {
				row := cincShowRow{Node: res.Target.Name}
				outcome := res
				switch {
				case res.Failed():
					row.Error = cincDetail(res)
				case res.Stdout == "":
				default:
					solo, err := parseCincSolo(res.Stdout)
					if err == nil {
						err = solo.check()
					}
					if err != nil {
						row.Error = fmt.Sprintf("%s: %v", path, err)
						outcome = cincRefused(res.Target, err)
						break
					}
					row.Configured, row.Archive, row.RunList = true, solo.URL, solo.RunList
				}
				switch {
				case row.Error != "":
					t.Add(row.Node, "failed: "+row.Error, "")
				case !row.Configured:
					t.Add(row.Node, "not configured", "")
				default:
					t.Add(row.Node, row.Archive, row.RunList)
				}
				rows = append(rows, row)
				outcomes = append(outcomes, outcome)
			}
			if err := a.Print(output.Result{Table: t, Object: rows}); err != nil {
				return err
			}
			return failureError(outcomes)
		}))
}

func newCincRunCommand(r *root) *cobra.Command {
	var runList string

	cmd := leaf("run [NODESET]", "Run the configuration management client on a node set", `
Run the configuration management client on each node, using the archive and
run list it is configured with. Without a run list, in the file or given
here, the client uses the run list of the archive.

The configuration file is read, never sourced: a node whose file holds
anything but plain assignments, or names no http or https archive, is refused
and the client is not started there.

The client may run for 30 minutes on each node, or for fanout.commandTimeout
when that is longer, since converging a freshly installed node takes longer
than most commands.

This changes the nodes, so it asks first.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			if err := checkCincRunList(runList); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			binary := a.Spec.Services.Cinc.Binary
			path := a.Spec.Services.Cinc.SoloConfigPath
			// The archive and run list come from each node's file, which is
			// read only once the run is confirmed, so a dry run cannot show
			// them and says so.
			detail := binary
			if a.DryRun() {
				detail += fmt.Sprintf(", with the archive and run list from each node's %s, which a dry run does not read", path)
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb: "run the configuration management on", Targets: ns, Detail: detail,
			}); err != nil {
				return err
			}

			reads, err := runOnNodes(a.Context(), a, ns, transport.Request{
				Script: cincReadScript(path),
			})
			if err != nil {
				return err
			}

			// Each node is handed what its own file names as arguments, so
			// no value in the file reaches a shell.
			results := make([]*transport.Result, len(reads))
			argv := map[string][]string{}
			index := map[string]int{}
			var ready []transport.Target
			for i, res := range reads {
				if res.Failed() {
					results[i] = res
					a.Printf("%s: not run: reading %s: %s\n", res.Target.Name, path, cincDetail(res))
					continue
				}
				solo, err := parseCincSolo(res.Stdout)
				if err == nil && res.Stdout == "" {
					err = errors.New("the file does not exist; point the node at an archive with cinc config")
				}
				if err == nil {
					err = solo.check()
				}
				if err != nil {
					results[i] = cincRefused(res.Target, fmt.Errorf("%s: %w", path, err))
					a.Printf("%s: not run: %s: %v\n", res.Target.Name, path, err)
					continue
				}
				if runList != "" {
					solo.RunList = runList
				}
				command := []string{binary, "--minimal-ohai", "--recipe-url", solo.URL}
				if solo.RunList != "" {
					command = append(command, "--override-runlist", solo.RunList)
				}
				argv[res.Target.Name] = command
				index[res.Target.Name] = i
				ready = append(ready, res.Target)
			}
			// What the run prints is what it is watched for, so a display
			// may show it as it arrives.
			executor := a.Executor()
			executor.Flags = progress.ShowLines
			for _, res := range executor.RunEach(a.Context(), ready, func(t transport.Target) transport.Request {
				return a.Collect(transport.Request{Argv: argv[t.Name], Timeout: max(a.Timeout().Get(), cincRunTimeout)})
			}) {
				results[index[res.Target.Name]] = res
			}

			if err := a.Print(output.Result{Table: resultsTable(results), Object: results}); err != nil {
				return err
			}
			return failureError(results)
		}))
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
Run the whole reinstallation of a node set: point the PXE service at the
installation, tell the machine to boot from the network once, forget its host
keys, and reset it.

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
		loseJobs bool
	)

	cmd := leaf("reinstall [NODESET]", "Reinstall a node set from the network", `
Reinstall nodes: configure the network boot, set the machines to boot from
the network once, remove their host keys, and reset them.

This destroys everything on the nodes. Before it asks, it resolves every
node's address, boot path, service processor and BMC credential, checks that
each boot path exists on the PXE host, and asks Slurm whether the nodes run
jobs, so that nothing is changed for a set that would stop half way. It
refuses protected hosts, lists each boot path with its nodes, and above the
configured host count asks for the count to be typed back.

A node that Slurm reports running a job, or cannot say about, is refused
unless --lose-jobs is given; --force gets past a protected host, not this
check. With --no-reset nothing is reset, so Slurm is not asked, and each node
reinstalls at its next network boot.

The boot override is set over Redfish, so a node whose bmc.order does not
start with redfish is refused.

When a step fails, the boot links and boot overrides of every node that was
not reset are removed again. Whatever could not be removed is named, with the
commands that remove it.

  clusterctl provision reinstall -n exe0001
  clusterctl provision reinstall -n @rack:R02 --dry-run`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			action := safety.Action{Verb: "reinstall", Targets: ns}
			// A protected host is named before anything is looked up or
			// asked, so that a refusal never leaves it out.
			if err := a.Gate.Check(action); err != nil {
				return err
			}

			// Everything is resolved before the first change, so that a
			// set with one node that cannot be reinstalled stops here
			// rather than halfway through. These lookups are plumbing a
			// display shows only when they fail or take long.
			var plan *reinstallPlan
			if err := inStep(a.Context(), "resolve", progress.Hidden, func(ctx context.Context) (err error) {
				plan, err = planReinstall(ctx, a, ns, bootPath, keepKeys)
				return err
			}); err != nil {
				return err
			}
			if !noReset {
				if err := checkSlurmIdle(a.Context(), a, ns, ipmi.ActionReset, loseJobs); err != nil {
					return err
				}
			}
			details := []string{"everything on these machines is lost", describeBootLinks(plan.links)}
			if noReset {
				details = append(details, "the machines are not reset: each one reinstalls at its next network boot")
			} else {
				details = append(details, "then each machine is set to boot from the network once and reset through Redfish")
			}
			if err := inStep(a.Context(), "check the boot paths", progress.Hidden, func(ctx context.Context) error {
				return checkBootLinks(ctx, a, plan.role, plan.root, plan.links)
			}); err != nil {
				return err
			}
			action.Detail = strings.Join(details, "\n  ")
			if err := a.Gate.Confirm(action); err != nil {
				return err
			}

			err = plan.run(a.Context(), a, noReset)
			t := output.NewTable(output.Cols("NODE", "BOOT PATH", "BOOT LINK", "BOOT ONCE", "RESET", "STATE").
				Wide("BOOT PATH")...)
			for _, n := range plan.nodes {
				t.Add(n.Node, n.BootPath, cmp.Or(n.BootLink, "-"), cmp.Or(n.BootOnce, "-"), cmp.Or(n.Reset, "-"), n.State)
			}
			t.Caption = plan.caption(noReset)
			if printErr := a.Print(output.Result{Table: t, Object: plan.nodes}); printErr != nil && err == nil {
				err = printErr
			}
			return err
		}))

	cmd.Flags().StringVar(&bootPath, "boot-path", "", "boot configuration to install from (default: from the cluster rules)")
	cmd.Flags().BoolVar(&keepKeys, "keep-host-keys", false, "leave the host key file alone")
	cmd.Flags().BoolVar(&noReset, "no-reset", false, "configure everything but do not reset the machines")
	addLoseJobsFlag(cmd, &loseJobs)
	return cmd
}

// provisionClient builds the Redfish client of a node's service processor.
// It is a variable so that the tests can put a fake processor behind it.
var provisionClient = func(a *app.App, ctx context.Context, node string) (*redfish.Client, error) {
	return a.RedfishClient(ctx, node)
}

// What became of one change of a reinstall on one node.
const (
	stepSet     = "set"
	stepSent    = "sent"
	stepFailed  = "failed"
	stepUnknown = "unknown"
	stepNotSent = "not sent"
	// stepUnreachable is a request that never reached the processor.
	stepUnreachable = "unreachable"
	stepRemoved     = "removed"
	stepCleared     = "cleared"
)

// The state a reinstall leaves a node in.
const (
	stateReinstalling = "reinstalling"
	stateArmed        = "armed"
	stateDisarmed     = "disarmed"
	stateUnchanged    = "unchanged"
)

// reinstallNode is what a reinstall does to one node, and how far it got.
type reinstallNode struct {
	Node     string `json:"node"`
	Address  string `json:"address"`
	BootPath string `json:"bootPath"`
	Mode     string `json:"mode"`
	BMC      string `json:"bmc"`
	// BootLink, BootOnce and Reset say what became of each change: set or
	// sent, failed, unknown or not sent, and removed or cleared when it was
	// undone again. They are empty for a change that was not tried.
	BootLink string `json:"bootLink,omitempty"`
	BootOnce string `json:"bootOnce,omitempty"`
	Reset    string `json:"reset,omitempty"`
	// State is reinstalling, armed when the node reinstalls at its next
	// network boot, disarmed when what was armed was undone, or unchanged.
	State  string   `json:"state"`
	Errors []string `json:"errors,omitempty"`

	hostNames []string
	client    *redfish.Client
}

func (n *reinstallNode) note(what string, err error) {
	if err != nil {
		n.Errors = append(n.Errors, output.EscapeCell(what+": "+err.Error()))
	}
}

// linked says whether a node may carry the boot link this reinstall wrote.
func (n *reinstallNode) linked() bool {
	return n.BootLink == stepSet || n.BootLink == stepUnknown
}

// overridden says whether a node's processor may hold the boot override
// this reinstall asked for. A request that failed may still have been
// carried out, so only one that was never sent, or never reached the
// processor, counts as not set.
func (n *reinstallNode) overridden() bool {
	return n.BootOnce == stepSet || n.BootOnce == stepFailed
}

// reinstallPlan is everything a reinstall needs, resolved before anything
// is changed.
type reinstallPlan struct {
	role, root string
	nodes      []*reinstallNode
	links      []bootLink
	// knownHosts is the host key file, or empty when it is left alone.
	knownHosts    string
	keysForgotten bool
}

// planReinstall resolves, for every node, the address and boot path of its
// boot link, its service processor, its BMC credential and the names its
// host keys are filed under. Nothing is changed, and nothing is sent to a
// node or a service processor.
func planReinstall(ctx context.Context, a *app.App, ns *nodeset.NodeSet, explicit string, keepKeys bool) (*reinstallPlan, error) {
	role, err := pxeRole(a)
	if err != nil {
		return nil, err
	}
	links, err := resolveBootLinks(ctx, a, ns, explicit, false)
	if err != nil {
		return nil, err
	}
	p := &reinstallPlan{role: role, root: a.Spec.Services.PXESrv.Root, links: links}
	if !keepKeys {
		p.knownHosts = a.Path(a.Spec.SSH.KnownHostsFile)
	}

	// The boot override is only ever set over Redfish. A node whose order
	// puts another transport first may have a processor that does not
	// speak Redfish, or should not be sent the account over it.
	var elsewhere []string
	for _, l := range links {
		if order := a.BMCOrder(l.Node); strings.ToLower(strings.TrimSpace(order[0])) != "redfish" {
			elsewhere = append(elsewhere, l.Node)
		}
	}
	if len(elsewhere) > 0 {
		return nil, exitcode.Errorf(exitcode.Usage,
			"bmc.order reaches %s over another transport before Redfish, and provision reinstall "+
				"sets the network boot over Redfish only; reinstall it step by step with "+
				"boot set, bmc boot set, hostkey remove and bmc power reset", fold(elsewhere))
	}

	for _, l := range links {
		n := &reinstallNode{Node: l.Node, Address: l.Address, BootPath: l.Path, Mode: l.Mode, State: stateUnchanged}
		if a.DryRun() {
			// A dry run contacts no processor, so it builds no client,
			// which would announce a certificate it is about to record;
			// the processor and the credential are resolved all the same.
			if n.BMC, err = a.BMCHost(l.Node); err != nil {
				return nil, err
			}
			if _, err := a.BMCCredential(ctx, l.Node); err != nil {
				return nil, fmt.Errorf("%s: %w", l.Node, err)
			}
		} else {
			if n.client, err = provisionClient(a, ctx, l.Node); err != nil {
				return nil, fmt.Errorf("%s: %w", l.Node, err)
			}
			n.BMC = n.client.Host
		}
		if !keepKeys {
			fqdn, err := a.Namer.FQDN(l.Node)
			if err != nil {
				return nil, exitcode.Wrap(exitcode.Usage, err)
			}
			n.hostNames = []string{fqdn, l.Node}
		}
		p.nodes = append(p.nodes, n)
	}
	return p, nil
}

// run carries out a plan: it writes the boot links, sets the boot overrides,
// forgets the host keys and resets the machines. The host keys, which
// cannot be put back, go only once every machine is armed. When a step
// fails, every node that was not reset is disarmed again.
//
// Each part is a step of its own, named as its failure names it: the one
// script that writes the boot links, each change sent to the processors,
// and forgetting the host keys, on this machine.
func (p *reinstallPlan) run(ctx context.Context, a *app.App, noReset bool) error {
	const configuring = "configuring the network boot"
	err := inStep(ctx, configuring, 0, func(ctx context.Context) error {
		return writeBootLinks(ctx, a, p.role, p.root, p.links)
	})
	for i, l := range p.links {
		p.nodes[i].BootLink = l.Result
		if l.Error != "" {
			p.nodes[i].Errors = append(p.nodes[i].Errors, "boot link: "+l.Error)
		}
	}
	if err != nil {
		return p.fail(ctx, a, configuring, err, p.nodes)
	}

	const bootOnce = "setting the machines to boot from the network once"
	errs := sendToBMCs(ctx, a, bootOnce, p.nodes, func(ctx context.Context, c *redfish.Client) error {
		return c.SetBootOverride(ctx, "Pxe", false)
	})
	for i, n := range p.nodes {
		n.BootOnce = stepOutcome(errs[i], stepSet)
		n.note("boot once", errs[i])
	}
	if err := nodeFailures(p.nodes, errs); err != nil {
		return p.fail(ctx, a, bootOnce, err, p.nodes)
	}

	if p.knownHosts != "" {
		const forgetting = "forgetting the host keys"
		err := inStep(ctx, forgetting, 0, func(ctx context.Context) error {
			return hostkeys.Modify(ctx, p.knownHosts, func(f *hostkeys.File) error {
				for _, n := range p.nodes {
					for _, name := range n.hostNames {
						f.Remove(name)
					}
				}
				return nil
			})
		})
		if err != nil {
			return p.fail(ctx, a, forgetting, err, p.nodes)
		}
		p.keysForgotten = true
	}

	if noReset {
		p.settle()
		return nil
	}
	const resetting = "resetting the machines"
	errs = sendToBMCs(ctx, a, resetting, p.nodes, func(ctx context.Context, c *redfish.Client) error {
		return c.Reset(ctx, redfish.ResetForceRestart)
	})
	// A node whose reset failed is disarmed with the rest, even though a
	// reset that timed out may have been carried out: the machine then
	// boots its old system, rather than reinstalling at some later boot
	// that nobody confirmed.
	var left []*reinstallNode
	for i, n := range p.nodes {
		n.Reset = stepOutcome(errs[i], stepSent)
		n.note("reset", errs[i])
		if errs[i] != nil {
			left = append(left, n)
		}
	}
	if err := nodeFailures(p.nodes, errs); err != nil {
		return p.fail(ctx, a, resetting, err, left)
	}
	p.settle()
	return nil
}

// sendToBMCs sends one change to the processor of each node, through the
// Redfish fan-out, as the step it names, and returns the error of each.
// With no nodes there is nothing to send, and no step.
func sendToBMCs(ctx context.Context, a *app.App, step string, nodes []*reinstallNode, do func(context.Context, *redfish.Client) error) []error {
	if len(nodes) == 0 {
		return nil
	}
	names := make([]string, len(nodes))
	clients := make([]*redfish.Client, len(nodes))
	for i, n := range nodes {
		names[i], clients[i] = n.Node, n.client
	}
	calls := redfishEach(ctx, a, step, names, clients, true, func(ctx context.Context, _ string, c *redfish.Client) (struct{}, error) {
		return struct{}{}, do(ctx, c)
	})
	errs := make([]error, len(calls))
	for i, call := range calls {
		errs[i] = call.err
	}
	return errs
}

// settle works out the state of every node from what became of each change.
func (p *reinstallPlan) settle() {
	for _, n := range p.nodes {
		switch {
		case n.Reset == stepSent:
			n.State = stateReinstalling
		case n.linked() || n.overridden():
			n.State = stateArmed
		case n.BootLink == stepRemoved || n.BootOnce == stepCleared:
			n.State = stateDisarmed
		default:
			n.State = stateUnchanged
		}
	}
}

// fail disarms the given nodes after a step failed: it clears each boot
// override that may have been set and removes each boot link that may have
// been written, as a step of its own, "disarming", when there is anything
// to undo. It returns the error of the step, followed by what became of the
// set and, for whatever is still armed, the commands that disarm it.
func (p *reinstallPlan) fail(ctx context.Context, a *app.App, step string, stepErr error, left []*reinstallNode) error {
	var overridden, linked []*reinstallNode
	var links []bootLink
	for _, n := range left {
		if n.overridden() {
			overridden = append(overridden, n)
		}
		if n.linked() {
			linked = append(linked, n)
			links = append(links, bootLink{Node: n.Node, Address: n.Address, Path: n.BootPath, Mode: n.Mode})
		}
	}
	if len(overridden) > 0 || len(links) > 0 {
		_ = inStep(ctx, "disarming", 0, func(ctx context.Context) error {
			errs := sendToBMCs(ctx, a, "clearing the boot overrides", overridden, func(ctx context.Context, c *redfish.Client) error {
				return c.ClearBootOverride(ctx)
			})
			for i, n := range overridden {
				if errs[i] == nil {
					n.BootOnce = stepCleared
				}
				n.note("clearing the boot override", errs[i])
			}
			var removed error
			if len(links) > 0 {
				removed = removeBootLinks(ctx, a, p.role, p.root, links)
				for i, n := range linked {
					if links[i].Result == stepRemoved {
						n.BootLink = stepRemoved
						continue
					}
					n.Errors = append(n.Errors, "removing the boot link: "+links[i].Error)
				}
			}
			return errors.Join(nodeFailures(overridden, errs), removed)
		})
	}
	p.settle()

	var parts []string
	if reinstalling := p.inState(stateReinstalling); len(reinstalling) > 0 {
		parts = append(parts, fold(reinstalling)+" is reinstalling")
	}
	if disarmed := p.inState(stateDisarmed); len(disarmed) > 0 {
		msg := "the boot links and boot overrides of " + fold(disarmed) + " were removed again"
		if p.keysForgotten {
			msg += fmt.Sprintf(`, but their host keys are forgotten; "clusterctl hostkey refresh -n %s" writes them again`,
				fold(disarmed))
		}
		parts = append(parts, msg)
	}
	if armed := p.inState(stateArmed); len(armed) > 0 {
		parts = append(parts, armedAdvice(p.nodes))
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing was changed")
	}
	return fmt.Errorf("%s failed: %w; %s", step, stepErr, strings.Join(parts, "; "))
}

// armedAdvice names the nodes left armed and the commands that disarm them.
func armedAdvice(nodes []*reinstallNode) string {
	var armed, overridden, linked []string
	for _, n := range nodes {
		if n.State != stateArmed {
			continue
		}
		armed = append(armed, n.Node)
		if n.overridden() {
			overridden = append(overridden, n.Node)
		}
		if n.linked() {
			linked = append(linked, n.Node)
		}
	}
	var commands []string
	if len(overridden) > 0 {
		commands = append(commands, fmt.Sprintf(`"clusterctl bmc boot unset -n %s"`, fold(overridden)))
	}
	if len(linked) > 0 {
		commands = append(commands, fmt.Sprintf(`"clusterctl boot unset -n %s"`, fold(linked)))
	}
	return fmt.Sprintf("%s is left armed and reinstalls at its next network boot; disarm it with %s",
		fold(armed), strings.Join(commands, " and "))
}

func (p *reinstallPlan) inState(state string) []string {
	var out []string
	for _, n := range p.nodes {
		if n.State == state {
			out = append(out, n.Node)
		}
	}
	return out
}

// caption sums up a reinstall that went through.
func (p *reinstallPlan) caption(noReset bool) string {
	if len(p.inState(stateReinstalling)) == len(p.nodes) {
		return fmt.Sprintf(`%s is reinstalling; follow it with "clusterctl provision status" and "clusterctl boot log"`,
			fold(p.inState(stateReinstalling)))
	}
	if noReset && len(p.inState(stateArmed)) == len(p.nodes) {
		return armedAdvice(p.nodes)
	}
	return ""
}

// stepOutcome names what became of one request to a processor.
func stepOutcome(err error, done string) string {
	switch {
	case err == nil:
		return done
	case errors.Is(err, errRequestNotSent):
		return stepNotSent
	case neverSent(err):
		return stepUnreachable
	default:
		return stepFailed
	}
}

// nodeFailures names the nodes whose request failed, with the code
// exitcode.Worst gives.
func nodeFailures(nodes []*reinstallNode, errs []error) error {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Node
	}
	return namedFailures(names, errs)
}

func namedFailures(names []string, errs []error) error {
	var parts []string
	var failed []error
	for i, err := range errs {
		if err == nil {
			continue
		}
		parts = append(parts, output.EscapeCell(fmt.Sprintf("%s: %v", names[i], err)))
		failed = append(failed, err)
	}
	if len(failed) == 0 {
		return nil
	}
	code := exitcode.Worst(failed...)
	return &exitcode.Error{Code: code, Err: &hostFailures{message: strings.Join(parts, "; "), errs: failed, code: code}}
}

// provisionState is where the reinstallation of one node stands.
type provisionState struct {
	Node               string `json:"node"`
	Address            string `json:"address,omitempty"`
	BootPath           string `json:"bootPath,omitempty"`
	PersistentBootPath string `json:"persistentBootPath,omitempty"`
	BMC                string `json:"bmc,omitempty"`
	Power              string `json:"power,omitempty"`
	// SSH says whether the node answers over ssh; SSHError says why not.
	// A node that is reinstalling does not, so neither fails the command.
	SSH      bool   `json:"ssh"`
	SSHError string `json:"sshError,omitempty"`
	Uptime   string `json:"uptime,omitempty"`
	// Error says why the boot path or the power state is not known.
	Error string `json:"error,omitempty"`

	err error
}

func (s *provisionState) fail(err error) {
	if s.err == nil {
		s.err = err
	} else {
		s.err = errors.Join(s.err, err)
	}
	if s.Error != "" {
		s.Error += "; "
	}
	s.Error += output.EscapeCell(err.Error())
}

func newProvisionStatusCommand(r *root) *cobra.Command {
	return leaf("status [NODESET]", "Show where a reinstallation stands", `
Show, for each node, the boot path its link on the PXE service points at,
the power state its service processor reports and whether it answers over
ssh yet.

The processors and the nodes are asked at the same time, each within its
own bound: fanout.max nodes at once, and bmc.redfish.maxConcurrent
processors, or fewer when --fanout is lower.

Every node is listed. A node that does not answer over ssh is shown as such
and does not fail the command, since a machine that is reinstalling does not
answer. A boot path or a power state that cannot be read is shown as
unknown, with the reason in the ERROR column and in the error field of the
node in the JSON output, and fails the command: with exit code 3 when a host
could not be reached, 2 for a configuration problem such as a missing
credential, and 1 when a host refused.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			root := a.Spec.Services.PXESrv.Root
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			// The nodes are named before anything is asked, so that one the
			// naming rules cannot name stops the command before a host or a
			// processor is.
			targets, err := a.NodeTargets(ns)
			if err != nil {
				return err
			}
			nodes := ns.Expand()
			states := make([]*provisionState, len(nodes))
			for i, node := range nodes {
				states[i] = &provisionState{Node: node}
			}

			// One listing answers for every node.
			suffix := a.Spec.Services.PXESrv.StaticSuffix
			links, linkErr := readBootLinks(a.Context(), a, role, root)
			for _, s := range states {
				address, err := nodeAddress(a.Context(), a, s.Node)
				if err != nil {
					s.fail(err)
					continue
				}
				s.Address = address
				if linkErr != nil {
					s.fail(fmt.Errorf("reading the boot links on %s: %w", role, linkErr))
					continue
				}
				s.BootPath = output.EscapeCell(cmp.Or(links[address], "none"))
				if suffix != "" {
					s.PersistentBootPath = output.EscapeCell(cmp.Or(links[address+suffix], "none"))
				}
			}

			// The credentials are looked up before either half starts,
			// so that a prompt for one never comes while either runs.
			clients := make([]*redfish.Client, len(nodes))
			for i, s := range states {
				// A node whose client fails is reported and the rest go
				// on, but once the command is interrupted no credential
				// is looked up for the others: at a prompt, each would
				// ask again.
				if a.Context().Err() != nil {
					s.fail(errNotSent())
					continue
				}
				c, err := provisionClient(a, a.Context(), s.Node)
				if err != nil {
					s.fail(err)
					continue
				}
				clients[i], s.BMC = c, c.Host
			}

			// The processors and the nodes are asked side by side, each
			// half within its own bound, and what they answered is taken
			// in once both are done, the processors' first.
			var (
				calls   []redfishCall[string]
				results []*transport.Result
				halves  sync.WaitGroup
			)
			halves.Go(func() {
				calls = redfishEach(a.Context(), a, "read the power state", nodes, clients, false, func(ctx context.Context, _ string, c *redfish.Client) (string, error) {
					return c.PowerState(ctx)
				})
			})
			halves.Go(func() {
				// A node ssh does not reach is an answer, the table's no,
				// not a failure of the command.
				executor := a.Executor()
				executor.Step, executor.Answers = "read the uptime", true
				results = executor.Run(a.Context(), targets, a.Collect(transport.Request{
					Argv:    []string{"uptime", "-p"},
					Timeout: 30 * time.Second,
				}))
			})
			halves.Wait()
			for i, s := range states {
				if calls[i].err != nil {
					s.fail(calls[i].err)
				}
				s.Power = output.EscapeCell(calls[i].value)
			}
			byName := map[string]*transport.Result{}
			for _, res := range results {
				byName[res.Target.Name] = res
			}
			for _, s := range states {
				res, ok := byName[s.Node]
				switch {
				case !ok:
					s.SSHError = "not tried"
				case res.Failed():
					s.SSHError = output.EscapeCell(sshReason(res))
				default:
					s.SSH, s.Uptime = true, output.EscapeCell(res.Output())
				}
			}

			cols := []string{"NODE", "BOOT PATH"}
			if suffix != "" {
				cols = append(cols, "PERSISTENT")
			}
			cols = append(cols, "POWER", "SSH", "UPTIME", "ERROR")
			t := output.NewTable(output.Cols(cols...)...)
			var failedNames []string
			var failedErrs []error
			for _, s := range states {
				row := []string{s.Node, cmp.Or(s.BootPath, "unknown")}
				if suffix != "" {
					row = append(row, cmp.Or(s.PersistentBootPath, "unknown"))
				}
				reachable := "no"
				if s.SSH {
					reachable = "yes"
				}
				row = append(row, cmp.Or(s.Power, "unknown"), reachable, s.Uptime, s.Error)
				t.Add(row...)
				if s.err != nil {
					failedNames = append(failedNames, s.Node)
					failedErrs = append(failedErrs, s.err)
				}
			}
			if err := a.Print(output.Result{Table: t, Object: states}); err != nil {
				return err
			}
			if err := namedFailures(failedNames, failedErrs); err != nil {
				return fmt.Errorf("where the reinstallation of %s stands is not fully known: %w", fold(failedNames), err)
			}
			return nil
		}))
}

// sshReason is the reason a node gave for failing, or the transport's.
func sshReason(res *transport.Result) string {
	for _, text := range []string{res.Stderr, res.Stdout} {
		for line := range strings.SplitSeq(text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	if res.Err != nil {
		return res.Err.Error()
	}
	return fmt.Sprintf("exit status %d", res.ExitCode)
}
