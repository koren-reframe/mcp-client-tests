package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"mcpclienttests/internal/harness"
	"mcpclienttests/internal/health"
)

const testToken = "sekrit-token"

// TestCONN2326_StreamableHTTPEnvelope checks the three things the client must
// get right on every HTTP request, from the server's own record of what
// arrived: the Accept header (CONN-24), the credential (CONN-26), and the
// session id handed out at initialize (CONN-23).
func TestCONN2326_StreamableHTTPEnvelope(t *testing.T) {
	t.Parallel()
	e := harness.New(t)

	fx := e.StartFixture("http", "ok")
	e.AddHTTP("remote", fx.MCP, "http", "Authorization: Bearer "+testToken)

	_, st := e.Health(60 * time.Second)
	e.RequireState(st, "remote", health.Connected)

	reqs := e.HTTPRequests()
	if len(reqs) == 0 {
		t.Fatal("no HTTP requests recorded: the client never reached the fixture")
	}

	sum := sha256.Sum256([]byte(testToken))
	wantHash := hex.EncodeToString(sum[:])[:12]

	var posts, withSession int
	for i, req := range reqs {
		path, _ := req["path"].(string)
		method, _ := req["method"].(string)

		// CONN-24: both content types must be acceptable, because the server
		// may answer a POST with either JSON or an event stream.
		if method == "POST" {
			posts++
			accept, _ := harness.Header(req, "Accept")
			as, _ := accept.(string)
			for _, want := range []string{"application/json", "text/event-stream"} {
				if !strings.Contains(as, want) {
					t.Errorf("CONN-24: POST %s Accept = %q, missing %q", path, as, want)
				}
			}
		}

		// CONN-26: the credential travels on every request, including the very
		// first one, before any session exists.
		auth, ok := harness.Header(req, "Authorization")
		if !ok {
			t.Errorf("CONN-26: request %d (%s %s) carried no Authorization header", i, method, path)
			continue
		}
		am, _ := auth.(map[string]any)
		if scheme, _ := am["scheme"].(string); scheme != "Bearer" {
			t.Errorf("CONN-26: request %d auth scheme = %q, want Bearer", i, scheme)
		}
		if got, _ := am["sha256_12"].(string); got != wantHash {
			t.Errorf("CONN-26: request %d carried a different credential: hash %s, want %s", i, got, wantHash)
		}

		if sid, ok := harness.Header(req, "Mcp-Session-Id"); ok {
			if s, _ := sid.(string); s != "" {
				withSession++
			}
		}
	}

	if posts < 2 {
		t.Errorf("expected several POSTs (discover, initialize, initialized, tools/list), saw %d", posts)
	}
	// CONN-23: the session id the fixture minted at initialize comes back on
	// later requests.
	if withSession == 0 {
		t.Error("CONN-23: client never echoed the Mcp-Session-Id issued at initialize")
	}
}

// TestCONN30_OAuthDiscoveryOn401 checks the discovery chain a 401 with a
// Bearer challenge sets off. Observed behaviour, now pinned: the client asks
// for protected-resource metadata at the path-inserted location first
// (RFC 9728 style, /.well-known/oauth-protected-resource/mcp), falls back to
// the root form, and only then fetches authorization-server metadata. Both
// CONN-30 and CONN-31 are covered here, since they are two steps of one chain.
func TestCONN30_OAuthDiscoveryOn401(t *testing.T) {
	t.Parallel()
	e := harness.New(t)

	fx := e.StartFixture("http", "http_401_oauth")
	e.AddHTTP("needsauth", fx.MCP, "http")

	r, st := e.Health(90 * time.Second)
	if r.TimedOut {
		t.Fatal("CONN-30: health check hung on a 401")
	}
	e.RequireState(st, "needsauth", health.NeedsAuth)

	var paths []string
	for _, req := range e.HTTPRequests() {
		p, _ := req["path"].(string)
		paths = append(paths, p)
	}

	const resourceMeta = "/.well-known/oauth-protected-resource"
	const asMeta = "/.well-known/oauth-authorization-server"

	resourceAt, asAt := -1, -1
	for i, p := range paths {
		if resourceAt < 0 && strings.HasPrefix(p, resourceMeta) {
			resourceAt = i
		}
		if asAt < 0 && strings.HasPrefix(p, asMeta) {
			asAt = i
		}
	}

	if resourceAt < 0 {
		t.Errorf("CONN-30: no protected-resource metadata request after the 401; paths = %v", paths)
	}
	// CONN-31: discovery continues to the authorization server, and only after
	// the resource metadata that names it.
	if asAt < 0 {
		t.Errorf("CONN-31: discovery stopped before authorization-server metadata; paths = %v", paths)
	}
	if resourceAt >= 0 && asAt >= 0 && asAt < resourceAt {
		t.Errorf("CONN-31: authorization-server metadata fetched before the resource metadata that names it; paths = %v", paths)
	}

	// Recorded rather than required: the path-inserted form is what RFC 9728
	// prescribes, but a client that only used the root form would still be
	// interoperable, so this is an observation, not a demand.
	pathInserted := false
	for _, p := range paths {
		if strings.HasPrefix(p, resourceMeta+"/") {
			pathInserted = true
		}
	}
	t.Logf("CONN-30/31: discovery chain = %v (RFC 9728 path-inserted form tried: %v)", paths, pathInserted)
}

