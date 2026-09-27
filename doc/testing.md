<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Testing

`make test` runs everything. `make cover` reports coverage, `make race` runs
under the race detector, and `make lint` vets and checks formatting.

## What is tested where

**Unit tests** cover the packages that hold the logic: node set parsing and
folding, configuration merging and validation, naming, the inventory, quoting,
output formatting, the DHCP parser, the host key store and the safety gate.
The node set engine has a document of its own,
[nodeset-testing.md](nodeset-testing.md): its fuzz target, the ClusterShell
corpus and the tests of size.

**A real shell** checks the quoting. `shellquote` is the one place where being
subtly wrong is invisible until it eats a production command, so every vector
in the test is quoted, run through `sh -c 'printf %s\n ...'`, and compared with
what went in.

**Fuzz targets** check what must hold for any input. The node set parser's is
described with the engine. Another checks that the DHCP parser never panics on
any input. CI runs it for a minute on every change, and
`go test ./internal/dhcp/ -fuzz FuzzParse` runs it locally. A third checks
`progress.Sanitize`, which every remote line and error passes through before a
display may draw it: whatever the input, what comes out holds nothing a
terminal would act on and keeps to its bound. CI runs it for a minute too, and
`go test ./internal/progress/ -fuzz FuzzSanitize` runs it locally. Two more
check `termtext`, the escaper under it: `FuzzEscape`, that neither escaper
leaves a rune its policy names and that escaping twice changes nothing, and
`FuzzTruncate`, that a cut row is a prefix that fits its columns. CI runs each
for half a minute.

**A fake BMC** serves the Redfish surface clusterctl uses, over TLS, from
`httptest`. It is how the reset-type check, the once-only action and the boot
override default are covered without hardware.

**The real sops** encrypts and decrypts the Secret documents in the tests.
clusterctl runs `sops` to read a Secret, so a test of that is only worth
something against the program itself: `sopstest` encrypts each fixture with
`sops encrypt` to an age or OpenSSH key the test has just generated, so no
private key is kept in the repository. A tampered file, a changed type tag, a
foreign key and an identity that only claims to be a recipient are all put to
the real binary. A fake `sops`, a shell script, covers what the real one cannot
be made to do on demand: an exit status sops uses after it has opened the data
key, and a refusal that must happen before sops runs at all, which the fake
records if it is started. The tests need `sops` in `PATH`; `mise install`
installs the version `mise.toml` pins, and a test fails, rather than skips,
without it. CI runs the whole suite a second time with the oldest sops
supported.

**A recording transport** stands in for ssh. `transport.Recorder` records what
would have been sent and replies with prepared output, so the subsystems and
the whole command tree can be driven without a cluster. A reply is handed the
payload a command streamed on standard input, so a test can run the script a
command sends in a real shell, in a temporary directory, and look at what it
left behind: that is how `cinc config` is shown to write a file that is safe
to source and never truncated. Its answer is handed to the request's `OnLine`
a line at a time, as a real run hands on what ssh prints, so a parser of the
lines can be driven with prepared output. A fan-out makes its calls in any order, so a
test that prepares answers for several nodes keys each to its node with
`ByTarget` rather than listing them in `Responses`, compares the recorded
calls in node set order with `Sorted`, and writes a `Reply` that is safe to
call from several goroutines at once. The command tests use
the example configuration that ships with the documentation, which keeps the
example honest.

**Counting what runs at once.** `fanout/fanouttest` counts the calls a fake
is in the middle of: the requests a runner is asked to run, those a Redfish
round tripper sends and the connections a host key scan dials. A fake that
answers at once rarely has two calls under way together, so each call is held
until one more than the limit are, which a fan-out that keeps to its limit
never allows: the test sees exactly the limit in flight, and one call too many
when the limit is broken. `fanout.Map`, the executor, the Redfish fan-out and
the host key scans are tested this way.

