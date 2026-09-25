// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/itchyny/gojq"
)

// compileJQ parses and compiles a jq program.
//
// The program cannot read the environment: -o is chosen by whoever runs the
// command, which through the MCP server is an agent, and $ENV and env would
// hand it every variable of the process.
func compileJQ(program string) (code *gojq.Code, err error) {
	defer recoverQuery(&err)
	query, err := gojq.Parse(program)
	if err != nil {
		return nil, fmt.Errorf("invalid jq expression %q: %w", program, err)
	}
	code, err = gojq.Compile(query, gojq.WithEnvironLoader(func() []string { return nil }))
	if err != nil {
		return nil, fmt.Errorf("invalid jq expression %q: %w", program, err)
	}
	return code, nil
}

// writeJQ filters the result with a jq program. The jq language is embedded,
// so no jq binary has to be installed. The program stops when ctx ends, since
// nothing else bounds one such as repeat(1).
func writeJQ(ctx context.Context, w io.Writer, program string, v any) (err error) {
	code, err := compileJQ(program)
	if err != nil {
		return err
	}
	input, err := toGeneric(v)
	if err != nil {
		return err
	}
	defer recoverQuery(&err)
	iter := code.RunWithContext(ctx, input)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("jq: %w", err)
		}
		out, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, ok := out.(error); ok {
			return fmt.Errorf("jq: %w", err)
		}
		if err := writeScalarOrJSON(w, out); err != nil {
			return err
		}
	}
}

// recoverQuery turns a panic in the jq code into an error, so that a bad
// program fails the one command rather than the process, which may be the
// MCP server.
func recoverQuery(err *error) {
	if p := recover(); p != nil {
		*err = fmt.Errorf("jq: the expression could not be evaluated: %v", p)
	}
}

// toGeneric converts a typed value into the maps, lists and scalars jq works
// on, using the json struct tags.
func toGeneric(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	// Numbers are kept as their JSON text: decoded into a float64, a PID
	// above 2^53 or an exit code of 0 would not print as it was.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// writeScalarOrJSON prints a string as itself and anything else as JSON, so
// that a query selecting one name does not print it in quotes.
func writeScalarOrJSON(w io.Writer, v any) error {
	if s, ok := v.(string); ok {
		_, err := fmt.Fprintln(w, s)
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(raw))
	return err
}
