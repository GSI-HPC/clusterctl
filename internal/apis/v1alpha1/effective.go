// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package v1alpha1

// EffectiveSpec is the configuration a command actually runs against: the
// site, the cluster and the workstation layers merged into one tree.
//
// It is never written by hand. "clusterctl config view" prints it, and
// "clusterctl config view --show-sources" says which layer set each value.
type EffectiveSpec struct {
	SiteSpec `json:",inline" yaml:",inline"`

	// Slurm, Groups and BootPaths come from the Cluster layer.
	Slurm     SlurmSpec      `json:"slurm,omitempty" yaml:"slurm,omitempty"`
	Groups    GroupsSpec     `json:"groups,omitempty" yaml:"groups,omitempty"`
	BootPaths []BootPathRule `json:"bootPaths,omitempty" yaml:"bootPaths,omitempty"`

	// Workstation comes from the Workstation layer.
	Workstation WorkstationSpec `json:"workstation,omitempty" yaml:"workstation,omitempty"`

	// DefaultUser is the remote account used for roles and nodes that name
	// none. The context sets it; an empty value means the local account.
	DefaultUser string `json:"defaultUser,omitempty" yaml:"defaultUser,omitempty"`
}

// Names of the merge layers, in the order they are applied. They appear in
// the output of config view --show-sources.
const (
	LayerDefaults    = "defaults"
	LayerSite        = "site"
	LayerCluster     = "cluster"
	LayerWorkstation = "workstation"
	LayerContext     = "context"
	LayerEnvironment = "environment"
	LayerFlags       = "flags"
)

// Layers lists the merge layers in the order they are applied.
func Layers() []string {
	return []string{
		LayerDefaults, LayerSite, LayerCluster,
		LayerWorkstation, LayerContext, LayerEnvironment, LayerFlags,
	}
}
