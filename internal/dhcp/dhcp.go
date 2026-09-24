// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package dhcp reads an ISC dhcpd configuration.
//
// The file is split into tokens and parsed rather than grepped or read line by
// line. Reading a fixed number of lines after a match, which the shell tools
// did, breaks as soon as a declaration is one line longer or the options are
// in another order. Reading line by line breaks as soon as a brace shares a
// line with a statement. Either silently reports a neighbour's address. A
// construct the parser does not understand is an error rather than a guess.
//
// A node's boot address is taken only from the one declaration named after
// it. A second interface and a comment that names the node are shown, but
// never give the node an address.
package dhcp

import (
	"errors"
	"fmt"
	"net"
	"path"
	"sort"
	"strings"
)

// Host is one host declaration.
type Host struct {
	// Name is the name the declaration carries.
	Name string `json:"name" yaml:"name"`
	// MACs are the hardware addresses it matches.
	MACs []string `json:"macs,omitempty" yaml:"macs,omitempty"`
	// Address is the fixed address it hands out. A declaration that hands
	// out several has them separated by ", ".
	Address string `json:"address,omitempty" yaml:"address,omitempty"`
	// Filename is the boot file it offers.
	Filename string `json:"filename,omitempty" yaml:"filename,omitempty"`
	// ClientIdentifier is the dhcp-client-identifier option it matches.
	ClientIdentifier string `json:"clientIdentifier,omitempty" yaml:"clientIdentifier,omitempty"`
	// Options holds every other option of the declaration.
	Options map[string]string `json:"options,omitempty" yaml:"options,omitempty"`
	// Comments are the comment lines directly above the declaration, which
	// is where sites often write the node name.
	Comments []string `json:"comments,omitempty" yaml:"comments,omitempty"`
}

// Config is a parsed dhcpd configuration.
type Config struct {
	Hosts []Host `json:"hosts" yaml:"hosts"`
}

// Reader returns the content of a file that an include statement names.
type Reader func(path string) ([]byte, error)

const (
	// maxFiles bounds how many files one configuration may include, which
	// is also how a file that includes itself is stopped.
	maxFiles = 256
	// maxDepth bounds how deeply blocks and includes may nest.
	maxDepth = 64
)

// Parse reads a dhcpd configuration held in memory. An include statement in
// it is an error, because there is nothing to read the named file from.
func Parse(data []byte) (*Config, error) {
	return parse("", data, nil)
}

// ParseFile reads the dhcpd configuration at file, and every file it
// includes, through read.
func ParseFile(file string, read Reader) (*Config, error) {
	data, err := read(file)
	if err != nil {
		return nil, err
	}
	return parse(file, data, read)
}

func parse(file string, data []byte, read Reader) (*Config, error) {
	cfg := &Config{}
	files := 1
	p := &parser{file: file, read: read, cfg: cfg, files: &files}
	if err := p.run(data); err != nil {
		return nil, err
	}
	sort.SliceStable(cfg.Hosts, func(i, j int) bool { return cfg.Hosts[i].Name < cfg.Hosts[j].Name })
	return cfg, nil
}

type tokenKind int

const (
	tokWord tokenKind = iota
	tokString
	tokComment
	tokSemicolon
	tokOpen
	tokClose
	tokComma
)

type token struct {
	kind tokenKind
	text string
	line int
	// first reports whether the token is the first on its line.
	first bool
}

