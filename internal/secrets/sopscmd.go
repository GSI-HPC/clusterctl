// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// MinSopsVersion is the oldest sops clusterctl decrypts with: 3.10.0 is the
// first that reads the file from stdin and opens it with an OpenSSH key.
const MinSopsVersion = "3.10.0"

// errUnreadable is all that is said about a file sops failed to decrypt
// once it may have opened the data key: what sops says then can quote a
// decrypted value.
var errUnreadable = errors.New("the file could not be decrypted or was changed without sops")

// errChanged is a file whose message authentication code does not match.
var errChanged = errors.New("the file was changed without sops: its message authentication code does not match")

// The exit statuses of the sops command that clusterctl tells apart
// (cmd/sops/codes in the sops source).
const (
	sopsMACMismatch   = 51
	sopsMACNotFound   = 52
	sopsNoKeyRetrieve = 128
)

// killGrace is how long sops has to go after it is asked to, and how long
// Wait waits for the output of anything sops left behind.
const killGrace = 5 * time.Second

// maxOutput bounds what is read from sops. A Secret is a small file; more
// than this is not one.
const (
	maxStdout = 64 << 20
	maxStderr = 64 << 10
)

// Sops is the sops command clusterctl decrypts with.
type Sops struct {
	// Binary is the command, "sops" when empty. A bare name is looked up
	// in PATH, as the ssh client is.
	Binary string

	mu      sync.Mutex
	checked bool
	found   SopsFound
	err     error
}

// SopsFound is the sops a Sops resolved to.
type SopsFound struct {
	Path    string
	Version string
}

func (f SopsFound) String() string { return f.Path + " " + f.Version }

func (s *Sops) binary() string {
	if s.Binary == "" {
		return "sops"
	}
	return s.Binary
}

// Find looks sops up and checks that it is recent enough, once. A command
// that uses no secret never calls it, and so runs without sops.
func (s *Sops) Find(ctx context.Context) (SopsFound, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checked {
		s.found, s.err = s.find(ctx)
		s.checked = true
	}
	return s.found, s.err
}

