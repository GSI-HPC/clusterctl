// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package safety_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func gate(t *testing.T, answer string) (*safety.Gate, *bytes.Buffer) {
	t.Helper()
	g, err := safety.NewGate(v1alpha1.SafetySpec{
		ProtectedHosts: []string{"wlm01", "dbm01"},
		ConfirmAbove:   4,
	})
	if err != nil {
		t.Fatalf("NewGate failed: %v", err)
	}
	out := &bytes.Buffer{}
	g.In = strings.NewReader(answer)
	g.Out = out
	g.Interactive = true
	return g, out
}

func action(verb, nodes string) safety.Action {
	return safety.Action{Verb: verb, Targets: nodeset.MustParse(nodes)}
}

func TestProtectedHostsAreRefused(t *testing.T) {
	t.Parallel()

	g, _ := gate(t, "y\n")
	err := g.Confirm(action("power off", "exe[1-2],wlm01"))
	if err == nil {
		t.Fatal("touching a protected host should be refused")
	}
	if !strings.Contains(err.Error(), "wlm01") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("error = %v, want it to name the host and the way out", err)
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}

	g, _ = gate(t, "y\n")
	g.Force = true
	if err := g.Confirm(action("power off", "wlm01")); err != nil {
		t.Errorf("--force should allow it, got %v", err)
	}
}

func TestSmallActionAsksYesNo(t *testing.T) {
	t.Parallel()

	g, out := gate(t, "y\n")
	if err := g.Confirm(action("power off", "exe[1-2]")); err != nil {
		t.Errorf("a confirmed action should proceed, got %v", err)
	}
	if !strings.Contains(out.String(), "exe[1-2]") {
		t.Errorf("the preview does not name the hosts:\n%s", out)
	}

	g, _ = gate(t, "n\n")
	err := g.Confirm(action("power off", "exe[1-2]"))
	if err == nil {
		t.Fatal("a declined action should not proceed")
	}
	if got, want := exitcode.From(err), exitcode.Interrupted; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
}

func TestLargeActionAsksForTheCount(t *testing.T) {
	t.Parallel()

	// Above the threshold a reflexive "y" is not enough: the count has to be
	// read off the preview and typed back.
	g, out := gate(t, "y\n")
	if err := g.Confirm(action("power off", "exe[1-10]")); err == nil {
		t.Error("a bare yes should not confirm a large action")
	}
	if !strings.Contains(out.String(), "Type the number of hosts") {
		t.Errorf("the prompt does not ask for the count:\n%s", out)
	}

	g, _ = gate(t, "10\n")
	if err := g.Confirm(action("power off", "exe[1-10]")); err != nil {
		t.Errorf("typing the right count should confirm, got %v", err)
	}

	g, _ = gate(t, "9\n")
	if err := g.Confirm(action("power off", "exe[1-10]")); err == nil {
		t.Error("the wrong count should not confirm")
	}
}

func TestAssumeYesAndDryRun(t *testing.T) {
	t.Parallel()

	g, _ := gate(t, "")
	g.AssumeYes = true
	if err := g.Confirm(action("power off", "exe[1-100]")); err != nil {
		t.Errorf("-y should confirm without a prompt, got %v", err)
	}

	g, out := gate(t, "")
	g.DryRun = true
	err := g.Confirm(safety.Action{
		Verb:    "power off",
		Targets: nodeset.MustParse("exe[1-3]"),
		Detail:  "ipmipower --off",
	})
	if !safety.IsDryRun(err) {
		t.Errorf("a dry run should report itself, got %v", err)
	}
	if !strings.Contains(out.String(), "Would power off 3 hosts") {
		t.Errorf("the dry run does not describe the action:\n%s", out)
	}
	if !strings.Contains(out.String(), "ipmipower --off") {
		t.Errorf("the dry run does not show what would run:\n%s", out)
	}
}

