// Package claudecli parses the client's own `--output-format stream-json`
// event stream.
//
// This is witness A: what the client reports about itself. On its own it proves
// little, because a client could report a tool call it never made. Its value is
// in being cross-checked against the fixture's transcript (witness B) and a
// side effect (witness C). Where the three agree, the tool really ran.
package claudecli

import (
	"encoding/json"
	"strings"
)

type ToolUse struct {
	ID    string
	Name  string // as the client names it, e.g. mcp__srv__echo_nonce
	Input map[string]any
}

type ToolResult struct {
	ToolUseID string
	Text      string
	IsError   bool
}

type Run struct {
	Events      []map[string]any
	Init        map[string]any // the system/init event, if present
	ToolUses    []ToolUse
	ToolResults []ToolResult
	Result      map[string]any // the terminal result event, if present
	Texts       []string       // assistant text blocks, in order
	Unparsed    []string       // lines that were not JSON
}

// Parse reads the NDJSON event stream. It is deliberately tolerant: a run that
// died halfway still yields whatever events arrived, because for several tests
// an incomplete stream is the finding rather than an error.
func Parse(stdout string) *Run {
	r := &Run{}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			r.Unparsed = append(r.Unparsed, line)
			continue
		}
		r.Events = append(r.Events, ev)

		switch ev["type"] {
		case "system":
			if r.Init == nil && ev["subtype"] == "init" {
				r.Init = ev
			}
		case "result":
			r.Result = ev
		}
		r.collectBlocks(ev)
	}
	return r
}

// collectBlocks pulls tool_use, tool_result and text blocks out of an event,
// wherever they sit. The envelope has changed shape across versions, so this
// looks in both the event and a nested message object rather than assuming one.
func (r *Run) collectBlocks(ev map[string]any) {
	for _, container := range []any{ev, ev["message"]} {
		m, ok := container.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			blk, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "tool_use":
				id, _ := blk["id"].(string)
				name, _ := blk["name"].(string)
				input, _ := blk["input"].(map[string]any)
				r.ToolUses = append(r.ToolUses, ToolUse{ID: id, Name: name, Input: input})
			case "tool_result":
				id, _ := blk["tool_use_id"].(string)
				isErr, _ := blk["is_error"].(bool)
				r.ToolResults = append(r.ToolResults, ToolResult{
					ToolUseID: id, Text: flattenContent(blk["content"]), IsError: isErr,
				})
			case "text":
				if t, _ := blk["text"].(string); t != "" {
					r.Texts = append(r.Texts, t)
				}
			}
		}
	}
}

// flattenContent renders a tool_result's content, which may be a string or a
// list of blocks.
func flattenContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, item := range c {
			switch it := item.(type) {
			case string:
				sb.WriteString(it)
			case map[string]any:
				if t, _ := it["text"].(string); t != "" {
					sb.WriteString(t)
				}
			}
			sb.WriteByte('\n')
		}
		return sb.String()
	}
	return ""
}

// TerminatedNormally reports whether the run produced a terminal result event.
// For the fault tests this is the central invariant: whatever the server did,
// the client must finish rather than crash or hang.
func (r *Run) TerminatedNormally() bool { return r.Result != nil }

// FinalText is the text the client ended with: the result event's own text
// when there is one, otherwise the concatenated assistant text.
func (r *Run) FinalText() string {
	if r.Result != nil {
		if s, _ := r.Result["result"].(string); s != "" {
			return s
		}
	}
	return strings.Join(r.Texts, "\n")
}

// AllText is every piece of text the run produced, for nonce searching.
func (r *Run) AllText() string {
	return strings.Join(append(append([]string{}, r.Texts...), r.FinalText()), "\n")
}

// UseMatching returns the first tool use whose name contains every fragment.
// Matching on fragments rather than an exact string keeps the assertion honest
// without hard-coding a namespacing scheme that is not ours to define.
func (r *Run) UseMatching(fragments ...string) (ToolUse, bool) {
	for _, u := range r.ToolUses {
		all := true
		for _, f := range fragments {
			if !strings.Contains(u.Name, f) {
				all = false
				break
			}
		}
		if all {
			return u, true
		}
	}
	return ToolUse{}, false
}

// ResultFor finds the tool_result paired with a tool_use id, which is how the
// client's own id correlation is verified.
func (r *Run) ResultFor(toolUseID string) (ToolResult, bool) {
	for _, res := range r.ToolResults {
		if res.ToolUseID == toolUseID {
			return res, true
		}
	}
	return ToolResult{}, false
}

// ToolNames lists the tool names the client used.
func (r *Run) ToolNames() []string {
	out := make([]string, 0, len(r.ToolUses))
	for _, u := range r.ToolUses {
		out = append(out, u.Name)
	}
	return out
}

// AvailableTools reads the tool list the init event advertises, which shows how
// MCP tools were named and namespaced before any call was made.
func (r *Run) AvailableTools() []string {
	if r.Init == nil {
		return nil
	}
	raw, _ := r.Init["tools"].([]any)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if s, ok := t.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// MCPServerStatuses reads the per-server status the init event reports.
func (r *Run) MCPServerStatuses() map[string]string {
	out := map[string]string{}
	if r.Init == nil {
		return out
	}
	raw, _ := r.Init["mcp_servers"].([]any)
	for _, s := range raw {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		status, _ := m["status"].(string)
		if name != "" {
			out[name] = status
		}
	}
	return out
}

// CostUSD and Turns come from the terminal result event and feed the run
// report, so the suite can state what it spent.
func (r *Run) CostUSD() float64 {
	if r.Result == nil {
		return 0
	}
	c, _ := r.Result["total_cost_usd"].(float64)
	return c
}

func (r *Run) Turns() int {
	if r.Result == nil {
		return 0
	}
	n, _ := r.Result["num_turns"].(float64)
	return int(n)
}

// IsError reports whether the run itself ended in an error state.
func (r *Run) IsError() bool {
	if r.Result == nil {
		return false
	}
	b, _ := r.Result["is_error"].(bool)
	return b
}
