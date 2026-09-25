// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// pxeHost is a PXE service root in a temporary directory. The scripts the
// boot commands send run there under a real shell, so a test sees what they
// would leave behind on the install host, not only what they would send.
type pxeHost struct {
	root  string
	layer string
	rec   *transport.Recorder
	// dhcp is what cat of dhcpd.conf answers.
	dhcp string
}

type pxeOptions struct {
	// static marks the exe rule of the example cluster static.
	static bool
	// inventory is extra NodeInventory entries, as YAML list items.
	inventory string
	dhcp      string
}

func newPXEHost(t *testing.T, opts pxeOptions) *pxeHost {
	t.Helper()
	p := &pxeHost{root: t.TempDir(), layer: t.TempDir(), dhcp: opts.dhcp}
	for _, dir := range []string{"wlm01", "dbm01", "exe"} {
		mustWrite(t, filepath.Join(p.root, "boot/cluster/1.0", dir, "ipxe.net2"), "#!ipxe\n")
	}

	// The example cluster, with its boot paths moved under the root.
	cluster, err := os.ReadFile(filepath.Join(exampleDir, "cluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(cluster)
	if opts.static {
		text = strings.Replace(text, "path: /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2",
			"path: /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2\n      static: true", 1)
	}
	text = strings.ReplaceAll(text, "/srv/pxesrv", p.root)
	mustWrite(t, filepath.Join(p.layer, "cluster.yaml"), text)
	if opts.inventory != "" {
		mustWrite(t, filepath.Join(p.layer, "extra.yaml"), `apiVersion: clusterctl/v1alpha1
kind: NodeInventory
metadata:
  name: zz-extra
spec:
  nodes:
`+opts.inventory)
	}

	p.rec = &transport.Recorder{Reply: p.reply}
	return p
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// reply runs a request on this machine, except for reading dhcpd.conf.
func (p *pxeHost) reply(tg transport.Target, req transport.Request) (*transport.Result, error) {
	var cmd *exec.Cmd
	switch {
	case len(req.Argv) > 0 && req.Argv[0] == "cat":
		return &transport.Result{Target: tg, Stdout: p.dhcp}, nil
	case len(req.Argv) > 1 && req.Argv[0] == "find":
		// The BSD find of a macOS runner has no -printf, so the listing
		// is answered here, the way GNU find on the PXE host prints it.
		return p.listLinks(tg, req.Argv[1])
	case req.Script != "":
		cmd = exec.CommandContext(context.Background(), "bash", "-c", req.Script)
	case len(req.Argv) > 0:
		cmd = exec.CommandContext(context.Background(), req.Argv[0], req.Argv[1:]...) //nolint:gosec // the test's own requests
	default:
		return &transport.Result{Target: tg}, nil
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := &transport.Result{Target: tg}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, err
		}
		res.ExitCode = exit.ExitCode()
	}
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	return res, nil
}

func (p *pxeHost) listLinks(tg transport.Target, dir string) (*transport.Result, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out.WriteString(e.Name() + "\t" + target + "\n")
	}
	return &transport.Result{Target: tg, Stdout: out.String()}, nil
}

// run runs clusterctl against the host, with -y unless the arguments say
// otherwise through opts.
func (p *pxeHost) run(t *testing.T, opts harnessOptions, args ...string) (*harness, error) {
	t.Helper()
	opts.recorder = p.rec
	opts.config = append(opts.config, p.layer)
	return run(t, opts, append([]string{"--set", "services.pxesrv.root=" + p.root}, args...)...)
}

func (p *pxeHost) link(t *testing.T, name, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(p.root, name)); err != nil {
		t.Fatal(err)
	}
}

func (p *pxeHost) target(name string) string {
	target, err := os.Readlink(filepath.Join(p.root, name))
	if err != nil {
		return ""
	}
	return target
}

func (p *pxeHost) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(p.root, name))
	return err == nil
}

func (p *pxeHost) exePath() string {
	return filepath.Join(p.root, "boot/cluster/1.0/exe/ipxe.net2")
}

func (p *pxeHost) scripts() []string {
	var out []string
	for _, c := range p.rec.Calls() {
		if c.Request.Script != "" {
			out = append(out, c.Request.Script)
		}
	}
	return out
}

