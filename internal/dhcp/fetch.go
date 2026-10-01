// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package dhcp

import (
	"errors"
	"path"
	"slices"
	"sync"
)

// prefetchers is how many included files are read at once, the bound on
// the sessions a command opens to one host (ADR 0022): every file comes
// from the DHCP server.
const prefetchers = 4

// errFetchStopped is what a file the parser never asked for was not read
// with, once the parse was over.
var errFetchStopped = errors.New("the configuration was read without it")

// fetcher reads the files a configuration includes, each once, for the
// parser. Each include read through ssh took a round trip of its own, one
// after the other as the parser reached it: ten includes, ten in a row. So
// once a file has been split into tokens, the files it includes are read
// side by side, prefetchers at a time, ahead of the parser, which still
// takes them in its own order and sees an error only where it reaches the
// include that failed.
type fetcher struct {
	read  Reader
	slots chan struct{}

	mu      sync.Mutex
	files   map[string]*fetch
	stopped bool
	running sync.WaitGroup
}

// fetch is one file read, or being read.
type fetch struct {
	done chan struct{}
	data []byte
	err  error
}

func newFetcher(read Reader) *fetcher {
	return &fetcher{read: read, slots: make(chan struct{}, prefetchers), files: map[string]*fetch{}}
}

// ahead starts reading files the parser is going to ask for.
func (f *fetcher) ahead(files []string) {
	for _, file := range files {
		f.start(file)
	}
}

// get returns the content of file, read ahead or read now.
func (f *fetcher) get(file string) ([]byte, error) {
	x := f.start(file)
	<-x.done
	return x.data, x.err
}

// start returns the read of file, starting it unless it has been.
func (f *fetcher) start(file string) *fetch {
	f.mu.Lock()
	defer f.mu.Unlock()
	if x, ok := f.files[file]; ok {
		return x
	}
	x := &fetch{done: make(chan struct{})}
	f.files[file] = x
	f.running.Go(func() {
		defer close(x.done)
		f.slots <- struct{}{}
		defer func() { <-f.slots }()
		f.mu.Lock()
		stopped := f.stopped
		f.mu.Unlock()
		if stopped {
			x.err = errFetchStopped
			return
		}
		x.data, x.err = f.read(file)
	})
	return x
}

// stop reads nothing more and waits for the reads under way, once the
// parse is over: one that failed early leaves files it never reached.
func (f *fetcher) stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	f.running.Wait()
}

// includes lists the files the include statements of the parser's tokens
// name, in order, that the parser would read: absolute, neither the file
// itself nor one on the chain that led to it, each once, and no more than
// the files left to the configuration. A statement is an include when its
// first word is include and a quoted name follows it alone, the shape
// include checks.
func (p *parser) includes() []string {
	var out []string
	left := maxFiles - *p.files
	statementStart := true
	for i, t := range p.toks {
		if t.kind == tokComment {
			continue
		}
		start := statementStart
		statementStart = t.kind == tokSemicolon || t.kind == tokOpen || t.kind == tokClose
		if !start || t.kind != tokWord || t.text != "include" || i+2 >= len(p.toks) {
			continue
		}
		name, end := p.toks[i+1], p.toks[i+2]
		if name.kind != tokString || end.kind != tokSemicolon || !path.IsAbs(name.text) ||
			name.text == p.file || slices.Contains(p.chain, name.text) || slices.Contains(out, name.text) {
			continue
		}
		if len(out) >= left {
			break
		}
		out = append(out, name.text)
	}
	return out
}
