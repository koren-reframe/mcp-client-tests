package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// tools is the advertised catalogue. The schemas are deliberately varied so
// that argument fidelity can be checked across JSON types rather than only on
// strings.
func (s *Session) tools() []map[string]any {
	if n := s.mode.ToolFlood; n > 0 {
		out := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{
				"name":        fmt.Sprintf("flood_tool_%02d", i),
				"description": fmt.Sprintf("generated tool %d of %d", i, n),
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			})
		}
		return out
	}

	str := map[string]any{"type": "string"}
	return []map[string]any{{
		"name":        "echo_nonce",
		"description": "Echo the caller's text back together with this server's run nonce. Use when asked to echo text.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": str},
			"required":   []string{"text"},
		},
	}, {
		"name":        "write_probe",
		"description": "Write content to a file on disk and return the path written. Use when asked to write a probe file.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filename": str,
				"content":  str,
			},
			"required": []string{"filename", "content"},
		},
	}, {
		"name":        "typed_args",
		"description": "Accept one argument of each JSON type and report back the type and value of each, for checking argument fidelity.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"s":   str,
				"i":   map[string]any{"type": "integer"},
				"b":   map[string]any{"type": "boolean"},
				"e":   map[string]any{"type": "string", "enum": []string{"alpha", "beta", "gamma"}},
				"arr": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
				"obj": map[string]any{
					"type":       "object",
					"properties": map[string]any{"inner": str},
				},
				"optional": str,
			},
			"required": []string{"s", "i", "b", "e", "arr", "obj"},
		},
	}, {
		"name":        "unicode_payload",
		"description": "Return a payload of unicode, emoji and CJK text, echoing back any text given.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": str},
		},
	}}
}

// handleToolCall dispatches a tools/call, applying whichever result-level fault
// the current mode asks for before the tool itself ever runs.
func (s *Session) handleToolCall(req Req) *Action {
	act := &Action{}

	if s.mode.HangTool {
		act.Hang = true
		return act
	}
	if s.mode.ExitOnCall != nil {
		s.rec.Note("crash_on_call", nil)
		act.Exit = s.mode.ExitOnCall
		return act
	}
	// A call that fails at the protocol level: no result at all, just an error
	// object. Distinct from a tool that runs and reports failure.
	if c := s.mode.ToolRPCError; c != 0 {
		act.Outs = append(act.Outs, s.rpcErr(req.ID, c, map[int]string{
			-32602: "invalid params: fixture rejected the arguments",
			-32601: "method not found: tools/call unsupported",
			-32603: "internal error: fixture failed on purpose",
		}[c]))
		return act
	}
	if s.mode.BadResultShape {
		act.Outs = append(act.Outs, s.result(req.ID, map[string]any{"unexpected": "no content field here"}))
		return act
	}
	if s.mode.NonUTF8 {
		// Hand-built so the JSON encoder cannot sanitise the bad bytes.
		raw := append([]byte(`{"jsonrpc":"2.0","id":`), req.ID...)
		raw = append(raw, []byte(`,"result":{"content":[{"type":"text","text":"bad bytes: `)...)
		raw = append(raw, 0xff, 0xfe, 0xfd)
		raw = append(raw, []byte(`"}]}}`)...)
		act.Outs = append(act.Outs, Out{Raw: raw})
		return act
	}
	if s.mode.HugeOutput {
		act.Outs = append(act.Outs, s.result(req.ID, textResult(strings.Repeat("A", 1<<20), false)))
		return act
	}

	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		act.Outs = append(act.Outs, s.rpcErr(req.ID, -32602, "could not parse params: "+err.Error()))
		return act
	}

	text, isErr := s.runTool(p.Name, p.Arguments)
	if s.mode.ToolIsError {
		isErr = true
		text = "fixture tool reporting failure on purpose: " + text
	}
	act.Outs = append(act.Outs, s.result(req.ID, textResult(text, isErr)))
	return act
}

func textResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// runTool is the tool implementation. Every tool is a side effect or an echo
// that a test can verify independently of the model: the nonce proves the
// result reached the model's context, and write_probe proves the call really
// executed rather than being imagined.
func (s *Session) runTool(name string, args map[string]any) (string, bool) {
	switch name {
	case "echo_nonce":
		text, _ := args["text"].(string)
		return fmt.Sprintf("nonce=%s text=%s", s.nonce, text), false

	case "write_probe":
		fname, _ := args["filename"].(string)
		content, _ := args["content"].(string)
		if fname == "" {
			fname = "probe.txt"
		}
		// Keep the write inside the probe directory regardless of what was asked.
		path := filepath.Join(s.probeDir, filepath.Base(fname))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return "write failed: " + err.Error(), true
		}
		return fmt.Sprintf("wrote %d bytes to %s (nonce=%s)", len(content), path, s.nonce), false

	case "typed_args":
		// Report the Go type the JSON decoder produced for each argument, so a
		// test can assert type fidelity rather than only string equality.
		report := map[string]any{}
		for _, k := range []string{"s", "i", "b", "e", "arr", "obj", "optional"} {
			v, present := args[k]
			if !present {
				report[k] = map[string]any{"present": false}
				continue
			}
			report[k] = map[string]any{"present": true, "gotype": fmt.Sprintf("%T", v), "value": v}
		}
		b, _ := json.Marshal(report)
		return fmt.Sprintf("nonce=%s types=%s", s.nonce, b), false

	case "unicode_payload":
		text, _ := args["text"].(string)
		return fmt.Sprintf("nonce=%s echo=%s payload=café 日本語 🚀 ✓ Ωmega naïve", s.nonce, text), false

	default:
		if strings.HasPrefix(name, "flood_tool_") {
			return fmt.Sprintf("nonce=%s tool=%s", s.nonce, name), false
		}
		return "no such tool: " + name, true
	}
}