// isBootPathCheck says whether a request is the check that the boot paths
// exist on the PXE host, which only reads.
func isBootPathCheck(req transport.Request) bool {
	return strings.HasPrefix(req.Script, "for p in") && strings.Contains(req.Script, `[ -f "$p" ]`)
}

func (p *pxeHost) changes() []string {
	var out []string
	for _, s := range p.scripts() {
		if strings.Contains(s, "ln -s") || strings.Contains(s, "rm -f") {
			out = append(out, s)
		}
	}
	return out
}

const staticSuffix = "services.pxesrv.staticSuffix=.static"

// 5.5: the persistent link is what reinstalls a machine at every boot, so
// removing the boot configuration has to remove it too.
func TestBootUnsetRemovesThePersistentLink(t *testing.T) {
	p := newPXEHost(t, pxeOptions{})
	p.link(t, "10.0.2.1", p.exePath())
	p.link(t, "10.0.2.1.static", p.exePath())

	h, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "boot", "unset", "-n", "exe0001", "-y")
	if err != nil {
		t.Fatalf("boot unset failed: %v\n%s", err, h.errOut)
	}
	for _, name := range []string{"10.0.2.1", "10.0.2.1.static"} {
		if p.exists(name) {
			t.Errorf("%s is still there after boot unset", name)
		}
	}
}

