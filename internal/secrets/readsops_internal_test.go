// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets

import (
	"reflect"
	"testing"

	"github.com/goccy/go-yaml"
)

// readSops parsed a file three times, once for its documents and once for
// each decode. It decodes the one document it parsed, and reads what the
// two decodes of the whole file read: the values, anchors, aliases and
// comments among them, and the sops metadata.
func TestReadSopsReadsWhatTheDecodesOfTheFileRead(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		`apiVersion: clusterctl/v1alpha1
kind: Secret
metadata: {name: example}
data:
  # a comment
  password: ENC[AES256_GCM,data:abc=,iv:x=,tag:y=,type:str]
  base: &base ENC[AES256_GCM,data:def=,iv:x=,tag:y=,type:str]
  again: *base
sops:
  age:
    - recipient: age1qqqq
      enc: |
        -----BEGIN AGE ENCRYPTED FILE-----
        xyz
        -----END AGE ENCRYPTED FILE-----
  lastmodified: "2026-09-01T10:00:00Z"
  mac: ENC[AES256_GCM,data:mac=,iv:x=,tag:y=,type:str]
  encrypted_regex: ^(data|binaryData)$
  version: 3.9.0
`,
		"---\nkind: Secret\nsops: {lastmodified: '2026-09-01T10:00:00Z', mac: m, version: 3.9.0, age: [{recipient: r, enc: e}]}\nlist: [1, two, {three: 3}]\n",
	} {
		got, err := readSops([]byte(doc))
		if err != nil {
			t.Fatalf("readSops: %v\n%s", err, doc)
		}
		var values map[string]any
		if err := yaml.Unmarshal([]byte(doc), &values); err != nil {
			t.Fatal(err)
		}
		delete(values, "sops")
		var meta struct {
			Sops sopsMetadata `yaml:"sops"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatal(err)
		}
		info, err := inspect(meta.Sops)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.values, values) || !reflect.DeepEqual(got.info, info) {
			t.Errorf("readSops = %+v, %+v; the decodes of the file read %+v, %+v", got.values, got.info, values, info)
		}
	}
}
