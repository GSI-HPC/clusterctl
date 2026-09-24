// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"golang.org/x/term"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/credentials"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/ipmi"
	"github.com/GSI-HPC/clusterctl/internal/naming"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// Credentials returns the credential resolver of the site. It is safe for
// concurrent use, which the BMC commands need: they build one client per
// node in parallel.
func (a *App) Credentials() *credentials.Resolver {
	a.credentialsOnce.Do(func() {
		a.credentials = &credentials.Resolver{
			Credentials: a.Spec.Credentials,
			BaseDir:     a.Resolved.BaseDir,
			Env:         a.opts.Env,
			Identities:  a.Spec.Workstation.Identities,
			Prompt:      a.promptPassword,
			Secret:      a.SecretValue,
			NoTerminal:  !a.IsTTY,
		}
	})
	return a.credentials
}

// promptPassword reads a password from the terminal without echoing it.
func (a *App) promptPassword(prompt string) (string, error) {
	if !a.IsTTY {
		return "", exitcode.Errorf(exitcode.Usage,
			"a password is needed but there is no terminal to ask on; configure another password source")
	}
	fd := int(os.Stdin.Fd())
	// ReadPassword turns echo off and cannot be cancelled. When an interrupt
	// ends the wait, the terminal is put back from here, or it would be left
	// without echo once the process has gone.
	state, stateErr := term.GetState(fd)
	type answer struct {
		secret []byte
		err    error
	}
	answered := make(chan answer, 1)
	_, _ = fmt.Fprint(a.Err, prompt)
	go func() {
		secret, err := term.ReadPassword(fd)
		answered <- answer{secret, err}
	}()
	var got answer
	select {
	case got = <-answered:
	case <-a.Context().Done():
		if stateErr == nil {
			_ = term.Restore(fd, state)
		}
		_, _ = fmt.Fprintln(a.Err)
		return "", exitcode.Wrap(exitcode.Interrupted, a.Context().Err())
	}
	// The terminal echoed nothing, so the cursor is still on the prompt.
	_, _ = fmt.Fprintln(a.Err)
	if got.err != nil {
		return "", got.err
	}
	return string(got.secret), nil
}

