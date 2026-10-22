package protocol

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"mcpclienttests/internal/harness"
	"mcpclienttests/internal/health"
)

// TestCONN03_ServerLifecycle covers the configuration surface end to end:
// add writes it, list health-checks it, get reports it, remove deletes it.
func TestCONN03_ServerLifecycle(t *testing.T) {
	t.Parallel()
	e := harness.New(t)

	e.AddStdio("lifecycle", "ok")

	_, st := e.Health(60 * time.Second)
	e.RequireState(st, "lifecycle", health.Connected)

	// CONN-04: get reports the configured transport, command and scope.
	get := e.Claude("mcp", "get", "lifecycle")
	if get.Code != 0 {
		t.Fatalf("mcp get exited %d: %s", get.Code, get.Stderr)
	}
	for _, want := range []string{"stdio", e.FixtureBin, "Local config"} {
		if !strings.Contains(get.Combined(), want) {
			t.Errorf("CONN-04: mcp get output missing %q\noutput: %s", want, get.Combined())
		}
	}

	// CONN-05: remove, then it is gone.
	rm := e.Claude("mcp", "remove", "lifecycle")
	if rm.Code != 0 {
		t.Fatalf("mcp remove exited %d: %s", rm.Code, rm.Stderr)
	}
	_, after := e.Health(60 * time.Second)
	if _, still := after["lifecycle"]; still {
		t.Errorf("CONN-05: server still listed after removal")
	}

	// A get for something that does not exist exits non-zero. Worth pinning
	// because `mcp list` does not signal failure that way (see ERR-31), so the
	// two commands have genuinely different contracts.
	missing := e.Claude("mcp", "get", "lifecycle")
	if missing.Code == 0 {
		t.Errorf("CONN-04: mcp get on a removed server exited 0, expected non-zero\noutput: %s", missing.Combined())
	}
}

// TestCONN13_NegotiatesOlderProtocol: a server answering with an older but
// supported version must still be usable.
func TestCONN13_NegotiatesOlderProtocol(t *testing.T) {
	t.Parallel()
	e := harness.New(t)
	e.AddStdio("older", "old_protocol")

	_, st := e.Health(60 * time.Second)
	e.RequireState(st, "older", health.Connected)

	fs := e.Frames()
	if !fs.Has("tools/list") {
		t.Errorf("CONN-13: client did not proceed to tools/list after negotiating down; methods = %v", fs.Methods())
	}
}

// TestCONN14_RejectsUnsupportedProtocol: an unsupported version must fail
// cleanly and promptly, and must not proceed to tool discovery.
func TestCONN14_RejectsUnsupportedProtocol(t *testing.T) {
	t.Parallel()
	e := harness.New(t)
	e.AddStdio("badproto", "unsupported_proto")

	r, st := e.Health(60 * time.Second)
	got := e.RequireState(st, "badproto", health.Failed)

	if !strings.Contains(strings.ToLower(got.Cause), "protocol version") {
		t.Errorf("CONN-14: cause should name the protocol version, got %q", got.Cause)
	}
	if r.TimedOut {
		t.Error("CONN-14: a version mismatch should fail fast, not hang")
	}
	if fs := e.Frames(); fs.Has("tools/list") {
		t.Errorf("CONN-14: client asked for tools after rejecting the version; methods = %v", fs.Methods())
	}
}

// TestCONN19_EnvExpandedIntoChild proves a ${VAR} reference in server config is
// resolved from the client's own environment before the server is spawned. The
// fixture reports its received environment, so this is verified from the child's
// point of view rather than from the config file.
func TestCONN19_EnvExpandedIntoChild(t *testing.T) {
	t.Parallel()
	e := harness.New(t)
	e.Extra = map[string]string{"OUTER_SECRET": "expanded-" + e.Nonce}

	add := e.Claude(append([]string{
		"mcp", "add", "envsrv", "-e", "INNER=${OUTER_SECRET}", "--",
	}, e.FixtureArgs("ok", "-record-env", "INNER,OUTER_SECRET")...)...)
	if add.Code != 0 {
		t.Fatalf("mcp add exited %d: %s", add.Code, add.Stderr)
	}
	e.Health(60 * time.Second)

	notes := e.Frames().Notes("spawn_env")
	if len(notes) == 0 {
		t.Fatalf("fixture recorded no spawn_env note; was it started at all? frames = %d", len(e.Frames()))
	}
	inner, _ := notes[0].Extra["INNER"].(map[string]any)
	if inner == nil {
		t.Fatalf("CONN-19: no INNER entry recorded: %v", notes[0].Extra)
	}
	if present, _ := inner["present"].(bool); !present {
		t.Fatalf("CONN-19: INNER was not passed to the child at all: %v", inner)
	}
	got, _ := inner["value"].(string)
	want := "expanded-" + e.Nonce
	if got != want {
		t.Errorf("CONN-19: INNER = %q, want %q (an unexpanded ${OUTER_SECRET} means the client "+
			"stores the literal and never resolves it)", got, want)
	}
}

