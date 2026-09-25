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
//  4. the exit code the error asks for: exitcode.Transport is
//     ClassTransport, exitcode.Usage ClassUsage, exitcode.Interrupted
//     ClassCanceled, and any other ClassTarget.
//
// A nil error is ClassNone.
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
	switch exitcode.From(err) {
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
