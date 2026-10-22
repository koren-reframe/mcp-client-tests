//go:build model

// Package toolcall holds the T1 tier: Q2, how a test can be sure a tool was
// called, that it was the right tool with the right arguments, and that it ran
// and returned its result.
//
// This tier spends tokens and needs ANTHROPIC_API_KEY, so it sits behind the
// `model` build tag and is invisible to every other target. It is also the only
// tier where the model sits between the test and the client, so nothing here
// asserts on prose: the only text ever matched is a nonce randomly generated
// per run, which no model could produce by guessing.
package toolcall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcpclienttests/internal/harness"
	"mcpclienttests/internal/transcript"
)

const srv = "srv"

// TestTOOL07_ThreeWitnesses is the core of Q2.
//
// Three independent witnesses must agree before the suite will say a tool ran:
//
//	A  the client's own event stream reports a tool_use, and a tool_result
//	   paired to it by id
//	B  the fixture's transcript records a tools/call with the expected name
//	   and arguments — proof the call left the client, not just that it was
//	   announced
//	C  the per-run nonce that only the fixture could supply comes back in the
//	   client's final text — proof the result re-entered the model's context
//
// Any one alone is weak. A is a claim; B could happen without the result ever
// reaching the model; C could in principle be echoed from the prompt, which is
// why the nonce is never in the prompt. Together they close the loop.
func TestTOOL07_ThreeWitnesses(t *testing.T) {
	e := harness.New(t)
	e.AddStdio(srv, "ok")

	marker := "marker-" + e.Nonce
	tool := harness.MCPToolName(srv, "echo_nonce")

	r, run := e.Prompt(fmt.Sprintf(
		"Call the %s tool with the text argument set to exactly %q. "+
			"Then reply with the tool's output verbatim and nothing else.", tool, marker),
		harness.PromptOpts{AllowTools: []string{tool}, Timeout: 120 * time.Second})

	if r.TimedOut {
		t.Fatalf("session timed out after %s", r.Dur)
	}
	if !run.TerminatedNormally() {
		t.Fatalf("no terminal result event; client did not finish cleanly\nstderr: %s", r.Stderr)
	}
	t.Logf("tools advertised to the model: %v", run.AvailableTools())

	// --- witness A: the client's own account -------------------------------
	use, ok := run.UseMatching(srv, "echo_nonce")
	if !ok {
		t.Fatalf("TOOL-05: no tool_use naming %s/echo_nonce; tools used = %v", srv, run.ToolNames())
	}
	// CONN-18 / TOOL-05: namespaced, not bare.
	if !strings.HasPrefix(use.Name, "mcp__") {
		t.Errorf("CONN-18: tool exposed as %q, expected an mcp__ namespaced name", use.Name)
	}
	if got, _ := use.Input["text"].(string); got != marker {
		t.Errorf("TOOL-04 (client side): text argument = %q, want %q", got, marker)
	}
	// TOOL-06: the result is correlated back to the call by id.
	res, ok := run.ResultFor(use.ID)
	if !ok {
		t.Errorf("TOOL-06: no tool_result paired with tool_use id %q", use.ID)
	} else if res.IsError {
		t.Errorf("TOOL-06: tool_result marked as error: %s", res.Text)
	}

	// --- witness B: what actually left the client --------------------------
	calls := e.Frames().ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("TOOL-02: fixture recorded %d tools/call, want exactly 1 (%v)", len(calls), callNames(calls))
	}
	call := calls[0]
	// TOOL-03: the server sees its own unmangled name; the namespacing is the
	// client's business and must not leak onto the wire.
	if call.Name != "echo_nonce" {
		t.Errorf("TOOL-03: server received tools/call for %q, want %q", call.Name, "echo_nonce")
	}
	if got, _ := call.Args["text"].(string); got != marker {
		t.Errorf("TOOL-04: server received text = %q, want %q", got, marker)
	}
	// TOOL-09: ids are unique within the session.
	seen := map[string]bool{}
	for _, f := range e.Frames().FromClient() {
		if f.ID == nil || f.Method == "" {
			continue
		}
		if key := string(f.ID); seen[key] {
			t.Errorf("TOOL-09: duplicate request id %s", key)
		} else {
			seen[key] = true
		}
	}

	// --- witness C: the result re-entered the model's context --------------
	// The nonce is generated per run and appears nowhere in the prompt, so it
	// can only have come back through the tool result.
	if !strings.Contains(run.AllText(), e.Nonce) {
		t.Errorf("TOOL-01/TOOL-10: nonce %q absent from the client's output, so the tool result "+
			"never reached the model\nfinal text: %s", e.Nonce, truncate(run.FinalText(), 400))
	}
}

