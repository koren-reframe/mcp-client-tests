// Package harness runs Claude Code as the system under test in a hermetic
// environment, and preserves the evidence when a test fails.
//
// Hermeticity here is load-bearing, not hygiene. Claude Code persists MCP
// configuration in the user's home directory keyed by project path, and
// project-scoped .mcp.json servers sit behind a trust gate. Sharing a HOME
// between tests makes them pass or fail by ordering, and can hang a
// non-interactive run waiting for approval. Every test therefore gets its own
// HOME, its own working directory and its own transcript.
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcpclienttests/internal/transcript"
)

// PinnedVersion is the version the suite is written against. CONN-39 asserts
// it, because newer versions are already published upstream and an image that
// quietly drifts stops testing the stated system under test.
const PinnedVersion = "2.1.296"

// DefaultTimeout bounds any single CLI invocation. Assertions about client
// timeouts use explicit bounds instead of relying on this.
const DefaultTimeout = 90 * time.Second

type Env struct {
	T          *testing.T
	Home       string // isolated HOME
	Work       string // isolated project directory (cwd for the CLI)
	Transcript string // where the fixture records frames
	ProbeDir   string // where write_probe writes
	Nonce      string // unique per test run; witness C
	FixtureBin string

	// Extra adds environment variables for the CLI process, used to prove that
	// ${VAR} references in server config are expanded from the client's own
	// environment at spawn time.
	Extra map[string]string

	// WithAPIKey opts a test into the model tier. It is false by default, so a
	// test that forgets to ask for a key simply cannot reach the model: that is
	// what keeps the T0 tier honestly keyless (DET-01).
	WithAPIKey bool

	runs []Result
}

type Result struct {
	Args     []string
	Stdout   string
	Stderr   string
	Code     int
	Dur      time.Duration
	TimedOut bool
}

// Combined is for matching against output without caring which stream it used.
func (r Result) Combined() string { return r.Stdout + "\n" + r.Stderr }

func New(t *testing.T) *Env {
	t.Helper()
	base := t.TempDir()

	e := &Env{
		T:          t,
		Home:       filepath.Join(base, "home"),
		Work:       filepath.Join(base, "work"),
		Transcript: filepath.Join(base, "frames.ndjson"),
		ProbeDir:   filepath.Join(base, "probe"),
		Nonce:      newNonce(),
		FixtureBin: fixtureBin(t),
	}
	for _, d := range []string{e.Home, e.Work, e.ProbeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("harness: mkdir %s: %v", d, err)
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			e.dumpArtifacts()
		}
	})
	return e
}

func newNonce() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "nonce-" + hex.EncodeToString(b)
}

func fixtureBin(t *testing.T) string {
	if p := os.Getenv("FIXTURE_BIN"); p != "" {
		return p
	}
	const def = "/usr/local/bin/fixture-mcp"
	if _, err := os.Stat(def); err == nil {
		return def
	}
	if p, err := exec.LookPath("fixture-mcp"); err == nil {
		return p
	}
	t.Skipf("fixture-mcp not found: set FIXTURE_BIN, or run the suite via ./run.sh")
	return ""
}

// FixtureArgs builds the argv for a stdio fixture server in the given fault
// mode, ready to hand to `claude mcp add ... -- <argv>`.
func (e *Env) FixtureArgs(mode string, extra ...string) []string {
	args := []string{e.FixtureBin,
		"-transport", "stdio",
		"-mode", mode,
		"-transcript", e.Transcript,
		"-nonce", e.Nonce,
		"-probe-dir", e.ProbeDir,
	}
	return append(args, extra...)
}

// Claude runs the CLI under test with the default timeout.
func (e *Env) Claude(args ...string) Result {
	return e.ClaudeTimeout(DefaultTimeout, args...)
}

// ClaudeTimeout runs the CLI with an explicit deadline. A timeout is returned
// as a Result with TimedOut set rather than failing the test, because for
// several tests the client hanging is the finding.
func (e *Env) ClaudeTimeout(d time.Duration, args ...string) Result {
	e.T.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = e.Work
	cmd.Env = e.env()
	// Never let the CLI block on stdin waiting for input nobody will give it.
	cmd.Stdin = strings.NewReader("")

	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	start := time.Now()
	err := cmd.Run()
	res := Result{
		Args:   args,
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		Dur:    time.Since(start),
	}
	res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)

	var ee *exec.ExitError
	switch {
	case err == nil:
		res.Code = 0
	case errors.As(err, &ee):
		res.Code = ee.ExitCode()
	default:
		res.Code = -1
		res.Stderr += "\nharness: " + err.Error()
	}

	e.runs = append(e.runs, res)
	return res
}

