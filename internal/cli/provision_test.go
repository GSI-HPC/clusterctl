// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// fakeBMCs answers the Redfish requests of every node's service processor
// and records them, as "node METHOD body".
type fakeBMCs struct {
	mu   sync.Mutex
	seen []string
	// down is the nodes whose processor refuses the connection.
	down map[string]bool
	// refuse is, per node, the request body a processor answers with a
	// 400, such as "ForceRestart" or "Disabled".
	refuse map[string]string
	// pinned is the nodes whose processor presents another certificate
	// than the one recorded.
	pinned map[string]bool
	// panics is, per node, the method or request body that makes
	// clusterctl panic while it is sent, such as "GET" or "Pxe".
	panics map[string]string
}

// newFakeBMCs puts fake processors behind the Redfish clients reinstall and
// status build, for the rest of the test.
func newFakeBMCs(t *testing.T) *fakeBMCs {
	t.Helper()
	f := &fakeBMCs{down: map[string]bool{}, refuse: map[string]string{}, pinned: map[string]bool{}, panics: map[string]string{}}
	old := provisionClient
	provisionClient = func(a *app.App, ctx context.Context, node string) (*redfish.Client, error) {
		c, err := old(a, ctx, node)
		if err != nil {
			return nil, err
		}
		c.Transport = roundTrip(func(req *http.Request) (*http.Response, error) { return f.answer(node, req) })
		return c, nil
	}
	t.Cleanup(func() { provisionClient = old })
	return f
}

type roundTrip func(*http.Request) (*http.Response, error)

func (r roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return r(req) }

func (f *fakeBMCs) answer(node string, req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		body = string(data)
	}
	f.mu.Lock()
	f.seen = append(f.seen, strings.TrimSpace(node+" "+req.Method+" "+body))
	down, refuse, pinned, panics := f.down[node], f.refuse[node], f.pinned[node], f.panics[node]
	f.mu.Unlock()
	switch {
	case down:
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	case pinned:
		// The handshake refuses the certificate before the request is
		// written.
		return nil, &redfish.PinMismatchError{Host: req.URL.Hostname(), Recorded: "SHA256:old", Seen: "SHA256:new"}
	case panics != "" && strings.Contains(req.Method+" "+body, panics):
		panic("index out of range [3] with length 3")
	}
	status, answer := http.StatusOK, ""
	switch {
	case refuse != "" && strings.Contains(body, refuse):
		status, answer = http.StatusBadRequest, `{"error":{"message":"refused"}}`
	case req.Method == http.MethodGet:
		answer = `{"PowerState":"On",
			"Boot":{"BootSourceOverrideTarget@Redfish.AllowableValues":["None","Pxe","Hdd"]},
			"Actions":{"#ComputerSystem.Reset":{"target":"/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
				"ResetType@Redfish.AllowableValues":["On","ForceOff","ForceRestart"]}}}`
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(answer)),
		Request:    req,
	}, nil
}

// changes is what was asked of the processors, leaving out the reads.
func (f *fakeBMCs) changes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.seen {
		if !strings.Contains(s, " GET") {
			out = append(out, s)
		}
	}
	return out
}

// of lists the changes sent to one node's processor, in order: "boot once",
// "reset" and "clear".
func (f *fakeBMCs) of(node string) []string {
	var out []string
	for _, s := range f.changes() {
		if !strings.HasPrefix(s, node+" ") {
			continue
		}
		switch {
		case strings.Contains(s, `"Pxe"`) && strings.Contains(s, `"Once"`):
			out = append(out, "boot once")
		case strings.Contains(s, "ForceRestart"):
			out = append(out, "reset")
		case strings.Contains(s, "Disabled"):
			out = append(out, "clear")
		default:
			out = append(out, s)
		}
	}
	return out
}