// TestTOOL08_SideEffectWitness proves the call executed rather than being
// described: the tool writes a file, and the file is checked on disk.
func TestTOOL08_SideEffectWitness(t *testing.T) {
	e := harness.New(t)
	e.AddStdio(srv, "ok")

	content := "written-by-" + e.Nonce
	tool := harness.MCPToolName(srv, "write_probe")

	r, run := e.Prompt(fmt.Sprintf(
		"Call the %s tool with filename %q and content %q. Then say DONE.",
		tool, "probe.txt", content),
		harness.PromptOpts{AllowTools: []string{tool}, Timeout: 120 * time.Second})

	if r.TimedOut {
		t.Fatalf("session timed out after %s", r.Dur)
	}
	if _, ok := run.UseMatching(srv, "write_probe"); !ok {
		t.Fatalf("TOOL-08: tool was never used; tools used = %v", run.ToolNames())
	}

	path := filepath.Join(e.ProbeDir, "probe.txt")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("TOOL-08: side effect missing: %v", err)
	}
	if string(got) != content {
		t.Errorf("TOOL-08: file content = %q, want %q", got, content)
	}
}

// TestTOOL11_ArgumentFidelity checks that JSON types survive the trip through
// the client, not merely that strings do.
func TestTOOL11_ArgumentFidelity(t *testing.T) {
	e := harness.New(t)
	e.AddStdio(srv, "ok")

	tool := harness.MCPToolName(srv, "typed_args")
	r, run := e.Prompt(fmt.Sprintf(
		"Call the %s tool with exactly these arguments: s=\"hello\", i=42, b=true, "+
			"e=\"beta\", arr=[1,2,3], obj={\"inner\":\"nested\"}. "+
			"Do not pass the optional argument. Then say DONE.", tool),
		harness.PromptOpts{AllowTools: []string{tool}, Timeout: 120 * time.Second})

	if r.TimedOut {
		t.Fatalf("session timed out after %s", r.Dur)
	}
	calls := e.Frames().ToolCalls()
	if len(calls) == 0 {
		t.Fatalf("TOOL-11: no tools/call recorded; tools used = %v", run.ToolNames())
	}
	args := calls[0].Args
	t.Logf("TOOL-11: arguments as received by the server: %#v", args)

	// Types are the contract. Values the model was told to send are checked for
	// the unambiguous ones; the rest are type-checked only, because scolding the
	// model for paraphrasing a string is not a client bug (DET-09).
	assertType(t, args, "s", "string")
	assertType(t, args, "i", "number")
	assertType(t, args, "b", "bool")
	assertType(t, args, "e", "string")
	assertType(t, args, "arr", "array")
	assertType(t, args, "obj", "object")

	if v, ok := args["i"].(float64); ok && v != 42 {
		t.Errorf("TOOL-11: i = %v, want 42", v)
	}
	if v, ok := args["b"].(bool); ok && !v {
		t.Errorf("TOOL-11: b = false, want true")
	}
	if v, ok := args["e"].(string); ok && v != "beta" {
		t.Errorf("TOOL-11: e = %q, want \"beta\" (an enum value outside the schema would be worse)", v)
	}

	// TOOL-12: an omitted optional must be absent, not present-and-null.
	if v, present := args["optional"]; present && v == nil {
		t.Errorf("TOOL-12: omitted optional argument was sent as null rather than omitted")
	}
}

// TestTOOL15_RoutingBetweenServers: with two servers configured, each call must
// reach the right one. Separate transcripts make mis-routing visible.
func TestTOOL15_RoutingBetweenServers(t *testing.T) {
	e := harness.New(t)

	for _, name := range []string{"alpha", "beta"} {
		args := append([]string{"mcp", "add", name, "--"},
			e.FixtureArgsT(e.TranscriptFor(name), "ok", "-name", name)...)
		if r := e.Claude(args...); r.Code != 0 {
			t.Fatalf("mcp add %s exited %d: %s", name, r.Code, r.Stderr)
		}
	}

	alphaTool := harness.MCPToolName("alpha", "echo_nonce")
	betaTool := harness.MCPToolName("beta", "echo_nonce")

	r, run := e.Prompt(fmt.Sprintf(
		"Do exactly two things. First call %s with text \"from-alpha\". "+
			"Then call %s with text \"from-beta\". Then say DONE.", alphaTool, betaTool),
		harness.PromptOpts{AllowTools: []string{alphaTool, betaTool}, Timeout: 180 * time.Second})

	if r.TimedOut {
		t.Fatalf("session timed out after %s", r.Dur)
	}
	t.Logf("TOOL-15: tools used = %v", run.ToolNames())

	for name, wantText := range map[string]string{"alpha": "from-alpha", "beta": "from-beta"} {
		calls := e.FramesFor(name).ToolCalls()
		if len(calls) == 0 {
			t.Errorf("TOOL-15: server %q received no call at all", name)
			continue
		}
		if len(calls) > 1 {
			t.Errorf("TOOL-15: server %q received %d calls, want 1", name, len(calls))
		}
		if got, _ := calls[0].Args["text"].(string); got != wantText {
			t.Errorf("TOOL-15: server %q got text %q, want %q (arguments may have been routed to the wrong server)",
				name, got, wantText)
		}
	}
}

