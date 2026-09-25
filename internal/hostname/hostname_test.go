// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package hostname_test

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/hostname"
)

func TestCheckAcceptsHostNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"exe0001",
		"wlm01.hpc.example.org",
		// A final dot only says the name is fully qualified.
		"wlm01.hpc.example.org.",
		"EXE0001",
		"a",
		"1",
		"10.0.1.1",
		// A decimal address is an all-digit label; it cannot carry a port.
		"2130706433",
		"x-1",
		strings.Repeat("a", 63) + ".org",
	} {
		if err := hostname.Check(name); err != nil {
			t.Errorf("Check(%q) = %v, want it accepted", name, err)
		}
	}
}

func TestCheckRefusesWhatSSHOrAURLGivesAMeaning(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"",
		".",
		// ssh reads these as options.
		"-oProxyCommand=touch${IFS}/tmp/pwned1;#",
		"-v",
		"exe0001.-oProxyCommand=x",
		// A URL reads these as port, userinfo, path, query and fragment.
		"localhost:8443#",
		"localhost?",
		"localhost/x",
		"x@localhost",
		"[::1]",
		// Neither belongs in a host name.
		"exe 0001",
		"exe\n0001",
		"exe_0001",
		"exe%0001",
		"exé0001",
		"exe0001-",
		"exe..org",
		".exe0001",
		"exe0001..",
		strings.Repeat("a", 64),
		strings.Repeat("a.", 127) + "ab",
	} {
		if err := hostname.Check(name); err == nil {
			t.Errorf("Check(%q) accepted it", name)
		}
	}
}

func TestCheckHostAcceptsAddresses(t *testing.T) {
	t.Parallel()

	for _, host := range []string{"bmc.example.org", "127.0.0.1", "::1", "fe80::1", "2001:db8::2"} {
		if err := hostname.CheckHost(host); err != nil {
			t.Errorf("CheckHost(%q) = %v, want it accepted", host, err)
		}
	}
	for _, host := range []string{"[::1]", "fe80::1%eth0", "127.0.0.1:443", "::1]:443", "a@127.0.0.1", "-1"} {
		if err := hostname.CheckHost(host); err == nil {
			t.Errorf("CheckHost(%q) accepted it", host)
		}
	}
}

func TestCheckQuotesWhatItRefuses(t *testing.T) {
	t.Parallel()

	// The name may come from an agent or a group source, so a control
	// character in it is escaped rather than written to the terminal.
	err := hostname.Check("exe\x1b[2J")
	if err == nil {
		t.Fatal("Check accepted an escape sequence")
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Errorf("the error carries the raw escape character: %q", err)
	}
}

func TestCheckUser(t *testing.T) {
	t.Parallel()

	for _, user := range []string{"alice_adm", "Alice.Adm", "root", ".svc", "_svc", "a-b", "1000"} {
		if err := hostname.CheckUser(user); err != nil {
			t.Errorf("CheckUser(%q) = %v, want it accepted", user, err)
		}
	}
	// "@" and "$" are Kerberos realms and machine accounts, which ssh
	// cannot be given in user@host; the rest ssh reads as an option or
	// ssh_config as the end of a value.
	for _, user := range []string{"", "-l", "-oProxyCommand=x", "alice@EXAMPLE.ORG", "svc$", `DOM\alice`,
		"alice adm", "al:ice", "ålice", "alice\nUser root"} {
		if err := hostname.CheckUser(user); err == nil {
			t.Errorf("CheckUser(%q) accepted it", user)
		} else if strings.ContainsRune(err.Error(), '\n') {
			t.Errorf("CheckUser(%q): the error carries the raw newline: %q", user, err)
		}
	}
}
