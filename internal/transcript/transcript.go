// Package transcript reads the NDJSON frame log written by cmd/fixture-mcp.
//
// This is witness B of the suite's three witnesses: the record of what actually
// crossed the wire. Tests query it rather than parsing the client's own output,
// so an assertion cannot be satisfied by the client merely claiming something
// happened.
package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

type Frame struct {
	Seq    int             `json:"seq"`
	At     string          `json:"at"`
	Dir    string          `json:"dir"` // "in" client->fixture, "out" fixture->client, "note" out of band
	Method string          `json:"method,omitempty"`
	ID     json.RawMessage `json:"id,omitempty"`
	Raw    string          `json:"raw"`
	Note   string          `json:"note,omitempty"`
	Extra  map[string]any  `json:"extra,omitempty"`
}

type Frames []Frame

// Read parses a transcript. A missing file is not an error: it is the
// meaningful observation that the client never connected, and tests assert on
// an empty result rather than crashing.
func Read(path string) (Frames, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return Frames{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out Frames
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var fr Frame
		if err := json.Unmarshal([]byte(line), &fr); err != nil {
			continue // a torn final line is itself expected under crash faults
		}
		out = append(out, fr)
	}
	return out, sc.Err()
}

// FromClient returns only frames the client sent to the fixture.
func (fs Frames) FromClient() Frames { return fs.dir("in") }

// FromServer returns only frames the fixture sent to the client.
func (fs Frames) FromServer() Frames { return fs.dir("out") }

func (fs Frames) dir(d string) Frames {
	var out Frames
	for _, f := range fs {
		if f.Dir == d {
			out = append(out, f)
		}
	}
	return out
}

// Method filters by JSON-RPC method.
func (fs Frames) Method(m string) Frames {
	var out Frames
	for _, f := range fs {
		if f.Method == m {
			out = append(out, f)
		}
	}
	return out
}

// First returns the first frame with the given method.
func (fs Frames) First(m string) (Frame, bool) {
	for _, f := range fs {
		if f.Method == m {
			return f, true
		}
	}
	return Frame{}, false
}

// Methods lists the methods the client sent, in order, which is what the
// handshake-ordering assertions compare against.
func (fs Frames) Methods() []string {
	var out []string
	for _, f := range fs.FromClient() {
		if f.Method != "" {
			out = append(out, f.Method)
		}
	}
	return out
}

// Notes returns out-of-band records (deliberate exits, hangs, HTTP envelopes).
func (fs Frames) Notes(note string) Frames {
	var out Frames
	for _, f := range fs {
		if f.Dir == "note" && f.Note == note {
			out = append(out, f)
		}
	}
	return out
}

// Has reports whether the client sent this method at all.
func (fs Frames) Has(m string) bool { return len(fs.FromClient().Method(m)) > 0 }

// Parsed decodes the frame body. Returns nil for a malformed frame, which under
// the malformed-output modes is the expected outcome.
func (f Frame) Parsed() map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(f.Raw), &m) != nil {
		return nil
	}
	return m
}

// Params returns the params object of a request frame.
func (f Frame) Params() map[string]any {
	p, _ := f.Parsed()["params"].(map[string]any)
	return p
}

// Dig walks nested maps, e.g. Dig("clientInfo", "name"). It returns nil rather
// than panicking on a missing or wrongly typed step.
func (f Frame) Dig(path ...string) any {
	var cur any = f.Parsed()
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// ToolCall is a decoded tools/call request.
type ToolCall struct {
	Frame Frame
	Name  string
	Args  map[string]any
}

// ToolCalls decodes every tools/call the client made, in order.
func (fs Frames) ToolCalls() []ToolCall {
	var out []ToolCall
	for _, f := range fs.FromClient().Method("tools/call") {
		p := f.Params()
		name, _ := p["name"].(string)
		args, _ := p["arguments"].(map[string]any)
		out = append(out, ToolCall{Frame: f, Name: name, Args: args})
	}
	return out
}
