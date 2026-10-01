// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
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
		cmd.Stdin = req.Stdin
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
	return req.Script == bootPathCheckScript
}

// linkRecordsOf reads the records a link script is given on standard input,
// each of the given number of fields.
func linkRecordsOf(req transport.Request, fields int) [][]string {
	if req.Stdin == nil {
		return nil
	}
	data, err := io.ReadAll(req.Stdin)
	if err != nil {
		panic(err)
	}
	parts := strings.Split(string(data), "\x00")
	if parts[len(parts)-1] != "" || (len(parts)-1)%fields != 0 {
		panic(fmt.Sprintf("the records %q are not of %d fields each", data, fields))
	}
	var out [][]string
	for i := 0; i+fields <= len(parts)-1; i += fields {
		out = append(out, parts[i:i+fields])
	}
	return out
}

// reportLinks answers a link script as a PXE host that changed every link
// it was given.
func reportLinks(req transport.Request) string {
	var out strings.Builder
	for _, r := range linkRecordsOf(req, 3) {
		out.WriteString("ok\t" + r[0] + "\n")
	}
	return out.String()
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

// The links travel to the PXE host as NUL-terminated fields, so a boot path
// arrives exactly as configured whatever it holds: a space, a quote, a word
// a shell would expand, even a line break.
func TestBootLinksArriveExactlyWhateverThePathHolds(t *testing.T) {
	p := newPXEHost(t, pxeOptions{})
	odd := filepath.Join(p.root, "boot", "it's \"odd\"\n$HOME;*", "ipxe.net2")
	mustWrite(t, odd, "#!ipxe\n")
	quoted, err := json.Marshal(odd)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(p.layer, "extra.yaml"), `apiVersion: clusterctl/v1alpha1
kind: NodeInventory
metadata:
  name: zz-extra
spec:
  nodes:
    - nodes: exe0002
      address: 10.0.2.2
      bootPath: `+string(quoted)+"\n")

	for _, mode := range []struct {
		flags []string
		link  string
	}{{nil, "10.0.2.2"}, {[]string{"--persistent"}, "10.0.2.2.static"}} {
		args := append([]string{"--set", staticSuffix, "boot", "set", "-y", "-n", "exe0002"}, mode.flags...)
		h, err := p.run(t, harnessOptions{}, args...)
		if err != nil {
			t.Fatalf("boot set %v failed: %v\n%s", mode.flags, err, h.errOut)
		}
		if got := p.target(mode.link); got != odd {
			t.Errorf("boot set %v linked %s to %q, want %q", mode.flags, mode.link, got, odd)
		}
	}
	h, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "boot", "unset", "-y", "-n", "exe0002")
	if err != nil {
		t.Fatalf("boot unset failed: %v\n%s", err, h.errOut)
	}
	for _, name := range []string{"10.0.2.2", "10.0.2.2.static"} {
		if p.exists(name) {
			t.Errorf("%s is still there after boot unset", name)
		}
	}
}

