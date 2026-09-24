// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package slurm

import (
	"regexp"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// Row is one line of a fake sinfo or squeue answer: the value of each field,
// keyed by its format letter, "N" for %N.
type Row map[string]string

// formatField matches one field of a sinfo or squeue format.
var formatField = regexp.MustCompile(`%[A-Za-z]`)

// Render prints rows the way sinfo or squeue print them for the format a
// request asks for, so that a test double does not have to know how the
// client lays its output out. A field the row does not name is empty.
func Render(req transport.Request, rows ...Row) string {
	format := Arg(req, "--format")
	var b strings.Builder
	for _, row := range rows {
		b.WriteString(formatField.ReplaceAllStringFunc(format, func(field string) string {
			return row[field[1:]]
		}))
		b.WriteString("\n")
	}
	return b.String()
}

// Arg returns the value that follows an option in a request, or the empty
// string when the request does not have the option.
func Arg(req transport.Request, option string) string {
	for i, arg := range req.Argv {
		if arg == option && i+1 < len(req.Argv) {
			return req.Argv[i+1]
		}
	}
	return ""
}
