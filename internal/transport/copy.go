// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// CopyRequest describes a file transfer.
type CopyRequest struct {
	// Sources are local paths when Upload is set, and remote paths
	// otherwise.
	Sources []string
	// Destination is the remote path when Upload is set, and a local path
	// otherwise.
	Destination string
	// Upload says which way the transfer goes.
	Upload bool
	// Recursive copies directories.
	Recursive bool
	// Preserve keeps modification times and modes.
	Preserve bool
}

// CopyArgs builds the scp argument vector for a transfer.
func (c *Client) CopyArgs(target Target, req CopyRequest) ([]string, error) {
	if len(req.Sources) == 0 {
		return nil, fmt.Errorf("no source was given to copy")
	}
	if req.Destination == "" {
		return nil, fmt.Errorf("no destination was given to copy to")
	}
	config, err := c.ConfigPath()
	if err != nil {
		return nil, err
	}

	args := []string{c.ScpBinary(), "-F", config}
	if req.Recursive {
		args = append(args, "-r")
	}
	if req.Preserve {
		args = append(args, "-p")
	}

	host := target.Host
	if user := c.userFor(target); user != "" {
		host = user + "@" + host
	}
	// A path holding a colon would be read as a host, and one starting with
	// a dash as an option, so both are made unambiguous.
	remote := func(path string) string { return fmt.Sprintf("%s:%s", host, path) }

	args = append(args, "--")
	if req.Upload {
		for _, src := range req.Sources {
			args = append(args, localPath(src))
		}
		args = append(args, remote(req.Destination))
		return args, nil
	}
	for _, src := range req.Sources {
		args = append(args, remote(src))
	}
	return append(args, localPath(req.Destination)), nil
}

// localPath makes a relative path explicit so that scp cannot read it as a
// host name or as an option.
func localPath(path string) string {
	switch {
	case strings.HasPrefix(path, "-"):
		return "./" + path
	case strings.Contains(path, ":") && !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "./"):
		return "./" + path
	default:
		return path
	}
}

// Copy runs a file transfer.
func (c *Client) Copy(ctx context.Context, target Target, req CopyRequest) (*Result, error) {
	args, err := c.CopyArgs(target, req)
	if err != nil {
		return nil, err
	}
	cmd := command(ctx, args)
	stderr := &capture{limit: DefaultMaxOutput}
	cmd.Stderr = stderr
	cmd.Stdout = os.Stderr
	// When the context ends scp is killed, but the ssh it started can hold
	// the pipe open on a stalled connection; stop waiting for it.
	cmd.WaitDelay = killGrace

	start := time.Now()
	runErr := cmd.Run()
	result := &Result{Target: target, Stderr: stderr.String(), Duration: time.Since(start)}
	result.ExitCode, result.Err = classify(ctx, target, runErr, result.Stderr)
	return result, nil
}
