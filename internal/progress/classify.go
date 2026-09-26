// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress

import (
	"context"
	"errors"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// Classify tells why work failed from its error. The first rule that
// applies wins:
//
//  1. the first error in the chain that is a Classifier, unless it
//     answers ClassNone;
//  2. context.Canceled is ClassCanceled;
//  3. context.DeadlineExceeded, or an error that reports Timeout, such as
//     a network timeout, is ClassTimeout;
//  4. the exit code the error asks for, as CodeClass tells.
//
// A nil error is ClassNone.
//
// An error that sums up the failures of many targets says its own class,
// the class of the exit code it asks for, since the first of its targets'
// errors that says one is no more the whole's than any other.
func Classify(err error) Class {
	if err == nil {
		return ClassNone
	}
	var c Classifier
	if errors.As(err, &c) {
		if class := c.ProgressClass(); class != ClassNone {
			return class
		}
	}
	if errors.Is(err, context.Canceled) {
		return ClassCanceled
	}
	var t interface{ Timeout() bool }
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &t) && t.Timeout() {
		return ClassTimeout
	}
	return CodeClass(exitcode.From(err))
}

// CodeClass is the class of work that failed with the exit code code:
// exitcode.Transport is ClassTransport, exitcode.Usage ClassUsage,
// exitcode.Interrupted ClassCanceled, and any other but exitcode.OK, which
// is no failure, ClassTarget.
func CodeClass(code int) Class {
	switch code {
	case exitcode.OK:
		return ClassNone
	case exitcode.Transport:
		return ClassTransport
	case exitcode.Usage:
		return ClassUsage
	case exitcode.Interrupted:
		return ClassCanceled
	default:
		return ClassTarget
	}
}
