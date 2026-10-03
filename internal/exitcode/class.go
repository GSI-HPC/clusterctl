// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package exitcode

import "github.com/GSI-HPC/go-clikit/progress"

// Class is the class of progress an error that fits none of the first
// rules of progress.Classify gets, the class of the exit code it asks
// for, as CodeClass tells. It is the fallback every Bus of clusterctl's
// is made with, progress.BusOptions.Classify, so that a span ends as the
// command would exit.
func Class(err error) progress.Class {
	return CodeClass(From(err))
}

// CodeClass is the class of work that failed with the exit code code:
// Transport is progress.ClassTransport, Usage progress.ClassUsage,
// Interrupted progress.ClassCanceled, and any other but OK, which is no
// failure, progress.ClassTarget.
func CodeClass(code int) progress.Class {
	switch code {
	case OK:
		return progress.ClassNone
	case Transport:
		return progress.ClassTransport
	case Usage:
		return progress.ClassUsage
	case Interrupted:
		return progress.ClassCanceled
	default:
		return progress.ClassTarget
	}
}