// InventoryNode returns the inventory entry of the machine a name refers to.
//
// Host names are not case sensitive and a trailing dot only makes a name
// absolute, so EXE0001 and exe0001. are exe0001, and so is exe1 when the
// inventory pads it. A name with a domain is the node only when it is the
// host name the naming rules give the node's short name; any other domain is
// another machine. Everything looked up per node goes through here, so that
// every spelling of a machine gets its own settings rather than the site's.
func (a *App) InventoryNode(name string) (*inventory.Node, bool) {
	if a.Inventory == nil {
		return nil, false
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if short := naming.Short(name); short != name {
		fqdn, err := a.Namer.FQDN(short)
		if err != nil || !strings.EqualFold(fqdn, name) {
			return nil, false
		}
		name = short
	}
	return a.Inventory.Lookup(name)
}

// BMCHost returns the host name or address of a node's service processor:
// the bmcAddress the inventory records for it, else the name the naming
// rules give it. The inventory wins because it names the one machine the
// site wrote down, where a derived name is only as good as its DNS record.
//
// A node whose service processor has neither is refused; nothing falls back
// to the node's own name, where the BMC password would be sent to the node.
func (a *App) BMCHost(node string) (string, error) {
	if entry, ok := a.InventoryNode(node); ok {
		if entry.BMCAddress != "" {
			return entry.BMCAddress, nil
		}
		node = entry.Name
	}
	host, err := a.Namer.BMC(node)
	if err != nil {
		return "", exitcode.Wrap(exitcode.Usage, err)
	}
	return host, nil
}

// BMCHosts maps every node of a set to its service processor, the way
// BMCHost does.
func (a *App) BMCHosts(nodes *nodeset.NodeSet) (*nodeset.NodeSet, error) {
	out := nodeset.New()
	for _, node := range nodes.Expand() {
		host, err := a.BMCHost(node)
		if err != nil {
			return nil, err
		}
		if err := out.Add(host); err != nil {
			return nil, exitcode.Errorf(exitcode.Usage,
				"the service processor %q of node %q is not a valid host name: %w", host, node, err)
		}
	}
	return out, nil
}

// VendorProfile returns the BMC overrides for a node's hardware vendor.
func (a *App) VendorProfile(node string) v1alpha1.VendorProfile {
	entry, ok := a.InventoryNode(node)
	if !ok {
		return v1alpha1.VendorProfile{}
	}
	vendor := entry.Attributes["vendor"]
	if vendor == "" {
		return v1alpha1.VendorProfile{}
	}
	return a.Spec.BMC.Vendors[vendor]
}

// BMCCredentialName names the credential used for a node's service
// processor: the one its vendor profile names, else the site's. Nodes with
// the same name share one account, which is how a set is grouped for a
// backend that takes one account for many processors.
func (a *App) BMCCredentialName(node string) string {
	if profile := a.VendorProfile(node); profile.Credential != "" {
		return profile.Credential
	}
	return a.Spec.BMC.Credential
}

// BMCCredential resolves the account used for a node's service processor,
// honouring a vendor profile that names its own.
func (a *App) BMCCredential(ctx context.Context, node string) (credentials.Credential, error) {
	name := a.BMCCredentialName(node)
	if name == "" {
		return credentials.Credential{}, exitcode.Errorf(exitcode.Usage,
			"no BMC credential is configured; set bmc.credential in the site document")
	}
	cred, err := a.Credentials().Get(ctx, name)
	if err != nil {
		return credentials.Credential{}, exitcode.Wrap(exitcode.Usage, err)
	}
	return cred, nil
}

// RedfishClient builds a client for a node's service processor.
func (a *App) RedfishClient(ctx context.Context, node string) (*redfish.Client, error) {
	host, err := a.BMCHost(node)
	if err != nil {
		return nil, err
	}
	cred, err := a.BMCCredential(ctx, node)
	if err != nil {
		return nil, err
	}

	spec := a.Spec.BMC.Redfish
	profile := a.VendorProfile(node)

	verify := false
	if spec.TLSVerify != nil {
		verify = *spec.TLSVerify
	}
	if profile.TLSVerify != nil {
		verify = *profile.TLSVerify
	}
	systemPath := spec.SystemPath
	if profile.SystemPath != "" {
		systemPath = profile.SystemPath
	}
	minTLS := spec.MinTLSVersion
	if profile.MinTLSVersion != "" {
		minTLS = profile.MinTLSVersion
	}

	pinPath := a.Path(spec.PinStore)
	if pinPath == "" {
		pinPath = a.StatePath("bmc-pins")
	}
	pins := &redfish.PinStore{Path: pinPath}
	if !verify {
		a.noteFirstContact(pins, host)
	}
	return &redfish.Client{
		Host:          host,
		Username:      cred.Username,
		Password:      cred.Password(),
		SystemPath:    systemPath,
		ResetTypes:    profile.ResetTypes,
		Timeout:       spec.Timeout.Get(),
		Verify:        verify,
		MinTLSVersion: minTLS,
		Pins:          pins,
	}, nil
}

// noteMu keeps the notes of clients built in parallel from interleaving.
var noteMu sync.Mutex

// noteFirstContact says so when no certificate is recorded for a service
// processor yet. The client records whatever the host presents, and sends
// the BMC account to it, so the administrator should know it trusted a
// certificate nobody vouched for, and which host it came from.
func (a *App) noteFirstContact(pins *redfish.PinStore, host string) {
	if _, ok, err := pins.Get(host); err != nil || ok {
		return
	}
	noteMu.Lock()
	defer noteMu.Unlock()
	a.Printf("no certificate is recorded for %s yet; the one it presents now will be recorded and trusted from then on\n", host)
}

// IPMIBackend builds the backend that runs the IPMI tools, on the host role
// bmc.ipmi.via names, with the account of the given node. Nothing runs the
// tools locally, so a site that names no role gets a usage error.
func (a *App) IPMIBackend(ctx context.Context, node string) (*ipmi.Backend, error) {
	cred, err := a.BMCCredential(ctx, node)
	if err != nil {
		return nil, err
	}
	spec := a.Spec.BMC.IPMI
	target, err := a.Role(spec.Via)
	if err != nil {
		if spec.Via != "" {
			return nil, err
		}
		return nil, exitcode.Errorf(exitcode.Usage,
			"no host role is configured to run the IPMI tools on; set bmc.ipmi.via")
	}
	return &ipmi.Backend{
		Runner:   a.Runner,
		Target:   target,
		Spec:     spec,
		Username: cred.Username,
		Password: cred.Password(),
	}, nil
}

// The transports a service processor can be reached over.
const (
	TransportRedfish = "redfish"
	TransportIPMI    = "ipmi"
)

// BMCOrder returns the transport preference for a node, as configured.
func (a *App) BMCOrder(node string) []string {
	if profile := a.VendorProfile(node); len(profile.Order) > 0 {
		return profile.Order
	}
	if len(a.Spec.BMC.Order) > 0 {
		return a.Spec.BMC.Order
	}
	return []string{TransportRedfish, TransportIPMI}
}

// BMCTransports returns the transports to try for a node, in order. An
// entry that names no transport is refused rather than read as Redfish, so a
// misspelt ipmi does not send the BMC account over another protocol.
func (a *App) BMCTransports(node string) ([]string, error) {
	setting := "bmc.order"
	if entry, ok := a.InventoryNode(node); ok {
		if vendor := entry.Attributes["vendor"]; len(a.Spec.BMC.Vendors[vendor].Order) > 0 {
			setting = "bmc.vendors." + vendor + ".order"
		}
	}
	var out []string
	for _, entry := range a.BMCOrder(node) {
		transport := strings.ToLower(strings.TrimSpace(entry))
		if transport != TransportRedfish && transport != TransportIPMI {
			return nil, exitcode.Errorf(exitcode.Usage,
				"%s names the transport %q, which is not one of %s and %s",
				setting, entry, TransportRedfish, TransportIPMI)
		}
		if !slices.Contains(out, transport) {
			out = append(out, transport)
		}
	}
	return out, nil
}

// PreferredBMCTransport returns the first transport of the order, or
// Redfish when the order is not valid.
func (a *App) PreferredBMCTransport(node string) string {
	order, err := a.BMCTransports(node)
	if err != nil || len(order) == 0 {
		return TransportRedfish
	}
	return order[0]
}
