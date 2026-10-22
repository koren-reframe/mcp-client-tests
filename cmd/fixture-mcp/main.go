// Command fixture-mcp is the measuring instrument for the Claude Code MCP
// client test suite.
//
// It is an MCP server we fully control, with three jobs:
//
//  1. Speak MCP over stdio, streamable HTTP or legacy HTTP+SSE.
//  2. Record every frame it receives and sends, as NDJSON, so that tests assert
//     against what actually crossed the wire rather than against model prose.
//  3. Misbehave on demand, selected by -mode, so that a fault injection is a
//     configuration change rather than new code.
//
// It has no dependencies outside the standard library, and deliberately
// hand-rolls JSON-RPC: a conformant SDK will not let us emit a malformed frame,
// and malformed frames are half of what the error tests need.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		transport  = flag.String("transport", env("FIXTURE_TRANSPORT", "stdio"), "transport: stdio | http | sse")
		modeName   = flag.String("mode", env("FIXTURE_MODE", "ok"), "fault injection mode (-list-modes to see all)")
		addr       = flag.String("addr", env("FIXTURE_ADDR", "127.0.0.1:0"), "listen address for http/sse")
		name       = flag.String("name", env("FIXTURE_NAME", "fixture"), "serverInfo.name")
		transcript = flag.String("transcript", env("FIXTURE_TRANSCRIPT", ""), "NDJSON transcript path")
		nonce      = flag.String("nonce", env("FIXTURE_NONCE", ""), "value echo_nonce returns")
		probeDir   = flag.String("probe-dir", env("FIXTURE_PROBE_DIR", os.TempDir()), "directory write_probe writes into")
		portFile   = flag.String("port-file", env("FIXTURE_PORT_FILE", ""), "write the chosen http/sse port here")
		listModes  = flag.Bool("list-modes", false, "print the fault catalogue and exit")
	)
	flag.Parse()

	if *listModes {
		names := make([]string, 0, len(modes))
		for n := range modes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("%-28s %s\n", n, modes[n].Desc)
		}
		return
	}

	mode, ok := modes[*modeName]
	if !ok {
		fmt.Fprintf(os.Stderr, "fixture-mcp: unknown mode %q (use -list-modes)\n", *modeName)
		os.Exit(2)
	}

	rec, err := NewRecorder(*transcript)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture-mcp: transcript: %v\n", err)
		os.Exit(2)
	}
	defer rec.Close()

	s := &Session{mode: mode, name: *name, nonce: *nonce, probeDir: *probeDir, rec: rec}

	// Faults that happen before a single byte is read.
	if mode.ExitBeforeInit != nil {
		rec.Note("exit_before_init", map[string]any{"code": *mode.ExitBeforeInit})
		os.Exit(*mode.ExitBeforeInit)
	}

	switch *transport {
	case "stdio":
		runStdio(s)
	case "http":
		runHTTP(s, *addr, *portFile, false)
	case "sse":
		runHTTP(s, *addr, *portFile, true)
	default:
		fmt.Fprintf(os.Stderr, "fixture-mcp: unknown transport %q\n", *transport)
		os.Exit(2)
	}
}

// runStdio serves newline-delimited JSON-RPC on stdin/stdout, which is how
// Claude Code drives a stdio MCP server.
func runStdio(s *Session) {
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	// stdout pollution faults: emit before anything else, so the client meets
	// them while it is still expecting a handshake.
	if s.mode.StdoutNoise {
		fmt.Fprintln(out, "fixture-mcp: starting up, this line is not JSON-RPC")
		fmt.Fprintln(out, "[info] listening on stdio")
		out.Flush()
	}
	if s.mode.StdoutJSONNotRPC {
		fmt.Fprintln(out, `{"hello":"world","note":"valid JSON, not JSON-RPC"}`)
		out.Flush()
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 32*1024*1024) // tool results can be large
	for in.Scan() {
		line := in.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		act := s.Handle(line)
		for _, o := range act.Outs {
			if o.Delay > 0 {
				time.Sleep(o.Delay)
			}
			b := o.Bytes(s.rec)
			out.Write(b)
			out.WriteByte('\n')
			out.Flush()
		}
		if act.Hang {
			s.rec.Note("hang", map[string]any{"after": "request"})
			// Block in a way the runtime will not interfere with. A bare
			// select{} looks like a deadlock to Go, which panics and exits,
			// and the client then sees a closed connection instead of a hang.
			// Those are different faults, so the distinction matters.
			time.Sleep(365 * 24 * time.Hour)
		}
		if act.Exit != nil {
			s.rec.Note("exit", map[string]any{"code": *act.Exit})
			out.Flush()
			os.Exit(*act.Exit)
		}
	}
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r' || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}

// Bytes renders an outbound item, recording it as it goes. Raw wins over Frame
// so that malformed-output faults can bypass the JSON encoder entirely.
func (o Out) Bytes(rec *Recorder) []byte {
	if o.Raw != nil {
		rec.Raw("out", o.Raw)
		return o.Raw
	}
	b, err := json.Marshal(o.Frame)
	if err != nil {
		b = []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"fixture marshal failure"}}`)
	}
	rec.Raw("out", b)
	return b
}
