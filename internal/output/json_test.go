// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// byPointer marshals itself through its pointer, which encoding/json calls
// for an element of a list, since it can address one.
type byPointer struct{ N int }

func (p *byPointer) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, `{"pointer":%d}`, p.N), nil
}

// byValue marshals itself through its value.
type byValue struct{ N int }

func (v byValue) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, `{"value":[%d,{}]}`, v.N), nil
}

// asText is a list that marshals itself as a whole.
type asText []int

func (a asText) MarshalText() ([]byte, error) { return fmt.Appendf(nil, "%d items", len(a)), nil }

// wholeJSON is what encoding the whole of v at once writes.
func wholeJSON(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// A list is written an element at a time, and what is written is, byte for
// byte, what encoding the whole list at once wrote: nested lists and maps,
// empty ones, HTML characters, elements that marshal themselves through
// their pointer or their value, and the values that are no list of
// elements, a []byte, an empty or nil slice, a list that marshals itself.
func TestJSONListsAreWrittenAsTheWholeWouldBe(t *testing.T) {
	t.Parallel()
	results := []*transport.Result{
		{Target: transport.Target{Name: "exe0001"}, Stdout: "<b>&\n", ExitCode: 0},
		nil,
		{Target: transport.Target{Name: "exe0002"}, ExitCode: 1, Err: errors.New("exe0002: command exited 1")},
	}
	for name, v := range map[string]any{
		"results":            results,
		"one result":         results[:1],
		"strings":            []string{"<a>", "&", "\u2028"},
		"anything":           []any{map[string]any{"a": []int{}, "b": map[string]any{}}, nil, []any{1, "x", []int{2}}, 3.5},
		"empty fields":       []struct{ A, B []int }{{nil, []int{}}, {}},
		"pointer marshalers": []byPointer{{1}, {2}},
		"value marshalers":   []byValue{{1}, {2}},
		"raw messages":       []json.RawMessage{json.RawMessage(`{"a":[1,2]}`), json.RawMessage(`[]`)},
		"nested lists":       [][]int{{1, 2}, {}, nil},
		"bytes":              []byte("bytes"),
		"empty":              []int{},
		"nil":                []int(nil),
		"a list as text":     asText{1, 2},
		"a map":              map[string][]int{"a": {1}},
		"a string":           "text",
	} {
		var got bytes.Buffer
		if err := (output.Format{Kind: output.FormatJSON}).Write(&got, output.Result{Object: v}); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if want := wholeJSON(t, v); got.String() != want {
			t.Errorf("%s:\n%s\nwant:\n%s", name, got.String(), want)
		}
	}
}

// marked marshals itself into a large string, and records that it did.
type marked struct {
	i    int
	seen *[]string
}

func (m marked) MarshalJSON() ([]byte, error) {
	*m.seen = append(*m.seen, fmt.Sprintf("marshal %d", m.i))
	return json.Marshal(strings.Repeat("x", 1<<16))
}

// recording records each write of the output, in the order of the work.
type recording struct{ seen *[]string }

func (r recording) Write(p []byte) (int, error) {
	*r.seen = append(*r.seen, fmt.Sprintf("write %d bytes", len(p)))
	return len(p), nil
}

// An element is written before the next is encoded, so the output of a
// list is never held whole.
func TestJSONListsAreWrittenAsTheyAreEncoded(t *testing.T) {
	t.Parallel()
	var seen []string
	list := make([]marked, 3)
	for i := range list {
		list[i] = marked{i, &seen}
	}
	if err := (output.Format{Kind: output.FormatJSON}).Write(recording{&seen}, output.Result{Object: list}); err != nil {
		t.Fatal(err)
	}
	want := []string{"write 2 bytes", "marshal 0", "write 65542 bytes", "marshal 1", "write 65542 bytes", "marshal 2", "write 65541 bytes", "write 2 bytes"}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("the work went:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
}