// reinstallHost is a PXE host with fake processors, a Slurm that answers
// through sinfo, and a host key file of its own.
type reinstallHost struct {
	*pxeHost
	bmcs       *fakeBMCs
	knownHosts string
	// sinfo answers the Slurm job check; nil reports every node idle.
	sinfo func(transport.Target, transport.Request) (*transport.Result, error)
}

const knownHostsLine = "exe0001.hpc.example.org ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq0bLkKBQi1uv2S9xFpd1fUzOs4R4Q9tq6lmH5TnOK9\n"

func newReinstallHost(t *testing.T, opts pxeOptions) *reinstallHost {
	t.Helper()
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "secret")
	h := &reinstallHost{pxeHost: newPXEHost(t, opts), bmcs: newFakeBMCs(t)}
	h.knownHosts = filepath.Join(t.TempDir(), "known_hosts")
	mustWrite(t, h.knownHosts, knownHostsLine)
	idle := slurmAllIdle(t).Reply
	pxe := h.rec.Reply
	h.rec.Reply = func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		switch {
		case isSinfo(req) && h.sinfo != nil:
			return h.sinfo(tg, req)
		case isSinfo(req):
			return idle(tg, req)
		case len(req.Argv) > 0 && req.Argv[0] == "uptime":
			return &transport.Result{Target: tg, Stdout: "up 3 minutes\n"}, nil
		}
		return pxe(tg, req)
	}
	return h
}

func (h *reinstallHost) run(t *testing.T, opts harnessOptions, args ...string) (*harness, error) {
	t.Helper()
	return h.pxeHost.run(t, opts, append([]string{"--set", "ssh.knownHostsFile=" + h.knownHosts}, args...)...)
}

// keysKept says whether the host key file still holds exe0001.
func (h *reinstallHost) keysKept(t *testing.T) bool {
	t.Helper()
	data, err := os.ReadFile(h.knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(data), "exe0001")
}

// untouched fails the test when anything was changed: a boot link, a
// processor or the host key file.
func (h *reinstallHost) untouched(t *testing.T) {
	t.Helper()
	if changes := h.changes(); len(changes) != 0 {
		t.Errorf("boot links were changed: %q", changes)
	}
	if changes := h.bmcs.changes(); len(changes) != 0 {
		t.Errorf("processors were changed: %q", changes)
	}
	if !h.keysKept(t) {
		t.Error("the host keys were forgotten")
	}
}

const threeNodes = "    - nodes: exe0002\n      address: 10.0.2.2\n    - nodes: exe0003\n      address: 10.0.2.3\n"

func TestReinstallReinstalls(t *testing.T) {
	h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
	out, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe[0001-0003]", "-y")
	if err != nil {
		t.Fatalf("reinstall failed: %v\n%s", err, out.errOut)
	}
	for _, node := range []string{"exe0001", "exe0002", "exe0003"} {
		if got, want := strings.Join(h.bmcs.of(node), ", "), "boot once, reset"; got != want {
			t.Errorf("%s: sent %s, want %s", node, got, want)
		}
	}
	for _, address := range []string{"10.0.2.1", "10.0.2.2", "10.0.2.3"} {
		if got := h.target(address); got != h.exePath() {
			t.Errorf("the link of %s points at %q, want %q", address, got, h.exePath())
		}
	}
	if h.keysKept(t) {
		t.Error("the host keys of exe0001 were kept")
	}
	if !strings.Contains(out.out.String(), `exe[0001-0003] is reinstalling`) {
		t.Errorf("output does not say the set is reinstalling:\n%s", out.out)
	}
}

