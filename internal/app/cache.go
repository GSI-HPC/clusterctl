// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"context"
	"io/fs"
	"os"
	"slices"
	"sync"

	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
)

// Cache keeps the configuration a long running process read, and the
// inventories it built from it, for as long as every file read is still the
// file it was: another inode, size or modification time, or another list of
// files, and they are read again. The MCP server reads the configuration for
// every call, which at 10,000 nodes took most of a second and half a
// gigabyte each time. The files are still found, and each of them looked at,
// on every call, so that an edit is seen by the next one.
//
// What is kept is only ever read, by any number of calls at once: the
// documents, and the inventories. Everything a command resolves from them,
// the merged configuration and its overrides among it, is resolved afresh.
type Cache struct {
	mu          sync.Mutex
	stamps      []stamp
	bundle      *config.Bundle
	inventories map[string]*inventory.Inventory
}

// stamp is a file as it was when it was read.
type stamp struct {
	path string
	info fs.FileInfo
}

func (s stamp) same(info fs.FileInfo) bool {
	return os.SameFile(s.info, info) && s.info.Size() == info.Size() && s.info.ModTime().Equal(info.ModTime())
}

type cacheKey struct{}

// WithCache returns a context whose commands read the configuration through
// the cache.
func WithCache(ctx context.Context, c *Cache) context.Context {
	return context.WithValue(ctx, cacheKey{}, c)
}

func cacheFrom(ctx context.Context) *Cache {
	c, _ := ctx.Value(cacheKey{}).(*Cache)
	return c
}

// load reads the configuration from files, or returns what it read last when
// the files are those it read. A file is looked at before it is read, so that
// one changed while it was read is read again the next time.
func (c *Cache) load(files []string) (*config.Bundle, error) {
	if c == nil {
		return config.Load(files)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	stamps := make([]stamp, 0, len(files))
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			// Load says what is wrong with it; nothing is kept.
			c.bundle = nil
			return config.Load(files)
		}
		stamps = append(stamps, stamp{f, info})
	}
	if c.bundle != nil && slices.EqualFunc(c.stamps, stamps, func(a, b stamp) bool {
		return a.path == b.path && a.same(b.info)
	}) {
		return c.bundle, nil
	}
	bundle, err := config.Load(files)
	if err != nil {
		c.bundle = nil
		return nil, err
	}
	c.stamps, c.bundle, c.inventories = stamps, bundle, map[string]*inventory.Inventory{}
	return bundle, nil
}

// inventory returns the inventory of a cluster of a bundle the cache holds,
// built by build the first time it is asked for.
func (c *Cache) inventory(bundle *config.Bundle, cluster string, build func() (*inventory.Inventory, error)) (*inventory.Inventory, error) {
	if c == nil {
		return build()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bundle != bundle {
		return build()
	}
	if inv, ok := c.inventories[cluster]; ok {
		return inv, nil
	}
	inv, err := build()
	if err == nil {
		c.inventories[cluster] = inv
	}
	return inv, err
}
