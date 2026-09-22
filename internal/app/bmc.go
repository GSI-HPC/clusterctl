// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/credentials"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/ipmi"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
)

// Credentials returns the credential resolver of the site.
func (a *App) Credentials() *credentials.Resolver {
	if a.credentials == nil {
		a.credentials = &credentials.Resolver{
			Credentials: a.Spec.Credentials,
			BaseDir:     a.Resolved.BaseDir,
			Identities:  a.Spec.Workstation.Identities,
			Prompt:      a.promptPassword,
		}
	}
	return a.credentials
}

// promptPassword reads a password from the terminal without echoing it.
func (a *App) promptPassword(prompt string) (string, error) {
	if !a.IsTTY {
		return "", exitcode.Errorf(exitcode.Usage,
			"a password is needed but there is no terminal to ask on; configure another password source")
	}
	_, _ = fmt.Fprint(a.Err, prompt)
	secret, err := term.ReadPassword(int(os.Stdin.Fd()))
	// The terminal echoed nothing, so the cursor is still on the prompt.
	_, _ = fmt.Fprintln(a.Err)
	if err != nil {
		return "", err
	}
	return string(secret), nil
}

// VendorProfile returns the BMC overrides for a node's hardware vendor.
func (a *App) VendorProfile(node string) v1alpha1.VendorProfile {
	entry, ok := a.Inventory.Lookup(node)
	if !ok {
		return v1alpha1.VendorProfile{}
	}
	vendor := entry.Attributes["vendor"]
	if vendor == "" {
		return v1alpha1.VendorProfile{}
	}
	return a.Spec.BMC.Vendors[vendor]
}

// BMCCredential resolves the account used for a node's service processor,
// honouring a vendor profile that names its own.
func (a *App) BMCCredential(ctx context.Context, node string) (credentials.Credential, error) {
	name := a.Spec.BMC.Credential
	if profile := a.VendorProfile(node); profile.Credential != "" {
		name = profile.Credential
	}
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
	host, err := a.Namer.BMC(node)
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
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
	return &redfish.Client{
		Host:          host,
		Username:      cred.Username,
		Password:      cred.Password(),
		SystemPath:    systemPath,
		Timeout:       spec.Timeout.Get(),
		Verify:        verify,
		MinTLSVersion: minTLS,
		Pins:          &redfish.PinStore{Path: pinPath},
	}, nil
}

// IPMIBackend builds the backend that runs the IPMI tools, on the host role
// the configuration names or locally when it names none.
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

// BMCOrder returns the transport preference for a node.
func (a *App) BMCOrder(node string) []string {
	if profile := a.VendorProfile(node); len(profile.Order) > 0 {
		return profile.Order
	}
	if len(a.Spec.BMC.Order) > 0 {
		return a.Spec.BMC.Order
	}
	return []string{"redfish", "ipmi"}
}

// PreferredBMCTransport returns the first transport of the order, which is
// what a command uses unless the administrator names another.
func (a *App) PreferredBMCTransport(node string) string {
	order := a.BMCOrder(node)
	return strings.ToLower(order[0])
}