func TestBootStatusShowsThePersistentLink(t *testing.T) {
	p := newPXEHost(t, pxeOptions{})
	p.link(t, "10.0.2.1.static", p.exePath())

	h, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "boot", "status", "-n", "exe0001")
	if err != nil {
		t.Fatalf("boot status failed: %v", err)
	}
	if !strings.Contains(h.out.String(), p.exePath()) {
		t.Errorf("the persistent link is missing from the table:\n%s", h.out)
	}

	h, err = p.run(t, harnessOptions{}, "--set", staticSuffix, "-o", "json", "boot", "status", "-n", "exe0001")
	if err != nil {
		t.Fatalf("boot status -o json failed: %v", err)
	}
	var got map[string]struct {
		Address    string `json:"address"`
		BootPath   string `json:"bootPath"`
		Persistent string `json:"persistentBootPath"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, h.out)
	}
	if e := got["exe0001"]; e.Persistent != p.exePath() || e.BootPath != "none" || e.Address != "10.0.2.1" {
		t.Errorf("exe0001 = %+v, want the persistent link and no one-shot link", e)
	}
}

func TestBootSetRefusesOverAPersistentLink(t *testing.T) {
	p := newPXEHost(t, pxeOptions{})
	p.link(t, "10.0.2.1.static", p.exePath())

	_, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "boot", "set", "-n", "exe0001", "-y")
	if err == nil {
		t.Fatal("a one-shot boot path over a persistent one should be refused")
	}
	if !strings.Contains(err.Error(), "persistent") {
		t.Errorf("error = %v, want it to name the persistent link", err)
	}
	if p.exists("10.0.2.1") || len(p.changes()) != 0 {
		t.Errorf("a link was written: %q", p.changes())
	}
}

// 1.4: an address becomes a file name under the PXE root, on a host where
// the script runs as root.
func TestBootAddressesAreCheckedBeforeAnythingIsSent(t *testing.T) {
	tests := []struct {
		name      string
		inventory string
		dhcp      string
		args      []string
		want      string
	}{
		{
			name:      "a CIDR typo",
			inventory: "    - nodes: exe0002\n      address: 10.0.2.2/24\n",
			args:      []string{"boot", "set", "-n", "exe[0001-0002]"},
			want:      "10.0.2.2/24",
		},
		{
			name:      "a path",
			inventory: "    - nodes: exe0002\n      address: ../../etc/x\n",
			args:      []string{"boot", "unset", "-n", "exe0002"},
			want:      "../../etc/x",
		},
		{
			name: "a host name from DHCP",
			dhcp: "host exe0005 {\n  hardware ethernet aa:bb:cc:00:00:05;\n  fixed-address exe0005.hpc.example.org;\n}\n",
			args: []string{"boot", "set", "-n", "exe0005"},
			want: "exe0005.hpc.example.org",
		},
		{
			name:      "an address in the set twice",
			inventory: "    - nodes: exe0002\n      address: 10.0.2.9\n    - nodes: exe0003\n      address: 10.0.2.9\n",
			args:      []string{"boot", "set", "-n", "exe[0002-0003]"},
			want:      "10.0.2.9",
		},
		{
			// Arming exe0006 must not arm exe0001, which nobody selected.
			name:      "another node's address",
			inventory: "    - nodes: exe0006\n      address: 10.0.2.1\n",
			args:      []string{"boot", "set", "-n", "exe0006"},
			want:      "exe0001",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newPXEHost(t, pxeOptions{inventory: tc.inventory, dhcp: tc.dhcp})
			_, err := p.run(t, harnessOptions{}, append(tc.args, "-y")...)
			if err == nil {
				t.Fatal("the address should be refused")
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d", got, want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			if len(p.scripts()) != 0 {
				t.Errorf("a script was sent: %q", p.scripts())
			}
		})
	}
}

// 2.12: boot unset resolves the addresses before it asks, so the preview
// never approves a run that then stops.
func TestBootUnsetResolvesBeforeTheGate(t *testing.T) {
	p := newPXEHost(t, pxeOptions{inventory: "    - nodes: exe0002\n      address: 10.0.2.2/24\n"})

	h, err := p.run(t, harnessOptions{}, "boot", "unset", "-n", "exe0002", "--dry-run")
	if err == nil {
		t.Fatalf("the dry run should fail as the real run would:\n%s", h.errOut)
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}

	h, err = p.run(t, harnessOptions{tty: true, stdin: "y\n"}, "boot", "unset", "-n", "exe0002")
	if err == nil {
		t.Fatal("an address that is not one should be refused")
	}
	if strings.Contains(h.errOut.String(), "Continue?") {
		t.Errorf("the question was asked before the address was resolved:\n%s", h.errOut)
	}
}

// 5.7: one failing line must not leave the rest of the set unchanged
// without saying so.
func TestBootSetAndUnsetReportEachNode(t *testing.T) {
	inventory := "    - nodes: exe0002\n      address: 10.0.2.2\n" +
		"    - nodes: exe0003\n      address: 10.0.2.3\n" +
		"    - nodes: exe0004\n      address: 10.0.2.4\n"

	p := newPXEHost(t, pxeOptions{inventory: inventory})
	if err := os.Mkdir(filepath.Join(p.root, "10.0.2.3"), 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := p.run(t, harnessOptions{}, "boot", "set", "-n", "exe[0001-0004]", "-y")
	if err == nil {
		t.Fatal("a node whose link could not be written should fail the command")
	}
	if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if !strings.Contains(err.Error(), "exe0003") {
		t.Errorf("error = %v, want it to name exe0003", err)
	}
	for _, name := range []string{"10.0.2.1", "10.0.2.2", "10.0.2.4"} {
		if p.target(name) != p.exePath() {
			t.Errorf("%s was not linked; the nodes after a failure must still be tried", name)
		}
	}
	out := h.out.String()
	for _, want := range []string{"exe0001", "exe0003", "exe0004", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the table does not mention %q:\n%s", want, out)
		}
	}

	h, err = p.run(t, harnessOptions{}, "-o", "json", "boot", "unset", "-n", "exe[0001-0004]", "-y")
	if err == nil {
		t.Fatal("a node whose link could not be removed should fail the command")
	}
	for _, name := range []string{"10.0.2.1", "10.0.2.2", "10.0.2.4"} {
		if p.exists(name) {
			t.Errorf("%s was not removed; the nodes after a failure must still be tried", name)
		}
	}
	var got []struct {
		Node   string `json:"node"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, h.out)
	}
	results := map[string]string{}
	for _, r := range got {
		results[r.Node] = r.Result
	}
	if results["exe0003"] != "failed" || results["exe0004"] != "removed" || len(results) != 4 {
		t.Errorf("results = %v, want every node, exe0003 failed", results)
	}
}

// 5.8: --persistent without a suffix writes exactly the one-shot link.
func TestBootSetPersistentNeedsAStaticSuffix(t *testing.T) {
	p := newPXEHost(t, pxeOptions{})
	_, err := p.run(t, harnessOptions{}, "boot", "set", "-n", "exe0001", "--persistent", "-y")
	if err == nil {
		t.Fatal("--persistent without services.pxesrv.staticSuffix should be refused")
	}
	if !strings.Contains(err.Error(), "staticSuffix") {
		t.Errorf("error = %v, want it to name the setting", err)
	}
	if len(p.changes()) != 0 || p.exists("10.0.2.1") {
		t.Errorf("a link was written: %q", p.changes())
	}

	h, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "boot", "set", "-n", "exe0001", "--persistent", "-y")
	if err != nil {
		t.Fatalf("boot set --persistent failed: %v\n%s", err, h.errOut)
	}
	if p.target("10.0.2.1.static") != p.exePath() || p.exists("10.0.2.1") {
		t.Error("--persistent should write the persistent link and only that")
	}
}