// The links went into the script, one argument to ssh, which the kernel
// caps at 128 KiB. boot set was refused above some 1,650 nodes, and boot
// unset with a static suffix above some 1,940, after the confirmation;
// between the two, a reinstall armed nodes its rollback could not disarm.
// The script is now the same for any number of nodes.
func TestBootLinksReachThousandsOfNodes(t *testing.T) {
	const n = 2000
	var entries strings.Builder
	for i := range n {
		fmt.Fprintf(&entries, "    - nodes: big%04d\n      address: 10.9.%d.%d\n      bootPath: /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2\n",
			i+1, i/250, i%250+1)
	}
	layer := t.TempDir()
	mustWrite(t, filepath.Join(layer, "extra.yaml"), `apiVersion: clusterctl/v1alpha1
kind: NodeInventory
metadata:
  name: zz-extra
spec:
  nodes:
`+entries.String())

	var mu sync.Mutex
	sent := map[string][]int{}
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if req.Script != bootLinkScript && req.Script != bootUnlinkScript {
			return &transport.Result{Target: tg}, nil
		}
		records := linkRecordsOf(req, 3)
		mu.Lock()
		sent[req.Script] = append(sent[req.Script], len(records))
		mu.Unlock()
		var out strings.Builder
		for _, r := range records {
			out.WriteString("ok\t" + r[0] + "\n")
		}
		return &transport.Result{Target: tg, Stdout: out.String()}, nil
	}}
	for _, verb := range []string{"set", "unset"} {
		h, err := run(t, harnessOptions{recorder: rec, config: []string{layer}},
			"--set", staticSuffix, "boot", verb, "-y", "-n", fmt.Sprintf("big[0001-%04d]", n))
		if err != nil {
			t.Fatalf("boot %s of %d nodes failed: %v\n%s", verb, n, err, h.errOut)
		}
	}
	for script, name := range map[string]string{bootLinkScript: "set", bootUnlinkScript: "unset"} {
		if got := sent[script]; !slices.Equal(got, []int{n}) {
			t.Errorf("boot %s sent links in %v, want all %d in one request", name, got, n)
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

// Without a node set, boot status listed the links in the order of a map,
// which changes from one run to the next, with the NODE column always
// empty and a persistent link on a row of its own under its file name. It
// lists one row per address now, in the order of the addresses, with the
// one-shot and the persistent link side by side and the inventory node
// whose boot address it is, from the inventory or from DHCP.
func TestBootStatusListsEveryLinkInOrderWithItsNode(t *testing.T) {
	p := newPXEHost(t, pxeOptions{dhcp: "host exe0002 {\n  fixed-address 10.0.2.2;\n}\n"})
	for _, name := range []string{"10.0.2.10", "10.0.2.1.static", "10.0.2.2", "10.0.2.1", "default"} {
		p.link(t, name, p.exePath())
	}

	path := p.exePath()
	want := []addressLinks{
		{[]string{"exe0001"}, "10.0.2.1", path, path},
		{[]string{"exe0002"}, "10.0.2.2", path, "none"},
		{nil, "10.0.2.10", path, "none"},
		{nil, "default", path, "none"},
	}
	// A map's order changes from one run to the next, so one run in order
	// proves little.
	for range 3 {
		h, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "-o", "json", "boot", "status")
		if err != nil {
			t.Fatalf("boot status: %v\n%s", err, h.errOut)
		}
		var got []addressLinks
		if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, h.out)
		}
		if !slices.EqualFunc(got, want, func(a, b addressLinks) bool {
			return slices.Equal(a.Nodes, b.Nodes) && a.Address == b.Address &&
				a.BootPath == b.BootPath && a.Persistent == b.Persistent
		}) {
			t.Fatalf("boot status -o json =\n%+v\nwant\n%+v", got, want)
		}
	}

	h, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "boot", "status")
	if err != nil {
		t.Fatalf("boot status: %v", err)
	}
	var addresses []string
	for line := range strings.SplitSeq(h.out.String(), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 4 && strings.HasPrefix(fields[0], "exe"):
			addresses = append(addresses, fields[0]+" "+fields[1])
		case len(fields) == 3 && fields[1] == path:
			addresses = append(addresses, fields[0])
		}
	}
	if got, want := strings.Join(addresses, ", "), "exe0001 10.0.2.1, exe0002 10.0.2.2, 10.0.2.10, default"; got != want {
		t.Errorf("the table lists %s, want %s:\n%s", got, want, h.out)
	}
}

