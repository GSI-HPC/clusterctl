// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package redfish talks to the Redfish interface of a service processor.
//
// The client is written here rather than taken from a library because three
// of its properties are requirements rather than preferences: the TLS
// certificate is pinned instead of verified against public roots, which is
// the only check available for a self signed BMC certificate; an action is
// never retried automatically, because a reset that timed out may well have
// been carried out; and the reset types a machine accepts are asked for
// rather than guessed, because firmware differs about which ones exist.
package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/hostname"
)

// DefaultSystemPath is where most firmware puts the computer system.
const DefaultSystemPath = "/redfish/v1/Systems/1"

// Client talks to one service processor.
type Client struct {
	// Host is the name or address of the service processor.
	Host string
	// Username and Password authenticate with it.
	Username string
	Password string
	// SystemPath is the Redfish path of the computer system.
	SystemPath string
	// Timeout bounds one request.
	Timeout time.Duration
	// Verify checks the certificate against the system roots instead of
	// pinning it. Few sites can use this.
	Verify bool
	// Pins records the certificate seen for each host.
	Pins *PinStore
	// MinTLSVersion allows old firmware to be reached: "1.0" to "1.3".
	MinTLSVersion string
	// Transport overrides the HTTP transport, which the tests use.
	Transport http.RoundTripper
	// DialContext overrides how the pinning transport connects, so that the
	// tests can reach a test server through the real TLS checks.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	client *http.Client
}

// BaseURL is the root of the Redfish service on this host.
//
// The host has to be a host name or an address and nothing more. A port,
// an account or a URL delimiter in it would send the request, and the
// password with it, somewhere other than the service processor.
func (c *Client) BaseURL() (string, error) {
	u, err := c.base()
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (c *Client) base() (*url.URL, error) {
	if err := hostname.CheckHost(c.Host); err != nil {
		return nil, fmt.Errorf("refusing to send a Redfish request: the service processor %w", err)
	}
	host := c.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return &url.URL{Scheme: "https", Host: host}, nil
}

func (c *Client) system() string {
	if c.SystemPath != "" {
		return c.SystemPath
	}
	return DefaultSystemPath
}

// httpClient builds the HTTP client, pinning the certificate unless full
// verification was asked for.
func (c *Client) httpClient(ctx context.Context) (*http.Client, error) {
	if c.client != nil {
		return c.client, nil
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	rt := c.Transport
	if rt == nil {
		tlsConfig := &tls.Config{MinVersion: minVersion(c.MinTLSVersion)}
		if !c.Verify {
			// The chain cannot be verified, so it is not: the certificate is
			// compared with the one recorded for this host instead, and a
			// change is refused.
			tlsConfig.InsecureSkipVerify = true
			tlsConfig.VerifyPeerCertificate = c.pinVerifier(ctx)
		}
		rt = &http.Transport{
			TLSClientConfig:     tlsConfig,
			TLSHandshakeTimeout: timeout,
			DialContext:         c.DialContext,
			// Actions must never be replayed, so nothing here retries.
			DisableKeepAlives: false,
		}
	}
	c.client = &http.Client{Transport: rt, Timeout: timeout}
	return c.client, nil
}

func minVersion(v string) uint16 {
	switch v {
	case "1.0":
		return tls.VersionTLS10
	case "1.1":
		return tls.VersionTLS11
	case "1.3":
		return tls.VersionTLS13
	default:
		return tls.VersionTLS12
	}
}

// pinVerifier compares the presented certificate with the recorded one, and
// records it the first time it is seen.
func (c *Client) pinVerifier(ctx context.Context) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("%s presented no certificate", c.Host)
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("%s presented an unreadable certificate: %w", c.Host, err)
		}
		seen := Fingerprint(cert)
		if c.Pins == nil || c.Pins.Path == "" {
			return nil
		}
		recorded, ok, err := c.Pins.Get(c.Host)
		if err != nil {
			return err
		}
		if !ok {
			return c.Pins.Set(ctx, c.Host, seen)
		}
		if recorded != seen {
			return &PinMismatchError{Host: c.Host, Recorded: recorded, Seen: seen}
		}
		return nil
	}
}

// Do performs one request and decodes the JSON body.
func (c *Client) Do(ctx context.Context, method, path string, body any) (map[string]any, error) {
	raw, status, err := c.DoRaw(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{"status": status}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s answered with something that is not JSON: %w", c.Host, err)
	}
	return out, nil
}

// DoRaw performs one request and returns the body and the status.
func (c *Client) DoRaw(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	client, err := c.httpClient(ctx)
	if err != nil {
		return nil, 0, err
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = bytes.NewReader(encoded)
	}

	base, err := c.base()
	if err != nil {
		return nil, 0, err
	}
	target := base.JoinPath(path)
	// JoinPath only ever changes the path, so this holds by construction.
	// It is checked anyway, because the password goes wherever the URL
	// points.
	if target.Host != base.Host || target.User != nil {
		return nil, 0, fmt.Errorf("the Redfish path %q leaves the service processor %s", path, c.Host)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), payload)
	if err != nil {
		return nil, 0, err
	}
	req.SetBasicAuth(c.Username, c.Password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, c.unreachable(ctx, fmt.Errorf("%s: %w", c.Host, unwrapURLError(err)))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, c.unreachable(ctx, fmt.Errorf("%s: reading the answer: %w", c.Host, err))
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return raw, resp.StatusCode, exitcode.Wrap(exitcode.Transport,
			fmt.Errorf("%s: %s: %s", c.Host, resp.Status, redfishMessage(raw)))
	}
	if resp.StatusCode >= 400 {
		return raw, resp.StatusCode, fmt.Errorf("%s: %s: %s", c.Host, resp.Status, redfishMessage(raw))
	}
	return raw, resp.StatusCode, nil
}

// unreachable marks an error that left no answer from the processor, which
// could not be reached, trusted or heard to the end, as a transport failure.
// An interrupt is left as it is, so that it still exits 130.
func (c *Client) unreachable(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	return exitcode.Wrap(exitcode.Transport, err)
}

// unwrapURLError strips the wrapper the HTTP client adds, which repeats the
// whole URL and hides the cause.
func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// redfishMessage pulls the human readable part out of an error body.
func redfishMessage(raw []byte) string {
	var body struct {
		Error struct {
			Message  string `json:"message"`
			Extended []struct {
				Message    string `json:"Message"`
				Resolution string `json:"Resolution"`
			} `json:"@Message.ExtendedInfo"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return strings.TrimSpace(string(raw))
	}
	var parts []string
	if body.Error.Message != "" {
		parts = append(parts, body.Error.Message)
	}
	for _, e := range body.Error.Extended {
		if e.Message != "" {
			parts = append(parts, e.Message)
		}
		if e.Resolution != "" && e.Resolution != "None." {
			parts = append(parts, e.Resolution)
		}
	}
	if len(parts) == 0 {
		return strings.TrimSpace(string(raw))
	}
	return strings.Join(parts, " ")
}

// Get reads a Redfish resource.
func (c *Client) Get(ctx context.Context, path string) (map[string]any, error) {
	return c.Do(ctx, http.MethodGet, path, nil)
}

// Post sends an action to a Redfish resource.
func (c *Client) Post(ctx context.Context, path string, body any) (map[string]any, error) {
	return c.Do(ctx, http.MethodPost, path, body)
}