// TestCONN36_SpawnedOncePerHealthCheck: one connection attempt means one
// server process, not several.
func TestCONN36_SpawnedOncePerHealthCheck(t *testing.T) {
	t.Parallel()
	e := harness.New(t)
	e.AddStdio("once", "ok")
	e.Health(60 * time.Second)

	if n := len(e.Frames().FromClient().Method("initialize")); n != 1 {
		t.Errorf("CONN-36: saw %d initialize frames, want exactly 1 (methods = %v)", n, e.Frames().Methods())
	}
}

// TestCONN37_TwoServersBothConnect: two independently configured servers both
// come up, each recording into its own transcript.
func TestCONN37_TwoServersBothConnect(t *testing.T) {
	t.Parallel()
	e := harness.New(t)

	for _, name := range []string{"alpha", "beta"} {
		args := append([]string{"mcp", "add", name, "--"}, e.FixtureArgsT(e.TranscriptFor(name), "ok", "-name", name)...)
		if r := e.Claude(args...); r.Code != 0 {
			t.Fatalf("mcp add %s exited %d: %s", name, r.Code, r.Stderr)
		}
	}

	_, st := e.Health(90 * time.Second)
	e.RequireState(st, "alpha", health.Connected)
	e.RequireState(st, "beta", health.Connected)

	for _, name := range []string{"alpha", "beta"} {
		fs := e.FramesFor(name)
		if _, ok := fs.FromClient().First("initialize"); !ok {
			t.Errorf("CONN-37: %s recorded no initialize; frames = %d", name, len(fs))
		}
	}
}

// TestCONN47_PendingApprovalIsNotConnected: a project-scoped .mcp.json server
// is gated behind approval, and the client must not connect to it before then.
// The empty transcript is the proof, and this is also why the suite never
// relies on .mcp.json for its own fixtures.
func TestCONN47_PendingApprovalIsNotConnected(t *testing.T) {
	t.Parallel()
	e := harness.New(t)

	e.WriteWorkFile(".mcp.json", fmt.Sprintf(`{"mcpServers":{"projsrv":{"command":%q,"args":["-transport","stdio","-mode","ok","-transcript",%q,"-nonce",%q]}}}`,
		e.FixtureBin, e.Transcript, e.Nonce))

	r, st := e.Health(60 * time.Second)
	e.RequireState(st, "projsrv", health.Pending)

	if fs := e.Frames(); len(fs) != 0 {
		t.Errorf("CONN-47: client connected to an unapproved server: %d frames recorded", len(fs))
	}
	if r.TimedOut {
		t.Error("CONN-47: health check hung, presumably waiting for approval input")
	}
}

// TestERR32_MalformedProjectConfig: invalid JSON in .mcp.json must produce a
// diagnostic rather than a crash or a hang.
func TestERR32_MalformedProjectConfig(t *testing.T) {
	t.Parallel()
	e := harness.New(t)
	e.WriteWorkFile(".mcp.json", "{ this is not valid json at all")

	r, _ := e.Health(60 * time.Second)
	if r.TimedOut {
		t.Fatal("ERR-32: malformed config hung the health check")
	}
	if r.Code < 0 {
		t.Fatalf("ERR-32: client did not exit normally: %s", r.Stderr)
	}
	out := r.Combined()
	if !strings.Contains(out, "diagnostics") && !strings.Contains(strings.ToLower(out), "json") {
		t.Errorf("ERR-32: no diagnostic for a malformed .mcp.json; output was:\n%s", out)
	}
}
