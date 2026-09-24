// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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

// The configuration file on a node holds shell assignments, the format the
// shell toolkit sourced. clusterctl writes every value quoted, so the file
// stays safe for anything that still sources it, and reads it back without
// a shell, so nothing in it is ever run as root.
const (
	cincURLKey     = "CHEF_RECIPE_URL"
	cincRunListKey = "CHEF_RUN_LIST"
)

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
		return escapeControl(line)
	}
	if res.Err != nil {
		return escapeControl(res.Err.Error())
	}
	return fmt.Sprintf("exit %d", res.ExitCode)
}

// cincRefused stands in for a node on which nothing was run because what it
// answered cannot be used.
func cincRefused(target transport.Target, err error) *transport.Result {
	return &transport.Result{Target: target, ExitCode: -1, Err: err}
}

// cincFailureError is failureError, except that a node that could not be
// reached makes it exit with the transport code, as the contract promises.
func cincFailureError(results []*transport.Result) error {
	err := failureError(results)
	if err == nil {
		return nil
	}
	for _, res := range results {
		if res.Failed() && exitcode.From(res.Err) == exitcode.Transport {
			return exitcode.Wrap(exitcode.Transport, err)
		}
	}
	return err
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
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			solo := cincSolo{URL: args[0], RunList: runList}
			if err := solo.check(); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			ns, err := selection(a, args[1:])
			if err != nil {
				return err
			}
			path := cincConfigPath(a)
			detail := solo.URL
			if solo.RunList != "" {
				detail += " with run list " + solo.RunList
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb: "set the configuration source of", Targets: ns, Detail: detail,
			}); err != nil {
				return dryRunOrError(err)
			}

			body := solo.render()
			script := cincWriteScript(path, len(body))

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
			return cincFailureError(results)
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
run list it is configured with. Without a run list, in the file or given
here, the client uses the run list of the archive.

The configuration file is read, never sourced: a node whose file holds
anything but plain assignments, or names no http or https archive, is refused
and the client is not started there.

This changes the nodes, so it asks first.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			if err := checkCincRunList(runList); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			binary := a.Spec.Services.Cinc.Binary
			if binary == "" {
				binary = "cinc-solo"
			}
			path := cincConfigPath(a)
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
				return dryRunOrError(err)
			}

			reads, err := runOnNodes(a, ns, func(string) transport.Request {
				return transport.Request{
					Script:  cincReadScript(path),
					Timeout: a.Timeout().Get(),
					TTY:     transport.TTYNone,
				}
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
			for _, res := range a.Executor().RunEach(a.Context(), ready, func(t transport.Target) transport.Request {
				return transport.Request{
					Argv:    argv[t.Name],
					Timeout: a.Timeout().Or(30 * time.Minute),
					TTY:     transport.TTYNone,
				}
			}) {
				results[index[res.Target.Name]] = res
			}

			if err := a.Print(output.Result{Table: resultsTable(results), Object: results}); err != nil {
				return err
			}
			return cincFailureError(results)
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
