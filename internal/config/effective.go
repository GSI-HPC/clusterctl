// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
)

// Resolved is the configuration one command runs against, together with the
// provenance of every value in it.
type Resolved struct {
	// Context is the context that was selected.
	Context v1alpha1.Context
	// SiteName and ClusterName name the documents that were used.
	SiteName    string
	ClusterName string
	// Spec is the merged configuration.
	Spec v1alpha1.EffectiveSpec
	// Tree is the merged tree with the origin of every value.
	Tree *Tree
	// Bundle is the configuration the resolution started from.
	Bundle *Bundle
	// BaseDir is the directory relative paths in the configuration resolve
	// against, the directory of the Site document.
	BaseDir string
}

// ResolveOptions carry the layers that do not come from files.
type ResolveOptions struct {
	// Context selects the context; empty takes the current one.
	Context string
	// Env reads environment variables; nil reads the process environment.
	Env func(string) string
	// Set are the --set assignments, applied last.
	Set map[string]string
	// Fanout is what --fanout gave, zero when it was not given. It is
	// applied with the flags, after --set, so that config explain reports
	// the value the commands use.
	Fanout int
}

// envPaths maps the environment variables that override configuration to the
// paths they set. Variables that select a file or a context are handled
// before resolution and are not listed here.
var envPaths = map[string]string{
	"CLUSTERCTL_FANOUT":          "fanout.max",
	"CLUSTERCTL_CONNECT_TIMEOUT": "ssh.connectTimeout",
	"CLUSTERCTL_COMMAND_TIMEOUT": "fanout.commandTimeout",
	"CLUSTERCTL_KNOWN_HOSTS":     "ssh.knownHostsFile",
	"CLUSTERCTL_SSH_BINARY":      "ssh.binary",
	"CLUSTERCTL_SCP_BINARY":      "ssh.scpBinary",
	"CLUSTERCTL_SSHUTTLE_BINARY": "workstation.sshuttleBinary",
	"CLUSTERCTL_SOPS_BINARY":     "workstation.sopsBinary",
	"CLUSTERCTL_BROWSER":         "workstation.browser",
}

// Resolve merges the layers in order and decodes the result.
func (b *Bundle) Resolve(opts ResolveOptions) (*Resolved, error) {
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}

	name := opts.Context
	if name == "" {
		name = env(EnvContext)
	}
	ctx, err := b.Context(name)
	if err != nil {
		return nil, err
	}

	clusterDoc, ok := b.Clusters[ctx.Cluster]
	if !ok {
		return nil, fmt.Errorf("context %q names cluster %q, which no document defines (known: %v)",
			ctx.Name, ctx.Cluster, names(b.Clusters))
	}
	siteName, _ := lookup(clusterDoc.Data, "spec.site").(string)
	siteDoc, ok := b.Sites[siteName]
	if !ok {
		return nil, fmt.Errorf("cluster %q names site %q, which no document defines (known: %v)",
			ctx.Cluster, siteName, names(b.Sites))
	}

	tree := NewTree()

	// 1. built-in defaults
	defaults, err := ParseDocuments("<defaults>", defaultsYAML)
	if err != nil {
		return nil, fmt.Errorf("the built-in defaults are broken: %w", err)
	}
	if len(defaults) > 0 {
		tree.MergeDocument(v1alpha1.LayerDefaults, defaults[0], "", "")
	}

	// 2. the site
	tree.MergeDocument(v1alpha1.LayerSite, siteDoc, "spec", "")

	// 3. the cluster, whose own spec fields land in the effective tree
	for _, field := range []string{"slurm", "groups", "bootPaths"} {
		tree.MergeDocument(v1alpha1.LayerCluster, clusterDoc, "spec."+field, field)
	}
	if err := applyOverrides(tree, v1alpha1.LayerCluster, clusterDoc, "spec.overrides"); err != nil {
		return nil, err
	}

	// 4. the workstation
	wsDoc := b.workstationFor()
	if wsDoc != nil {
		tree.MergeDocumentExcept(v1alpha1.LayerWorkstation, wsDoc, "spec", "workstation", "overrides")
		if err := applyOverrides(tree, v1alpha1.LayerWorkstation, wsDoc, "spec.overrides"); err != nil {
			return nil, err
		}
	}

	// 5. the context
	if err := applyContextOverrides(tree, ctx, b.contextSources[ctx.Name]); err != nil {
		return nil, err
	}

	// 6. the environment
	if err := applyEnv(tree, env); err != nil {
		return nil, err
	}

	// 7. --set
	if err := applySet(tree, opts.Set); err != nil {
		return nil, err
	}
	if opts.Fanout > 0 {
		o := Origin{Layer: v1alpha1.LayerFlags, File: "--fanout"}
		if err := tree.SetPath(v1alpha1.LayerFlags, "fanout.max", int64(opts.Fanout), o); err != nil {
			return nil, err
		}
	}

	resolved := &Resolved{
		Context:     ctx,
		SiteName:    siteName,
		ClusterName: ctx.Cluster,
		Tree:        tree,
		Bundle:      b,
		BaseDir:     dirOf(siteDoc),
	}
	if err := decodeInto(tree.Data(), &resolved.Spec); err != nil {
		return nil, fmt.Errorf("the merged configuration is not valid: %w", err)
	}
	if err := resolved.checkSecretRefs(); err != nil {
		return nil, err
	}
	return resolved, nil
}