func TestBootSetHonoursAStaticRule(t *testing.T) {
	p := newPXEHost(t, pxeOptions{static: true})
	_, err := p.run(t, harnessOptions{}, "boot", "set", "-n", "exe0001", "-y")
	if err == nil || !strings.Contains(err.Error(), "staticSuffix") {
		t.Fatalf("a static rule without a suffix should be refused, got %v", err)
	}
	if p.exists("10.0.2.1") {
		t.Error("a static rule wrote the one-shot link")
	}

	h, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "boot", "set", "-n", "exe0001", "-y")
	if err != nil {
		t.Fatalf("boot set failed: %v\n%s", err, h.errOut)
	}
	if p.target("10.0.2.1.static") != p.exePath() || p.exists("10.0.2.1") {
		t.Error("a static rule should write the persistent link")
	}
}

func TestBootGrubSetSaysItPersistsAndCanBeUnset(t *testing.T) {
	h, err := run(t, harnessOptions{}, "boot", "grub", "set", "exe0001", "/srv/tftp/grub/1.0/grub.cfg.install-exec", "--dry-run")
	if err != nil {
		t.Fatalf("boot grub set --dry-run failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "boot grub unset") {
		t.Errorf("the preview does not say the link stays until it is removed:\n%s", h.errOut)
	}

	h, err = run(t, harnessOptions{}, "boot", "grub", "unset", "exe0001", "-y")
	if err != nil {
		t.Fatalf("boot grub unset failed: %v", err)
	}
	commands := h.recorder.Commands()
	if len(commands) != 1 || !strings.Contains(commands[0], "rm -f -- /srv/tftp/grub/grub.cfg-0A000201") {
		t.Errorf("commands = %q, want the GRUB link removed", commands)
	}

	h, err = run(t, harnessOptions{}, "boot", "grub", "unset", "exe0001")
	if err == nil || len(h.recorder.Calls()) != 0 {
		t.Errorf("boot grub unset without a terminal should be refused and send nothing, got %v", err)
	}
}

// 5.9: the question has to show every boot path that will be written.
func TestBootSetPreviewListsEveryPath(t *testing.T) {
	h, err := run(t, harnessOptions{}, "--force", "--dry-run", "boot", "set", "-n", "dbm01,exe0001")
	if err != nil {
		t.Fatalf("the dry run failed: %v", err)
	}
	preview := h.errOut.String()
	for _, want := range []string{
		"/srv/pxesrv/boot/cluster/1.0/dbm01/ipxe.net2", "dbm01",
		"/srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2", "exe0001",
		"10.0.1.2", "10.0.2.1",
	} {
		if !strings.Contains(preview, want) {
			t.Errorf("the preview does not mention %q:\n%s", want, preview)
		}
	}
}

func TestBootSetRefusesAMissingBootPath(t *testing.T) {
	p := newPXEHost(t, pxeOptions{})
	missing := filepath.Join(p.root, "boot/cluster/1.O/exe/ipxe.net2")
	_, err := p.run(t, harnessOptions{}, "boot", "set", "-n", "exe0001", missing, "-y")
	if err == nil {
		t.Fatal("a boot path that does not exist should be refused")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name the path", err)
	}
	if p.exists("10.0.2.1") || len(p.changes()) != 0 {
		t.Errorf("a dangling link was written: %q", p.changes())
	}
}

// A dry run reads the PXE host as the real run does, so it is refused where
// the real run would be: it used to skip the check and succeed.
func TestBootSetDryRunChecksThePXEHost(t *testing.T) {
	t.Run("a missing boot path", func(t *testing.T) {
		p := newPXEHost(t, pxeOptions{})
		missing := filepath.Join(p.root, "boot/cluster/1.O/exe/ipxe.net2")
		_, err := p.run(t, harnessOptions{}, "--dry-run", "boot", "set", "-n", "exe0001", missing)
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Fatalf("exit code = %d, want %d (%v)", got, want, err)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error = %v, want it to name the path", err)
		}
		if len(p.changes()) != 0 {
			t.Errorf("a dry run changed the PXE host: %q", p.changes())
		}
	})

	t.Run("a persistent link in the way", func(t *testing.T) {
		p := newPXEHost(t, pxeOptions{})
		p.link(t, "10.0.2.1.static", p.exePath())
		_, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "--dry-run", "boot", "set", "-n", "exe0001")
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Fatalf("exit code = %d, want %d (%v)", got, want, err)
		}
		if !strings.Contains(err.Error(), "persistent") {
			t.Errorf("error = %v, want it to name the persistent link", err)
		}
		if p.exists("10.0.2.1") || len(p.changes()) != 0 {
			t.Errorf("a dry run changed the PXE host: %q", p.changes())
		}
	})

	t.Run("a boot path that exists", func(t *testing.T) {
		p := newPXEHost(t, pxeOptions{})
		h, err := p.run(t, harnessOptions{}, "--dry-run", "boot", "set", "-n", "exe0001")
		if err != nil {
			t.Fatalf("the dry run failed: %v\n%s", err, h.errOut)
		}
		if strings.Contains(h.errOut.String(), "not checked") {
			t.Errorf("the preview says something was not checked:\n%s", h.errOut)
		}
		if len(p.scripts()) == 0 {
			t.Error("the dry run did not read the PXE host")
		}
		if p.exists("10.0.2.1") || len(p.changes()) != 0 {
			t.Errorf("a dry run changed the PXE host: %q", p.changes())
		}
	})
}

