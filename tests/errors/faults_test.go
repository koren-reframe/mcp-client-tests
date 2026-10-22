// Package errors holds the Q3 tier: what the client does when the server, the
// connection or a tool call fails.
//
// Every case here is T0 — no model, no API key — because the keyless health
// check performs a real connection and handshake. A fault is one row in the
// table below, which is the whole point of giving the fixture a -mode flag:
// adding the next fault costs three lines, not a new test.
//
// Faults that can only be reached by actually calling a tool (a tool that
// hangs, a tool answering with a JSON-RPC error, an oversized result) are not
// here: the health check never issues tools/call. Those live in the model tier.
package errors

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"mcpclienttests/internal/harness"
	"mcpclienttests/internal/health"
)

type fault struct {
	id   string       // PLAN.md row
	mode string       // fixture -mode
	want health.State // expected health-check verdict

	causeHas    string        // substring expected in the reported cause
	mustSend    []string      // methods the client must have sent
	mustNotSend []string      // methods it must not have sent
	maxDur      time.Duration // upper bound, asserted loosely (DET-14)
	why         string        // what this row is really checking
}

func faults() []fault {
	return []fault{
		// --- the server dies ------------------------------------------------
		{id: "ERR-02", mode: "exit_before_init", want: health.Failed, causeHas: "closed",
			maxDur: 30 * time.Second, why: "server exits non-zero before the handshake"},
		{id: "ERR-03", mode: "exit_zero", want: health.Failed, causeHas: "closed",
			maxDur: 30 * time.Second, why: "a clean exit is still a failed connection"},
		{id: "ERR-11", mode: "crash_after_init", want: health.Partial, causeHas: "closed",
			mustSend: []string{"initialize"}, mustNotSend: []string{"tools/list"},
			maxDur: 30 * time.Second, why: "handshake succeeds, tool discovery does not"},

		// --- the server stops answering -------------------------------------
		// The bound is generous on purpose: the measured connect timeout is
		// about 27s, and asserting a tighter number would fail on a slow runner
		// without telling us anything about the client.
		{id: "ERR-04", mode: "hang_initialize", want: health.Failed, causeHas: "timed out",
			mustSend: []string{"initialize"}, maxDur: 45 * time.Second,
			why: "no answer to initialize must time out, not hang forever"},

		// --- the server talks nonsense --------------------------------------
		// All of these are expected to still connect. The client is robust here,
		// so the assertion is continued success; inventing a failure these rows
		// do not produce would be a test that lies.
		{id: "ERR-05", mode: "stdout_noise", want: health.Connected,
			mustSend: []string{"initialize", "tools/list"}, why: "plain log lines on stdout are tolerated"},
		{id: "ERR-06", mode: "stdout_json_not_rpc", want: health.Connected,
			mustSend: []string{"initialize", "tools/list"}, why: "valid JSON that is not JSON-RPC is tolerated"},
		{id: "ERR-07", mode: "truncated_frame", want: health.Connected,
			mustSend: []string{"tools/list"}, why: "a torn frame mid-session does not poison the session"},
		{id: "ERR-09", mode: "unknown_notification", want: health.Connected,
			mustSend: []string{"tools/list"}, why: "an unknown notification is ignored, not fatal"},
		{id: "ERR-10", mode: "extra_init_fields", want: health.Connected,
			mustSend: []string{"tools/list"}, why: "unknown fields in initialize are ignored (forward compatibility)"},
		{id: "ERR-08", mode: "unknown_id", want: health.Connected,
			mustSend: []string{"tools/list"}, why: "a response to an id nobody asked for is discarded"},

		// --- negotiation ----------------------------------------------------
		{id: "CONN-14", mode: "unsupported_proto", want: health.Failed, causeHas: "protocol version",
			mustNotSend: []string{"tools/list"}, maxDur: 30 * time.Second,
			why: "an unsupported protocol version is refused by name"},
		{id: "CONN-13", mode: "old_protocol", want: health.Connected,
			mustSend: []string{"tools/list"}, why: "an older supported version still works"},

		// --- catalogue shape ------------------------------------------------
		{id: "TOOL-24", mode: "tool_flood", want: health.Connected,
			mustSend: []string{"tools/list"}, why: "64 advertised tools are accepted"},
		{id: "CONN-15", mode: "no_capabilities", want: health.Connected,
			mustNotSend: []string{"resources/list", "prompts/list"},
			why:         "a server advertising nothing is not interrogated for features"},
		{id: "CONN-48", mode: "discover_ok", want: health.Connected,
			mustSend: []string{"initialize"},
			why:      "answering the undocumented discover pre-probe does not derail the handshake"},
	}
}