// TestCONN25_LegacySSETransport: the deprecated HTTP+SSE transport, where the
// server announces its POST endpoint as the first event on the stream.
func TestCONN25_LegacySSETransport(t *testing.T) {
	t.Parallel()
	e := harness.New(t)

	fx := e.StartFixture("sse", "ok")
	e.AddHTTP("legacy", fx.SSE, "sse")

	r, st := e.Health(90 * time.Second)
	if r.TimedOut {
		t.Fatal("CONN-25: health check hung on the legacy SSE transport")
	}

	got, ok := st["legacy"]
	if !ok {
		t.Fatalf("CONN-25: no status for the SSE server\noutput: %s", r.Combined())
	}
	t.Logf("CONN-25: legacy SSE status = %s (%s)", got.State, got.Raw)

	if len(e.Frames().Notes("sse_endpoint_announced")) == 0 {
		t.Error("CONN-25: client never opened the SSE stream, so the endpoint event was never sent")
	}
	if got.State != health.Connected {
		t.Errorf("CONN-25: legacy SSE did not connect: %s", got.Raw)
	}
}

// TestERR20_ConnectionRefused: nothing listening at all.
func TestERR20_ConnectionRefused(t *testing.T) {
	t.Parallel()
	e := harness.New(t)

	port := harness.ClosedPort(t)
	e.AddHTTP("dead", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), "http")

	r, st := e.Health(90 * time.Second)
	if r.TimedOut {
		t.Fatal("ERR-20: a refused connection should fail fast, not hang")
	}
	e.RequireState(st, "dead", health.Failed)
}

// TestERR2122_HTTPErrorStatuses distinguishes the two HTTP failure verdicts.
// A 500 is a connection failure with a cause; a 401 is its own actionable state,
// "Needs authentication", and carries no cause text. Treating the second as a
// generic failure is what this test exists to prevent.
func TestERR2122_HTTPErrorStatuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id, mode  string
		want      health.State
		wantCause bool
		causeHas  string
	}{
		{id: "ERR-21", mode: "http_500_init", want: health.Failed, wantCause: true, causeHas: "500"},
		{id: "ERR-22", mode: "http_401_bare", want: health.NeedsAuth},
		// The same verdict with a Bearer challenge as without one: the challenge
		// does not change the health outcome, only what a later login could do.
		{id: "ERR-22b", mode: "http_401_oauth", want: health.NeedsAuth},
	} {
		tc := tc
		t.Run(tc.id+"/"+tc.mode, func(t *testing.T) {
			t.Parallel()
			e := harness.New(t)

			fx := e.StartFixture("http", tc.mode)
			e.AddHTTP("bad", fx.MCP, "http")

			r, st := e.Health(90 * time.Second)
			if r.TimedOut {
				t.Fatalf("%s: client hung instead of reporting the HTTP error", tc.id)
			}
			got := e.RequireState(st, "bad", tc.want)

			if tc.wantCause && got.Cause == "" {
				t.Errorf("%s: failure reported with no cause: %s", tc.id, got.Raw)
			}
			if tc.causeHas != "" && !strings.Contains(got.Cause, tc.causeHas) {
				t.Errorf("%s: cause = %q, want it to mention %q", tc.id, got.Cause, tc.causeHas)
			}
			t.Logf("%s [%s] -> %s | %s", tc.id, tc.mode, got.State, got.Raw)
		})
	}
}