// TestTOOL17_AllowListIsEnforced: a tool the session disallows must never reach
// the server. The empty transcript is the proof, and it also answers RDY-02 —
// a server cannot talk its way past the permission layer, because the decision
// is made before anything is sent.
func TestTOOL17_AllowListIsEnforced(t *testing.T) {
	e := harness.New(t)
	e.AddStdio(srv, "ok")

	tool := harness.MCPToolName(srv, "echo_nonce")
	r, run := e.Prompt(fmt.Sprintf(
		"Call the %s tool with text \"should-not-happen\". If you cannot, say BLOCKED.", tool),
		harness.PromptOpts{
			Timeout: 120 * time.Second,
			Extra:   []string{"--disallowedTools", tool},
		})

	if r.TimedOut {
		t.Fatalf("session timed out after %s", r.Dur)
	}
	if calls := e.Frames().ToolCalls(); len(calls) != 0 {
		t.Errorf("TOOL-17/RDY-02: %d tools/call reached the server despite being disallowed: %v",
			len(calls), callNames(calls))
	}
	t.Logf("TOOL-17: tools used = %v, final text = %s", run.ToolNames(), truncate(run.FinalText(), 200))
}

// TestTOOL21_UnicodeRoundTrip: non-ASCII must survive in both directions.
func TestTOOL21_UnicodeRoundTrip(t *testing.T) {
	e := harness.New(t)
	e.AddStdio(srv, "ok")

	payload := "café 日本語 🚀 Ωmega"
	tool := harness.MCPToolName(srv, "unicode_payload")

	r, run := e.Prompt(fmt.Sprintf(
		"Call the %s tool with text set to exactly %q, then reply with its output verbatim.", tool, payload),
		harness.PromptOpts{AllowTools: []string{tool}, Timeout: 120 * time.Second})

	if r.TimedOut {
		t.Fatalf("session timed out after %s", r.Dur)
	}
	calls := e.Frames().ToolCalls()
	if len(calls) == 0 {
		t.Fatalf("TOOL-21: no call recorded; tools used = %v", run.ToolNames())
	}
	if got, _ := calls[0].Args["text"].(string); got != payload {
		t.Errorf("TOOL-21: server received %q, want %q (mangled on the way out)", got, payload)
	}
	// The fixture always appends its own unicode block, so the return path is
	// checked independently of what the model chose to send.
	for _, want := range []string{"café", "日本語", "🚀"} {
		if !strings.Contains(run.AllText(), want) {
			t.Errorf("TOOL-21: %q missing from the client's output (mangled on the way back)", want)
		}
	}
}

// TestTOOL22_OversizedResult: a tool returning about a megabyte must not break
// the session. What the client does with the size is characterized rather than
// demanded, since no public contract fixes it.
func TestTOOL22_OversizedResult(t *testing.T) {
	e := harness.New(t)
	e.AddStdio(srv, "huge_output")

	tool := harness.MCPToolName(srv, "echo_nonce")
	r, run := e.Prompt(fmt.Sprintf("Call the %s tool with text \"big\". Then say DONE.", tool),
		harness.PromptOpts{AllowTools: []string{tool}, Timeout: 180 * time.Second})

	if r.TimedOut {
		t.Fatalf("TOOL-22: session timed out after %s on a 1 MiB tool result", r.Dur)
	}
	if calls := e.Frames().ToolCalls(); len(calls) == 0 {
		t.Fatalf("TOOL-22: no call recorded; tools used = %v", run.ToolNames())
	}
	if !run.TerminatedNormally() {
		t.Errorf("TOOL-22: client did not finish cleanly on an oversized result\nstderr: %s", truncate(r.Stderr, 500))
	}
	// The byte counts below come from this package's own flattening of the
	// content blocks, so the raw block shapes are logged next to them: a short
	// count may mean the client truncated, or merely that a block type is not
	// text and this parser rendered it as nothing. Characterizing truncation
	// off an unexplained number would be guesswork.
	for i, res := range run.ToolResults {
		t.Logf("TOOL-22: tool_result[%d] flattened to %d bytes (fixture sent 1048576), isError=%v",
			i, len(res.Text), res.IsError)
	}
	for _, ev := range run.Events {
		if ev["type"] != "user" {
			continue
		}
		if b, err := json.Marshal(ev); err == nil {
			t.Logf("TOOL-22: raw tool_result envelope (first 600 bytes): %s", truncate(string(b), 600))
		}
	}
}

// --- helpers ---------------------------------------------------------------

func assertType(t *testing.T, args map[string]any, key, want string) {
	t.Helper()
	v, present := args[key]
	if !present {
		t.Errorf("TOOL-11: required argument %q absent", key)
		return
	}
	var got string
	switch v.(type) {
	case string:
		got = "string"
	case float64:
		got = "number"
	case bool:
		got = "bool"
	case []any:
		got = "array"
	case map[string]any:
		got = "object"
	case nil:
		got = "null"
	default:
		got = fmt.Sprintf("%T", v)
	}
	if got != want {
		t.Errorf("TOOL-11: argument %q arrived as %s, want %s (value %#v)", key, got, want, v)
	}
}

// callNames lists the tool names the server actually received, which is what
// a mis-routing or mis-naming failure message needs to show.
func callNames(calls []transcript.ToolCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
