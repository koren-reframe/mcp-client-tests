// Package protocol holds the T0 tier: tests that need no model and no API key.
//
// They are driven through `claude mcp list` and `claude mcp get`, which perform
// a real connection and handshake against a configured server. That makes the
// whole of connection, negotiation and transport failure testable for free, on
// every commit, without a key.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"mcpclienttests/internal/harness"
	"mcpclienttests/internal/transcript"
)

// TestCONN39_VersionPin guards the pin. Newer versions are published upstream,
// so if the image drifts every other assertion here is about a different
// program than the one this suite documents.
func TestCONN39_VersionPin(t *testing.T) {
	e := harness.New(t)
	r := e.Claude("--version")
	if r.Code != 0 {
		t.Fatalf("claude --version exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	if !strings.Contains(r.Stdout, harness.PinnedVersion) {
		t.Fatalf("version pin drifted: want %s, got %q", harness.PinnedVersion, strings.TrimSpace(r.Stdout))
	}
}

// TestCONN40_BaselineHandshake is the suite's baseline and its smoke test.
//
// One keyless `claude mcp list` settles a whole group of plan rows at once,
// because they are independent assertions over a single recorded transcript:
// CONN-40 (the health check needs no key), CONN-48 (the undocumented
// server/discover pre-probe and its fallback), CONN-49 (advertised
// capabilities), CONN-50 (id discipline), CONN-11 and CONN-51 (frame order,
// including notifications/initialized), CONN-12 (tools/list comes after init).
func TestCONN40_BaselineHandshake(t *testing.T) {
	e := harness.New(t)

	add := e.Claude(append([]string{"mcp", "add", "fixture", "--"}, e.FixtureArgs("ok")...)...)
	if add.Code != 0 {
		t.Fatalf("mcp add exited %d\nstdout: %s\nstderr: %s", add.Code, add.Stdout, add.Stderr)
	}

	list := e.ClaudeTimeout(60*time.Second, "mcp", "list")
	if list.TimedOut {
		t.Fatalf("mcp list timed out after %s; the health check should be prompt", list.Dur)
	}

	// CONN-01 / CONN-40: a complete handshake happened, with no API key in the
	// environment at all (harness.WithAPIKey defaults to false).
	if !strings.Contains(list.Combined(), "Connected") {
		t.Errorf("expected the fixture to be reported Connected\noutput: %s", list.Combined())
	}

	fs := e.Frames()
	if len(fs) == 0 {
		t.Fatalf("no frames recorded: the client never reached the fixture\noutput: %s", list.Combined())
	}

	// CONN-51: the observed handshake order.
	assertSubsequence(t, fs.Methods(),
		"server/discover", "initialize", "notifications/initialized", "tools/list")

	// CONN-48: the pre-probe carries its own, newer protocol version under
	// io.modelcontextprotocol/* _meta keys, and our fixture refuses it with
	// -32601. The client must fall back to initialize regardless, which the
	// subsequence above has already proven.
	disc, ok := fs.FromClient().First("server/discover")
	if !ok {
		t.Error("CONN-48: expected a server/discover pre-probe before initialize")
	} else {
		meta, _ := disc.Dig("params", "_meta").(map[string]any)
		if meta == nil {
			t.Errorf("CONN-48: server/discover had no params._meta: %s", disc.Raw)
		} else if _, found := meta["io.modelcontextprotocol/protocolVersion"]; !found {
			t.Errorf("CONN-48: _meta lacks io.modelcontextprotocol/protocolVersion: %v", keysOf(meta))
		}
	}

	init, ok := fs.FromClient().First("initialize")
	if !ok {
		t.Fatal("no initialize frame recorded")
	}

	// CONN-10: the initialize envelope.
	if pv, _ := init.Dig("params", "protocolVersion").(string); pv == "" {
		t.Error("CONN-10: initialize sent no protocolVersion")
	}
	if got, _ := init.Dig("params", "clientInfo", "name").(string); got != "claude-code" {
		t.Errorf("CONN-10: clientInfo.name = %q, want %q", got, "claude-code")
	}
	if got, _ := init.Dig("params", "clientInfo", "version").(string); got != harness.PinnedVersion {
		t.Errorf("CONN-10: clientInfo.version = %q, want %q", got, harness.PinnedVersion)
	}

	// CONN-49: exactly the capabilities observed during recon. roots and
	// elicitation are advertised; sampling is not, and a server must not expect
	// it.
	caps, _ := init.Dig("params", "capabilities").(map[string]any)
	if caps == nil {
		t.Fatalf("CONN-49: initialize sent no capabilities: %s", init.Raw)
	}
	if lc, _ := init.Dig("params", "capabilities", "roots", "listChanged").(bool); !lc {
		t.Errorf("CONN-49: roots.listChanged not advertised; capabilities = %v", keysOf(caps))
	}
	if _, found := caps["elicitation"]; !found {
		t.Errorf("CONN-49: elicitation not advertised; capabilities = %v", keysOf(caps))
	}
	if _, found := caps["sampling"]; found {
		t.Errorf("CONN-49: sampling is now advertised, which recon said it was not. "+
			"This is a real behaviour change, not a broken test; re-baseline and tell the team. capabilities = %v", keysOf(caps))
	}

	// CONN-12: tools/list comes after initialization, never before.
	if !fs.Has("tools/list") {
		t.Error("CONN-12: client never asked for tools/list")
	}

	// CONN-50: session ids are numeric and strictly increasing. The discover
	// probe uses a string id, so id handling must tolerate both shapes.
	assertNumericMonotonicIDs(t, fs, "initialize", "tools/list")
}

// TestCONN15_NoCapabilityNoQuery checks the client respects what the server
// advertises: with no capabilities at all, it must not go asking for tools,
// resources or prompts.
func TestCONN15_NoCapabilityNoQuery(t *testing.T) {
	e := harness.New(t)

	add := e.Claude(append([]string{"mcp", "add", "nocaps", "--"}, e.FixtureArgs("no_capabilities")...)...)
	if add.Code != 0 {
		t.Fatalf("mcp add exited %d: %s", add.Code, add.Stderr)
	}
	e.ClaudeTimeout(60*time.Second, "mcp", "list")

	fs := e.Frames()
	if _, ok := fs.FromClient().First("initialize"); !ok {
		t.Fatalf("no initialize recorded; transcript has %d frames", len(fs))
	}
	for _, m := range []string{"resources/list", "prompts/list"} {
		if fs.Has(m) {
			t.Errorf("CONN-15: client called %s although the server advertised no such capability", m)
		}
	}
}

// --- helpers ---------------------------------------------------------------

// assertSubsequence checks that want appears in got in order, allowing other
// methods in between. Order is the contract; an extra frame is not a failure.
func assertSubsequence(t *testing.T, got []string, want ...string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Errorf("handshake order wrong:\n  want subsequence: %v\n  got methods:      %v", want, got)
	}
}

func assertNumericMonotonicIDs(t *testing.T, fs transcript.Frames, methods ...string) {
	t.Helper()
	last := -1
	for _, m := range methods {
		f, ok := fs.FromClient().First(m)
		if !ok {
			continue
		}
		var n json.Number
		if err := json.Unmarshal(f.ID, &n); err != nil {
			t.Errorf("CONN-50: %s id %s is not numeric", m, f.ID)
			continue
		}
		v, err := n.Int64()
		if err != nil {
			t.Errorf("CONN-50: %s id %s is not an integer", m, f.ID)
			continue
		}
		if int(v) <= last {
			t.Errorf("CONN-50: %s id %d did not increase (previous %d)", m, v, last)
		}
		last = int(v)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
