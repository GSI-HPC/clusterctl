// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
)

// panicLogMu keeps the stacks of panics in workers running side by side
// from interleaving.
var panicLogMu sync.Mutex

// Recovered turns a panic in the work for one target into that target's
// error, and writes the stack to log, the front end's diagnostics, or the
// process's standard error when log is nil. It is called as
// Recovered(log, program, name, recover()) in a function deferred by the
// goroutine doing the work, and returns a *PanicError, or nil when nothing
// panicked. program
// names the program, in front of the line in the log, as "clusterctl: ",
// and in the error, which asks for the bug to be reported; empty leaves the
// name out of the line and calls it "the program" in the error.
//
// recover only stops a panic in its own goroutine, so each worker of a
// fan-out needs its own; without it, a panic on one target ends the
// process, and with it every other target's work.
func Recovered(log io.Writer, program, target string, v any) error {
	if v == nil {
		return nil
	}
	if log == nil {
		log = os.Stderr
	}
	prefix := ""
	if program != "" {
		prefix = program + ": "
	}
	panicked := fmt.Sprint(v)
	panicLogMu.Lock()
	// The log is a courtesy; a write that fails changes nothing.
	_, _ = fmt.Fprintf(log, "%spanic while working on %s: %q\n%s", prefix, target, panicked, debug.Stack())
	panicLogMu.Unlock()
	return &PanicError{Program: program, Value: panicked}
}

// PanicError is the error a panic in the work for an item became, which
// tells it from an error the work returned.
type PanicError struct {
	// Program is the program that panicked, as Recovered was told it.
	Program string
	// Value is what the panic was called with, as fmt.Sprint prints it.
	Value string
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("%s panicked; this is a bug, please report it: %q", cmp.Or(e.Program, "the program"), e.Value)
}