// The node of an address that DHCP gives cannot be named while DHCP cannot
// be read: the links are listed with the nodes that are known, and the
// command fails with the reason.
func TestBootStatusFailsWhenDHCPCannotNameTheNodes(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "find" {
			return &transport.Result{Target: tg, Stdout: "10.0.2.2\t/srv/pxesrv/boot/exe/ipxe.net2\n" +
				"10.0.2.1\t/srv/pxesrv/boot/exe/ipxe.net2\n"}, nil
		}
		return transport.ExitResult(tg, 255, "", "ssh: connect to host dhcp01.example.org port 22: Connection refused\n"), nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "-o", "json", "boot", "status")
	wantCode(t, err, exitcode.Transport)
	if err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Errorf("error = %v, want the reason DHCP could not be read", err)
	}
	var got []addressLinks
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, h.out)
	}
	if len(got) != 2 || got[0].Address != "10.0.2.1" || !slices.Equal(got[0].Nodes, []string{"exe0001"}) ||
		got[1].Address != "10.0.2.2" || got[1].Nodes != nil {
		t.Errorf("boot status -o json = %+v, want both links, exe0001 named from the inventory", got)
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

// boot set checked that the boot paths exist and only then listed the
// links, one session to the PXE host after the other. The two are read
// side by side: each is held until a third is under way, which never
// comes, so both are under way at once only when neither waits for the
// other. A missing boot path is still what is reported, although the
// listing found a persistent link too.
func TestBootSetChecksThePathsBesideListingTheLinks(t *testing.T) {
	p := newPXEHost(t, pxeOptions{})
	calls := &fanouttest.InFlight{Hold: 3}
	p.rec = &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		defer calls.Enter()()
		return p.reply(tg, req)
	}}
	if h, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "boot", "set", "-n", "exe0001", "-y"); err != nil {
		t.Fatalf("boot set failed: %v\n%s", err, h.errOut)
	}
	if got := calls.Peak(); got != 2 {
		t.Errorf("%d reads of the PXE host were under way at once, want the check and the listing", got)
	}

	p.link(t, "10.0.2.1.static", p.exePath())
	if err := os.Remove(p.exePath()); err != nil {
		t.Fatal(err)
	}
	_, err := p.run(t, harnessOptions{}, "--set", staticSuffix, "boot", "set", "-n", "exe0001", "-y")
	if err == nil || !strings.Contains(err.Error(), "no boot configuration exists") {
		t.Errorf("error = %v, want the missing boot path", err)
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
			wantCode(t, err, exitcode.Usage)
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
	wantCode(t, err, exitcode.Usage)

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
	wantCode(t, err, exitcode.TargetFailed)
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

// boot grub show looked its argument up as it was written, while boot grub
// set resolves it the way -n is: EXE0001 and the host name had no address,
// exe1 was shown as exe1 rather than as exe0001, and exe[1-2] was taken for
// one node without an address instead of being refused as two.
func TestBootGrubShowResolvesTheNodeAsSetDoes(t *testing.T) {
	for _, arg := range []string{"exe0001", "exe1", "EXE0001", "exe0001.hpc.example.org"} {
		t.Run(arg, func(t *testing.T) {
			h, err := run(t, harnessOptions{}, "-o", "json", "boot", "grub", "show", arg)
			if err != nil {
				t.Fatalf("boot grub show %s: %v", arg, err)
			}
			var got map[string]string
			if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, h.out)
			}
			want := map[string]string{"node": "exe0001", "address": "10.0.2.1", "grubFile": "grub.cfg-0A000201"}
			if !maps.Equal(got, want) {
				t.Errorf("boot grub show %s = %v, want %v", arg, got, want)
			}
			wantNoCalls(t, h)
		})
	}

	for arg, want := range map[string]string{"exe[1-2]": "2 nodes", "": "no node"} {
		h, err := run(t, harnessOptions{}, "boot", "grub", "show", arg)
		wantCode(t, err, exitcode.Usage)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("boot grub show %q: %v, want it to say %q", arg, err, want)
		}
		wantNoCalls(t, h)
	}
}

// TestBootGrubSetChecksTheTarget covers services.tftp.root, which #90 found
// unread: boot grub set linked whatever it was given, so a mistyped target
// left a dangling link that only the node's next boot revealed, and an
// absolute link was one a TFTP server confined to its root cannot follow.
// The commands run in a real shell against a TFTP root in a temporary
// directory.
func TestBootGrubSetChecksTheTarget(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "grub/1.0/grub.cfg.install-exec"), "menuentry install {}\n")
	mustWrite(t, filepath.Join(root, "images/rescue.cfg"), "menuentry rescue {}\n")
	link := filepath.Join(root, "grub/grub.cfg-0A000201")
	grubSet := func(target string, extra ...string) (*pxeHost, *harness, error) {
		p := newPXEHost(t, pxeOptions{})
		h, err := p.run(t, harnessOptions{}, append([]string{
			"--set", "services.tftp.root=" + root, "--set", "services.tftp.grubPath=" + root + "/grub",
			"boot", "grub", "set", "exe0001", target, "-y"}, extra...)...)
		return p, h, err
	}

	for target, want := range map[string]string{
		root + "/grub/1.0/grub.cfg.install-exec": "1.0/grub.cfg.install-exec",
		"1.0/grub.cfg.install-exec":              "1.0/grub.cfg.install-exec",
		root + "/images/rescue.cfg":              "../images/rescue.cfg",
	} {
		_ = os.Remove(link)
		if _, h, err := grubSet(target); err != nil {
			t.Fatalf("boot grub set %s: %v\n%s", target, err, h.errOut)
		}
		if got, err := os.Readlink(link); err != nil || got != want {
			t.Errorf("boot grub set %s linked %q (%v), want %q", target, got, err, want)
		}
		if _, err := os.Stat(link); err != nil {
			t.Errorf("boot grub set %s left a link that does not resolve: %v", target, err)
		}
	}

	_ = os.Remove(link)
	for _, target := range []string{"/etc/hostname", "../../../../etc/hostname", root + "/grub/1.0/nosuch.cfg", root + "/grub/1.0"} {
		for _, dryRun := range []bool{false, true} {
			var extra []string
			if dryRun {
				extra = []string{"--dry-run"}
			}
			p, _, err := grubSet(target, extra...)
			wantCode(t, err, exitcode.Usage)
			for _, c := range p.rec.Calls() {
				if slices.Contains(c.Request.Argv, "ln") {
					t.Errorf("boot grub set %s %v linked it: %q", target, extra, c.Command)
				}
			}
		}
		if _, err := os.Lstat(link); err == nil {
			t.Fatalf("boot grub set %s left a link", target)
		}
	}
}

