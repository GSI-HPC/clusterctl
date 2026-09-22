// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/safety"
)

// maxCommandOutput bounds what read_command returns. A command that says
// more is cut, and the agent is told to narrow it.
const maxCommandOutput = 64 << 10

// pinnedFlags are the global options the server sets itself. An agent that
// could pass them could point a command at another configuration or cluster,
// or answer a confirmation.
var pinnedFlags = []string{"config", "context", "set", "yes", "force"}

func (s *Server) addCommandTool() {
	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:  "read_command",
		Title: "Run a read-only clusterctl command",
		Description: "Run one clusterctl command that only reads, for what the other tools do not cover. " +
			"args are the words after clusterctl, for example [\"dhcp\", \"hosts\", \"-n\", \"exe[1-4]\"] or " +
			"[\"fabric\", \"state\", \"-n\", \"@rack:R02\"]. Output is JSON unless -o asks for another format; " +
			"-o jq=EXPR or -o jsonpath=EXPR filters it. Add --help to any command to read its options. " +
			"Commands that change something, prompt or keep running are refused. " +
			"The commands offered are:\n" + s.readCommandList(),
		Annotations: readOnly(),
	}, s.readCommand)
}

// commandInput is the argument of read_command.
type commandInput struct {
	Args []string `json:"args" jsonschema:"the command and its arguments, without the program name"`
}

// commandOutput is what read_command returns.
type commandOutput struct {
	Context   string `json:"context"`
	Command   string `json:"command"`
	ExitCode  int    `json:"exitCode" jsonschema:"0 success, 1 a target failed, 3 a host could not be reached"`
	Error     string `json:"error,omitempty"`
	Output    any    `json:"output,omitempty" jsonschema:"the parsed JSON output, or the text when it is not JSON"`
	Notes     string `json:"notes,omitempty" jsonschema:"what the command reported besides its output"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (s *Server) readCommand(ctx context.Context, _ *mcp.CallToolRequest, in commandInput) (*mcp.CallToolResult, *commandOutput, error) {
	cmd, err := s.resolveReadCommand(ctx, in.Args)
	if err != nil {
		return nil, nil, callError(err)
	}

	var stdout, stderr bytes.Buffer
	streams := s.streams(&stderr)
	streams.Out = &stdout
	root := s.opts.Command(ctx, streams)
	// A command prints its result through the streams it was built with, and
	// its help through the command's own; both are captured.
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(append(s.globalArgs(), in.Args...))
	runErr := root.ExecuteContext(ctx)
	if safety.IsDryRun(runErr) {
		runErr = nil
	}

	out := &commandOutput{Context: s.context, Command: cmd, ExitCode: exitcode.From(runErr)}
	if runErr != nil {
		out.Error = runErr.Error()
		if out.ExitCode == exitcode.Usage && stdout.Len() == 0 {
			return nil, nil, callError(runErr)
		}
	}
	text := stdout.Bytes()
	if len(text) > maxCommandOutput {
		text, out.Truncated = text[:maxCommandOutput], true
	}
	var parsed any
	if !out.Truncated && json.Valid(text) && json.Unmarshal(text, &parsed) == nil {
		out.Output = parsed
	} else if len(text) > 0 {
		out.Output = string(text)
	}
	notes := stderr.String()
	if len(notes) > maxCommandOutput {
		notes = notes[:maxCommandOutput]
	}
	out.Notes = strings.TrimSpace(notes)
	if out.Truncated {
		out.Notes = strings.TrimSpace(out.Notes + "\nthe output was cut; narrow the node set or filter it with -o jq=EXPR")
	}
	return nil, out, nil
}

// globalArgs are the options every command runs with: the server's
// configuration and context, and JSON output unless the agent asks for
// another format, which it may since a later -o wins.
func (s *Server) globalArgs() []string {
	var args []string
	for _, file := range s.opts.App.ConfigFiles {
		args = append(args, "--config", file)
	}
	if s.context != "" {
		args = append(args, "--context", s.context)
	}
	keys := make([]string, 0, len(s.opts.App.Set))
	for key := range s.opts.App.Set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--set", key+"="+s.opts.App.Set[key])
	}
	if s.opts.App.Fanout > 0 {
		args = append(args, "--fanout", fmt.Sprint(s.opts.App.Fanout))
	}
	return append(args, "--output", "json")
}

// resolveReadCommand finds the command the arguments name on a tree of its
// own, checks that it only reads and that no pinned option is given, and
// returns its path.
func (s *Server) resolveReadCommand(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", exitcode.Errorf(exitcode.Usage, "no command was given; the commands offered are:\n%s", s.readCommandList())
	}
	root := s.opts.Command(ctx, s.streams(io.Discard))
	cmd, rest, err := root.Find(args)
	if err != nil || cmd == root {
		return "", exitcode.Errorf(exitcode.Usage, "%q names no clusterctl command; the commands offered are:\n%s",
			strings.Join(args, " "), s.readCommandList())
	}
	path := strings.TrimPrefix(cmd.CommandPath(), root.Name()+" ")
	if cmd.HasSubCommands() {
		return "", exitcode.Errorf(exitcode.Usage, "%q is a group of commands; name one of them", path)
	}
	if effect := safety.EffectOf(cmd.Annotations); effect != safety.EffectRead {
		return "", exitcode.Errorf(exitcode.Usage,
			"%q is not a read-only command (%s); changes go through plan_change, and interactive commands are not offered",
			path, effect)
	}
	// The help flag is added when a command runs; it is added here so that
	// asking for help is not taken for an unknown option.
	cmd.InitDefaultHelpFlag()
	if err := cmd.ParseFlags(rest); err != nil {
		return "", exitcode.Wrap(exitcode.Usage, err)
	}
	for _, name := range pinnedFlags {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			return "", exitcode.Errorf(exitcode.Usage,
				"--%s is set by the server and cannot be given to a command", name)
		}
	}
	return path, nil
}

// readCommandList lists the read-only commands with their short help, one
// per line, from the command tree itself.
func (s *Server) readCommandList() string {
	root := s.opts.Command(context.Background(), s.streams(io.Discard))
	var lines []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c.HasSubCommands() || c == root {
			return
		}
		if safety.EffectOf(c.Annotations) == safety.EffectRead {
			use := strings.TrimPrefix(c.CommandPath(), root.Name()+" ")
			if _, argsPart, ok := strings.Cut(c.Use, " "); ok {
				use += " " + argsPart
			}
			lines = append(lines, fmt.Sprintf("  %s: %s", use, c.Short))
		}
	}
	walk(root)
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
