package main

import (
	"encoding/json"
	"time"
)

// Wire types. ID is kept as RawMessage because the client uses a string id for
// its server/discover probe and numeric ids for the session proper, and a
// response must echo back exactly what it was sent.
type Req struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Resp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Out is one thing to send. Raw bypasses the JSON encoder so that malformed
// frame faults are expressible.
type Out struct {
	Frame any
	Raw   []byte
	Delay time.Duration
}

// Action is the result of handling one inbound frame.
type Action struct {
	Outs []Out
	Exit *int // exit with this code after writing Outs
	Hang bool // never reply; the client's timeout is what is under test
}

// Session holds per-connection state. One fixture process serves one client.
type Session struct {
	mode     Mode
	name     string
	nonce    string
	probeDir string
	rec      *Recorder

	initialized bool
	calls       int
	reqs        int
}

const (
	// Observed from the pinned client: initialize negotiates 2025-11-25, while
	// the server/discover pre-probe advertises 2026-07-28 under _meta.
	defaultProtocolVersion = "2025-11-25"
)

func (s *Session) result(id json.RawMessage, res any) Out {
	return Out{Frame: Resp{JSONRPC: "2.0", ID: id, Result: res}}
}

func (s *Session) rpcErr(id json.RawMessage, code int, msg string) Out {
	return Out{Frame: Resp{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}}
}

// Handle processes one inbound frame and returns what to do about it.
func (s *Session) Handle(raw []byte) *Action {
	s.rec.Raw("in", raw)
	s.reqs++

	var req Req
	if err := json.Unmarshal(raw, &req); err != nil {
		s.rec.Note("unparsable_inbound", map[string]any{"err": err.Error()})
		return &Action{}
	}

	act := &Action{}
	switch req.Method {

	case "server/discover":
		// Undocumented pre-probe the client sends before initialize. The
		// default is to refuse it, which is what a spec-conformant server does
		// and which the client must tolerate; discover_ok answers it instead so
		// the alternative path is also exercised.
		if s.mode.DiscoverResult {
			act.Outs = append(act.Outs, s.result(req.ID, map[string]any{
				"serverInfo": map[string]any{"name": s.name, "version": "0.0.1"},
			}))
		} else {
			act.Outs = append(act.Outs, s.rpcErr(req.ID, -32601, "server/discover not supported"))
		}

	case "initialize":
		if s.mode.HangInitialize {
			act.Hang = true
			return act
		}
		pv := defaultProtocolVersion
		if s.mode.ProtocolVersion != "" {
			pv = s.mode.ProtocolVersion
		} else {
			// Echo whatever the client proposed, which is what a cooperative
			// server does.
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
				pv = p.ProtocolVersion
			}
		}
		res := map[string]any{
			"protocolVersion": pv,
			"capabilities":    s.capabilities(),
			"serverInfo":      map[string]any{"name": s.name, "version": "0.0.1"},
		}
		if s.mode.ExtraInitFields {
			// Forward compatibility: unknown fields must be ignored, not fatal.
			res["experimentalFutureField"] = map[string]any{"nested": []int{1, 2, 3}}
			res["_meta"] = map[string]any{"fixture/unknown": true}
		}
		out := s.result(req.ID, res)
		out.Delay = s.mode.InitDelay
		act.Outs = append(act.Outs, out)
		if s.mode.ExitAfterInit != nil {
			act.Exit = s.mode.ExitAfterInit
		}

	case "notifications/initialized":
		s.initialized = true
		act.Outs = append(act.Outs, s.postInitProbes()...)

	case "tools/list":
		act.Outs = append(act.Outs, s.result(req.ID, map[string]any{"tools": s.tools()}))

	case "tools/call":
		s.calls++
		act = s.handleToolCall(req)

	case "resources/list":
		act.Outs = append(act.Outs, s.result(req.ID, map[string]any{"resources": []any{}}))

	case "prompts/list":
		act.Outs = append(act.Outs, s.result(req.ID, map[string]any{"prompts": []any{}}))

	case "ping":
		act.Outs = append(act.Outs, s.result(req.ID, map[string]any{}))

	default:
		// Responses to our own server->client requests (roots/list etc.) arrive
		// with an id and no method; record them and say nothing.
		if req.Method == "" {
			s.rec.Note("client_response", map[string]any{"raw": string(raw)})
			return act
		}
		if req.ID != nil {
			act.Outs = append(act.Outs, s.rpcErr(req.ID, -32601, "method not found: "+req.Method))
		}
	}

	// Stream-level faults are appended after the legitimate reply so the client
	// meets them mid-session, with a working session already established.
	act.Outs = append(act.Outs, s.streamFaults(req)...)
	return act
}

func (s *Session) capabilities() map[string]any {
	if s.mode.NoToolsCapability {
		// Advertise nothing: the client must then not ask for tools, resources
		// or prompts.
		return map[string]any{}
	}
	caps := map[string]any{"tools": map[string]any{"listChanged": true}}
	if s.mode.AdvertiseResources {
		caps["resources"] = map[string]any{"subscribe": false, "listChanged": false}
	}
	if s.mode.AdvertisePrompts {
		caps["prompts"] = map[string]any{"listChanged": false}
	}
	return caps
}

// postInitProbes lets the fixture turn around and make requests of the client.
// The client advertises roots and elicitation but not sampling, so these probes
// are how the suite checks those claims are true.
func (s *Session) postInitProbes() []Out {
	var outs []Out
	if s.mode.ProbeRoots {
		outs = append(outs, Out{Frame: Req{JSONRPC: "2.0", ID: json.RawMessage(`"probe-roots"`), Method: "roots/list"}})
	}
	if s.mode.ProbeSampling {
		outs = append(outs, Out{Frame: map[string]any{
			"jsonrpc": "2.0", "id": "probe-sampling", "method": "sampling/createMessage",
			"params": map[string]any{
				"messages":  []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "ping"}}},
				"maxTokens": 16,
			},
		}})
	}
	if s.mode.ProbeElicitation {
		outs = append(outs, Out{Frame: map[string]any{
			"jsonrpc": "2.0", "id": "probe-elicit", "method": "elicitation/create",
			"params": map[string]any{"message": "fixture asks", "requestedSchema": map[string]any{
				"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}},
			}},
		}})
	}
	return outs
}

// streamFaults injects protocol-level garbage once the session is live.
func (s *Session) streamFaults(req Req) []Out {
	var outs []Out
	if req.Method != "tools/list" { // fire once, after the handshake settles
		return outs
	}
	if s.mode.TruncatedFrame {
		outs = append(outs, Out{Raw: []byte(`{"jsonrpc":"2.0","id":99,"result":{"content":[{"type":"te`)})
	}
	if s.mode.UnknownID {
		outs = append(outs, Out{Frame: Resp{JSONRPC: "2.0", ID: json.RawMessage(`4242`), Result: map[string]any{"unsolicited": true}}})
	}
	if s.mode.UnknownNotification {
		outs = append(outs, Out{Frame: Notification{JSONRPC: "2.0", Method: "notifications/fixture/somethingNew",
			Params: map[string]any{"detail": "a notification from a future spec version"}}})
	}
	return outs
}
