// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testServer is an SSH server that lets anyone in and answers every command
// with a fixed line. It stands in for a cluster host, so that the generated
// configuration is exercised by the real OpenSSH client: what matters about
// that file is how ssh reads it, not how it looks.
type testServer struct {
	Host string
	Port int
	// KnownHosts is the line a host key file holds for this server.
	KnownHosts string
}

// serverOutput is what the server prints for every command.
const serverOutput = "hello from the test server"

// requireSSH skips a test that needs the OpenSSH client when there is none.
func requireSSH(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("the OpenSSH client is not installed")
	}
	return path
}

// startServer runs a test server until the test ends.
func startServer(t *testing.T) *testServer {
	t.Helper()
	requireSSH(t)

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				serveConn(conn, config)
			})
		}
	})

	port := listener.Addr().(*net.TCPAddr).Port
	return &testServer{
		Host: "127.0.0.1",
		Port: port,
		KnownHosts: fmt.Sprintf("[127.0.0.1]:%d %s", port,
			strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))),
	}
}

// otherKnownHosts returns a host key line for the server's address that holds
// a key the server does not have.
func (s *testServer) otherKnownHosts(t *testing.T) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("[127.0.0.1]:%d %s", s.Port,
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))))
}

// Options returns the role options that point ssh at the server.
func (s *testServer) Options() map[string]string {
	return map[string]string{"Port": strconv.Itoa(s.Port), "BatchMode": "yes"}
}

func serveConn(conn net.Conn, config *ssh.ServerConfig) {
	defer func() { _ = conn.Close() }()
	_, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		channel, reqs, err := newChannel.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = channel.Close() }()
			for req := range reqs {
				switch req.Type {
				case "exec", "shell":
					_ = req.Reply(true, nil)
					_, _ = fmt.Fprintln(channel, serverOutput)
					status := make([]byte, 4)
					binary.BigEndian.PutUint32(status, 0)
					_, _ = channel.SendRequest("exit-status", false, status)
					return
				default:
					_ = req.Reply(req.Type == "env" || req.Type == "pty-req", nil)
				}
			}
		}()
	}
}

// writeFile writes a file for a test and returns its path.
func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// resolved runs ssh -G with a configuration and returns what ssh settles on
// for a host, keyword by keyword.
func resolved(t *testing.T, config, host string) map[string]string {
	t.Helper()
	bin := requireSSH(t)
	out, err := exec.Command(bin, "-G", "-F", config, host).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G -F %s %s failed: %v\n%s", config, host, err, out)
	}
	values := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if ok {
			if prev, seen := values[key]; seen {
				value = prev + "\n" + value
			}
			values[key] = value
		}
	}
	return values
}
