// SPDX-License-Identifier: LGPL-3.0-or-later

package v1alpha1

import "fmt"

// GroupVersion identifies this version of the configuration schema.
const GroupVersion = "clusterctl/v1alpha1"

// The kinds a configuration file may hold.
const (
	KindConfig        = "Config"
	KindSite          = "Site"
	KindCluster       = "Cluster"
	KindNodeInventory = "NodeInventory"
	KindWorkstation   = "Workstation"
)

// Kinds lists every kind in the order documentation and validation use.
func Kinds() []string {
	return []string{KindConfig, KindSite, KindCluster, KindNodeInventory, KindWorkstation}
}

// TypeMeta identifies the schema a document is written against. Every kind
// embeds it.
type TypeMeta struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion" jsonschema:"required,description=Schema version of this document, example=clusterctl/v1alpha1"`
	Kind       string `json:"kind" yaml:"kind" jsonschema:"required,description=The kind of object this document describes"`
}

// ObjectMeta names a document so that other documents can refer to it.
type ObjectMeta struct {
	Name        string            `json:"name,omitempty" yaml:"name,omitempty" jsonschema:"description=Name other documents refer to this one by"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty" jsonschema:"description=Free text shown in config view"`
	Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty" jsonschema:"description=Arbitrary key/value labels"`
}

// KnownKind reports whether kind is one this version defines.
func KnownKind(kind string) bool {
	for _, k := range Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// CheckTypeMeta validates the apiVersion and kind of a document.
func CheckTypeMeta(tm TypeMeta) error {
	if tm.APIVersion == "" {
		return fmt.Errorf("apiVersion is missing; expected %q", GroupVersion)
	}
	if tm.APIVersion != GroupVersion {
		return fmt.Errorf("unsupported apiVersion %q; this build understands %q", tm.APIVersion, GroupVersion)
	}
	if tm.Kind == "" {
		return fmt.Errorf("kind is missing; expected one of %v", Kinds())
	}
	if !KnownKind(tm.Kind) {
		return fmt.Errorf("unknown kind %q; expected one of %v", tm.Kind, Kinds())
	}
	return nil
}
