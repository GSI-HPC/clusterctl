// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package dhcp_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/dhcp"
	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
)

// The files a configuration includes were read one after the other, as the
// parser reached each: ten includes over ssh, ten round trips in a row. A
// file's includes are read side by side, four at a time, the bound on the
// sessions to one host: each read is held until a fifth is under way,
// which never happens while the bound is kept. What is parsed is what was.
func TestIncludesAreReadSideBySide(t *testing.T) {
	t.Parallel()
	files := map[string]string{}
	var main strings.Builder
	for i := range 10 {
		name := fmt.Sprintf("/etc/dhcp/hosts%d.conf", i)
		fmt.Fprintf(&main, "group {\n  include %q;\n}\n", name)
		files[name] = fmt.Sprintf("host exe%d { fixed-address 10.0.2.%d; }\n", i, i)
	}
	files["/etc/dhcp/dhcpd.conf"] = main.String()
	reads := &fanouttest.InFlight{Hold: 5}
	cfg, err := dhcp.ParseFile("/etc/dhcp/dhcpd.conf", func(file string) ([]byte, error) {
		defer reads.Enter()()
		return []byte(files[file]), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := reads.Peak(); got != 4 {
		t.Errorf("%d files were read at once, want 4", got)
	}
	if got := len(cfg.Hosts); got != 10 {
		t.Errorf("%d hosts, want 10", got)
	}
}

// A file that could not be read is reported where the parser reaches its
// include, not before: an error in a file included earlier is reported
// first, as when the files were read one after the other. Only the files
// the parser would read are read, each once: not a relative name, not the
// file itself, and not a name that only looks like an include.
func TestIncludesAreReadAsTheParserWould(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"/etc/dhcp/dhcpd.conf": `include "/etc/dhcp/broken.conf";
include "/etc/dhcp/gone.conf";
`,
		"/etc/dhcp/broken.conf": "host x {\n",
		"/etc/dhcp/main.conf": `include "/etc/dhcp/a.conf";
include "/etc/dhcp/a.conf";
option domain-name include "/etc/dhcp/not-an-include.conf";
group { include "/etc/dhcp/b.conf"; }
`,
		"/etc/dhcp/a.conf": "host a { fixed-address 10.0.2.1; }\n",
		"/etc/dhcp/b.conf": "include \"/etc/dhcp/b.conf\";\n",
	}
	var (
		mu   sync.Mutex
		read []string
	)
	reader := func(file string) ([]byte, error) {
		mu.Lock()
		read = append(read, file)
		mu.Unlock()
		content, ok := files[file]
		if !ok {
			return nil, errors.New("no such file")
		}
		return []byte(content), nil
	}

	_, err := dhcp.ParseFile("/etc/dhcp/dhcpd.conf", reader)
	if err == nil || !strings.Contains(err.Error(), "broken.conf") || strings.Contains(err.Error(), "no such file") {
		t.Errorf("error = %v, want the error in broken.conf", err)
	}

	read = nil
	_, err = dhcp.ParseFile("/etc/dhcp/main.conf", reader)
	if err == nil || !strings.Contains(err.Error(), "includes itself") {
		t.Errorf("error = %v, want b.conf's include of itself", err)
	}
	slices.Sort(read)
	if want := []string{"/etc/dhcp/a.conf", "/etc/dhcp/b.conf", "/etc/dhcp/main.conf"}; !slices.Equal(read, want) {
		t.Errorf("read %q, want %q", read, want)
	}
}
