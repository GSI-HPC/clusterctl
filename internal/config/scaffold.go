// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"bytes"
	"cmp"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/hostname"
)

// scaffoldFS holds the templates of a new configuration, one file each. A
// Workstation and a Secret are not among them: neither is needed for the
// configuration to resolve, and both hold what only the administrator knows.
//
//go:embed scaffold/*.yaml.tmpl
var scaffoldFS embed.FS

// ScaffoldOptions are the values a new configuration is written with.
type ScaffoldOptions struct {
	// Site names the Site document, and the NodeInventory of its nodes.
	Site string
	// Cluster names the Cluster document and the context that acts on it.
	Cluster string
	// Domain is the DNS domain of the cluster nodes, domains.hpc.
	Domain string
	// Login is the host name of the login node. Empty takes login in
	// Domain.
	Login string
	// User is the remote account of the context. Empty leaves it to ssh.
	User string
}

// ScaffoldDefaults returns the values a scaffold is written with when nothing
// else is given: those of the example site.
func ScaffoldDefaults() ScaffoldOptions {
	return ScaffoldOptions{Site: "example", Cluster: "cluster1", Domain: "hpc.example.org"}
}

// ScaffoldFile is one file of a new configuration.
type ScaffoldFile struct {
	// Name is the file name, without a directory.
	Name string
	// Kind and DocName describe the document the file holds.
	Kind    string
	DocName string
	// Data is the content to write.
	Data []byte
}

// Scaffold renders the least configuration that resolves: a Config with one
// context, a Site, a Cluster and an empty NodeInventory, one document to a
// file, with comments saying what to fill in.
//
// The files are loaded and resolved the way files read from disk are before
// they are returned, so a value that would not resolve is reported instead of
// written.
func Scaffold(opts ScaffoldOptions) ([]ScaffoldFile, error) {
	opts = opts.withDefaults()
	if err := opts.check(); err != nil {
		return nil, err
	}

	templates, err := template.New("scaffold").Funcs(template.FuncMap{
		"yaml":   yamlScalar,
		"schema": schemaURL,
	}).ParseFS(scaffoldFS, "scaffold/*.yaml.tmpl")
	if err != nil {
		return nil, fmt.Errorf("the built-in scaffold is broken: %w", err)
	}
	sources, err := fs.Glob(scaffoldFS, "scaffold/*.yaml.tmpl")
	if err != nil {
		return nil, fmt.Errorf("the built-in scaffold is broken: %w", err)
	}

	names := make([]string, 0, len(sources))
	content := make(map[string][]byte, len(sources))
	for _, source := range sources {
		var buf bytes.Buffer
		if err := templates.ExecuteTemplate(&buf, path.Base(source), opts); err != nil {
			return nil, fmt.Errorf("the built-in scaffold is broken: %w", err)
		}
		name := strings.TrimSuffix(path.Base(source), ".tmpl")
		names = append(names, name)
		content[name] = buf.Bytes()
	}

	b, err := load(names, func(name string) ([]byte, error) { return content[name], nil })
	if err != nil {
		return nil, err
	}
	// The environment is left out: the scaffold has to resolve on its own,
	// not only with the context this shell happens to select.
	if _, err := b.Resolve(ResolveOptions{Env: func(string) string { return "" }}); err != nil {
		return nil, err
	}

	files := make([]ScaffoldFile, 0, len(b.Documents))
	for _, doc := range b.Documents {
		files = append(files, ScaffoldFile{
			Name:    doc.File,
			Kind:    doc.Kind,
			DocName: documentName(doc),
			Data:    content[doc.File],
		})
	}
	return files, nil
}

// withDefaults fills in what was left empty.
func (o ScaffoldOptions) withDefaults() ScaffoldOptions {
	d := ScaffoldDefaults()
	o.Site = cmp.Or(o.Site, d.Site)
	o.Cluster = cmp.Or(o.Cluster, d.Cluster)
	o.Domain = cmp.Or(o.Domain, d.Domain)
	if o.Login == "" {
		o.Login = "login." + o.Domain
	}
	return o
}

var (
	// scaffoldName is what a scaffold names its documents and its context:
	// a word a shell and a completion list carry without quoting.
	scaffoldName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// scaffoldHost is a DNS name of one label or several.
	scaffoldHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)
)

// check refuses a value that could only be a mistake. The schema accepts any
// string in these places; a name with a space in it, or a domain with a
// trailing dot, would be written and then fail far from where it was given.
func (o ScaffoldOptions) check() error {
	const nameRule = `may only hold letters, digits, ".", "_" and "-", and starts with a letter or a digit`
	switch {
	case !scaffoldName.MatchString(o.Site):
		return fmt.Errorf("the site name %q %s", o.Site, nameRule)
	case !scaffoldName.MatchString(o.Cluster):
		return fmt.Errorf("the cluster name %q %s", o.Cluster, nameRule)
	case !scaffoldHost.MatchString(o.Domain):
		return fmt.Errorf("the domain %q is not a DNS name", o.Domain)
	case !scaffoldHost.MatchString(o.Login):
		return fmt.Errorf("the login host %q is not a host name", o.Login)
	case o.User != "":
		return hostname.CheckUser(o.User)
	}
	return nil
}

// yamlScalar writes a string so that it reads back as the same string with
// every YAML reader. It is always double quoted: a cluster called 1 or a site
// called yes is a number or a boolean unquoted, and which plain scalars are
// strings differs between YAML 1.1 and 1.2 readers, so that 1e3 and 08 read
// as numbers to yq although they did not to the parser that wrote them.
func yamlScalar(s string) (string, error) { return QuoteYAML(s), nil }

// QuoteYAML writes a string as a double quoted YAML scalar. The escapes Go
// writes are a subset of the ones YAML reads.
func QuoteYAML(s string) string { return strconv.Quote(s) }

// schemaURL is where the JSON Schema of a kind is published, for the comment
// that points an editor at it.
func schemaURL(kind string) (string, error) {
	if !v1alpha1.KnownKind(kind) {
		return "", fmt.Errorf("unknown kind %q", kind)
	}
	return schemaID + strings.ToLower(kind) + ".json", nil
}
