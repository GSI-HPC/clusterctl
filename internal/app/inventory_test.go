// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/config"
)

// A list of inventories that did not decode was dropped, and the cluster
// read every inventory that was loaded instead.
func TestClusterInventoriesThatDoNotDecodeAreReported(t *testing.T) {
	doc := func(data map[string]any) *config.Document { return &config.Document{File: "cluster.yaml", Data: data} }
	bundle := &config.Bundle{
		Sites: map[string]*config.Document{"sitea": doc(nil)},
		Clusters: map[string]*config.Document{
			"alpha": doc(map[string]any{"spec": map[string]any{"site": "sitea", "inventories": "sitea"}}),
			"beta":  doc(map[string]any{"spec": map[string]any{"site": "sitea"}}),
		},
		Inventories: map[string]*config.Document{"sitea": doc(nil), "other": doc(nil)},
	}
	names, err := clusterInventories(bundle, "alpha")
	if err == nil || !strings.Contains(err.Error(), "cluster.yaml") || !strings.Contains(err.Error(), `"alpha"`) {
		t.Errorf("clusterInventories = %v, %v; want the decode error reported with the file", names, err)
	}

	// One site loaded: every inventory is its own.
	names, err = clusterInventories(bundle, "beta")
	if err != nil || !slices.Equal(names, []string{"other", "sitea"}) {
		t.Errorf("clusterInventories = %v, %v; want every inventory of the one site", names, err)
	}
}
