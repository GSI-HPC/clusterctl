// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// PanicLog is where the stack of a recovered panic is written: the
// process's standard error, which under clusterctl mcp is the server's log.
var PanicLog io.Writer = os.Stderr

var panicLogMu sync.Mutex

// Recovered turns a panic in the work for one target into that target's
// error, and writes the stack to PanicLog. It is called as
// Recovered(name, recover()) in a function deferred by the goroutine doing
// the work, and returns nil when nothing panicked.
//
// recover only stops a panic in its own goroutine, so each worker of a
// fan-out needs its own; without it, a panic on one target ends the
// process, and under clusterctl mcp every plan waiting to be applied.
func Recovered(target string, v any) error {
	if v == nil {
		return nil
	}
	panicked := fmt.Sprint(v)
	panicLogMu.Lock()
	// The log is a courtesy; a write that fails changes nothing.
	_, _ = fmt.Fprintf(PanicLog, "clusterctl: panic while working on %s: %q\n%s", target, panicked, debug.Stack())
	panicLogMu.Unlock()
	return exitcode.Errorf(exitcode.TargetFailed, "clusterctl panicked; this is a bug, please report it: %q", panicked)
}