// Section 3.3 of the September 2026 review: reinstall force-restarted nodes
// without asking Slurm, which bmc power reset refuses for a busy node.
func TestReinstallAsksSlurmFirst(t *testing.T) {
	busy := func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: "exe0001 allocated\n"}, nil
	}

	t.Run("a busy node is refused", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{})
		h.sinfo = busy
		_, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe0001", "-y")
		if err == nil {
			t.Fatal("a node running a job was reinstalled")
		}
		wantCode(t, err, exitcode.Usage)
		if !strings.Contains(err.Error(), "--lose-jobs") {
			t.Errorf("error = %v, want it to name the override", err)
		}
		h.untouched(t)
	})

	t.Run("a dry run asks too", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{})
		h.sinfo = busy
		if _, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe0001", "--dry-run"); err == nil {
			t.Fatal("the dry run approved what the real run refuses")
		}
		if sinfo, _ := sinfoCalls(h.rec); sinfo != 1 {
			t.Errorf("sinfo was sent %d times, want once", sinfo)
		}
	})

	t.Run("--force does not lose the jobs", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{})
		h.sinfo = busy
		if _, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe0001", "-y", "--force"); err == nil {
			t.Fatal("--force lost the jobs")
		}
		h.untouched(t)
	})

	t.Run("--lose-jobs goes ahead", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{})
		h.sinfo = busy
		out, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe0001", "-y", "--lose-jobs")
		if err != nil {
			t.Fatalf("reinstall failed: %v\n%s", err, out.errOut)
		}
		if got, want := strings.Join(h.bmcs.of("exe0001"), ", "), "boot once, reset"; got != want {
			t.Errorf("sent %s, want %s", got, want)
		}
	})

	// Without a reset nothing is lost, so Slurm is not asked.
	t.Run("--no-reset does not ask", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{})
		h.sinfo = busy
		out, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe0001", "-y", "--no-reset")
		if err != nil {
			t.Fatalf("reinstall --no-reset failed: %v\n%s", err, out.errOut)
		}
		if sinfo, _ := sinfoCalls(h.rec); sinfo != 0 {
			t.Errorf("sinfo was sent %d times", sinfo)
		}
		if got, want := strings.Join(h.bmcs.of("exe0001"), ", "), "boot once"; got != want {
			t.Errorf("sent %s, want %s", got, want)
		}
	})
}

// Section 5.4: reinstall resolved the BMC host and credential only after it
// had forgotten the host keys and written the boot links, and read the DHCP
// configuration a second time after the question.
func TestReinstallResolvesEverythingBeforeTheFirstChange(t *testing.T) {
	ipmiFirst := exampleWith(t, "site.yaml", func(s string) string {
		return strings.Replace(s, "order: [redfish, ipmi]", "order: [ipmi, redfish]", 1)
	})
	tests := []struct {
		name   string
		opts   pxeOptions
		config []string
		env    map[string]string
		args   []string
		code   int
		want   string
	}{
		{
			name: "a missing BMC password",
			env:  map[string]string{"BMC_PASSWORD": ""},
			args: []string{"-n", "exe0001"},
			code: exitcode.Usage,
			want: "BMC_PASSWORD",
		},
		{
			name: "a missing BMC password in a dry run",
			env:  map[string]string{"BMC_PASSWORD": ""},
			args: []string{"-n", "exe0001", "--dry-run"},
			code: exitcode.Usage,
			want: "BMC_PASSWORD",
		},
		{
			name: "a node with no address",
			args: []string{"-n", "exe[0001,0004]"},
			code: exitcode.Usage,
			want: "exe0004",
		},
		{
			name:   "a processor reached over IPMI first",
			config: []string{ipmiFirst},
			args:   []string{"-n", "exe0001"},
			code:   exitcode.Usage,
			want:   "Redfish only",
		},
		{
			name: "a missing boot path",
			opts: pxeOptions{inventory: "    - nodes: exe0002\n      address: 10.0.2.2\n"},
			args: []string{"-n", "exe[0001-0002]", "--boot-path", "/nonexistent/ipxe.net2"},
			code: exitcode.Usage,
			want: "/nonexistent/ipxe.net2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newReinstallHost(t, tc.opts)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := h.run(t, harnessOptions{config: tc.config},
				append([]string{"provision", "reinstall", "-y"}, tc.args...)...)
			if err == nil {
				t.Fatal("the reinstall went ahead")
			}
			if got := exitcode.From(err); got != tc.code {
				t.Errorf("exit code = %d, want %d (%v)", got, tc.code, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			h.untouched(t)
		})
	}
}

