package harness

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mcpclienttests/internal/health"
)

// Fixture is a fixture-mcp process the test started itself, for the HTTP and
// SSE transports where the client connects out rather than spawning a child.
type Fixture struct {
	Port int
	Base string // http://127.0.0.1:<port>
	MCP  string // streamable HTTP endpoint
	SSE  string // legacy SSE endpoint
	cmd  *exec.Cmd
}

var portLine = regexp.MustCompile(`listening port=(\d+)`)

// StartFixture launches an HTTP or SSE fixture and waits for it to report its
// port, so tests never guess a port or sleep hoping it is up. transport is
// "http" or "sse".
func (e *Env) StartFixture(transport, mode string, extra ...string) *Fixture {
	e.T.Helper()

	args := append([]string{
		"-transport", transport,
		"-mode", mode,
		"-addr", "127.0.0.1:0",
		"-transcript", e.Transcript,
		"-nonce", e.Nonce,
		"-probe-dir", e.ProbeDir,
	}, extra...)

	cmd := exec.Command(e.FixtureBin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		e.T.Fatalf("harness: fixture stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		e.T.Fatalf("harness: start fixture: %v", err)
	}

	portCh := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if m := portLine.FindStringSubmatch(sc.Text()); m != nil {
				p, _ := strconv.Atoi(m[1])
				select {
				case portCh <- p:
				default:
				}
			}
		}
	}()

	var port int
	select {
	case port = <-portCh:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		e.T.Fatalf("harness: fixture did not report a port within 10s")
	}

	f := &Fixture{
		Port: port,
		Base: fmt.Sprintf("http://127.0.0.1:%d", port),
		cmd:  cmd,
	}
	f.MCP, f.SSE = f.Base+"/mcp", f.Base+"/sse"

	e.T.Cleanup(f.Stop)
	return f
}

// Stop kills the fixture. Safe to call twice.
func (f *Fixture) Stop() {
	if f.cmd != nil && f.cmd.Process != nil {
		f.cmd.Process.Kill()
		f.cmd.Wait()
		f.cmd = nil
	}
}

// --- convenience wrappers over the CLI -------------------------------------

// AddStdio configures a stdio fixture server in the given fault mode.
func (e *Env) AddStdio(name, mode string, fixtureFlags ...string) Result {
	e.T.Helper()
	args := append([]string{"mcp", "add", name, "--"}, e.FixtureArgs(mode, fixtureFlags...)...)
	r := e.Claude(args...)
	if r.Code != 0 {
		e.T.Fatalf("mcp add %s (mode %s) exited %d\nstdout: %s\nstderr: %s", name, mode, r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// AddHTTP configures an HTTP or SSE server. headers are "Name: value" strings.
func (e *Env) AddHTTP(name, url, transport string, headers ...string) Result {
	e.T.Helper()
	args := []string{"mcp", "add", "--transport", transport, name, url}
	for _, h := range headers {
		args = append(args, "--header", h)
	}
	r := e.Claude(args...)
	if r.Code != 0 {
		e.T.Fatalf("mcp add %s (%s) exited %d\nstdout: %s\nstderr: %s", name, transport, r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// Health runs the keyless health check and returns the raw output alongside the
// parsed per-server status.
func (e *Env) Health(timeout time.Duration) (Result, map[string]health.Status) {
	e.T.Helper()
	r := e.ClaudeTimeout(timeout, "mcp", "list")
	return r, health.Parse(r.Combined())
}

// RequireState asserts a server reached an expected state, printing the raw
// line on failure because the cause text is where the diagnosis lives.
func (e *Env) RequireState(statuses map[string]health.Status, server string, want health.State) health.Status {
	e.T.Helper()
	st, ok := statuses[server]
	if !ok {
		e.T.Fatalf("server %q absent from health output; saw %v", server, serverNames(statuses))
		return st
	}
	if st.State != want {
		e.T.Errorf("server %q: state = %s, want %s\n  line: %s", server, st.State, want, st.Raw)
	}
	return st
}

func serverNames(m map[string]health.Status) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// HTTPRequests returns the HTTP request envelopes the fixture recorded, in
// order. Credential headers in these are already redacted to a hash.
func (e *Env) HTTPRequests() []map[string]any {
	e.T.Helper()
	var out []map[string]any
	for _, f := range e.Frames().Notes("http_request") {
		out = append(out, f.Extra)
	}
	return out
}

// Header reads one header from a recorded request envelope.
func Header(req map[string]any, name string) (any, bool) {
	hs, _ := req["headers"].(map[string]any)
	if hs == nil {
		return nil, false
	}
	v, ok := hs[strings.ToLower(name)]
	return v, ok
}
