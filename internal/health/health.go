// Package health parses the output of `claude mcp list` and `claude mcp get`.
//
// The health check is the suite's keyless driver: it performs a real connection
// and handshake without the model, which is what makes the T0 tier possible.
// Its result vocabulary is wider than it first appears — six states, two of
// which contain the word "Connected" — so the parsing lives here once instead
// of in every test. An unrecognised state is deliberately not mapped to
// anything: the server then goes missing from the result, and the test fails
// loudly, rather than being quietly mis-classified as a failure.
package health

import "strings"

type State string

const (
	// Connected: handshake and tool discovery both succeeded.
	Connected State = "connected"
	// Partial: handshake succeeded but tools/list did not. Reported as
	// "! Connected · tools fetch failed — <cause>". Note this line also
	// contains the word "Connected", which is the trap.
	Partial State = "partial"
	// NeedsAuth: the server answered 401. Reported as "! Needs authentication",
	// with no cause text. A distinct, actionable state rather than a generic
	// connection failure, and the same verdict whether or not the 401 carried a
	// WWW-Authenticate challenge.
	NeedsAuth State = "needs_auth"
	// Failed: never got a usable session. "✘ Failed to connect — <cause>".
	Failed State = "failed"
	// Pending: a project-scoped .mcp.json server awaiting approval. The client
	// does not connect to these at all.
	Pending State = "pending"
	// Disabled: configured but switched off for this project.
	Disabled State = "disabled"
)

type Status struct {
	Server string
	State  State
	Cause  string // text after the em dash, when the client gave one
	Raw    string
}

// markers are ordered most specific first: "Failed to connect" and
// "tools fetch failed" must both be tested before the bare "Connected".
var markers = []struct {
	text  string
	state State
}{
	{"Failed to connect", Failed},
	{"tools fetch failed", Partial},
	{"Needs authentication", NeedsAuth},
	{"Pending approval", Pending},
	{"disabled", Disabled},
	{"Connected", Connected},
}

// Parse extracts one Status per server from health-check output. Only lines
// carrying a recognised status marker are considered, so surrounding prose and
// the key/value lines of `mcp get` cannot be mistaken for servers.
func Parse(out string) map[string]Status {
	res := map[string]Status{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(stripANSI(line))
		if line == "" {
			continue
		}

		state := State("")
		for _, m := range markers {
			if strings.Contains(line, m.text) {
				state = m.state
				break
			}
		}
		if state == "" {
			continue // not a server status line
		}

		// "<name>: <command> - <status>"
		colon := strings.Index(line, ":")
		if colon <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if name == "" || strings.ContainsAny(name, " \t") {
			continue // prose, not an identifier
		}

		st := Status{Server: name, State: state, Raw: line}
		// The client separates the cause with an em dash.
		if i := strings.LastIndex(line, "—"); i >= 0 {
			st.Cause = strings.TrimSpace(line[i+len("—"):])
		}
		res[name] = st
	}
	return res
}

// Of returns the status of one server, and whether it was present at all.
func Of(out, server string) (Status, bool) {
	st, ok := Parse(out)[server]
	return st, ok
}

// stripANSI removes escape sequences so parsing survives a colourised run.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			if i < len(s) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
