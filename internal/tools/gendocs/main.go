// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Command gendocs writes the generated part of the documentation site: the
// command reference and the JSON Schemas of the configuration kinds.
//
// Neither is committed. Both describe the binary they were generated from, and
// a copy in the tree is a copy that drifts.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra/doc"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/cli"
	"github.com/GSI-HPC/clusterctl/internal/config"
)

func main() {
	out := flag.String("out", "site/content/reference", "directory for the command reference")
	schemaDir := flag.String("schema", "site/static/schema/v1alpha1", "directory for the JSON Schemas")
	flag.Parse()

	if err := generate(*out, *schemaDir); err != nil {
		fmt.Fprintf(os.Stderr, "gendocs: %v\n", err)
		os.Exit(1)
	}
}

func generate(out, schemaDir string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		return err
	}

	root := cli.NewRootCommand(context.Background(), app.Streams{Out: os.Stdout, Err: os.Stderr})
	root.DisableAutoGenTag = true

	if err := writeIndex(out); err != nil {
		return err
	}
	if err := doc.GenMarkdownTreeCustom(root, out, prepend, link); err != nil {
		return fmt.Errorf("generating the command reference: %w", err)
	}

	for _, kind := range config.SchemaKinds() {
		data, err := config.SchemaJSON(kind)
		if err != nil {
			return err
		}
		path := filepath.Join(schemaDir, strings.ToLower(kind)+".json")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "gendocs: wrote the reference to %s and %d schemas to %s\n",
		out, len(config.SchemaKinds()), schemaDir)
	return nil
}

// writeIndex writes the section page the reference hangs under.
func writeIndex(dir string) error {
	const index = `---
title: Command reference
weight: 90
cascade:
  type: docs
---

Every command, its flags and its arguments, generated from the binary.

This is the exhaustive list. The [manual](../docs/) is the place to start;
come here to check a flag.
`
	return os.WriteFile(filepath.Join(dir, "_index.md"), []byte(index), 0o644)
}

// prepend writes the front matter Hugo needs in front of each page.
func prepend(filename string) string {
	name := strings.TrimSuffix(filepath.Base(filename), ".md")
	title := strings.ReplaceAll(name, "_", " ")

	var b bytes.Buffer
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", title)
	// The root page sorts first, then the groups alphabetically.
	if name == "clusterctl" {
		b.WriteString("weight: 1\n")
	}
	b.WriteString("---\n\n")
	return b.String()
}

// link turns a cross reference between commands into a site-relative link.
func link(filename string) string {
	return "../" + strings.TrimSuffix(filename, ".md") + "/"
}