// The DHCP configuration is read once, before the question, even when its
// cache expires at once, and the address it gives is the one linked.
func TestReinstallReadsDHCPOnce(t *testing.T) {
	h := newReinstallHost(t, pxeOptions{
		dhcp: "host exe0002 {\n  hardware ethernet aa:bb:cc:00:00:02;\n  fixed-address 10.0.2.2;\n}\n",
	})
	out, err := h.run(t, harnessOptions{}, "--set", "services.dhcp.cacheTtl=1ns",
		"provision", "reinstall", "-n", "exe[0001-0002]", "-y")
	if err != nil {
		t.Fatalf("reinstall failed: %v\n%s", err, out.errOut)
	}
	reads := 0
	for _, c := range h.rec.Calls() {
		if len(c.Request.Argv) > 0 && c.Request.Argv[0] == "cat" {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("dhcpd.conf was read %d times, want once", reads)
	}
	if got := h.target("10.0.2.2"); got != h.exePath() {
		t.Errorf("the link of exe0002 points at %q, want %q", got, h.exePath())
	}
}

// Section 5.6: a processor that failed left the others armed, with a boot
// override and a link, and nothing said so.
func TestReinstallDisarmsWhatItArmed(t *testing.T) {
	t.Run("a boot override fails", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
		h.bmcs.down["exe0002"] = true
		out, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe[0001-0003]", "-y")
		if err == nil {
			t.Fatal("the reinstall succeeded")
		}
		wantCode(t, err, exitcode.Transport)
		for _, node := range []string{"exe0001", "exe0003"} {
			if got, want := strings.Join(h.bmcs.of(node), ", "), "boot once, clear"; got != want {
				t.Errorf("%s: sent %s, want %s", node, got, want)
			}
		}
		for _, address := range []string{"10.0.2.1", "10.0.2.2", "10.0.2.3"} {
			if h.exists(address) {
				t.Errorf("the link of %s was left", address)
			}
		}
		if !h.keysKept(t) {
			t.Error("the host keys were forgotten although nothing was reset")
		}
		// Its processor never answered, so there is no override to clear.
		if got := h.bmcs.of("exe0002"); len(got) != 0 {
			t.Errorf("exe0002: sent %q", got)
		}
		if !strings.Contains(err.Error(), "exe0002") || !strings.Contains(err.Error(), "removed again") {
			t.Errorf("error = %v, want it to name the failed node and say the rest was disarmed", err)
		}
		if strings.Contains(err.Error(), "left armed") {
			t.Errorf("error = %v, but nothing is left armed", err)
		}
		if !strings.Contains(out.out.String(), "disarmed") {
			t.Errorf("the table does not say the nodes were disarmed:\n%s", out.out)
		}
	})

	t.Run("a reset fails", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
		h.bmcs.refuse["exe0002"] = "ForceRestart"
		_, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe[0001-0003]", "-y")
		if err == nil {
			t.Fatal("the reinstall succeeded")
		}
		wantCode(t, err, exitcode.TargetFailed)
		if got, want := strings.Join(h.bmcs.of("exe0002"), ", "), "boot once, reset, clear"; got != want {
			t.Errorf("exe0002: sent %s, want %s", got, want)
		}
		if h.exists("10.0.2.2") {
			t.Error("the link of exe0002, which was not reset, was left")
		}
		for _, address := range []string{"10.0.2.1", "10.0.2.3"} {
			if !h.exists(address) {
				t.Errorf("the link of %s, which is reinstalling, was removed", address)
			}
		}
		for _, want := range []string{"exe[0001,0003] is reinstalling", "hostkey refresh -n exe0002"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to say %q", err, want)
			}
		}
	})

	t.Run("what cannot be disarmed is named", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
		h.bmcs.down["exe0002"] = true
		h.bmcs.refuse["exe0003"] = "Disabled"
		_, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe[0001-0003]", "-y")
		if err == nil {
			t.Fatal("the reinstall succeeded")
		}
		// exe0002 was never reached, so only exe0003 holds an override.
		for _, want := range []string{
			"exe0003 is left armed",
			`"clusterctl bmc boot unset -n exe0003"`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to say %q", err, want)
			}
		}
	})

	t.Run("a boot link fails", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
		// A directory where the link of exe0002 goes cannot be replaced.
		if err := os.Mkdir(filepath.Join(h.root, "10.0.2.2"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe[0001-0003]", "-y")
		if err == nil {
			t.Fatal("the reinstall succeeded")
		}
		if changes := h.bmcs.changes(); len(changes) != 0 {
			t.Errorf("processors were changed: %q", changes)
		}
		for _, address := range []string{"10.0.2.1", "10.0.2.3"} {
			if h.exists(address) {
				t.Errorf("the link of %s was left", address)
			}
		}
		if !h.keysKept(t) {
			t.Error("the host keys were forgotten")
		}
	})
}

