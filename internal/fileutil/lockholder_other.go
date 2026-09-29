// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build !linux

package fileutil

// lockHolder does not know who holds a lock where no /proc/locks tells it.
func lockHolder(string) string { return "" }
