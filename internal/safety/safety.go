// SPDX-License-Identifier: LGPL-3.0-or-later

// Package safety gates the commands that change or destroy something.
//
// The shell toolkit had no preview, no confirmation and no dry run, and one
// tool scheduled a reboot when it was asked for its help text. Every command
// that powers off, reinstalls, drains or overwrites goes through here first.
package safety

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// Gate decides whether a destructive action may go ahead.
type Gate struct {
	// Protected are the hosts no destructive command touches unless Force
	// is set.
	Protected *nodeset.NodeSet
	// ConfirmAbove asks for the host count to be typed back when an action
	// targets more than this many hosts.
	ConfirmAbove int
	// AssumeYes answers every prompt with yes, for -y and for scripts.
	AssumeYes bool
	// Force allows a protected host to be touched.
	Force bool
	// DryRun reports what would happen and does nothing.
	DryRun bool
	// Interactive says whether a prompt can be answered at all. When it is
	// false and AssumeYes is not set, an action is refused rather than
	// silently carried out.
	Interactive bool

	// In and Out are the terminal the prompt uses.
	In  io.Reader
	Out io.Writer
}

// NewGate builds a gate from the safety configuration of a site.
func NewGate(spec v1alpha1.SafetySpec) (*Gate, error) {
	protected := nodeset.New()
	for i, expr := range spec.ProtectedHosts {
		ns, err := nodeset.Parse(expr)
		if err != nil {
			return nil, fmt.Errorf("safety.protectedHosts[%d]: %w", i, err)
		}
		protected = protected.Union(ns)
	}
	return &Gate{Protected: protected, ConfirmAbove: spec.ConfirmAbove}, nil
}

// Action describes what is about to happen, for the prompt and the preview.
type Action struct {
	// Verb is what is being done, "power off" or "reinstall".
	Verb string
	// Targets are the hosts it happens to.
	Targets *nodeset.NodeSet
	// Detail is an extra line shown before the prompt, such as the command
	// that will run.
	Detail string
}

// Check refuses an action that touches a protected host, unless the gate was
// forced.
func (g *Gate) Check(a Action) error {
	if a.Targets == nil || a.Targets.IsEmpty() {
		return exitcode.Errorf(exitcode.Usage, "no hosts were selected for %s", a.Verb)
	}
	if g.Force || g.Protected == nil || g.Protected.IsEmpty() {
		return nil
	}
	hit := a.Targets.Intersection(g.Protected)
	if hit.IsEmpty() {
		return nil
	}
	return exitcode.Errorf(exitcode.Usage,
		"%s would touch the protected host%s %s; pass --force to do it anyway",
		a.Verb, plural(hit.Len()), hit)
}

// Confirm runs the full gate: the protected host check, then the preview and
// the prompt. It returns nil when the action may proceed.
func (g *Gate) Confirm(a Action) error {
	if err := g.Check(a); err != nil {
		return err
	}
	count := a.Targets.Len()

	if g.DryRun {
		g.printf("Would %s %d host%s: %s\n", a.Verb, count, plural(count), a.Targets)
		if a.Detail != "" {
			g.printf("  %s\n", a.Detail)
		}
		return ErrDryRun
	}
	if g.AssumeYes {
		return nil
	}
	if !g.Interactive {
		return exitcode.Errorf(exitcode.Usage,
			"%s needs a confirmation but there is no terminal to ask on; pass -y to confirm in advance", a.Verb)
	}

	g.printf("About to %s %d host%s: %s\n", a.Verb, count, plural(count), a.Targets)
	if a.Detail != "" {
		g.printf("  %s\n", a.Detail)
	}

	// Above the threshold a yes is too easy to type by reflex, so the count
	// has to be read off the preview and typed back.
	if g.ConfirmAbove > 0 && count > g.ConfirmAbove {
		g.printf("This is more than %d hosts. Type the number of hosts to continue: ", g.ConfirmAbove)
		answer, err := g.read()
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(strings.TrimSpace(answer))
		if err != nil || n != count {
			return exitcode.Errorf(exitcode.Interrupted, "not confirmed, nothing was done")
		}
		return nil
	}

	g.printf("Continue? [y/N] ")
	answer, err := g.read()
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return exitcode.Errorf(exitcode.Interrupted, "not confirmed, nothing was done")
	}
}

// ErrDryRun is returned by Confirm when the gate is in dry run mode. A
// command treats it as "stop here, successfully".
var ErrDryRun = errDryRun{}

type errDryRun struct{}

func (errDryRun) Error() string { return "dry run: nothing was done" }

// IsDryRun reports whether an error is the dry run signal.
func IsDryRun(err error) bool {
	var signal errDryRun
	return errors.As(err, &signal)
}

func (g *Gate) read() (string, error) {
	if g.In == nil {
		return "", exitcode.Errorf(exitcode.Interrupted, "nothing to read a confirmation from")
	}
	line, err := bufio.NewReader(g.In).ReadString('\n')
	if err != nil && line == "" {
		return "", exitcode.Errorf(exitcode.Interrupted, "not confirmed, nothing was done")
	}
	return line, nil
}

func (g *Gate) printf(format string, args ...any) {
	if g.Out == nil {
		return
	}
	// A prompt that cannot be written is reported by the read that follows.
	_, _ = fmt.Fprintf(g.Out, format, args...)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