**Progress events.** `progress/progresstest` holds a command to what it
reports. `Capture` is a sink that keeps the events of a Bus, and `Check` tests
them against every promise the progress package makes to a display: each span
starts and ends once, under a parent that is still open; the targets of a step
are announced, queued, before the first of them runs, and add up to its total
however the step ended, an interrupt included; no more run at once than its
limit; every suspension of the display is resumed; and no text holds anything
a terminal would act on. `Checked` gives a test a Bus with a capture whose
events are checked once the test is over, and `Watch` one whose events are
checked, and drawn as a tree, when the test asks; both check the events before
the Bus is closed, which would end a span left open and hide it. The command
test harness gives every command it runs a Bus from `Checked`, so each
command test is also a test of what the command reports. `Tree` draws the
spans as an indented tree that does not depend on how concurrent work was
scheduled: the targets that read the same are folded into one line naming
them as a node set, and siblings are sorted. The tests of a reinstall, the
power batches, secrets push, provision status, `dns lookup`, `doctor --remote`,
`fabric state`, the host key scans and a dry run that looks up a group compare
trees; a test that does takes its own Bus with `Watch`. Both take
`progresstest.Classify(exitcode.Class)`, so that the events carry the classes
a command's Bus gives them. One test runs a set of
commands with a Bus and without one and compares their standard output,
standard error and exit status byte for byte, since progress must never reach
either stream.

**The displays** are tested on a terminal that is a buffer and a clock the
test moves: `display.Tree.Draw` and `display.Counter.Draw` draw one frame,
`display.Plain.Draw` writes the lines held and the heartbeats due, and the
command tests replace `startDisplay` with `fakeDisplays`, whose displays are
drawn only when the test says, from a fake transport's answer or a pause
between batches, on a clock the displays' Bus reads too. So a frame or a plain
line shows the same counts and times on every run, and no test waits for the
display's second, a heartbeat's ten or their ticks. `progresstest.Screen` is
the terminal the tree is drawn on: it applies the carriage returns, the rows
moved up to and erased and the rest of the screen cleared the way a terminal
does, keeps what scrolled off, and wraps a row at its width, so a test
compares what a person would see, frame by frame, and a row drawn too wide
shows as the two it would be. The frames of a wide fan-out, failures grouped,
a hidden lookup that turns slow, a step that fails at once, a power-on in
batches, two steps side by side, a terminal too small for the tree, the ASCII
marks, an interrupt, a question and a write in the middle of a frame are
compared whole, and so are the plain lines of a fan-out, a power-on in
batches and a reinstall that fails, the summary among them.
The Terminal's `Lines` is tested around a question, an open line, a frame and
the display's end, and against its bound; a stress test has four goroutines
write 200 lines each through it, half of them in two writes, while the command
writes its own lines, asks questions and the counter draws, and checks on the
`Screen` that every line arrived whole, on a row of its own, in its writer's
order, and none inside a question. A command test has one node's worker panic
while another's asks for a password, and checks that the stack comes after
the answer.

**The event log** is compared line for line, with the span ids, which each
Bus draws at random, replaced by their order, and a clock the test moves: the
lines of hand-made spans in `progress`, and those of `exec` under a
`TRACEPARENT` in `cli`. A test sets every part of an event and fails when one
is neither logged nor left out on purpose, so a part added to `Event` or
`Fields` is not logged, or kept out, without a decision.

**Command tests** drive the real command tree end to end and assert on what
would be sent, not on whether the code compiles: that a glob and an apostrophe
survive the trip, that a declined confirmation sends nothing, that a dry run
sends nothing, that a protected host is refused, and that exit codes are what
the contract says.

## What is deliberately not tested

Anything that needs real hardware or a real cluster: IPMI against a service
processor, fabric diagnostics, a Slurm controller, a PXE boot. Those paths are
covered up to the point where a command is handed to the transport — the
command that would be sent is asserted, its effect is not.

That boundary is honest, not convenient. The alternative is mocking a fabric
diagnostic tool's output and testing the mock.

If this is ever taken further, the review that preceded the rewrite names the
backends worth standing up: the DMTF Redfish mockup server, sushy-tools,
OpenIPMI's `ipmi_sim`, a containerised Slurm, and ClusterShell itself as the
reference for node set output.

## Coverage

Coverage is reported per package and is not a target in itself. The packages
that hold the logic sit between 70 and 96 per cent, and the node set engine
is covered completely; the command tree is lower because much of it is the
last step before a remote host.

Two rules keep the number meaningful:

- Every package has at least one test file, which is also what keeps
  `go test -cover ./...` working across the module.
- A test asserts on behaviour that would be wrong if it changed, not on the
  shape of an implementation.

## Writing a test here

Table-driven, with the case name saying what property is being checked rather
than which function is being called. A test that needs a reason has a comment
giving it: several tests exist because a specific thing went wrong in the shell
toolkit, and that is worth recording next to the assertion.