// workstationFor picks the Workstation document for this machine: the one
// named after the host, else the unnamed one.
func (b *Bundle) workstationFor() *Document {
	if host, err := os.Hostname(); err == nil {
		if doc, ok := b.Workstations[host]; ok {
			return doc
		}
		if short, _, found := strings.Cut(host, "."); found {
			if doc, ok := b.Workstations[short]; ok {
				return doc
			}
		}
	}
	return b.Workstations[""]
}

// applyOverrides applies the overrides table at a path of a document.
func applyOverrides(tree *Tree, layer string, doc *Document, at string) error {
	overrides, ok := lookup(doc.Data, at).(map[string]any)
	if !ok {
		return nil
	}
	for _, k := range slices.Sorted(maps.Keys(overrides)) {
		src := at + "." + k
		o := doc.Position(src)
		if err := tree.override(layer, k, overrides[k], o, docOrigin(doc), src); err != nil {
			return err
		}
	}
	return nil
}

// applyContextOverrides applies the user and the overrides of the selected
// context, attributed to the Config document that defined it.
func applyContextOverrides(tree *Tree, ctx v1alpha1.Context, src contextSource) error {
	from := func(path string) Origin {
		if src.doc == nil {
			return Origin{}
		}
		return src.doc.Position(joinPath(src.at, path))
	}
	if ctx.User != "" {
		if err := tree.override(v1alpha1.LayerContext, "defaultUser", ctx.User,
			from("user"), from, "user"); err != nil {
			return fmt.Errorf("context %q: %w", ctx.Name, err)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(ctx.Overrides)) {
		src := "overrides." + k
		if err := tree.override(v1alpha1.LayerContext, k, ctx.Overrides[k],
			from(src), from, src); err != nil {
			return fmt.Errorf("context %q: %w", ctx.Name, err)
		}
	}
	return nil
}

// applyEnv applies the environment variables that override configuration.
func applyEnv(tree *Tree, env func(string) string) error {
	for _, name := range slices.Sorted(maps.Keys(envPaths)) {
		raw := env(name)
		if raw == "" {
			continue
		}
		value, err := ParseSetValue(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		o := Origin{Layer: v1alpha1.LayerEnvironment, File: name}
		if err := tree.SetPath(v1alpha1.LayerEnvironment, envPaths[name], value, o); err != nil {
			return err
		}
	}
	return nil
}

// applySet applies the --set assignments.
func applySet(tree *Tree, set map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(set)) {
		value, err := ParseSetValue(set[k])
		if err != nil {
			return fmt.Errorf("--set %s: %w", k, err)
		}
		if err := tree.SetPath(v1alpha1.LayerFlags, k, value, Origin{Layer: v1alpha1.LayerFlags, File: "--set"}); err != nil {
			return err
		}
	}
	return nil
}

// decodeInto converts a parsed tree into a typed value, rejecting keys the
// type does not define. The tree goes through JSON, which is what the struct
// tags describe.
func decodeInto(data map[string]any, target any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	return nil
}

// Decode converts a parsed document into a typed value, rejecting keys the
// type does not define.
func Decode(data map[string]any, target any) error { return decodeInto(data, target) }

// FromCommandLine reports whether the value at a dotted path was given in the
// environment or on the command line rather than in a document, by the path
// itself or by a section it is part of. A relative path given there means what
// it means to the shell it was typed in, not what it means to the site.
func (r *Resolved) FromCommandLine(path string) bool {
	for p := path; p != ""; {
		if o, ok := r.Tree.Origin(p); ok {
			return o.Layer == v1alpha1.LayerEnvironment || o.Layer == v1alpha1.LayerFlags
		}
		cut := strings.LastIndex(p, ".")
		if cut < 0 {
			break
		}
		p = p[:cut]
	}
	return false
}