// TestBootGrubLogShowsWhatTheTFTPServiceLogged covers services.tftp.logPath,
// which #90 found unread: no command showed whether a node had asked the
// TFTP service for its GRUB configuration. The command runs in a real shell
// against a log in a temporary directory.
func TestBootGrubLogShowsWhatTheTFTPServiceLogged(t *testing.T) {
	log := filepath.Join(t.TempDir(), "syslog")
	mustWrite(t, log, `Sep 25 10:00:01 tftp in.tftpd[101]: RRQ from 10.0.2.1 filename grub/grub.cfg-0A000201
Sep 25 10:00:02 tftp cron[7]: (root) CMD (true)
Sep 25 10:00:03 tftp in.tftpd[102]: RRQ from 10.0.2.2 filename grub/grub.cfg-0A000202
Sep 25 10:00:04 tftp in.tftpd[103]: RRQ from 10.0.2.3 filename grub/grub.cfg-0A000203
Sep 25 10:00:05 tftp tftpd-watch[8]: not the server
Sep 25 10:00:06 tftp dnsmasq-tftp[9]: sent /srv/tftp/grub/grub.cfg-0A000204 to 10.0.2.4
`)
	p := newPXEHost(t, pxeOptions{})
	logAt := func(path string, args ...string) (*harness, error) {
		return p.run(t, harnessOptions{}, append([]string{"--set", "services.tftp.logPath=" + path, "boot", "grub", "log"}, args...)...)
	}

	h, err := logAt(log, "--lines", "2")
	if err != nil {
		t.Fatalf("boot grub log: %v\n%s", err, h.errOut)
	}
	if got, want := h.out.String(), "Sep 25 10:00:04 tftp in.tftpd[103]: RRQ from 10.0.2.3 filename grub/grub.cfg-0A000203\n"+
		"Sep 25 10:00:06 tftp dnsmasq-tftp[9]: sent /srv/tftp/grub/grub.cfg-0A000204 to 10.0.2.4\n"; got != want {
		t.Errorf("boot grub log --lines 2 printed\n%s\nwant\n%s", got, want)
	}
	if calls := p.rec.Calls(); len(calls) != 1 || calls[0].Target.Name != "tftp" {
		t.Errorf("the log was read from %v, want the tftp role", calls)
	}

	h, err = logAt(log, "-o", "json")
	if err != nil {
		t.Fatalf("boot grub log -o json: %v", err)
	}
	var lines []string
	if err := json.Unmarshal(h.out.Bytes(), &lines); err != nil || len(lines) != 4 {
		t.Errorf("boot grub log -o json = %s (%v), want the 4 lines of the TFTP server", h.out, err)
	}

	_, err = logAt(filepath.Join(t.TempDir(), "nosuch"))
	wantCode(t, err, exitcode.TargetFailed)
	if err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("a log that is not there: %v, want it named", err)
	}

	calls := len(p.rec.Calls())
	_, err = logAt(log, "--lines", "0")
	wantCode(t, err, exitcode.Usage)
	if len(p.rec.Calls()) != calls {
		t.Error("boot grub log --lines 0 contacted the host")
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
