// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// Report 4.19: the instructions offered @idle as a working node set, which
// no group source provides. Every group the agent is shown has to resolve
// against the example site.
func TestInstructionsNameGroupsThatExist(t *testing.T) {
	f := start(t, setup{})
	texts := []string{f.session.InitializeResult().Instructions}
	tools, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		schema, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(schema))
	}

	refs := map[string]bool{}
	pattern := regexp.MustCompile(`@[A-Za-z0-9_.:-]*[A-Za-z0-9_]`)
	for _, text := range texts {
		for _, ref := range pattern.FindAllString(text, -1) {
			// "a bare @name" explains the syntax; it is no example.
			if ref != "@name" {
				refs[ref] = true
			}
		}
	}
	if len(refs) == 0 {
		t.Fatal("no group reference found; the pattern is wrong")
	}

	// The login node knows the example cluster's partitions and nothing
	// else, so a Slurm state is not mistaken for a partition.
	partitions := map[string]string{"main": "exe[0001-0008]", "debug": "exe[0009-0010]"}
	sinfo := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if slices.Contains(req.Argv, "-p") {
			return &transport.Result{Target: tg, Stdout: partitions[req.Argv[len(req.Argv)-1]]}, nil
		}
		return &transport.Result{Target: tg, Stdout: "main\ndebug\n"}, nil
	}}
	dir := t.TempDir()
	a, err := app.New(context.Background(), app.Streams{
		Out: &strings.Builder{}, Err: &strings.Builder{},
		StateDir: filepath.Join(dir, "state"), CacheDir: filepath.Join(dir, "cache"),
	}, app.Options{ConfigFiles: []string{exampleDir}, Runner: sinfo})
	if err != nil {
		t.Fatal(err)
	}
	for ref := range refs {
		if _, err := a.Select(ref); err != nil {
			t.Errorf("the agent is shown %s, which does not resolve: %v", ref, err)
		}
	}
}
