// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
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
	"sync"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/hostname"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// Gate decides whether a destructive action may go ahead.
type Gate struct {
	// ProtectedHosts are the node set expressions of safety.protectedHosts.
	// The hosts they name are touched by no destructive command unless
	// Force is set.
	ProtectedHosts []string
	// Resolve turns one protected host entry into the machines it names,
	// under the names the targets of an action are given. It is called the
	// first time the protected hosts are needed, not when the gate is built,
	// so that an entry naming a group asks its source only when a command
	// is about to change something. NewGate sets nodeset.Parse.
	Resolve func(expr string) (*nodeset.NodeSet, error)
	// Known are the nodes the site knows. When it is set, an action on a
	// name outside it is refused unless Force is set: a name the site does
	// not know may be another spelling of a protected machine.
	Known *nodeset.NodeSet
	// ConfirmAbove asks for the host count to be typed back when an action
	// targets more than this many hosts. At zero it is always typed.
	ConfirmAbove int
	// AssumeYes answers every prompt with yes, for -y and for scripts.
	AssumeYes bool
	// Force allows a protected host, or one the site does not know, to be
	// touched.
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

	protectOnce  sync.Once
	protected    *nodeset.NodeSet
	protectedErr error
}

// NewGate builds a gate from the safety configuration of a site. resolve
// turns a protected host entry into the machines it names; nil parses it as
// a plain node set.
//
// A limit that would switch a safeguard off is refused here, because it is
// the one place every setting of the site has passed through, --set
// included.
func NewGate(spec v1alpha1.SafetySpec, resolve func(expr string) (*nodeset.NodeSet, error)) (*Gate, error) {
	if spec.ConfirmAbove < 0 {
		return nil, fmt.Errorf("safety.confirmAbove is %d; it must be 0 or more, and 0 asks for the count every time",
			spec.ConfirmAbove)
	}
	if spec.PowerOnBatch < 1 {
		return nil, fmt.Errorf("safety.powerOnBatch is %d; it must be at least 1, or a whole rack powers on at once",
			spec.PowerOnBatch)
	}
	if resolve == nil {
		resolve = func(expr string) (*nodeset.NodeSet, error) { return nodeset.Parse(expr) }
	}
	return &Gate{
		ProtectedHosts: append([]string(nil), spec.ProtectedHosts...),
		Resolve:        resolve,
		ConfirmAbove:   spec.ConfirmAbove,
	}, nil
}

// Protected returns every protected host. It resolves the entries once, and
// an entry that cannot be resolved is an error rather than an entry that
// protects nothing.
func (g *Gate) Protected() (*nodeset.NodeSet, error) {
	g.protectOnce.Do(func() {
		protected := nodeset.New()
		for i, expr := range g.ProtectedHosts {
			resolve := g.Resolve
			if resolve == nil {
				resolve = func(expr string) (*nodeset.NodeSet, error) { return nodeset.Parse(expr) }
			}
			ns, err := resolve(expr)
			if err != nil {
				err = fmt.Errorf("safety.protectedHosts[%d] %q: %w", i, expr, err)
				// A group source that could not be reached has said so
				// with a code of its own; anything else is the
				// configuration's fault.
				var coded *exitcode.Error
				if !errors.As(err, &coded) {
					err = exitcode.Wrap(exitcode.Usage, err)
				}
				g.protectedErr = err
				return
			}
			protected = protected.Union(ns)
		}
		g.protected = protected
	})
	return g.protected, g.protectedErr
}

// ProtectedIn returns the targets that are protected hosts.
//
// A target is protected when it is a protected host, and also when its
// short name is one: the name may have been written with a domain the site
// does not use, and the gate cannot tell whether that reaches the same
// machine, so it assumes it does.
func (g *Gate) ProtectedIn(targets *nodeset.NodeSet) (*nodeset.NodeSet, error) {
	protected, err := g.Protected()
	if err != nil {
		return nil, err
	}
	hit := targets.Intersection(protected)
	if protected.IsEmpty() {
		return hit, nil
	}
	shorts := nodeset.New()
	for _, name := range protected.Expand() {
		if !hostname.IsIP(name) {
			_ = shorts.Add(strings.ToLower(short(name)))
		}
	}
	for _, name := range targets.Expand() {
		if hostname.IsIP(name) || hit.Contains(name) {
			continue
		}
		if shorts.Contains(strings.ToLower(short(name))) {
			_ = hit.Add(name)
		}
	}
	return hit, nil
}

// short returns a host name without its domain.
func short(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
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
	// NotNodes says the targets are not nodes, such as the accounting
	// database, so the inventory has nothing to say about them.
	NotNodes bool
}