// env builds the hermetic environment. The updater and non-essential traffic
// are disabled so the pinned version cannot drift mid-run and so the client
// does not make network calls the test did not ask for.
func (e *Env) env() []string {
	env := []string{
		"HOME=" + e.Home,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + filepath.Join(e.Home, "tmp"),
		"FIXTURE_TRANSCRIPT=" + e.Transcript,
		"FIXTURE_NONCE=" + e.Nonce,
		"FIXTURE_PROBE_DIR=" + e.ProbeDir,
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_BUG_COMMAND=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CI=1",
		"TERM=dumb",
		"NO_COLOR=1",
	}
	for k, v := range e.Extra {
		env = append(env, k+"="+v)
	}
	if e.WithAPIKey {
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			e.T.Skip("model tier: ANTHROPIC_API_KEY not set")
		}
		env = append(env, "ANTHROPIC_API_KEY="+key)
	}
	return env
}

// TranscriptFor gives a second, independent transcript path, for tests that
// configure more than one server and must keep their recordings apart.
func (e *Env) TranscriptFor(name string) string {
	return filepath.Join(filepath.Dir(e.Transcript), "frames-"+name+".ndjson")
}

// FramesFor reads a named transcript.
func (e *Env) FramesFor(name string) transcript.Frames {
	e.T.Helper()
	fs, err := transcript.Read(e.TranscriptFor(name))
	if err != nil {
		e.T.Fatalf("harness: read transcript %s: %v", name, err)
	}
	return fs
}

// FixtureArgsT is FixtureArgs with an explicit transcript path.
func (e *Env) FixtureArgsT(transcriptPath, mode string, extra ...string) []string {
	args := []string{e.FixtureBin,
		"-transport", "stdio",
		"-mode", mode,
		"-transcript", transcriptPath,
		"-nonce", e.Nonce,
		"-probe-dir", e.ProbeDir,
	}
	return append(args, extra...)
}

// WriteWorkFile writes a file into the isolated project directory, for things
// like a project-scoped .mcp.json.
func (e *Env) WriteWorkFile(rel, content string) string {
	e.T.Helper()
	path := filepath.Join(e.Work, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.T.Fatalf("harness: mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.T.Fatalf("harness: write %s: %v", rel, err)
	}
	return path
}

// ClosedPort returns a port that was bound and released, so a connection to it
// is refused rather than merely slow.
func ClosedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("harness: reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// Frames reads the fixture transcript.
func (e *Env) Frames() transcript.Frames {
	e.T.Helper()
	fs, err := transcript.Read(e.Transcript)
	if err != nil {
		e.T.Fatalf("harness: read transcript: %v", err)
	}
	return fs
}

// dumpArtifacts preserves everything needed to diagnose a failure without
// rerunning it. Only failures produce artifacts, so a green run stays quiet.
func (e *Env) dumpArtifacts() {
	root := os.Getenv("ARTIFACT_DIR")
	if root == "" {
		root = "/artifacts"
	}
	dir := filepath.Join(root, sanitize(e.T.Name()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.T.Logf("harness: could not create artifact dir %s: %v", dir, err)
		return
	}

	if b, err := os.ReadFile(e.Transcript); err == nil {
		os.WriteFile(filepath.Join(dir, "frames.ndjson"), b, 0o644)
	}
	var sb strings.Builder
	for i, r := range e.runs {
		fmt.Fprintf(&sb, "=== run %d: claude %s\n", i, strings.Join(r.Args, " "))
		fmt.Fprintf(&sb, "exit=%d timedOut=%v dur=%s\n", r.Code, r.TimedOut, r.Dur)
		fmt.Fprintf(&sb, "--- stdout ---\n%s\n--- stderr ---\n%s\n\n", r.Stdout, r.Stderr)
	}
	os.WriteFile(filepath.Join(dir, "runs.txt"), []byte(sb.String()), 0o644)
	os.WriteFile(filepath.Join(dir, "env.txt"),
		[]byte(fmt.Sprintf("home=%s\nwork=%s\nnonce=%s\nfixture=%s\nwithKey=%v\n",
			e.Home, e.Work, e.Nonce, e.FixtureBin, e.WithAPIKey)), 0o644)
	e.T.Logf("harness: artifacts written to %s", dir)
}

func sanitize(s string) string {
	return strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(s)
}
