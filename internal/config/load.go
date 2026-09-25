// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"cmp"
	_ "embed"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
)

//go:embed defaults.yaml
var defaultsYAML []byte

// Bundle is everything read from the configuration files, before a context
// selects what applies.
type Bundle struct {
	// Files are the files that were read, in layering order.
	Files []string
	// Documents are all documents, in the order they were read.
	Documents []*Document
	// Config is the merged Config document.
	Config v1alpha1.Config
	// Sites, Clusters, Inventories and Workstations are indexed by name. A
	// document without a name is stored under the empty string, which is
	// what a single cluster installation usually has.
	Sites        map[string]*Document
	Clusters     map[string]*Document
	Inventories  map[string]*Document
	Workstations map[string]*Document
	// Secrets are the sops encrypted Secret documents, indexed by name.
	// Their values stay encrypted until a command asks for one.
	Secrets map[string]*Document

	// contextSources says which Config document defined each context, so
	// that its user and overrides keep their file and line.
	contextSources map[string]contextSource
}

// contextSource is where a context was defined: a Config document and the
// path of the context in it.
type contextSource struct {
	doc *Document
	at  string
}

// Load reads the configuration files, validates every document against the
// schema of its kind and indexes them by name.
//
// Validation happens per document, before anything is merged, so a mistake is
// reported at the line it was written on.
func Load(files []string) (*Bundle, error) {
	return load(files, os.ReadFile)
}

// load is Load with the files read by read, so that a configuration that is
// not on disk yet can be checked the same way.
func load(files []string, read func(string) ([]byte, error)) (*Bundle, error) {
	b := &Bundle{
		Files:        files,
		Sites:        map[string]*Document{},
		Clusters:     map[string]*Document{},
		Inventories:  map[string]*Document{},
		Workstations: map[string]*Document{},
		Secrets:      map[string]*Document{},
	}

	configDocs := make([]*Document, 0, 2)
	for _, file := range files {
		data, err := read(file)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", file, err)
		}
		docs, err := ParseDocuments(file, data)
		if err != nil {
			return nil, err
		}
		if err := checkSecretFile(file, data, docs); err != nil {
			return nil, err
		}
		for _, doc := range docs {
			if err := ValidateDocument(doc); err != nil {
				return nil, err
			}
			if err := checkDocumentOverrides(doc); err != nil {
				return nil, err
			}
			b.Documents = append(b.Documents, doc)
			var index map[string]*Document
			switch doc.Kind {
			case v1alpha1.KindConfig:
				configDocs = append(configDocs, doc)
				continue
			case v1alpha1.KindSite:
				index = b.Sites
			case v1alpha1.KindCluster:
				index = b.Clusters
			case v1alpha1.KindNodeInventory:
				index = b.Inventories
			case v1alpha1.KindWorkstation:
				index = b.Workstations
			case v1alpha1.KindSecret:
				index = b.Secrets
			default:
				continue
			}
			if err := addDocument(index, doc); err != nil {
				return nil, err
			}
		}
	}

	if err := b.mergeConfigDocuments(configDocs); err != nil {
		return nil, err
	}
	return b, nil
}

// addDocument indexes a document by name. Two documents of one kind and name
// are an error rather than one replacing the other: the files of a directory
// are read in name order, so a stale copy such as site_old.yaml, or a
// personal directory that repeats a site's name, would otherwise take the
// place of the document every administrator reads, protected hosts and all.
func addDocument(index map[string]*Document, doc *Document) error {
	name := documentName(doc)
	if first, ok := index[name]; ok {
		return fmt.Errorf("%s: a second %s %s; the first is at %s. "+
			"Each %s is defined once: remove one of them or give it a name of its own",
			documentAt(doc), doc.Kind, quotedName(name), documentAt(first), doc.Kind)
	}
	index[name] = doc
	return nil
}

// documentAt is where a document starts, for a message that names it.
func documentAt(doc *Document) Origin {
	if o, ok := doc.Positions["kind"]; ok {
		return o
	}
	return Origin{File: doc.File}
}

// quotedName names a document in a message; an unnamed one says so.
func quotedName(name string) string {
	if name == "" {
		return "without a name"
	}
	return strconv.Quote(name)
}

// mergeConfigDocuments folds the Config documents into one. Contexts of the
// same name are replaced by the later document, so a personal file can
// override a system wide one. One document that names a context twice is an
// error: only another file can mean to replace it.
func (b *Bundle) mergeConfigDocuments(docs []*Document) error {
	b.Config.APIVersion = v1alpha1.GroupVersion
	b.Config.Kind = v1alpha1.KindConfig
	b.contextSources = map[string]contextSource{}

	index := map[string]int{}
	for _, doc := range docs {
		var cfg v1alpha1.Config
		if err := decodeInto(doc.Data, &cfg); err != nil {
			return fmt.Errorf("%s: %w", doc.File, err)
		}
		if cfg.CurrentContext != "" {
			b.Config.CurrentContext = cfg.CurrentContext
		}
		seen := map[string]string{}
		for i, ctx := range cfg.Contexts {
			at := fmt.Sprintf("contexts[%d]", i)
			if first, ok := seen[ctx.Name]; ok {
				return fmt.Errorf("%s: a second context %q in one document; the first is at %s",
					doc.Position(at+".name"), ctx.Name, doc.Position(first+".name"))
			}
			seen[ctx.Name] = at
			b.contextSources[ctx.Name] = contextSource{doc: doc, at: at}
			if at, ok := index[ctx.Name]; ok {
				b.Config.Contexts[at] = ctx
				continue
			}
			index[ctx.Name] = len(b.Config.Contexts)
			b.Config.Contexts = append(b.Config.Contexts, ctx)
		}
	}
	return nil
}

// Context returns the named context, or the current one when name is empty.
func (b *Bundle) Context(name string) (v1alpha1.Context, error) {
	name = cmp.Or(name, b.Config.CurrentContext)
	if name == "" {
		if len(b.Config.Contexts) == 1 {
			return b.Config.Contexts[0], nil
		}
		return v1alpha1.Context{}, fmt.Errorf(
			"no context is current; set currentContext or pass --context (known: %v)", b.ContextNames())
	}
	for _, ctx := range b.Config.Contexts {
		if ctx.Name == name {
			return ctx, nil
		}
	}
	return v1alpha1.Context{}, fmt.Errorf("unknown context %q (known: %v)", name, b.ContextNames())
}

// ContextNames lists the configured contexts in the order they were read.
func (b *Bundle) ContextNames() []string {
	out := make([]string, 0, len(b.Config.Contexts))
	for _, ctx := range b.Config.Contexts {
		out = append(out, ctx.Name)
	}
	return out
}

// documentName reads metadata.name, defaulting to the empty name.
func documentName(doc *Document) string {
	meta, ok := doc.Data["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := meta["name"].(string)
	return name
}

// names returns the keys of an index in sorted order.
func names(index map[string]*Document) []string {
	return slices.Sorted(maps.Keys(index))
}

// dirOf returns the directory a document was read from, which relative paths
// in it resolve against.
func dirOf(doc *Document) string {
	if doc == nil || doc.File == "" {
		return ""
	}
	return filepath.Dir(doc.File)
}
