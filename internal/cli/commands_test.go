// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/secrets/sopstest"
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
	for _, want := range []string{"CLUSTERCTL_CONTEXT=cluster2", "--context cluster2", "currentContext: cluster2"} {
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
	h, err := run(t, harnessOptions{tty: true, stdin: "y\n"}, "boot", "set", "-n", "exe0001")
	if err != nil {
		t.Fatalf("boot set failed: %v", err)
	}
	if len(h.recorder.Calls()) == 0 {
		t.Fatal("nothing was sent")
	}
	command := h.recorder.Commands()[0]
	if !strings.Contains(command, "ln -sfn /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2") {
		t.Errorf("command = %q, want the boot path from the cluster rules", command)
	}
	if !strings.Contains(command, "/srv/pxesrv/10.0.2.1") {
		t.Errorf("command = %q, want the link named after the node address", command)
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
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: strings.Join([]string{
			"1|alice|proj|main|PENDING|||1|1:00:00|0:00|1000|Priority||",
			"2|alice|proj|main|PENDING|||1|1:00:00|0:00|1000|Priority||",
			"3|bob|proj|main|PENDING|||1|1:00:00|0:00|900|Priority||",
		}, "\n")}, nil
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
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
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
	h, err := run(t, harnessOptions{}, "bmc", "forget", "exe0001")
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

// decryptableSecret writes a Secret named example, encrypted to a fresh key,
// and a Workstation whose identities open it, into a directory the harness
// reads after the example configuration.
func decryptableSecret(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
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

	h, err := run(t, harnessOptions{config: []string{dir}}, "secrets", "check", "--decrypt")
	if err != nil {
		t.Fatalf("secrets check --decrypt failed: %v\n%s", err, h.out)
	}
	if !strings.Contains(h.out.String(), "decrypts") || strings.Contains(h.out.String(), "hunter2") {
		t.Errorf("the check should decrypt and print nothing of the secret:\n%s", h.out)
	}

	h, err = run(t, harnessOptions{config: []string{dir}, tty: true, stdin: "y\n"}, "secrets", "push", "-n", "exe0001")
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
	h, err = run(t, harnessOptions{config: []string{dir}, tty: true, stdin: "y\n"}, "secrets", "push", "-n", "exe0001")
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