// tokenize splits a configuration into words, quoted strings, comments and
// the punctuation dhcpd gives meaning to.
func tokenize(data []byte) ([]token, error) {
	var (
		out   []token
		line  = 1
		first = true
	)
	emit := func(kind tokenKind, text string) {
		out = append(out, token{kind: kind, text: text, line: line, first: first})
		first = false
	}
	for i := 0; i < len(data); {
		c := data[i]
		switch c {
		case '\n':
			line++
			first = true
			i++
		case ' ', '\t', '\r', '\f', '\v':
			i++
		case '#':
			end := i
			for end < len(data) && data[end] != '\n' {
				end++
			}
			emit(tokComment, strings.TrimSpace(string(data[i+1:end])))
			i = end
		case '"':
			start := line
			var b strings.Builder
			i++
			for {
				if i >= len(data) {
					return nil, fmt.Errorf("line %d: a quoted string is not closed", start)
				}
				c := data[i]
				if c == '"' {
					i++
					break
				}
				if c == '\\' && i+1 < len(data) && (data[i+1] == '"' || data[i+1] == '\\') {
					b.WriteByte(data[i+1])
					i += 2
					continue
				}
				if c == '\n' {
					line++
				}
				b.WriteByte(c)
				i++
			}
			out = append(out, token{kind: tokString, text: b.String(), line: start, first: first})
			first = false
		case ';':
			emit(tokSemicolon, ";")
			i++
		case '{':
			emit(tokOpen, "{")
			i++
		case '}':
			emit(tokClose, "}")
			i++
		case ',':
			emit(tokComma, ",")
			i++
		default:
			end := i
			for end < len(data) && !strings.ContainsRune(" \t\r\f\v\n#\";{},", rune(data[end])) {
				end++
			}
			emit(tokWord, string(data[i:end]))
			i = end
		}
	}
	return out, nil
}

type parser struct {
	file string
	read Reader
	cfg  *Config
	// chain is the include chain that led to this file.
	chain []string
	files *int
	depth int

	toks []token
	pos  int

	// comments is the block of whole-line comments read last, and
	// commentLine the line it ends on.
	comments    []string
	commentLine int
}

func (p *parser) errorf(line int, format string, args ...any) error {
	where := fmt.Sprintf("line %d", line)
	if p.file != "" {
		where = fmt.Sprintf("%s:%d", p.file, line)
	}
	return fmt.Errorf("%s: %w", where, fmt.Errorf(format, args...))
}

func (p *parser) run(data []byte) error {
	toks, err := tokenize(data)
	if err != nil {
		if p.file != "" {
			return fmt.Errorf("%s: %w", p.file, err)
		}
		return err
	}
	p.toks = toks
	return p.block(0)
}

// block reads statements up to the brace that closes the block opened on
// line open, or to the end of the file when open is 0.
func (p *parser) block(open int) error {
	if p.depth > maxDepth {
		return p.errorf(open, "blocks nest more than %d deep", maxDepth)
	}
	for {
		if p.pos >= len(p.toks) {
			if open > 0 {
				return p.errorf(open, "the block opened here is not closed")
			}
			return nil
		}
		t := p.toks[p.pos]
		switch t.kind {
		case tokComment:
			p.pos++
			if !t.first {
				continue
			}
			if len(p.comments) == 0 || t.line != p.commentLine+1 {
				p.comments = nil
			}
			p.comments = append(p.comments, t.text)
			p.commentLine = t.line
			continue
		case tokClose:
			p.pos++
			if open == 0 {
				return p.errorf(t.line, "} without a matching {")
			}
			return nil
		case tokSemicolon:
			p.pos++
			continue
		}

		var comments []string
		if len(p.comments) > 0 && t.line == p.commentLine+1 {
			comments = p.comments
		}
		p.comments = nil

		words, end, err := p.statement()
		if err != nil {
			return err
		}
		keyword := ""
		if words[0].kind == tokWord {
			keyword = words[0].text
		}
		switch {
		case keyword == "host" && end == tokOpen:
			if err := p.host(words, comments); err != nil {
				return err
			}
		case keyword == "host":
			return p.errorf(t.line, "the host declaration %s has no body", describe(words[1:]))
		case end == tokOpen:
			p.depth++
			if err := p.block(t.line); err != nil {
				return err
			}
			p.depth--
		case keyword == "include":
			if err := p.include(words); err != nil {
				return err
			}
		}
	}
}