func TestNonInteractiveWithoutYesIsRefused(t *testing.T) {
	t.Parallel()

	g, _ := gate(t, "")
	g.Interactive = false
	err := g.Confirm(action("power off", "exe1"))
	if err == nil {
		t.Fatal("a destructive action in a script must not proceed unasked")
	}
	if !strings.Contains(err.Error(), "-y") {
		t.Errorf("error = %v, want it to mention -y", err)
	}
}

func TestEmptyTargetsAreRefused(t *testing.T) {
	t.Parallel()

	g, _ := gate(t, "y\n")
	err := g.Confirm(safety.Action{Verb: "power off", Targets: nodeset.New()})
	if err == nil {
		t.Error("an action with no hosts should be refused")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
}

func TestNewGateRejectsBadProtectedHosts(t *testing.T) {
	t.Parallel()

	if _, err := safety.NewGate(v1alpha1.SafetySpec{ProtectedHosts: []string{"exe["}}); err == nil {
		t.Error("a malformed protected host expression should be reported")
	}
}

func TestPreviewAsksWithoutPrompting(t *testing.T) {
	t.Parallel()

	g, out := gate(t, "")
	g.Interactive = false

	small, err := g.Preview(safety.Action{Verb: "drain", Targets: nodeset.MustParse("exe[1-3]"), Detail: "reason: DIMM"})
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}
	if small.CountRequired || small.Count != 3 || small.Targets != "exe[1-3]" {
		t.Errorf("preview = %+v, want 3 hosts exe[1-3] without the count", small)
	}
	if got, want := small.Summary(), "drain 3 hosts: exe[1-3]"; got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
	if out.Len() != 0 {
		t.Errorf("a preview printed something:\n%s", out)
	}

	large, err := g.Preview(action("drain", "exe[1-10]"))
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}
	if !large.CountRequired || !strings.Contains(large.Question(), "more than 4 hosts") {
		t.Errorf("preview = %+v, question %q; want the count required above 4", large, large.Question())
	}

	if _, err := g.Preview(action("drain", "exe1,wlm01")); err == nil {
		t.Error("a preview touching a protected host should be refused")
	}
}

func TestAcceptJudgesAnswersLikeThePrompt(t *testing.T) {
	t.Parallel()

	small := safety.Preview{Verb: "drain", Targets: "exe[1-2]", Count: 2}
	large := safety.Preview{Verb: "drain", Targets: "exe[1-10]", Count: 10, CountRequired: true, ConfirmAbove: 4}
	tests := []struct {
		preview safety.Preview
		answer  string
		ok      bool
	}{
		{small, "y", true},
		{small, " YES\n", true},
		{small, "n", false},
		{small, "", false},
		{large, "10", true},
		{large, "10\n", true},
		{large, "yes", false},
		{large, "9", false},
	}
	for _, tc := range tests {
		err := tc.preview.Accept(tc.answer)
		if (err == nil) != tc.ok {
			t.Errorf("Accept(%q) on %d hosts = %v, want ok %v", tc.answer, tc.preview.Count, err, tc.ok)
		}
		if err != nil && exitcode.From(err) != exitcode.Interrupted {
			t.Errorf("Accept(%q) exit code = %d, want %d", tc.answer, exitcode.From(err), exitcode.Interrupted)
		}
	}
}

func TestEffectOfTreatsUnknownAsChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		annotations map[string]string
		want        safety.Effect
	}{
		{nil, safety.EffectChange},
		{map[string]string{safety.EffectAnnotation: "read"}, safety.EffectRead},
		{map[string]string{safety.EffectAnnotation: "interactive"}, safety.EffectInteractive},
		{map[string]string{safety.EffectAnnotation: "harmless"}, safety.EffectChange},
	}
	for _, tc := range tests {
		if got := safety.EffectOf(tc.annotations); got != tc.want {
			t.Errorf("EffectOf(%v) = %q, want %q", tc.annotations, got, tc.want)
		}
	}
}
