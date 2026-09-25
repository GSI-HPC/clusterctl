// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"context"
	"sync"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/dhcp"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// dhcpMemo is the DHCP configuration a command has read.
type dhcpMemo struct {
	mu     sync.Mutex
	done   bool
	config *dhcp.Config
	err    error
}

// DHCPConfig fetches and parses the DHCP server's configuration, and every
// file it includes, once per command.
//
// A command asks for it for each node the inventory gives no address, and
// boot status and provision status go on past a node that failed. Read for
// each, the configuration was fetched once per node whenever its cached copy
// had expired, and a server that could not be read was asked again for every
// node, an ssh timeout each. So what the first read found is kept for the
// command, a failure as much as a configuration. A read the context stopped
// is not kept, since it says nothing about the server. A caller that asks
// while the file is being read waits for that read.
func (a *App) DHCPConfig(ctx context.Context) (*dhcp.Config, error) {
	m := &a.dhcp
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done {
		return m.config, m.err
	}
	config, err := a.readDHCPConfig(ctx)
	if err != nil && ctx.Err() != nil {
		return nil, err
	}
	m.done, m.config, m.err = true, config, err
	return config, err
}

func (a *App) readDHCPConfig(ctx context.Context) (*dhcp.Config, error) {
	spec := a.Spec.Services.DHCP
	if spec.Role == "" {
		return nil, exitcode.Errorf(exitcode.Usage,
			"no host role runs the DHCP server; set services.dhcp.role")
	}
	path := spec.ConfigPath
	ttl := spec.CacheTTL.Or(2 * time.Minute)
	cfg, err := dhcp.ParseFile(path, func(file string) ([]byte, error) {
		return a.RemoteFile(ctx, spec.Role, file, ttl)
	})
	if err != nil {
		// A file that could not be fetched keeps the exit code saying so.
		if exitcode.Has(err) {
			return nil, err
		}
		return nil, exitcode.Errorf(exitcode.TargetFailed, "parsing %s: %w", path, err)
	}
	return cfg, nil
}