// TestFaultCatalogue drives every fault in the table through the keyless health
// check and checks the client's verdict, the cause it reported, which frames it
// did and did not send, and that it finished inside a bound.
func TestFaultCatalogue(t *testing.T) {
	for _, c := range faults() {
		c := c
		t.Run(c.id+"/"+c.mode, func(t *testing.T) {
			t.Parallel()
			e := harness.New(t)
			e.AddStdio("srv", c.mode)

			budget := c.maxDur
			if budget == 0 {
				budget = 45 * time.Second
			}
			r, st := e.Health(budget + 20*time.Second)

			if r.TimedOut {
				t.Fatalf("%s (%s): client never returned within %s — %s",
					c.id, c.mode, budget+20*time.Second, c.why)
			}
			if c.maxDur > 0 && r.Dur > c.maxDur {
				t.Errorf("%s: took %s, expected to resolve within %s", c.id, r.Dur.Round(time.Millisecond), c.maxDur)
			}

			got := e.RequireState(st, "srv", c.want)
			if c.causeHas != "" && !strings.Contains(strings.ToLower(got.Cause), strings.ToLower(c.causeHas)) {
				t.Errorf("%s: cause = %q, want it to mention %q\n  line: %s", c.id, got.Cause, c.causeHas, got.Raw)
			}

			fs := e.Frames()
			for _, m := range c.mustSend {
				if !fs.Has(m) {
					t.Errorf("%s: client never sent %s; methods = %v", c.id, m, fs.Methods())
				}
			}
			for _, m := range c.mustNotSend {
				if fs.Has(m) {
					t.Errorf("%s: client sent %s when it should not have; methods = %v", c.id, m, fs.Methods())
				}
			}

			t.Logf("%s [%s] %s -> %s in %s | %s", c.id, c.mode, c.why, got.State, r.Dur.Round(time.Millisecond), got.Raw)
		})
	}
}

// TestERR01_ServerBinaryMissing is separate because the fault is in the
// configuration rather than in the server's behaviour: there is no server.
func TestERR01_ServerBinaryMissing(t *testing.T) {
	t.Parallel()
	e := harness.New(t)

	if r := e.Claude("mcp", "add", "ghost", "--", "/nonexistent/mcp-server", "--flag"); r.Code != 0 {
		t.Fatalf("mcp add exited %d: %s", r.Code, r.Stderr)
	}

	r, st := e.Health(60 * time.Second)
	if r.TimedOut {
		t.Fatal("ERR-01: a missing binary should fail immediately")
	}
	got := e.RequireState(st, "ghost", health.Failed)
	if !strings.Contains(got.Cause, "ENOENT") && !strings.Contains(strings.ToLower(got.Cause), "no such file") {
		t.Errorf("ERR-01: cause should identify the missing file, got %q", got.Cause)
	}
}

// TestERR31_ExitCodeDoesNotReflectFailure pins behaviour the suite found rather
// than behaviour it wants: `claude mcp list` exits 0 even when a server fails
// outright. Anything building a CI health gate on that exit code is blind, and
// this test exists so a future change in either direction is noticed.
func TestERR31_ExitCodeDoesNotReflectFailure(t *testing.T) {
	t.Parallel()
	e := harness.New(t)
	e.AddStdio("broken", "exit_before_init")

	r, st := e.Health(60 * time.Second)
	e.RequireState(st, "broken", health.Failed)

	if r.Code != 0 {
		t.Errorf("ERR-31: mcp list now exits %d on a failed server. That is arguably an "+
			"improvement, but it contradicts the recorded baseline — re-baseline this test "+
			"and the README note that goes with it.", r.Code)
	}
	// The failure must at least be discoverable somewhere (RDY-12).
	if !strings.Contains(r.Combined(), "Failed to connect") {
		t.Error("RDY-12: failure is neither in the exit code nor in the output")
	}
}

// TestERR28_NoOrphanedServerProcesses: after the client exits, the servers it
// spawned must be gone. The per-test transcript path makes the process
// identifiable, so this does not depend on counting all fixture processes in a
// container that runs tests in parallel.
func TestERR28_NoOrphanedServerProcesses(t *testing.T) {
	t.Parallel()
	e := harness.New(t)
	e.AddStdio("clean", "ok")

	_, st := e.Health(60 * time.Second)
	e.RequireState(st, "clean", health.Connected)

	// Give the client a moment to reap on its way out.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n := countProcesses(t, e.Transcript); n == 0 {
			return
		} else if time.Now().After(deadline) {
			t.Errorf("ERR-28: %d fixture process(es) still alive 10s after the client exited "+
				"(transcript %s)", n, e.Transcript)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// countProcesses counts live processes whose command line mentions the marker.
func countProcesses(t *testing.T, marker string) int {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "args=").Output()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, marker) && strings.Contains(line, "fixture-mcp") {
			n++
		}
	}
	return n
}
