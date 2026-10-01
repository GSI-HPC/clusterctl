// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport

import "time"

// Reach is how long reaching target may take before its command starts,
// which Run adds to a command's timeout and the kill grace.
func Reach(c *Client, target Target) time.Duration {
	return c.reach(target)
}

// RoleByHost is the role a host is taken as: the first, in name order, whose
// host it is.
func RoleByHost(c *Client, host string) string {
	return c.roleByHost(host)
}