func (s *Sops) find(ctx context.Context) (SopsFound, error) {
	path, err := exec.LookPath(s.binary())
	if err != nil {
		return SopsFound{}, fmt.Errorf("sops %s or later is needed to read a Secret document: %w", MinSopsVersion, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "clusterctl-sops-")
	if err != nil {
		return SopsFound{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// Without --disable-version-check, sops asks GitHub for its latest
	// release before it answers.
	cmd := command(ctx, path, dir, isolated(dir), false, "--disable-version-check", "--version")
	cmd.Env = append(cmd.Env, "SOPS_DISABLE_VERSION_CHECK=true")
	var stdout bytes.Buffer
	cmd.Stdout = &capped{w: &stdout, n: maxStderr}
	if err := cmd.Run(); err != nil {
		return SopsFound{}, fmt.Errorf("%s --version failed: %w; sops %s or later is needed to read a Secret document",
			path, err, MinSopsVersion)
	}
	version, ok := parseSopsVersion(stdout.String())
	if !ok {
		return SopsFound{}, fmt.Errorf("%s does not say which version it is; sops %s or later is needed to read a Secret document",
			path, MinSopsVersion)
	}
	found := SopsFound{Path: path, Version: version}
	if !atLeast(version, MinSopsVersion) {
		return found, fmt.Errorf("%s is sops %s; sops %s or later is needed to read a Secret document", path, version, MinSopsVersion)
	}
	return found, nil
}

var sopsVersionLine = regexp.MustCompile(`^sops (\d+\.\d+\.\d+)`)

func parseSopsVersion(out string) (string, bool) {
	m := sopsVersionLine.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// atLeast compares two versions of the form major.minor.patch.
func atLeast(version, min string) bool {
	a, b := strings.Split(version, "."), strings.Split(min, ".")
	for i := range b {
		x, _ := strconv.Atoi(a[i])
		y, _ := strconv.Atoi(b[i])
		if x != y {
			return x > y
		}
	}
	return true
}

// SopsKeys says which keys may open a sops file.
type SopsKeys struct {
	// Identities are the files of workstation.identities. They are tried
	// first, and only those that hold a recipient of the file.
	Identities []IdentityFile
	// Discover lets sops look for keys itself as well: SOPS_AGE_KEY_FILE
	// and its other variables, ~/.config/sops/age/keys.txt, a PGP agent,
	// the credentials of a cloud key management service or Vault. That can
	// run a program or ask for a passphrase, so it is for a terminal only.
	Discover bool
	// Types are the kinds of master key trusted, DefaultSopsKeyTypes when
	// empty. A file encrypted to any other kind is refused.
	Types []string
}

// DecryptSops has sops decrypt a sops encrypted YAML file into memory and
// returns the values of the given top level mappings, by key.
//
// Before sops runs, the file is checked the way loading checks it, a kind of
// master key keys.Types does not trust is refused, and so is a value sops
// would parse as anything but a string. sops is then given exactly those
// bytes, on stdin, so the file cannot change in between.
//
// The data key is recovered with the files of keys.Identities that hold a
// recipient of the file, one run each, and then, with keys.Discover, by
// whatever sops finds itself; that run keeps the terminal, and the progress
// displays of ctx leave it meanwhile. sops verifies the integrity of the
// whole file before it writes anything. It writes the plaintext to a pipe,
// as JSON, so that no value is typed again, and nothing reaches the disk.
//
// No error says anything of the values: what sops prints is passed on only
// when it could not open the data key, and then it lists the keys it tried.
func DecryptSops(ctx context.Context, s *Sops, data []byte, keys SopsKeys, sections []string) (map[string]map[string]string, error) {
	doc, err := readSops(data)
	if err != nil {
		return nil, err
	}
	if err := doc.info.CheckKeyTypes(keys.Types); err != nil {
		return nil, err
	}
	if err := checkValueTypes(doc.values); err != nil {
		return nil, err
	}

	var candidates []IdentityFile
	for _, id := range keys.Identities {
		if slices.ContainsFunc(doc.info.Keys, func(k SopsKey) bool { return k.Type == ageKeyType && id.Opens(k.ID) }) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 && !keys.Discover {
		if len(keys.Identities) == 0 {
			return nil, errors.New("no key is available without a terminal: set workstation.identities; " +
				"sops looks for keys itself only at a terminal, where it may ask for a passphrase")
		}
		return nil, fmt.Errorf("no key available here opens it: it is encrypted to %s, and none of workstation.identities is one of them; "+
			"without a terminal only workstation.identities are tried", recipients(doc.info))
	}

	found, err := s.Find(ctx)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "clusterctl-sops-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	var tried []string
	for _, id := range candidates {
		env := isolated(dir)
		if id.SSH {
			env = append(env, "SOPS_AGE_SSH_PRIVATE_KEY_FILE="+id.Path)
		} else {
			env = append(env, "SOPS_AGE_KEY_FILE="+id.Path)
		}
		out, said, err := decrypt(ctx, command(ctx, found.Path, dir, env, false, decryptArgs...), data)
		if err == nil {
			return sectionValues(out, sections)
		}
		if !errors.Is(err, errNoKey) {
			return nil, err
		}
		tried = append(tried, fmt.Sprintf("with %s from workstation.identities, sops says: %s", id.Path, said))
	}
	if keys.Discover {
		cwd, _ := os.Getwd()
		// This sops keeps the terminal, where it may ask for a passphrase,
		// so the progress displays leave it while sops runs.
		resume := progress.Suspend(ctx)
		out, said, err := decrypt(ctx, command(ctx, found.Path, cwd, discovering(), true, decryptArgs...), data)
		resume()
		if err == nil {
			return sectionValues(out, sections)
		}
		if !errors.Is(err, errNoKey) {
			return nil, err
		}
		tried = append(tried, "with the keys sops found itself, sops says: "+said)
	}
	msg := "no key available here opens it: " + strings.Join(tried, "; ")
	if !keys.Discover {
		msg += "; without a terminal only workstation.identities are tried"
	}
	return nil, errors.New(msg)
}

// recipients names the age recipients of a file, for an error.
func recipients(info SopsInfo) string {
	var out []string
	for _, k := range info.Keys {
		if k.Type == ageKeyType {
			out = append(out, k.ID)
		}
	}
	if len(out) == 0 {
		return "no age key"
	}
	return strings.Join(out, ", ")
}

// decryptArgs have sops read the file from stdin and write the plaintext to
// stdout, as JSON. --config names an empty configuration, so that no
// .sops.yaml above the working directory changes how the file is read, and
// the decryption order is the one the sops command uses by default, age and
// PGP keys before any service.
var decryptArgs = []string{
	"--config", os.DevNull,
	"decrypt",
	"--input-type", "yaml",
	"--output-type", "json",
	"--decryption-order", "age,pgp",
}

// errNoKey is a run of sops that did not open the data key.
var errNoKey = errors.New("sops could not open the data key")

// decrypt runs sops on the file and returns what it wrote. When it could not
// open the data key, the error is errNoKey and said is what sops printed,
// made safe to show: nothing was decrypted yet, and it lists the keys it
// tried. Any other failure says nothing of what sops printed.
func decrypt(ctx context.Context, cmd *exec.Cmd, data []byte) (out []byte, said string, err error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = &capped{w: &stdout, n: maxStdout}
	cmd.Stderr = &capped{w: &stderr, n: maxStderr}
	err = cmd.Run()
	if err == nil {
		return stdout.Bytes(), "", nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, "", fmt.Errorf("decrypting was stopped: %w", ctxErr)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return nil, "", fmt.Errorf("running sops: %w", err)
	}
	switch code := exitErr.ExitCode(); code {
	case sopsNoKeyRetrieve:
		return nil, oneLine(stderr.String()), errNoKey
	case sopsMACMismatch, sopsMACNotFound:
		return nil, "", errChanged
	default:
		return nil, "", fmt.Errorf("%w (sops exited %d; what it printed is not shown, since it can quote a decrypted value)", errUnreadable, code)
	}
}

// oneLine folds what sops printed into one line of untrusted text.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, " | ", " ")
	return output.EscapeCell(strings.Join(strings.Fields(s), " "))
}

// command prepares sops to run until ctx ends, in dir, with exactly the
// environment env. It gets pipes of its own for its standard streams, never
// clusterctl's, which under mcp serve carry the protocol.
//
// Without terminal, sops is started in a session of its own, with no
// controlling terminal: nothing it starts can open /dev/tty to ask for a
// passphrase.
func command(ctx context.Context, path, dir string, env []string, terminal bool, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = killGrace
	if !terminal {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	return cmd
}

// passedOn are the variables of clusterctl's environment an isolated sops
// gets. None of them names a key or a service.
var passedOn = []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TZ"}

// isolated is the environment of a sops that must find no key but the one
// it is given: the variables of passedOn, and a home and configuration
// directory that are empty, so that neither ~/.config/sops/age/keys.txt nor
// ~/.ssh/id_ed25519 nor a gpg keyring is found. SOPS_AGE_KEY_CMD and the
// credentials of every key service stay behind.
func isolated(empty string) []string {
	env := []string{"HOME=" + empty, "XDG_CONFIG_HOME=" + empty, "GNUPGHOME=" + empty}
	for _, name := range passedOn {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// withheld are the variables sops reads that would change how clusterctl
// has it read a file: a remote key service to send the data key to, the
// local one turned off, another configuration, another decryption order.
var withheld = []string{"SOPS_KEYSERVICE", "SOPS_ENABLE_LOCAL_KEYSERVICE", "SOPS_CONFIG", "SOPS_DECRYPTION_ORDER"}

// discovering is the environment of a sops that looks for keys itself, at a
// terminal: clusterctl's, as sops would have it at the same prompt, without
// the variables of withheld.
func discovering() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(withheld, name) {
			env = append(env, kv)
		}
	}
	return env
}

// capped is a writer that keeps at most n bytes and fails after that, which
// ends the command.
type capped struct {
	w io.Writer
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if len(p) > c.n {
		return 0, errors.New("sops wrote more than a Secret document can hold")
	}
	c.n -= len(p)
	return c.w.Write(p)
}

// sectionValues reads the strings of the named top level mappings out of
// the JSON sops wrote. An error names a key, never a value, and never
// passes on what the JSON decoder said, which can quote the input.
func sectionValues(out []byte, sections []string) (map[string]map[string]string, error) {
	errNotJSON := errors.New("sops wrote something that is not the JSON of one document")
	dec := json.NewDecoder(bytes.NewReader(out))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errNotJSON
	}
	result := map[string]map[string]string{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errNotJSON
		}
		section, _ := tok.(string)
		if !slices.Contains(sections, section) {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, errNotJSON
			}
			continue
		}
		if _, dup := result[section]; dup {
			return nil, fmt.Errorf("%s is given twice", section)
		}
		values, err := sectionOf(dec, section)
		if err != nil {
			return nil, err
		}
		result[section] = values
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errNotJSON
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errNotJSON
	}
	return result, nil
}

// sectionOf reads one mapping of keys to strings.
func sectionOf(dec *json.Decoder, section string) (map[string]string, error) {
	values := map[string]string{}
	tok, err := dec.Token()
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s: sops wrote something that is not JSON", section)
	case tok == nil:
		return values, nil
	case tok != json.Delim('{'):
		return nil, fmt.Errorf("%s: must be a mapping of keys to values", section)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%s: sops wrote something that is not JSON", section)
		}
		key, _ := tok.(string)
		if _, dup := values[key]; dup {
			return nil, fmt.Errorf("%s.%s: the key is given twice", section, key)
		}
		tok, err = dec.Token()
		value, ok := tok.(string)
		if err != nil || !ok {
			return nil, fmt.Errorf("%s.%s: a value must be a string", section, key)
		}
		values[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("%s: sops wrote something that is not JSON", section)
	}
	return values, nil
}