// Each change a reinstall sends to the processors is a step with every node
// it goes to as a target, and so is disarming what a failed step armed. A
// step with nothing to send, such as disarming after a boot link failed
// before any processor was asked, is not reported at all.
func TestReinstallReportsEveryNodeOfEachStep(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *reinstallHost)
		code  int
		want  string
	}{
		{"every node reinstalls", func(*reinstallHost) {}, exitcode.OK, `
step resetting the machines total=3 limit=8 [fold]: ok
  target exe[0001-0003]: ok
step setting the machines to boot from the network once total=3 limit=8 [fold]: ok
  target exe[0001-0003]: ok
`},
		{"a boot override fails", func(h *reinstallHost) { h.bmcs.down["exe0002"] = true }, exitcode.Transport, `
step clearing the boot overrides total=2 limit=8 [fold]: ok
  target exe[0001,0003]: ok
step setting the machines to boot from the network once total=3 limit=8 [fold]: failed (transport): 1 of 3 failed: exe0002
  target exe0002: failed (transport): {}: dial tcp: connection refused
  target exe[0001,0003]: ok
`},
		{"a reset fails", func(h *reinstallHost) { h.bmcs.refuse["exe0002"] = "ForceRestart" }, exitcode.TargetFailed, `
step clearing the boot overrides total=1 limit=8 [fold]: ok
  target exe0002: ok
step resetting the machines total=3 limit=8 [fold]: failed (target): 1 of 3 failed: exe0002
  target exe0002: failed (target): {}: 400 Bad Request: refused
  target exe[0001,0003]: ok
step setting the machines to boot from the network once total=3 limit=8 [fold]: ok
  target exe[0001-0003]: ok
`},
		{"a boot link fails", func(h *reinstallHost) {
			// A directory where the link of exe0002 goes cannot be
			// replaced.
			if err := os.Mkdir(filepath.Join(h.root, "10.0.2.2"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, exitcode.TargetFailed, "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
			tc.setup(h)
			ctx, tree := watch(t)
			_, err := h.run(t, harnessOptions{ctx: ctx}, "provision", "reinstall", "-n", "exe[0001-0003]", "-y")
			wantCode(t, err, tc.code)
			if got := tree(); got != tc.want[1:] {
				t.Errorf("progress:\n%s\nwant:\n%s", got, tc.want[1:])
			}
		})
	}
}

// Section 5.12: --no-reset said the set was reinstalling.
func TestReinstallWithoutResetSaysTheSetIsArmed(t *testing.T) {
	h := newReinstallHost(t, pxeOptions{})
	out, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe0001", "-y", "--no-reset")
	if err != nil {
		t.Fatalf("reinstall failed: %v\n%s", err, out.errOut)
	}
	text := out.out.String()
	if strings.Contains(text, "is reinstalling") {
		t.Errorf("output says the set is reinstalling, but nothing was reset:\n%s", text)
	}
	for _, want := range []string{"reinstalls at its next network boot", "clusterctl bmc boot unset -n exe0001", "clusterctl boot unset -n exe0001"} {
		if !strings.Contains(text, want) {
			t.Errorf("output does not say %q:\n%s", want, text)
		}
	}
}

// A dry run of a reinstall reads the PXE host as the real run does, so it
// is refused where the real run would be: it used to skip the check and
// succeed.
func TestReinstallDryRunChecksThePXEHost(t *testing.T) {
	t.Run("a missing boot path", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{})
		_, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe0001",
			"--boot-path", "/nonexistent/ipxe.net2", "--dry-run")
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Fatalf("exit code = %d, want %d (%v)", got, want, err)
		}
		if !strings.Contains(err.Error(), "/nonexistent/ipxe.net2") {
			t.Errorf("error = %v, want it to name the path", err)
		}
		h.untouched(t)
	})

	t.Run("a persistent link in the way", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{})
		h.link(t, "10.0.2.1.static", h.exePath())
		_, err := h.run(t, harnessOptions{}, "--set", staticSuffix,
			"provision", "reinstall", "-n", "exe0001", "--dry-run")
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Fatalf("exit code = %d, want %d (%v)", got, want, err)
		}
		if !strings.Contains(err.Error(), "persistent") {
			t.Errorf("error = %v, want it to name the persistent link", err)
		}
		h.untouched(t)
	})
}

