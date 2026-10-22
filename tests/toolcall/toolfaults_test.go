//go:build model

// The Q3 faults that cannot be reached without actually calling a tool.
//
// The keyless tier covers everything up to and including tool discovery, but it
// never issues tools/call, so a tool that hangs, answers with a JSON-RPC error,
// reports its own failure, or returns a malformed result is invisible to it.
// Those faults live here, as one table, in the same shape as the keyless
// catalogue.
package toolcall

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"mcpclienttests/internal/claudecli"
	"mcpclienttests/internal/harness"
)

type toolFault struct {
	id   string
	mode string

	// wantCall: the client must actually have issued the call. If it did not,
	// the rest of the row proves nothing — a client that never calls the tool
	// trivially survives every tool fault.
	wantCall bool
	// wantSurfaced: the failure must reach the model as an error rather than
	// being swallowed or presented as success.
	wantSurfaced bool
	// characterize: record what happens without demanding a particular
	// outcome, for cases the spec leaves open.
	characterize bool

	maxDur time.Duration
	why    string
}

func toolFaults() []toolFault {
	return []toolFault{
		// A tool that reports its own failure, versus a call that fails at the
		// protocol level. These are the pair most often conflated, and they are
		// different code paths in any client.
		{id: "ERR-13", mode: "tool_is_error", wantCall: true, wantSurfaced: true,
			why: "result with isError: true — the tool ran and reported failure"},
		{id: "ERR-14", mode: "rpc_invalid_args", wantCall: true, wantSurfaced: true,
			why: "JSON-RPC -32602: the call itself was rejected"},
		{id: "ERR-15", mode: "rpc_no_method", wantCall: true, wantSurfaced: true,
			why: "JSON-RPC -32601: tools/call unsupported"},
		{id: "ERR-16", mode: "rpc_internal", wantCall: true, wantSurfaced: true,
			why: "JSON-RPC -32603: server failed internally"},

		// The server dies or stops answering mid-call.
		{id: "ERR-12", mode: "crash_on_call", wantCall: true, wantSurfaced: true,
			why: "server exits during tools/call"},
		{id: "ERR-17", mode: "hang_tool", wantCall: true, wantSurfaced: true,
			maxDur: 240 * time.Second,
			why:    "no answer to tools/call must time out rather than hang the session"},

		// Results the client cannot make sense of. Left as characterizations
		// because no public contract says what must happen.
		{id: "ERR-18", mode: "bad_result_shape", wantCall: true, characterize: true,
			why: "result with no content field"},
		{id: "ERR-19", mode: "non_utf8", wantCall: true, characterize: true,
			why: "result containing invalid UTF-8"},
	}
}

// TestToolFaultCatalogue drives each tool-level fault through a real session.
//
// The invariant every row shares is that the client finishes: it issues the
// call, meets the fault, and still produces a terminal result event. A client
// that hangs or dies here would take the user's whole session with it, which
// matters more than the specific wording of the error.
func TestToolFaultCatalogue(t *testing.T) {
	for _, c := range toolFaults() {
		c := c
		t.Run(c.id+"/"+c.mode, func(t *testing.T) {
			e := harness.New(t)
			e.AddStdio(srv, c.mode)

			budget := c.maxDur
			if budget == 0 {
				budget = 150 * time.Second
			}
			tool := harness.MCPToolName(srv, "echo_nonce")

			r, run := e.Prompt(fmt.Sprintf(
				"Call the %s tool with text \"fault-probe\". Report what happened in one short line.", tool),
				harness.PromptOpts{AllowTools: []string{tool}, Timeout: budget})

			if r.TimedOut {
				t.Fatalf("%s (%s): the session never returned within %s. %s",
					c.id, c.mode, budget, c.why)
			}

			// The client must have tried, or the row is vacuous.
			calls := e.Frames().ToolCalls()
			if c.wantCall && len(calls) == 0 {
				t.Fatalf("%s: the client never issued tools/call, so this row proves nothing "+
					"about the fault. Tools used = %v", c.id, run.ToolNames())
			}

			// The central invariant: the session survived the fault.
			if !run.TerminatedNormally() {
				t.Errorf("%s: no terminal result event — the client did not finish after %s\nstderr: %s",
					c.id, c.why, truncate(r.Stderr, 400))
			}

			surfaced, how := errorSurfaced(run)
			switch {
			case c.wantSurfaced && !surfaced:
				t.Errorf("%s: the failure was not surfaced as an error. A tool failure presented "+
					"as success is worse than a loud one, because the model will build on it.\n"+
					"  final text: %s", c.id, truncate(run.FinalText(), 300))
			case c.characterize:
				t.Logf("%s [%s] %s -> surfaced=%v (%s) in %s", c.id, c.mode, c.why, surfaced, how, r.Dur.Round(time.Millisecond))
			}

			t.Logf("%s [%s] calls=%d duration=%s surfaced=%v (%s)",
				c.id, c.mode, len(calls), r.Dur.Round(time.Millisecond), surfaced, how)
		})
	}
}

