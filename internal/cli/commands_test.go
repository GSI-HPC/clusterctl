// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/config/configtest"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/secrets/sopstest"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func TestConfigContextsMarksTheCurrentOne(t *testing.T) {
	h, err := run(t, harnessOptions{}, "config", "contexts")
	if err != nil {
		t.Fatalf("config contexts failed: %v", err)
	}
	out := h.out.String()
	if !strings.Contains(out, "*") || !strings.Contains(out, "cluster1") {
		t.Errorf("the current context is not marked:\n%s", out)
	}
}

func TestConfigUseContextExplainsWhatToChange(t *testing.T) {
	h, err := run(t, harnessOptions{}, "config", "use-context", "cluster2")
	if err != nil {
		t.Fatalf("config use-context failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"CLUSTERCTL_CONTEXT=cluster2", "--context cluster2", `currentContext: "cluster2"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}

	if _, err := run(t, harnessOptions{}, "config", "use-context", "nope"); err == nil {
		t.Error("an unknown context should be reported")
	}
}

func TestConfigSchemaIsUsableByAnEditor(t *testing.T) {
	h, err := run(t, harnessOptions{}, "config", "schema", "Site")
	if err != nil {
		t.Fatalf("config schema failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"$schema", "properties", "naming"} {
		if !strings.Contains(out, want) {
			t.Errorf("the schema is missing %q", want)
		}
	}

	if _, err := run(t, harnessOptions{}, "config", "schema", "Nope"); err == nil {
		t.Error("an unknown kind should be reported")
	}
}

func TestConfigViewPrintsTheMergedConfiguration(t *testing.T) {
	h, err := run(t, harnessOptions{}, "config", "view", "-o", "yaml")
	if err != nil {
		t.Fatalf("config view failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "hpc.example.org") {
		t.Errorf("the merged configuration is missing:\n%s", h.out)
	}

	h, err = run(t, harnessOptions{}, "config", "view", "--show-sources")
	if err != nil {
		t.Fatalf("config view --show-sources failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"defaults", "site", "workstation", "LAYER"} {
		if !strings.Contains(out, want) {
			t.Errorf("the sources listing is missing %q:\n%s", want, out)
		}
	}
}

// TestConfigInitWritesAConfigurationThatResolves runs what a new
// administrator runs: init, then the commands the next steps name, against
// nothing but what init wrote.
func TestConfigInitWritesAConfigurationThatResolves(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site-config")
	h, err := run(t, harnessOptions{bare: true, config: []string{dir}},
		"config", "init", dir,
		"--site", "lab", "--cluster", "alpha", "--domain", "hpc.lab.example", "--user", "alice_adm")
	if err != nil {
		t.Fatalf("config init failed: %v", err)
	}
	out := h.out.String()
	for _, name := range []string{"config.yaml", "site.yaml", "cluster.yaml", "inventory.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
		if !strings.Contains(out, filepath.Join(dir, name)) {
			t.Errorf("the output does not list %s:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "clusterctl config validate") {
		t.Errorf("the output does not say what to run next:\n%s", out)
	}
	// --config names the directory, so there is nothing to point at it.
	if strings.Contains(out, "export CLUSTERCTL_CONFIG") {
		t.Errorf("the output asks for a directory that is read already to be named:\n%s", out)
	}

	h, err = run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "validate")
	if err != nil {
		t.Fatalf("config validate of the written files failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "4 documents are valid; context alpha resolves") {
		t.Errorf("config validate did not accept the written files:\n%s", h.out)
	}

	h, err = run(t, harnessOptions{bare: true, config: []string{dir}}, "node", "fqdn", "-n", "node[1-2]")
	if err != nil {
		t.Fatalf("node fqdn against the written files failed: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "node[1-2].hpc.lab.example"; got != want {
		t.Errorf("node fqdn = %q, want %q", got, want)
	}

	h, err = run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "explain", "defaultUser")
	if err != nil {
		t.Fatalf("config explain against the written files failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "alice_adm") {
		t.Errorf("the context user was not written:\n%s", h.out)
	}
}

// isolateHome points the user's configuration directory into a temporary
// one, so that a config init without DIR cannot reach the real one, and
// returns it.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLUSTERCTL_CONFIG", "")
	t.Setenv("CLUSTERCTL_CONTEXT", "")
	dir, err := config.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestConfigInitDefaultsToTheUserConfigDirectory(t *testing.T) {
	dir := isolateHome(t)

	h, err := run(t, harnessOptions{bare: true}, "config", "init")
	if err != nil {
		t.Fatalf("config init failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "site.yaml")); err != nil {
		t.Errorf("site.yaml was not written to %s: %v", dir, err)
	}
	// The README and the manual send the administrator to the paths
	// printed here, because the directory differs between systems.
	if !strings.Contains(h.out.String(), filepath.Join(dir, "site.yaml")) {
		t.Errorf("the output does not name the file it wrote in %s:\n%s", dir, h.out)
	}
	// The directory is on the search path, so the next steps need no
	// variable.
	if strings.Contains(h.out.String(), "export CLUSTERCTL_CONFIG") {
		t.Errorf("the output asks for the searched directory to be named:\n%s", h.out)
	}

	// And the search path finds it with nothing else set, unless this
	// machine has a site configuration of its own that is read as well.
	if _, err := os.Stat("/etc/clusterctl"); err == nil {
		return
	}
	h, err = run(t, harnessOptions{bare: true}, "config", "validate")
	if err != nil {
		t.Fatalf("config validate over the search path failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "context cluster1 resolves") {
		t.Errorf("config validate did not find the written files:\n%s", h.out)
	}
}

func TestConfigInitSaysHowToReadAnotherDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "elsewhere")
	h, err := run(t, harnessOptions{}, "config", "init", dir)
	if err != nil {
		t.Fatalf("config init failed: %v", err)
	}
	if want := "export CLUSTERCTL_CONFIG=" + dir; !strings.Contains(h.out.String(), want) {
		t.Errorf("the output does not say %q:\n%s", want, h.out)
	}
}

func TestConfigInitWritesWhereTheEnvironmentPoints(t *testing.T) {
	// Directories that are missing, parents included, are created.
	isolateHome(t)
	dir := filepath.Join(t.TempDir(), "srv", "site-config")
	t.Setenv("CLUSTERCTL_CONFIG", dir)

	h, err := run(t, harnessOptions{bare: true}, "config", "init")
	if err != nil {
		t.Fatalf("config init failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "site.yaml")); err != nil {
		t.Errorf("site.yaml was not written to %s: %v", dir, err)
	}
	if strings.Contains(h.out.String(), "export CLUSTERCTL_CONFIG") {
		t.Errorf("the output asks for the directory CLUSTERCTL_CONFIG names to be named:\n%s", h.out)
	}
	if _, err := run(t, harnessOptions{bare: true}, "config", "validate"); err != nil {
		t.Errorf("config validate of what was written failed: %v", err)
	}
}

func TestConfigInitWritesWhereConfigPoints(t *testing.T) {
	isolateHome(t)
	t.Setenv("CLUSTERCTL_CONFIG", filepath.Join(t.TempDir(), "not-this-one"))
	dir := filepath.Join(t.TempDir(), "site-config")

	if _, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "init"); err != nil {
		t.Fatalf("config init failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "site.yaml")); err != nil {
		t.Errorf("site.yaml was not written to the --config directory: %v", err)
	}
}

// TestConfigInitRefusesSeveralPlaces keeps init from writing a complete
// configuration into one layer of several, where it would change what the
// others resolve to.
func TestConfigInitRefusesSeveralPlaces(t *testing.T) {
	own := isolateHome(t)
	base := t.TempDir()
	first, second := filepath.Join(base, "site"), filepath.Join(base, "mine")
	t.Setenv("CLUSTERCTL_CONFIG", first+string(os.PathListSeparator)+second)

	_, err := run(t, harnessOptions{bare: true}, "config", "init")
	if err == nil {
		t.Fatal("config init chose one of several places")
	}
	wantCode(t, err, exitcode.Usage)
	if !strings.Contains(err.Error(), "clusterctl config init DIR") {
		t.Errorf("the error does not say how to name the directory: %v", err)
	}
	for _, dir := range []string{first, second, own} {
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("a refused config init created %s", dir)
		}
	}
}

// TestConfigInitRefusesADirectoryThatIsNotEmpty keeps init from writing next
// to anything it did not write, and from writing over it.
func TestConfigInitRefusesADirectoryThatIsNotEmpty(t *testing.T) {
	const content = "# already here\n"
	for _, name := range []string{"mine.yaml", "README.md", ".sops.yaml", ".git"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			if name == ".git" {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			for _, extra := range [][]string{nil, {"--dry-run"}} {
				_, err := run(t, harnessOptions{}, append([]string{"config", "init", dir}, extra...)...)
				if err == nil {
					t.Fatalf("config init %v wrote into a directory holding %s", extra, name)
				}
				wantCode(t, err, exitcode.Usage)
				if !strings.Contains(err.Error(), name) {
					t.Errorf("the error does not name what is there: %v", err)
				}
			}
			items, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 {
				t.Errorf("the refused directory holds %d entries, want only %s", len(items), name)
			}
			if name != ".git" {
				if data, _ := os.ReadFile(path); string(data) != content {
					t.Errorf("%s changed: %q", name, data)
				}
			}
		})
	}

	// An empty directory is taken, and a second run finds the first one's
	// files.
	dir := t.TempDir()
	if _, err := run(t, harnessOptions{}, "config", "init", dir); err != nil {
		t.Fatalf("config init into an empty directory failed: %v", err)
	}
	if _, err := run(t, harnessOptions{}, "config", "init", dir); err == nil {
		t.Error("a second config init into the same directory was not refused")
	}
}

func TestConfigInitDryRunWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site-config")
	h, err := run(t, harnessOptions{}, "config", "init", dir, "--dry-run")
	if err != nil {
		t.Fatalf("config init --dry-run failed: %v", err)
	}
	if !strings.Contains(h.out.String(), filepath.Join(dir, "site.yaml")) {
		t.Errorf("the dry run does not list what it would write:\n%s", h.out)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("the dry run created %s", dir)
	}
}

func TestConfigInitRejectsWhatCannotBeWritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site-config")
	for _, args := range [][]string{
		{"--domain", "hpc example.org"},
		{"--cluster", "my cluster"},
		{"--login", "-oProxyCommand=true"},
	} {
		_, err := run(t, harnessOptions{}, append([]string{"config", "init", dir}, args...)...)
		if err == nil {
			t.Errorf("config init %v succeeded", args)
			continue
		}
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("config init %v: exit code = %d, want %d", args, got, want)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("a rejected config init created %s", dir)
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, harnessOptions{}, "config", "init", file); err == nil {
		t.Error("config init into a file succeeded")
	}
}

func TestNodeDescribeShowsNamesAndGroups(t *testing.T) {
	h, err := run(t, harnessOptions{}, "node", "describe", "exe0001")
	if err != nil {
		t.Fatalf("node describe failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{
		"exe0001.hpc.example.org", "exe0001.mgmt.hpc.example.org", "10.0.2.1",
		"attribute.class", "groups.inventory",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestNodeAttrsAndRack(t *testing.T) {
	h, err := run(t, harnessOptions{}, "node", "attrs")
	if err != nil {
		t.Fatalf("node attrs failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "vendor") {
		t.Errorf("the attribute listing is missing a key:\n%s", h.out)
	}

	h, err = run(t, harnessOptions{}, "node", "attrs", "vendor")
	if err != nil {
		t.Fatalf("node attrs vendor failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "exe[0001-0010]") {
		t.Errorf("the values listing is missing the nodes:\n%s", h.out)
	}

	if _, err := run(t, harnessOptions{}, "node", "attrs", "nope"); err == nil {
		t.Error("an attribute no node carries should be reported")
	}

	h, err = run(t, harnessOptions{}, "node", "rack", "exe0001")
	if err != nil {
		t.Fatalf("node rack failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "R02") {
		t.Errorf("the rack of the node was not found:\n%s", h.out)
	}
	if _, err := run(t, harnessOptions{}, "node", "rack", "R99"); err == nil {
		t.Error("an unknown rack should be reported")
	}
}

func TestNodeGroupsListsTheSources(t *testing.T) {
	h, err := run(t, harnessOptions{}, "node", "groups")
	if err != nil {
		t.Fatalf("node groups failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"inventory", "rack", "static"} {
		if !strings.Contains(out, want) {
			t.Errorf("the group listing is missing %q:\n%s", want, out)
		}
	}

	h, err = run(t, harnessOptions{}, "node", "groups", "exe0001")
	if err != nil {
		t.Fatalf("node groups exe0001 failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "exe") {
		t.Errorf("the memberships of the node are missing:\n%s", h.out)
	}
}

func TestNodeHardwareParsesWhatTheNodesReport(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg,
			Stdout: "Vendor|Model X|Board Co|B1|BIOS Co|2.1|2026-01-01|MT4123 [ConnectX-6]"}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "node", "hw", "-n", "exe[1-2]", "-o", "wide")
	if err != nil {
		t.Fatalf("node hw failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"Vendor", "Model X", "ConnectX-6", "2026-01-01"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// One script per node answers everything, so a node is contacted once.
	if got, want := len(rec.Calls()), 2; got != want {
		t.Errorf("%d calls for 2 nodes, want %d", got, want)
	}
}

func TestBootStatusReadsTheLinksOnce(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: "10.0.2.1\t/srv/pxesrv/boot/exe/ipxe.net2\n"}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "boot", "status", "-n", "exe0001")
	if err != nil {
		t.Fatalf("boot status failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "ipxe.net2") {
		t.Errorf("the boot path is missing:\n%s", h.out)
	}
	// One listing answers for every node.
	if got := len(rec.Calls()); got != 1 {
		t.Errorf("%d calls, want 1", got)
	}
}

func TestBootSetUsesTheClusterRules(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if strings.Contains(req.Script, "bootlink 0") {
			return &transport.Result{Target: tg, Stdout: "ok\t0\n"}, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec, tty: true, stdin: "y\n"}, "boot", "set", "-n", "exe0001")
	if err != nil {
		t.Fatalf("boot set failed: %v", err)
	}
	commands := h.recorder.Commands()
	if len(commands) == 0 {
		t.Fatal("nothing was sent")
	}
	// The boot path from the cluster rules, linked under the node address.
	command := commands[len(commands)-1]
	if want := "bootlink 0 /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2 /srv/pxesrv/10.0.2.1"; !strings.Contains(command, want) {
		t.Errorf("command = %q, want it to contain %q", command, want)
	}
}

func TestBootGrubShowsTheHexadecimalName(t *testing.T) {
	h, err := run(t, harnessOptions{}, "boot", "grub", "show", "exe0001")
	if err != nil {
		t.Fatalf("boot grub show failed: %v", err)
	}
	// 10.0.2.1 is 0A000201.
	if !strings.Contains(h.out.String(), "grub.cfg-0A000201") {
		t.Errorf("the GRUB file name is wrong:\n%s", h.out)
	}
}

func TestFabricGUIDDerivesFromTheHardwareAddress(t *testing.T) {
	h, err := run(t, harnessOptions{}, "fabric", "guid", "-n", "exe0001")
	if err != nil {
		t.Fatalf("fabric guid failed: %v", err)
	}
	// 00:11:22:33:44:55 becomes 0x001122 0300 334455.
	if !strings.Contains(h.out.String(), "0x0011220300334455") {
		t.Errorf("the identifier is wrong:\n%s", h.out)
	}
}

func TestHCALinkParsesTheAdapterState(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: "mlx5_0|Active|LinkUp|200 Gb/sec (4X HDR)\n"}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "hca", "link", "-n", "exe0001")
	if err != nil {
		t.Fatalf("hca link failed: %v", err)
	}
	for _, want := range []string{"mlx5_0", "Active", "LinkUp"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, h.out)
		}
	}
}

func TestSecretsListShowsWhereEachFileLands(t *testing.T) {
	h, err := run(t, harnessOptions{}, "secrets", "list")
	if err != nil {
		t.Fatalf("secrets list failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"/etc/munge/munge.key", "0400", "munge:munge", "secret example/munge-key"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestCincShowReadsTheNodeConfiguration(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg,
			Stdout: "CHEF_RECIPE_URL=http://installer/cinc/latest.tgz\nCHEF_RUN_LIST=role[exe]\n"}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "cinc", "show", "-n", "exe0001")
	if err != nil {
		t.Fatalf("cinc show failed: %v", err)
	}
	for _, want := range []string{"latest.tgz", "role[exe]"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, h.out)
		}
	}
}

func TestCincConfigSendsTheFileOverStdin(t *testing.T) {
	h, err := run(t, harnessOptions{tty: true, stdin: "y\n"},
		"cinc", "config", "http://installer/cinc/latest.tgz", "-n", "exe0001", "--run-list", "role[exe]")
	if err != nil {
		t.Fatalf("cinc config failed: %v", err)
	}
	if len(h.recorder.Calls()) == 0 {
		t.Fatal("nothing was sent")
	}
	// The content travels on standard input, so it is never an argument.
	command := h.recorder.Commands()[0]
	if strings.Contains(command, "latest.tgz") {
		t.Errorf("the content was passed as an argument: %q", command)
	}
	if !strings.Contains(command, "cat >") {
		t.Errorf("command = %q, want it to read from standard input", command)
	}
}

func TestSlurmPartitionAndAccounts(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg,
			Stdout: "main|up|all|10|1-00:00:00|01:00:00|515000|128|0/128/0/128|exe[0001-0010]\n"}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "slurm", "partition")
	if err != nil {
		t.Fatalf("slurm partition failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "main") {
		t.Errorf("the partition is missing:\n%s", h.out)
	}

	rec = &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: "proj|Project|example|alice\n"}, nil
	}}
	h, err = run(t, harnessOptions{recorder: rec}, "slurm", "account", "list")
	if err != nil {
		t.Fatalf("slurm account list failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "alice") {
		t.Errorf("the coordinator is missing:\n%s", h.out)
	}
}

func TestSlurmJobSummaryCountsPerUser(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: slurm.Render(req,
			slurm.Row{"i": "1", "u": "alice", "a": "proj", "P": "main", "T": "PENDING"},
			slurm.Row{"i": "2", "u": "alice", "a": "proj", "P": "main", "T": "PENDING"},
			slurm.Row{"i": "3", "u": "bob", "a": "proj", "P": "main", "T": "PENDING"},
		)}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "slurm", "job", "summary")
	if err != nil {
		t.Fatalf("slurm job summary failed: %v", err)
	}
	out := h.out.String()
	// The busiest user comes first, which is the point of the summary.
	if strings.Index(out, "alice") > strings.Index(out, "bob") {
		t.Errorf("the counts are not ordered:\n%s", out)
	}
}

