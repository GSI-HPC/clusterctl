// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress

import (
	"context"
	"errors"
)

// Classify tells why work failed from its error. The first rule that
// applies wins:
//
//  1. the first error in the chain that is a Classifier, unless it
//     answers ClassNone;
//  2. context.Canceled is ClassCanceled;
//  3. context.DeadlineExceeded, or an error that reports Timeout, such as
//     a network timeout, is ClassTimeout;
//  4. what fallback says of the error, the program's own rule, such as
//     one that reads the exit code the error asks for; a nil fallback
//     says ClassTarget.
//
// A nil error is ClassNone, and fallback is not asked.
//
// An error that sums up the failures of many targets is best a Classifier
// of its own, since the first of its targets' errors that says a class is
// no more the whole's than any other.
func Classify(err error, fallback func(error) Class) Class {
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
	if fallback == nil {
		return ClassTarget
	}
	return fallback(err)
}