// Check refuses an action that touches a protected host, or a host the site
// does not know, unless the gate was forced. When the protected hosts cannot
// be worked out, every action is refused: an entry that cannot be resolved
// must not protect nothing. --force gets past that too, since it would get
// past the protected hosts anyway, but says so.
func (g *Gate) Check(a Action) error {
	if a.Targets == nil || a.Targets.IsEmpty() {
		return exitcode.Errorf(exitcode.Usage, "no hosts were selected for %s", a.Verb)
	}
	hit, err := g.ProtectedIn(a.Targets)
	if g.Force {
		if err != nil {
			g.printf("The protected hosts could not be worked out; going ahead because --force was given: %v\n", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s was refused, because the protected hosts could not be worked out: %w", a.Verb, err)
	}
	if !hit.IsEmpty() {
		return exitcode.Errorf(exitcode.Usage,
			"%s would touch the protected host%s %s; pass --force to do it anyway",
			a.Verb, plural(hit.Len()), hit)
	}
	if g.Known != nil && !a.NotNodes {
		if unknown := a.Targets.Difference(g.Known); !unknown.IsEmpty() {
			return exitcode.Errorf(exitcode.Usage,
				"%s would touch %s, which the inventory does not know, so the gate cannot tell whether "+
					"%s protected; pass --force to do it anyway",
				a.Verb, unknown, isAre(unknown.Len()))
		}
	}
	return nil
}

func isAre(n int) string {
	if n == 1 {
		return "it is"
	}
	return "they are"
}

// Confirm runs the full gate: the protected host check, then the preview and
// the prompt. It returns nil when the action may proceed.
func (g *Gate) Confirm(a Action) error {
	p, err := g.Preview(a)
	if err != nil {
		return err
	}

	if g.DryRun {
		g.printf("Would %s\n", p.Summary())
		if p.Detail != "" {
			g.printf("  %s\n", p.Detail)
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

	g.printf("About to %s\n", p.Summary())
	if p.Detail != "" {
		g.printf("  %s\n", p.Detail)
	}
	g.printf("%s ", p.Question())
	answer, err := g.read()
	if err != nil {
		return err
	}
	return p.Accept(answer)
}

// Preview is an action that passed the checks and waits for an answer. It
// carries everything the question is asked from, so that a caller without a
// terminal can put the same question some other way and have the answer
// judged by the same rule.
type Preview struct {
	// Verb is what is being done.
	Verb string `json:"verb"`
	// Targets is the folded node set the action touches.
	Targets string `json:"targets"`
	// Count is how many hosts that is.
	Count int `json:"count"`
	// Detail is the extra line shown before the question.
	Detail string `json:"detail,omitempty"`
	// CountRequired says that a yes is not enough: the host count has to be
	// read off the preview and given back.
	CountRequired bool `json:"countRequired"`
	// ConfirmAbove is the host count above which the count is required.
	ConfirmAbove int `json:"confirmAbove,omitempty"`
}

// Preview runs the checks of the gate and describes the question it would
// ask, without asking it.
func (g *Gate) Preview(a Action) (Preview, error) {
	if err := g.Check(a); err != nil {
		return Preview{}, err
	}
	count := a.Targets.Len()
	return Preview{
		Verb:    a.Verb,
		Targets: a.Targets.String(),
		Count:   count,
		Detail:  a.Detail,
		// Above the threshold a yes is too easy to give by reflex. At zero
		// the count is always typed, which is what a cautious site means by
		// it.
		CountRequired: count > g.ConfirmAbove,
		ConfirmAbove:  g.ConfirmAbove,
	}, nil
}

// Summary says what is about to happen, for example "drain 3 hosts:
// exe[1-3]".
func (p Preview) Summary() string {
	return fmt.Sprintf("%s %d host%s: %s", p.Verb, p.Count, plural(p.Count), p.Targets)
}

// Question is what the administrator is asked.
func (p Preview) Question() string {
	if p.CountRequired && p.ConfirmAbove == 0 {
		return "Type the number of hosts to continue:"
	}
	if p.CountRequired {
		return fmt.Sprintf("This is more than %d hosts. Type the number of hosts to continue:", p.ConfirmAbove)
	}
	return "Continue? [y/N]"
}

// Accept judges an answer to the question: the host count when it is
// required, a yes otherwise. It returns nil when the action may proceed.
func (p Preview) Accept(answer string) error {
	answer = strings.TrimSpace(answer)
	if p.CountRequired {
		n, err := strconv.Atoi(answer)
		if err != nil || n != p.Count {
			return exitcode.Errorf(exitcode.Interrupted, "not confirmed, nothing was done")
		}
		return nil
	}
	switch strings.ToLower(answer) {
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

// Effect says what running a command does to the site. The command tree
// records it on every command, so that anything driving the tree on behalf
// of someone else, such as the MCP server, can tell a question from a change
// without keeping its own list.
type Effect string

const (
	// EffectRead only looks. It may contact hosts, but changes nothing on
	// them.
	EffectRead Effect = "read"
	// EffectChange changes or destroys something, or may do so depending on
	// its arguments.
	EffectChange Effect = "change"
	// EffectInteractive needs a terminal, opens a program or keeps running
	// until it is stopped.
	EffectInteractive Effect = "interactive"
)

// EffectAnnotation is the key under which a command records its effect.
const EffectAnnotation = "clusterctl.effect"

// EffectOf reads the effect a command recorded in its annotations. An
// unmarked command counts as a change, so that forgetting to classify one
// never makes it look harmless.
func EffectOf(annotations map[string]string) Effect {
	switch effect := Effect(annotations[EffectAnnotation]); effect {
	case EffectRead, EffectChange, EffectInteractive:
		return effect
	default:
		return EffectChange
	}
}
