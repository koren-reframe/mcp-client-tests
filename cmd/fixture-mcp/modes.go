package main

import "time"

// Mode is the fault catalogue. Every field is a lever on otherwise correct
// behaviour, so a new fault is a one-line entry in the registry below rather
// than new code. That is what lets the error tests be a single table-driven Go
// test with one row per fault.
type Mode struct {
	Desc string

	// Process lifecycle.
	ExitBeforeInit *int // exit before reading anything
	ExitAfterInit  *int // exit right after answering initialize
	ExitOnCall     *int // exit while handling tools/call
	HangInitialize bool // read initialize, never answer
	HangTool       bool // read tools/call, never answer
	InitDelay      time.Duration

	// Handshake.
	DiscoverResult     bool   // answer server/discover instead of refusing it
	ProtocolVersion    string // override the version echoed in initialize
	ExtraInitFields    bool   // unknown fields in the initialize result
	NoToolsCapability  bool   // advertise no capabilities at all
	AdvertiseResources bool
	AdvertisePrompts   bool

	// Stream integrity.
	StdoutNoise         bool // plain log lines on stdout
	StdoutJSONNotRPC    bool // valid JSON that is not JSON-RPC
	TruncatedFrame      bool // a half-written frame mid-session
	UnknownID           bool // a response to an id nobody asked for
	UnknownNotification bool // a notification from a hypothetical future spec

	// Tool results.
	ToolIsError    bool // result with isError: true (a tool reporting failure)
	ToolRPCError   int  // JSON-RPC error code instead of a result (a call failing)
	BadResultShape bool // result with no content field
	HugeOutput     bool // ~1 MiB of text
	NonUTF8        bool // invalid UTF-8 bytes in the result

	// Catalogue shape.
	ToolFlood int // advertise this many tools

	// Server -> client requests.
	ProbeRoots       bool
	ProbeSampling    bool
	ProbeElicitation bool

	// HTTP / SSE only.
	HTTPStatusOnInit   int    // reply to the initialize POST with this status
	HTTPAuthChallenge  string // WWW-Authenticate value to pair with a 401
	ExpireSessionAfter int    // 404 the session after this many requests
	SSECloseMid        bool   // close the SSE stream mid-response
	HTTPStall          bool   // send headers, then stall
}

func code(i int) *int { return &i }

// modes is the registry. Names are stable and referenced from test tables and
// from PLAN.md, so renaming one is a breaking change to the suite.
var modes = map[string]Mode{
	// --- the baseline -------------------------------------------------------
	"ok": {Desc: "well-behaved server: refuses server/discover, handshakes, serves tools"},

	// --- Q1 handshake and negotiation --------------------------------------
	"discover_ok":       {Desc: "answer the server/discover pre-probe instead of refusing it", DiscoverResult: true},
	"old_protocol":      {Desc: "negotiate down to an older protocol version", ProtocolVersion: "2025-06-18"},
	"unsupported_proto": {Desc: "answer initialize with a protocol version no client supports", ProtocolVersion: "1999-01-01"},
	"extra_init_fields": {Desc: "unknown fields in the initialize result (forward compatibility)", ExtraInitFields: true},
	"no_capabilities":   {Desc: "advertise no capabilities; client must not ask for tools", NoToolsCapability: true},
	"with_resources":    {Desc: "advertise the resources capability", AdvertiseResources: true},
	"with_prompts":      {Desc: "advertise the prompts capability", AdvertisePrompts: true},
	"slow_init":         {Desc: "delay the initialize response by 3s", InitDelay: 3 * time.Second},

	// --- Q3 process lifecycle ----------------------------------------------
	"exit_before_init": {Desc: "exit 1 before reading anything", ExitBeforeInit: code(1)},
	"exit_zero":        {Desc: "exit 0 immediately, cleanly but uselessly", ExitBeforeInit: code(0)},
	"hang_initialize":  {Desc: "read initialize and never answer (connect timeout)", HangInitialize: true},
	"crash_after_init": {Desc: "answer initialize then exit 1 before tools/list", ExitAfterInit: code(1)},
	"crash_on_call":    {Desc: "exit 1 in the middle of a tools/call", ExitOnCall: code(1)},

	// --- Q3 stream integrity ------------------------------------------------
	"stdout_noise":         {Desc: "plain log lines on stdout before the handshake", StdoutNoise: true},
	"stdout_json_not_rpc":  {Desc: "valid JSON that is not JSON-RPC on stdout", StdoutJSONNotRPC: true},
	"truncated_frame":      {Desc: "a truncated frame mid-session", TruncatedFrame: true},
	"unknown_id":           {Desc: "a response carrying an id nobody requested", UnknownID: true},
	"unknown_notification": {Desc: "a notification method the client cannot know", UnknownNotification: true},

	// --- Q3 tool failures ---------------------------------------------------
	// These two are the pair most often conflated: a tool that *reports*
	// failure, versus a call that *fails at the protocol level*.
	"tool_is_error":    {Desc: "tool result with isError: true (tool reports failure)", ToolIsError: true},
	"rpc_invalid_args": {Desc: "tools/call answered -32602 invalid params", ToolRPCError: -32602},
	"rpc_no_method":    {Desc: "tools/call answered -32601 method not found", ToolRPCError: -32601},
	"rpc_internal":     {Desc: "tools/call answered -32603 internal error", ToolRPCError: -32603},
	"hang_tool":        {Desc: "read tools/call and never answer (tool timeout)", HangTool: true},
	"bad_result_shape": {Desc: "tool result with no content field", BadResultShape: true},
	"huge_output":      {Desc: "tool returns ~1 MiB of text", HugeOutput: true},
	"non_utf8":         {Desc: "tool result containing invalid UTF-8", NonUTF8: true},

	// --- Q2 / Q5 catalogue shape -------------------------------------------
	"tool_flood": {Desc: "advertise 64 tools", ToolFlood: 64},

	// --- Q5 client capability probes ---------------------------------------
	"probe_roots":      {Desc: "ask the client for roots/list (it advertises roots)", ProbeRoots: true},
	"probe_sampling":   {Desc: "ask the client to sample (it does not advertise sampling)", ProbeSampling: true},
	"probe_elicit":     {Desc: "ask the client to elicit (it advertises elicitation)", ProbeElicitation: true},
	"probe_all_client": {Desc: "probe roots, sampling and elicitation together", ProbeRoots: true, ProbeSampling: true, ProbeElicitation: true},

	// --- Q1 / Q3 HTTP and SSE ----------------------------------------------
	"http_500_init":     {Desc: "HTTP 500 on the initialize POST", HTTPStatusOnInit: 500},
	"http_401_bare":     {Desc: "HTTP 401 with no WWW-Authenticate header", HTTPStatusOnInit: 401},
	"http_401_oauth":    {Desc: "HTTP 401 with a Bearer challenge, to trigger OAuth discovery", HTTPStatusOnInit: 401, HTTPAuthChallenge: `Bearer resource_metadata="/.well-known/oauth-protected-resource"`},
	"http_session_gone": {Desc: "404 the Mcp-Session-Id after the handshake", ExpireSessionAfter: 3},
	"sse_close_mid":     {Desc: "close the SSE stream mid-response", SSECloseMid: true},
	"http_stall":        {Desc: "send headers then stall without a body", HTTPStall: true},
}
