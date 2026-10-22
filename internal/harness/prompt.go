package harness

import (
	"os"
	"strings"
	"time"

	"mcpclienttests/internal/claudecli"
)

// Model is the model the suite pins for the T1 tier. An alias rather than a
// dated id, so the pin survives a model being retired, and overridable for a
// run that wants to check behaviour on a different one. Recorded in the run
// report either way (DET-08).
func Model() string {
	if m := os.Getenv("SUITE_MODEL"); m != "" {
		return m
	}
	return "haiku"
}

// PromptOpts shapes a single non-interactive session.
type PromptOpts struct {
	// AllowTools are passed to --allowedTools. Keeping this to exactly the tool
	// under test is the main lever the suite has against model
	// non-determinism: with one tool available there is little else to do.
	AllowTools []string

	// Restricted removes the built-in tools, narrowing the model's options
	// further (DET-16).
	Restricted bool

	// Timeout bounds the session. A hanging tool is a finding, so the timeout
	// is returned rather than fatal.
	Timeout time.Duration

	// Extra arguments appended verbatim.
	Extra []string
}

// Prompt runs one non-interactive session and parses the client's event stream.
//
// Everything here that fights non-determinism is deliberate: the model is
// pinned, the tool set is narrowed to the tool under test, and the prompt is
// imperative. Even so the result is a probability, not a certainty, which is
// why the assertions on top of this are invariant-based and why the model tier
// is reported as a rate rather than a single pass (DET-06).
func (e *Env) Prompt(prompt string, opts PromptOpts) (Result, *claudecli.Run) {
	e.T.Helper()
	e.WithAPIKey = true // skips the test if no key is present

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}

	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--model", Model(),
	}
	if len(opts.AllowTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(opts.AllowTools, ","))
	}
	if opts.Restricted {
		args = append(args, "--restricted")
	}
	args = append(args, opts.Extra...)

	r := e.ClaudeTimeout(timeout, args...)
	run := claudecli.Parse(r.Stdout)

	// Cost is logged per session so a run's spend is visible without extra
	// tooling, and so an accidentally expensive test is obvious.
	if run.Result != nil {
		e.T.Logf("session: turns=%d cost=$%.5f model=%s tools_used=%v",
			run.Turns(), run.CostUSD(), Model(), run.ToolNames())
	}
	return r, run
}

// MCPToolName builds the namespaced name the client is expected to use.
func MCPToolName(server, tool string) string {
	return "mcp__" + server + "__" + tool
}
