// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progresstest

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/GSI-HPC/clusterctl/internal/output"
)

// Screen is a terminal for a test to draw on and read back: it shows what
// is written to it the way a terminal would, as far as a display writes
// it. It knows text, a newline, which starts the next row at its first
// column as a terminal's output processing has it do, a carriage return,
// and the sequences ESC [ n A, which moves the cursor up n rows, ESC [ 2K,
// which erases the row the cursor is on, ESC [ K, which erases the rest of
// it, and ESC [ J, which erases from the cursor to the end of the screen.
// Any other control character or sequence is shown as text, "^[[?25l" or
// "^G", so that a test sees it.
//
// Every row is kept, those that have scrolled off the top too, so that what
// a Screen shows is the scrollback and the screen in one. With Width set, a
// row wraps at that column, as a terminal's does, so that a row drawn too
// long shows as the two it is. A Screen is safe for concurrent use.
type Screen struct {
	// Width is the number of columns, at which a row wraps; 0 is a row
	// that never does.
	Width int

	mu       sync.Mutex
	rows     [][]rune
	row, col int
	// pending is the start of a sequence or of a rune that the next
	// write completes.
	pending []byte
}

// Write shows p on the screen.
func (s *Screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := append(s.pending, p...)
	s.pending = nil
	for len(b) > 0 {
		switch c := b[0]; {
		case c == 0x1b:
			n, ok := s.escape(b)
			if !ok {
				s.pending = append([]byte(nil), b...)
				return len(p), nil
			}
			b = b[n:]
		case c == '\n':
			s.row++
			s.col = 0
			s.at()
			b = b[1:]
		case c == '\r':
			s.col = 0
			b = b[1:]
		case c < 0x20 || c == 0x7f:
			s.text(fmt.Sprintf("^%c", c^0x40))
			b = b[1:]
		default:
			if !utf8.FullRune(b) {
				s.pending = append([]byte(nil), b...)
				return len(p), nil
			}
			r, n := utf8.DecodeRune(b)
			s.put(r)
			b = b[n:]
		}
	}
	return len(p), nil
}

// escape applies the sequence b starts with, and returns its length; false
// when b ends before the sequence does.
func (s *Screen) escape(b []byte) (int, bool) {
	if len(b) < 2 {
		return 0, false
	}
	if b[1] != '[' {
		s.text("^[" + string(b[1]))
		return 2, true
	}
	end := 2
	for end < len(b) && (b[end] < 0x40 || b[end] > 0x7e) {
		end++
	}
	if end == len(b) {
		return 0, false
	}
	param, final := string(b[2:end]), b[end]
	n, err := strconv.Atoi(param)
	if param == "" {
		n, err = 0, nil
	}
	switch {
	case final == 'A' && err == nil:
		s.row = max(0, s.row-max(n, 1))
	case final == 'K' && param == "2":
		s.at()
		s.rows[s.row] = s.rows[s.row][:0]
	case final == 'K' && n == 0 && err == nil:
		s.at()
		if s.col < len(s.rows[s.row]) {
			s.rows[s.row] = s.rows[s.row][:s.col]
		}
	case final == 'J' && n == 0 && err == nil:
		s.at()
		if s.col < len(s.rows[s.row]) {
			s.rows[s.row] = s.rows[s.row][:s.col]
		}
		s.rows = s.rows[:s.row+1]
	default:
		s.text("^[" + string(b[1:end+1]))
	}
	return end + 1, true
}

// at makes sure the row the cursor is on exists.
func (s *Screen) at() {
	for len(s.rows) <= s.row {
		s.rows = append(s.rows, nil)
	}
}

func (s *Screen) text(t string) {
	for _, r := range t {
		s.put(r)
	}
}

// wideRest fills the column a wide rune takes after its own, which String
// leaves out.
const wideRest = rune(0)

// put writes r where the cursor is, over what was there. A wide rune, as
// CJK text and emoji are, takes two columns, and wraps when only one is
// left, as it does on a terminal.
func (s *Screen) put(r rune) {
	w := max(1, output.RuneWidth(r))
	if s.Width > 0 && s.col+w > s.Width {
		s.row++
		s.col = 0
	}
	s.at()
	row := s.rows[s.row]
	for len(row) < s.col+w {
		row = append(row, ' ')
	}
	row[s.col] = r
	if w == 2 {
		row[s.col+1] = wideRest
	}
	s.rows[s.row] = row
	s.col += w
}

// String returns what the screen shows, each row ended by a newline, up to
// the last row that is not blank.
func (s *Screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := len(s.rows) - 1
	for last >= 0 && len(s.rows[last]) == 0 {
		last--
	}
	var b strings.Builder
	for _, row := range s.rows[:last+1] {
		for _, r := range row {
			if r != wideRest {
				b.WriteRune(r)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