// 10.3: the log is read into memory whole, so the count is bounded.
func TestBootLogBoundsTheLines(t *testing.T) {
	for _, lines := range []string{"0", "-5", "100000000"} {
		h, err := run(t, harnessOptions{}, "boot", "log", "--lines", lines)
		if err == nil {
			t.Errorf("--lines %s should be refused", lines)
			continue
		}
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("--lines %s: exit code = %d, want %d", lines, got, want)
		}
		if len(h.recorder.Calls()) != 0 {
			t.Errorf("--lines %s sent %q", lines, h.recorder.Commands())
		}
	}
}

// 10.7: a node the status cannot be read for is an error, in every format.
func TestBootStatusReportsUnknownNodes(t *testing.T) {
	p := newPXEHost(t, pxeOptions{})
	p.link(t, "10.0.2.1", p.exePath())

	h, err := p.run(t, harnessOptions{}, "-o", "json", "boot", "status", "-n", "exe1,nosuchnode")
	if err == nil {
		t.Fatal("a node with no known address should fail the command")
	}
	var got map[string]struct {
		BootPath string `json:"bootPath"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, h.out)
	}
	if got["exe0001"].BootPath != p.exePath() {
		t.Errorf("exe0001 = %+v, want its boot path", got["exe0001"])
	}
	if got["nosuchnode"].Error == "" {
		t.Errorf("nosuchnode = %+v, want an error", got["nosuchnode"])
	}
}

// 2.16: a pull changes what every node boots.
func TestBootSyncGoesThroughTheGate(t *testing.T) {
	h, err := run(t, harnessOptions{}, "boot", "sync")
	if err == nil || !strings.Contains(err.Error(), "-y") {
		t.Errorf("boot sync without a terminal should ask for -y, got %v", err)
	}
	if len(h.recorder.Calls()) != 0 {
		t.Errorf("boot sync sent %q unasked", h.recorder.Commands())
	}

	h, err = run(t, harnessOptions{tty: true, stdin: "n\n"}, "boot", "sync")
	if err == nil || len(h.recorder.Calls()) != 0 {
		t.Errorf("a declined boot sync should send nothing, got %v and %q", err, h.recorder.Commands())
	}

	h, err = run(t, harnessOptions{}, "boot", "sync", "-y")
	if err != nil {
		t.Fatalf("boot sync -y failed: %v", err)
	}
	if commands := h.recorder.Commands(); len(commands) != 1 || !strings.Contains(commands[0], "git -C /srv/pxesrv/boot pull --ff-only") {
		t.Errorf("commands = %q, want the pull", commands)
	}
}
