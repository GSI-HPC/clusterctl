// SPDX-License-Identifier: LGPL-3.0-or-later

// Package v1alpha1 defines the configuration objects clusterctl reads.
//
// Configuration is split over five kinds, each a YAML document carrying an
// apiVersion and a kind, so that several of them may share one file:
//
//	Config         per administrator: the contexts and which one is current
//	Site           one site: domains, naming, roles, networks, credentials
//	Cluster        one cluster: its site, Slurm, groups and boot paths
//	NodeInventory  the nodes: attributes, racks, addresses and boot paths
//	Workstation    the machine clusterctl runs on: local overrides
//
// The layers are applied in this order, each winning over the ones before it:
// built-in defaults, Site, Cluster, Workstation, the overrides of the current
// context, the environment, and finally --set and the command line flags.
//
// # Compatibility
//
// v1alpha1 carries no compatibility promise. The apiVersion is checked on
// load, so a file written for a later version is reported rather than
// misread.
package v1alpha1