// TestERR27_OneBrokenServerDoesNotBreakTheOthers is the single most important
// error test in the suite. A user with five servers configured, one of them
// broken, must still be able to work. If a client fails this, nothing else
// about its error handling matters much.
func TestERR27_OneBrokenServerDoesNotBreakTheOthers(t *testing.T) {
	e := harness.New(t)

	// One server that dies before it can be used, one that works.
	broken := append([]string{"mcp", "add", "broken", "--"},
		e.FixtureArgsT(e.TranscriptFor("broken"), "exit_before_init")...)
	if r := e.Claude(broken...); r.Code != 0 {
		t.Fatalf("mcp add broken exited %d: %s", r.Code, r.Stderr)
	}
	healthy := append([]string{"mcp", "add", "healthy", "--"},
		e.FixtureArgsT(e.TranscriptFor("healthy"), "ok", "-name", "healthy")...)
	if r := e.Claude(healthy...); r.Code != 0 {
		t.Fatalf("mcp add healthy exited %d: %s", r.Code, r.Stderr)
	}

	tool := harness.MCPToolName("healthy", "echo_nonce")
	r, run := e.Prompt(fmt.Sprintf(
		"Call the %s tool with text \"isolation\". Then reply with its output verbatim.", tool),
		harness.PromptOpts{AllowTools: []string{tool}, Timeout: 150 * time.Second})

	if r.TimedOut {
		t.Fatalf("ERR-27: session timed out after %s with one broken server configured", r.Dur)
	}
	t.Logf("ERR-27: server statuses reported at init: %v", run.MCPServerStatuses())

	// The healthy server must have been usable.
	calls := e.FramesFor("healthy").ToolCalls()
	if len(calls) == 0 {
		t.Fatalf("ERR-27: the healthy server received no call; a broken sibling disabled it. "+
			"Tools used = %v", run.ToolNames())
	}
	if !strings.Contains(run.AllText(), e.Nonce) {
		t.Errorf("ERR-27: the healthy tool's result never reached the model, so the broken " +
			"server degraded the working one")
	}
	// And the broken one must really have been broken, or the test is hollow.
	if fs := e.FramesFor("broken"); fs.Has("tools/list") {
		t.Errorf("ERR-27: the 'broken' server completed tool discovery; the fault did not apply")
	}
}

// errorSurfaced reports whether a failure reached the model as an error, and by
// which route: a tool_result flagged as an error, the run's own error flag, or
// not at all.
func errorSurfaced(run *claudecli.Run) (bool, string) {
	for _, res := range run.ToolResults {
		if res.IsError {
			return true, "tool_result is_error"
		}
	}
	if run.IsError() {
		return true, "result event is_error"
	}
	// Last resort: the client may describe the failure in the result text
	// without flagging it. Weaker evidence, so it is reported as such.
	low := strings.ToLower(run.FinalText())
	for _, kw := range []string{"error", "failed", "failure", "timed out", "timeout", "unable"} {
		if strings.Contains(low, kw) {
			return true, "result text mentions failure (" + kw + ")"
		}
	}
	return false, "not surfaced"
}