// Section 5.9: the preview names each boot path with its nodes, not only
// the set.
func TestReinstallPreviewListsEveryBootPath(t *testing.T) {
	h := newReinstallHost(t, pxeOptions{})
	out, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe0001,dbm01", "--force", "--dry-run")
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out.errOut)
	}
	preview := out.errOut.String()
	for _, want := range []string{
		h.exePath() + ", for the next request: exe0001 (10.0.2.1)",
		filepath.Join(h.root, "boot/cluster/1.0/dbm01/ipxe.net2") + ", for the next request: dbm01 (10.0.1.2)",
	} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview does not say %q:\n%s", want, preview)
		}
	}
	if strings.Contains(preview, "not checked") {
		t.Errorf("the preview says something was not checked:\n%s", preview)
	}
	h.untouched(t)
}

// Section 10.7: provision status showed a processor that failed as power
// unknown and exited 0.
func TestProvisionStatusReportsEveryNode(t *testing.T) {
	h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
	h.link(t, "10.0.2.1", h.exePath())
	h.bmcs.down["exe0002"] = true
	out, err := h.run(t, harnessOptions{}, "-o", "json", "provision", "status", "-n", "exe[0001-0003]")
	if err == nil {
		t.Fatal("status succeeded although a processor could not be reached")
	}
	wantCode(t, err, exitcode.Transport)
	var states []provisionState
	if err := json.Unmarshal(out.out.Bytes(), &states); err != nil {
		t.Fatalf("output is not a list of nodes: %v\n%s", err, out.out)
	}
	if len(states) != 3 {
		t.Fatalf("got %d nodes, want 3: %s", len(states), out.out)
	}
	if s := states[0]; s.BootPath != h.exePath() || s.Power != "On" || !s.SSH || s.Error != "" {
		t.Errorf("exe0001 = %+v", s)
	}
	if s := states[1]; s.Power != "" || s.Error == "" {
		t.Errorf("exe0002 = %+v, want an error and no power state", s)
	}
	if s := states[2]; s.BootPath != "none" {
		t.Errorf("exe0003 = %+v, want no boot path", s)
	}
}