// statement reads the tokens of one statement up to the semicolon that ends
// it or the brace that opens its block, and reports which of the two it was.
// Comments inside a statement are skipped.
func (p *parser) statement() ([]token, tokenKind, error) {
	start := p.toks[p.pos]
	var words []token
	for {
		if p.pos >= len(p.toks) {
			return nil, 0, p.errorf(start.line, "the statement %s is not ended with ;", describe(words))
		}
		t := p.toks[p.pos]
		switch t.kind {
		case tokComment:
			p.pos++
		case tokSemicolon:
			p.pos++
			return words, tokSemicolon, nil
		case tokClose:
			return nil, 0, p.errorf(start.line, "the statement %s is not ended with ;", describe(words))
		case tokOpen:
			// An option definition gives a record type in braces after
			// "=", which is part of the statement and not a block.
			if len(words) > 0 && words[len(words)-1].kind == tokWord && words[len(words)-1].text == "=" {
				record, err := p.record()
				if err != nil {
					return nil, 0, err
				}
				words = append(words, record...)
				continue
			}
			p.pos++
			if len(words) == 0 {
				return nil, 0, p.errorf(t.line, "a block opens without a statement")
			}
			return words, tokOpen, nil
		default:
			words = append(words, t)
			p.pos++
		}
	}
}

// record reads a brace-enclosed record type, which holds only words and
// commas.
func (p *parser) record() ([]token, error) {
	open := p.toks[p.pos]
	p.pos++
	out := []token{open}
	for ; p.pos < len(p.toks); p.pos++ {
		t := p.toks[p.pos]
		switch t.kind {
		case tokClose:
			p.pos++
			return append(out, t), nil
		case tokWord, tokString, tokComma:
			out = append(out, t)
		case tokComment:
		default:
			return nil, p.errorf(t.line, "unexpected %q in a record type", t.text)
		}
	}
	return nil, p.errorf(open.line, "the record type opened here is not closed")
}

// host reads the body of a host declaration.
func (p *parser) host(words []token, comments []string) error {
	line := words[0].line
	if len(words) != 2 || (words[1].kind != tokWord && words[1].kind != tokString) || words[1].text == "" {
		return p.errorf(line, "a host declaration needs exactly one name, not %s", describe(words[1:]))
	}
	h := Host{Name: words[1].text, Options: map[string]string{}, Comments: comments}
	for {
		if p.pos >= len(p.toks) {
			return p.errorf(line, "the host declaration %q is not closed", h.Name)
		}
		t := p.toks[p.pos]
		switch t.kind {
		case tokComment, tokSemicolon:
			p.pos++
			continue
		case tokClose:
			p.pos++
			p.cfg.Hosts = append(p.cfg.Hosts, h)
			return nil
		case tokOpen:
			return p.errorf(t.line, "unexpected { in the host declaration %q", h.Name)
		}

		statement, end, err := p.statement()
		if err != nil {
			return fmt.Errorf("in the host declaration %q: %w", h.Name, err)
		}
		if statement[0].kind == tokWord && statement[0].text == "host" {
			return p.errorf(t.line, "the host declaration %s is inside the host declaration %q",
				describe(statement[1:]), h.Name)
		}
		if end == tokOpen {
			return p.errorf(t.line, "unexpected block %s in the host declaration %q", describe(statement), h.Name)
		}
		if err := p.apply(&h, statement); err != nil {
			return err
		}
	}
}