func TestSlurmUserAddChecksTheDirectoryFirst(t *testing.T) {
	// getent answering nothing means the cluster does not know the account.
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, ExitCode: 2}, nil
	}}
	_, err := run(t, harnessOptions{tty: true, stdin: "y\n", recorder: rec},
		"slurm", "user", "add", "ghost", "proj")
	if err == nil {
		t.Fatal("adding a user the cluster does not know should be refused")
	}
	wantCode(t, err, exitcode.Usage)
}

func TestPDUListDerivesTheNames(t *testing.T) {
	h, err := run(t, harnessOptions{}, "pdu", "list")
	if err != nil {
		t.Fatalf("pdu list failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "pdu0-R02.mgmt.example.org") {
		t.Errorf("the derived name is wrong:\n%s", h.out)
	}
}

func TestCopyRequiresADirectoryWhenDownloadingFromMany(t *testing.T) {
	_, err := run(t, harnessOptions{tty: true, stdin: "y\n"},
		"copy", "-n", "exe[1-4]", "--download", "/var/log/messages", "./here")
	if err == nil {
		t.Fatal("downloading from four nodes into one file should be refused")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error = %v, want it to say a directory is needed", err)
	}
}

func TestExecScriptAndCommandContradict(t *testing.T) {
	_, err := run(t, harnessOptions{}, "exec", "-n", "exe1", "--script", "true", "--", "false")
	if err == nil {
		t.Fatal("a script and a command at once should be refused")
	}
}

func TestLoginRejectsTheJumpFlag(t *testing.T) {
	// A jump host is configuration, not a flag: a role that needs one says
	// so in the site document, and every command then uses it.
	_, err := run(t, harnessOptions{}, "login", "mgmt", "-J", "other")
	if err == nil {
		t.Fatal("-J should be refused")
	}
	if !strings.Contains(err.Error(), "proxyJump") {
		t.Errorf("error = %v, want it to point at the configuration", err)
	}
}

func TestBMCWebPrintsTheURL(t *testing.T) {
	h, err := run(t, harnessOptions{}, "bmc", "web", "exe0001")
	if err != nil {
		t.Fatalf("bmc web failed: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "https://exe0001.mgmt.hpc.example.org"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestBMCForgetRemovesAPin(t *testing.T) {
	_, set := pinFile(t, "exe0001.mgmt.hpc.example.org")
	h, err := run(t, harnessOptions{}, append(set, "bmc", "forget", "-y", "exe0001")...)
	if err != nil {
		t.Fatalf("bmc forget failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "exe0001.mgmt.hpc.example.org") {
		t.Errorf("the forgotten host is not named:\n%s", h.errOut)
	}
}

func TestSecretsCheckReadsTheSecretsWithoutAKey(t *testing.T) {
	h, err := run(t, harnessOptions{}, "secrets", "check")
	if err != nil {
		t.Fatalf("secrets check failed: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"example", "secrets.sops.yaml", "2 age", "ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}

	// The example is encrypted to keys nobody has, so decrypting it fails,
	// and says so.
	h, err = run(t, harnessOptions{}, "secrets", "check", "--decrypt")
	if exitcode.From(err) != exitcode.TargetFailed {
		t.Fatalf("secrets check --decrypt: err = %v, want the failure reported", err)
	}
	if !strings.Contains(h.out.String(), "fails: ") {
		t.Errorf("the failure is not in the table:\n%s", h.out)
	}
}

// decryptableSecret copies the example configuration into a directory, with
// its Secret named example encrypted to a fresh key instead and a Workstation
// whose identities open it. The harness reads it in place of the example: a
// second Secret or Workstation of the same name would be refused.
func decryptableSecret(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := configtest.CopyDir(t, exampleDir, "secrets.sops.yaml", "workstation.yaml")
	keyFile := filepath.Join(dir, ".identity")
	secret := sopstest.Encrypt(t, "apiVersion: clusterctl/v1alpha1\nkind: Secret\nmetadata:\n  name: example\n"+
		"data:\n  bmc-password: hunter2\nbinaryData:\n  munge-key: czNjcjN0LWtleQ==\n", id.Recipient().String())
	files := map[string]string{
		".identity":         id.String() + "\n",
		"secrets.sops.yaml": string(secret),
		"workstation.yaml":  "apiVersion: clusterctl/v1alpha1\nkind: Workstation\nspec:\n  identities:\n    - " + keyFile + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSecretsPushStreamsASecretRef(t *testing.T) {
	dir := decryptableSecret(t)

	h, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "secrets", "check", "--decrypt")
	if err != nil {
		t.Fatalf("secrets check --decrypt failed: %v\n%s", err, h.out)
	}
	if !strings.Contains(h.out.String(), "decrypts") || strings.Contains(h.out.String(), "hunter2") {
		t.Errorf("the check should decrypt and print nothing of the secret:\n%s", h.out)
	}

	h, err = run(t, harnessOptions{bare: true, config: []string{dir}, tty: true, stdin: "y\n"}, "secrets", "push", "-n", "exe0001")
	// The example's nslcd keytab is a file it does not ship. Everything is
	// decrypted before anything is written, so nothing is.
	if err == nil || !strings.Contains(err.Error(), "nslcd.keytab.age") {
		t.Fatalf("secrets push: err = %v, want it to stop at the missing keytab", err)
	}
	if calls := h.recorder.Commands(); len(calls) != 0 {
		t.Fatalf("a secret was written although another could not be read: %q", calls)
	}

	// With the keytab taken from the Secret as well, the push goes through.
	override := filepath.Join(dir, "override.yaml")
	if err := os.WriteFile(override, []byte(`apiVersion: clusterctl/v1alpha1
kind: Config
contexts:
  - name: cluster1
    cluster: cluster1
    overrides:
      services.cinc.secrets:
        - target: /etc/munge/munge.key
          secretRef: {name: example, key: munge-key}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err = run(t, harnessOptions{bare: true, config: []string{dir}, tty: true, stdin: "y\n"}, "secrets", "push", "-n", "exe0001")
	if err != nil {
		t.Fatalf("secrets push failed: %v", err)
	}
	commands := h.recorder.Commands()
	if len(commands) != 1 || !strings.Contains(commands[0], "/etc/munge/munge.key") {
		t.Fatalf("commands = %q, want the munge key written", commands)
	}
	// The decoded key travels on standard input, never in the command.
	if strings.Contains(commands[0], "s3cr3t") || strings.Contains(commands[0], "czNjcjN0") {
		t.Errorf("the secret travelled in the command: %q", commands[0])
	}
}