// apply reads one statement of a host declaration.
func (p *parser) apply(h *Host, words []token) error {
	line := words[0].line
	keyword := words[0].text
	args := words[1:]
	var values []string
	for _, t := range args {
		if t.kind != tokComma {
			values = append(values, t.text)
		}
	}

	switch {
	case words[0].kind != tokWord:
		return p.errorf(line, "unexpected %s in the host declaration %q", describe(words), h.Name)
	case keyword == "include":
		return p.errorf(line, "include inside the host declaration %q is not supported", h.Name)
	case keyword == "hardware":
		if len(values) != 2 || len(args) != 2 {
			return p.errorf(line, "hardware in the host declaration %q needs a type and an address, not %s",
				h.Name, describe(args))
		}
		if values[0] == "ethernet" {
			h.MACs = append(h.MACs, strings.ToLower(values[1]))
		} else {
			h.Options["hardware "+values[0]] = values[1]
		}
	case keyword == "fixed-address":
		if len(values) == 0 {
			return p.errorf(line, "fixed-address in the host declaration %q names no address", h.Name)
		}
		if h.Address != "" {
			return p.errorf(line, "the host declaration %q has more than one fixed-address", h.Name)
		}
		h.Address = strings.Join(values, ", ")
	case keyword == "filename":
		if len(args) != 1 {
			return p.errorf(line, "filename in the host declaration %q needs one value, not %s", h.Name, describe(args))
		}
		h.Filename = args[0].text
	case keyword == "option":
		if len(args) < 2 || args[0].kind != tokWord {
			return p.errorf(line, "option in the host declaration %q needs a name and a value, not %s",
				h.Name, describe(args))
		}
		if args[0].text == "dhcp-client-identifier" {
			h.ClientIdentifier = join(args[1:])
		} else {
			h.Options[args[0].text] = join(args[1:])
		}
	default:
		h.Options[keyword] = join(args)
	}
	return nil
}

// include reads the file an include statement names.
func (p *parser) include(words []token) error {
	line := words[0].line
	if len(words) != 2 || words[1].kind != tokString {
		return p.errorf(line, "include needs one quoted file name, not %s", describe(words[1:]))
	}
	file := words[1].text
	if p.read == nil {
		return p.errorf(line, "include %q cannot be followed here", file)
	}
	// dhcpd resolves a relative name against its working directory, which
	// is not known here.
	if !path.IsAbs(file) {
		return p.errorf(line, "include %q is relative; name the file by its absolute path", file)
	}
	for _, f := range append(p.chain, p.file) {
		if f == file {
			return p.errorf(line, "include %q includes itself", file)
		}
	}
	*p.files++
	if *p.files > maxFiles {
		return p.errorf(line, "more than %d files are included", maxFiles)
	}
	if p.depth > maxDepth {
		return p.errorf(line, "includes nest more than %d deep", maxDepth)
	}
	data, err := p.read(file)
	if err != nil {
		return p.errorf(line, "include %q: %w", file, err)
	}
	sub := &parser{
		file:  file,
		read:  p.read,
		cfg:   p.cfg,
		chain: append(append([]string(nil), p.chain...), p.file),
		files: p.files,
		depth: p.depth + 1,
	}
	return sub.run(data)
}

// join renders statement arguments for display, with strings unquoted.
func join(args []token) string {
	var b strings.Builder
	for i, t := range args {
		if t.kind == tokComma {
			b.WriteString(",")
			continue
		}
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(t.text)
	}
	return b.String()
}

// describe renders tokens for an error message.
func describe(words []token) string {
	if len(words) == 0 {
		return "(empty)"
	}
	return fmt.Sprintf("%q", join(words))
}

// MatchKind says how a declaration belongs to a node.
type MatchKind string

const (
	// ByName is a declaration named after the node, or after a fully
	// qualified name whose first label is the node.
	ByName MatchKind = "name"
	// ByInterface is a declaration whose name is the node name followed by
	// "-" or "_", which is how a second interface or the BMC is usually
	// written.
	ByInterface MatchKind = "interface"
	// ByComment is a declaration that a comment directly above it
	// associates with the node. It is shown, never used.
	ByComment MatchKind = "comment"
)

// Match is a declaration found for a node.
type Match struct {
	Host
	// By is how the declaration was found.
	By MatchKind `json:"match" yaml:"match"`
}

// Lookup finds the declarations that belong to a node by their name: the
// node's own and those of its other interfaces.
//
// A comment that names the node is not enough; Mentions finds those.
func (c *Config) Lookup(node string) []Match {
	var out []Match
	for _, h := range c.Hosts {
		if kind, ok := matchName(h.Name, node); ok {
			out = append(out, Match{Host: h, By: kind})
		}
	}
	return out
}

// Mentions finds the declarations that do not belong to a node by name but
// that a comment directly above them names it in. A comment such as
// "# chassis C07: exe0003 exe0004" names several nodes, so these are for
// display only.
func (c *Config) Mentions(node string) []Match {
	var out []Match
	for _, h := range c.Hosts {
		if _, ok := matchName(h.Name, node); ok {
			continue
		}
		if mentions(h.Comments, node) {
			out = append(out, Match{Host: h, By: ByComment})
		}
	}
	return out
}

func matchName(name, node string) (MatchKind, bool) {
	if node == "" {
		return "", false
	}
	if name == node || strings.HasPrefix(name, node+".") {
		return ByName, true
	}
	if strings.HasPrefix(name, node) {
		if rest := name[len(node):]; rest[0] == '-' || rest[0] == '_' {
			return ByInterface, true
		}
	}
	return "", false
}

func mentions(comments []string, node string) bool {
	for _, comment := range comments {
		for _, word := range strings.FieldsFunc(comment, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == ':' || r == ';' || r == '(' || r == ')'
		}) {
			if word == node {
				return true
			}
		}
	}
	return false
}

var (
	// ErrNoAddress is returned when no declaration named after a node
	// carries an address.
	ErrNoAddress = errors.New("no DHCP declaration named after the node carries an address")
	// ErrAmbiguous is returned when a node's address is not decided by one
	// declaration, or the one declaration hands out several addresses.
	ErrAmbiguous = errors.New("the DHCP address is ambiguous")
	// ErrInvalidAddress is returned when the fixed address of a node's
	// declaration is not an IP address.
	ErrInvalidAddress = errors.New("the DHCP fixed-address is not an IP address")
)

// BootAddress returns the address a node boots with: the fixed address of the
// one declaration named after the node or its fully qualified name.
//
// Another interface of the node, such as its BMC, and a declaration that a
// comment associates with the node never give the address. When more than one
// declaration named after the node carries an address, or the one hands out
// several, which of them the node boots with cannot be told, and the answer is
// an error rather than a guess.
func (c *Config) BootAddress(node string) (string, error) {
	var found []Host
	for _, m := range c.Lookup(node) {
		if m.By == ByName && m.Address != "" {
			found = append(found, m.Host)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%s: %w", node, ErrNoAddress)
	case 1:
	default:
		described := make([]string, 0, len(found))
		for _, h := range found {
			described = append(described, fmt.Sprintf("%s at %s", h.Name, h.Address))
		}
		return "", fmt.Errorf("%s: %w: several declarations are named after it (%s)",
			node, ErrAmbiguous, strings.Join(described, ", "))
	}

	h := found[0]
	if strings.Contains(h.Address, ",") {
		return "", fmt.Errorf("%s: %w: the declaration %s hands out several addresses (%s)",
			node, ErrAmbiguous, h.Name, h.Address)
	}
	if net.ParseIP(h.Address) == nil {
		return "", fmt.Errorf("%s: %w: the declaration %s has %q", node, ErrInvalidAddress, h.Name, h.Address)
	}
	return h.Address, nil
}

// GUIDFromMAC derives the InfiniBand port GUID of a Mellanox adapter from its
// hardware address, which is what the fabric tools are asked about.
//
// The address is split in the middle and 0300 is inserted, which is the
// mapping Mellanox firmware uses between the two.
func GUIDFromMAC(mac string) (string, error) {
	clean := strings.NewReplacer(":", "", "-", "", ".", "").Replace(strings.TrimSpace(mac))
	if len(clean) != 12 {
		return "", fmt.Errorf("%q is not a hardware address", mac)
	}
	for _, c := range clean {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return "", fmt.Errorf("%q is not a hardware address", mac)
		}
	}
	clean = strings.ToLower(clean)
	return "0x" + clean[:6] + "0300" + clean[6:], nil
}
