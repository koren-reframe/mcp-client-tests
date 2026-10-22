# PLAN — black-box test suite for the MCP client in Claude Code

System under test: **the MCP client embedded in Claude Code 2.1.296**, exercised
only through its public surfaces (CLI flags, config files, environment, and the
wire protocol it speaks to MCP servers). No source access, no internal hooks.

Status legend: `impl` implemented in this suite · `plan` specified here, not
implemented · `blocked` cannot be reached black-box/headlessly (reason given).
Tier legend: `T0` no model, no API key · `T1` one model turn (cheap, pinned
model) · `T2` multi-turn or high-volume · `—` harness property, not a test case.

Plan IDs are carried in Go test names (`TestTOOL01_NonceRoundTrip`), so every
implemented row is greppable from this file to the code and back.

---

## 0. Scope: what is and is not under test

**Under test.** How the client discovers and starts servers; the frames it puts
on the wire and their shape and ordering; how it maps MCP tools into its own
tool namespace; how it forwards arguments and returns results into model
context; how it behaves when a server, a transport or a tool call fails; its
process and resource lifecycle; and whether its failures stay contained.

**Not under test.** The quality of the model's answers, whether the model
*chooses* to call a tool for a loosely-worded prompt, prose wording, the MCP
servers of third parties, and the Anthropic API itself. Where a test must pass
through the model to reach the client, the model is treated as an unreliable
transport (§2), never as the thing being asserted on.

**The central problem.** A naive suite prompts Claude and greps its prose. That
tests the model, not the client, and fails on rephrasing. This suite instead
makes the **MCP server the measuring instrument**: a fixture server we own that
records every JSON-RPC frame it receives, and that can be told to misbehave in
a specific way. Assertions run against that recording.

---

## 0.1 Recon: the surface the pinned binary actually exposes

Step zero was not writing tests, it was interrogating `claude --help`,
`claude mcp --help` and each subcommand inside the container at the pinned
version, because black-box means the public surface is the specification. What
that turned up, and what it changed:

- **`claude mcp list` and `claude mcp get` health-check configured servers.**
  They connect, handshake and report status. This is the single most useful
  finding: connection, handshake, negotiation, transport faults and timeouts can
  all be driven **without the model and without spending a token**, which moves
  most of Q1 and Q3 into tier T0. Probed and confirmed before relying on it.
- **`claude mcp login <name>` / `logout <name>`** exist as discrete commands,
  and **`claude mcp add --callback-port <port>`** pins the OAuth redirect port.
  Together these move full OAuth from "blocked, needs a browser" to a plausible
  headless test against a fixture authorization server (`CONN-34`).
- **`claude mcp reset-project-choices`** resets approved/rejected `.mcp.json`
  servers per project — a supported lever for the isolation the suite needs.
- **`add-json` documents a WebSocket transport that `--transport` does not
  offer** (`stdio, sse, http`). A documented-surface discrepancy is exactly what
  a black-box suite should pin down rather than assume (`CONN-43`).
- **`--debug [filter]` plus `--debug-file <path>`** give categorised client logs
  written straight to a file, which is a better failure artifact than scraping
  stderr, and makes `RDY-13` directly testable.
- **`--restricted`** removes the built-in tools. That is a determinism lever: if
  the MCP tool is the only tool in the session, the model has far less room to
  wander, which tightens the T1 tier (`DET-16`).
- `--allowedTools` / `--disallowedTools`, `--permission-mode`, `-e`, `-H`,
  `-s local|user|project` (default `local`), `--client-id`, `--client-secret`
  (or `MCP_CLIENT_SECRET`) are all confirmed present and are what the tests
  drive.
- **Out of scope, explicitly:** `claude mcp serve` makes Claude Code act *as* an
  MCP server. That is the server half of the protocol, not the client under
  test. `add-from-claude-desktop` is Mac/WSL-only import and not a client
  behaviour.

### 0.2 Observed baseline handshake (probed, not assumed)

Driving `claude mcp list` against a throwaway recording server, with
`ANTHROPIC_API_KEY` unset, produced this exchange. It is the baseline every
Q1 assertion is written against:

```
→ server/discover        id "server-discover-probe-1", params._meta carries
                         io.modelcontextprotocol/protocolVersion  2026-07-28
                         io.modelcontextprotocol/clientInfo       claude-code 2.1.296
                         io.modelcontextprotocol/clientCapabilities
← error -32601           (our fixture refuses it)
→ initialize             id 0, protocolVersion 2025-11-25
                         clientInfo {name: claude-code, title: Claude Code,
                                     version: 2.1.296, description, websiteUrl}
                         capabilities {roots:{listChanged:true},
                                       elicitation:{form:{},url:{}}}
← result                 protocolVersion 2025-11-25, capabilities {tools:{}}
→ notifications/initialized
→ tools/list             id 1
← result                 one tool
   ⇒ `fix: node /tmp/fix.mjs - ✔ Connected`, exit 0
```

Five things this settles that the documentation did not:

1. **The T0 tier is real.** A complete handshake happens with no API key and no
   model. Connection, negotiation, transport faults and timeouts are therefore
   free to test, on every commit, on forks.
2. **There is an undocumented `server/discover` pre-probe** carrying a *newer*
   protocol version (`2026-07-28`) than the `initialize` it precedes
   (`2025-11-25`), under `io.modelcontextprotocol/*` `_meta` keys. A server that
   rejects it with `-32601` is fine — the client falls back cleanly. This is the
   kind of behaviour only black-box observation finds, and it gets pinned
   (`CONN-48`) so a regression in the fallback is caught.
3. **The client advertises `roots` and `elicitation`, and does not advertise
   `sampling`.** That converts three `characterize` rows into concrete
   assertions: roots and elicitation must actually work (`RDY-17`, `RDY-18`),
   and a server must not expect sampling (`RDY-16`).
4. **Request ids are numeric and monotonic from 0** in the session proper, while
   the discover probe uses a string id — so id handling must tolerate both.
5. **`claude mcp list` exits 0 even when a server fails to connect.** The broken
   server is reported in the output (`✘ Failed to connect — ENOENT…`) but the
   exit code is still 0. Any CI health gate built on the exit code would be
   silently blind. Pinned as a finding in `ERR-31`/`RDY-12`, and called out in
   the README as the one behaviour I would push back on as a product question.

Two consequences for the instrument: it must answer or explicitly reject
`server/discover`, and because `claude` ships as a single ~256 MB native binary
(no JavaScript bundle to read), **timeout and limit env vars cannot be
discovered by inspection and must be established empirically** — set the
variable, measure the bound. The shipped `sdk-tools.d.ts` does name the
client-side MCP tool surfaces (`ListMcpResources`, `ReadMcpResource`,
`ReadMcpResourceDir`, `RefreshMcpTools`), which gives `CONN-17` and `TOOL-26`
concrete targets instead of guesses.

---

## 1. Architecture

```
  ┌─────────────────────── docker image (one command: ./run.sh) ──────────────────────┐
  │                                                                                   │
  │   go test ./tests/...                                                             │
  │        │                                                                          │
  │        │ 1. build hermetic env: fresh HOME, fresh cwd, config, transcript path     │
  │        │ 2. spawn:  claude -p "<prompt>" --output-format stream-json               │
  │        ▼                       --mcp-config <fixture> --strict-mcp-config          │
  │   ┌──────────────────┐   stdio / HTTP / SSE   ┌──────────────────────────────┐     │
  │   │  Claude Code     │◀──── JSON-RPC 2.0 ────▶│  cmd/fixture-mcp  (the       │     │
  │   │  2.1.296  (SUT)  │                        │  instrument)                 │     │
  │   │  MCP client      │                        │   -transport stdio|http|sse  │     │
  │   └────────┬─────────┘                        │   -mode  <fault catalogue>   │     │
  │            │ NDJSON on stdout                 │   records every frame ──┐    │     │
  │            ▼                                  └─────────────────────────┼────┘     │
  │   witness A: client-side event stream              witness B: server transcript    │
  │            │                                                           │           │
  │            └──────────────┬────────────────────────────────────────────┘           │
  │                           ▼                                                        │
  │                  witness C: side effects (nonce in final text, file written)       │
  │                           │                                                        │
  │                           ▼   agreement of A+B+C == the tool really ran             │
  │                  artifacts/<test>/  (dumped only on failure)                        │
  └───────────────────────────────────────────────────────────────────────────────────┘
```

### 1.1 The instrument — `cmd/fixture-mcp`

One static Go binary, no dependencies, three jobs:

- **Speaks MCP** over `stdio`, `streamable HTTP` or legacy `SSE`, selected by flag.
- **Records** every received frame, plus every frame it sends, as NDJSON to
  `$FIXTURE_TRANSCRIPT` with a monotonic sequence number and timestamp. This is
  witness B and the whole basis of the suite.
- **Misbehaves on demand** via `-mode`: a fault is a config change, not new
  code. Modes cover the catalogue in §Q3 (hang on initialize, exit non-zero,
  pollute stdout with log lines, emit a truncated frame, answer with a bad
  protocol version, die mid-call, return `isError`, return a JSON-RPC error
  code, stall, oversize, non-UTF8, …).

Deliberately **hand-rolled JSON-RPC rather than an SDK**: a conformant SDK will
not let us emit a malformed frame, and malformed frames are half of Q3.

Tool catalogue it exposes: `echo_nonce` (returns a per-run nonce), `write_probe`
(side-effect witness), `typed_args` (string/int/bool/enum/array/nested object,
to check JSON type fidelity), `crash_now`, `hang_forever`, `huge_output`,
`unicode_payload`, `bad_result_shape`, plus a `tool_flood` mode advertising 60+
tools.

### 1.2 Harness — `internal/`

- `claudecli` — spawns the CLI under a context deadline, parses the
  `stream-json` NDJSON event stream into typed events (`tool_use`, `tool_result`,
  `result` with usage/cost), captures stdout/stderr separately. Witness A.
- `transcript` — reads and queries witness B: frames by method, call arguments,
  ordering, id correlation, timing.
- `witness` — the three-witness assertions, so each test reads as one statement
  of intent rather than twenty lines of JSON poking.
- `harness` — per-test hermetic environment (fresh `HOME`, fresh cwd, generated
  MCP config, unique transcript path, ephemeral port), and failure-time artifact
  dump.
- `mcpspec` — frame types and protocol-version constants.

### 1.3 Hermeticity (this is load-bearing, not hygiene)

Claude Code persists MCP configuration in the user's home directory keyed by
project path, and project-scoped `.mcp.json` servers are gated behind a trust
prompt. Without isolation, tests pass or fail by ordering and hang waiting for
approval in a non-interactive run. Therefore every test gets a fresh `HOME` and
cwd via `t.TempDir()`, passes its server with `--mcp-config`, and adds
`--strict-mcp-config` so ambient config cannot bleed in. `DET-12` proves this
by running the suite twice inside one container.

### 1.4 One command

`./run.sh` → `docker build` then `docker run --rm -e ANTHROPIC_API_KEY`. The
image carries Go, Node, Claude Code pinned at `2.1.296` with the auto-updater
disabled (2.1.308 is already published upstream; an unpinned image silently
stops testing the stated SUT), and the compiled fixture binary. No host setup
beyond Docker, git and the key.

---

## 2. Determinism policy (the answer to Q4, applied to every row below)

The client is deterministic; the model in front of it is not. The suite is
therefore layered so that **most coverage never touches the model**:

| Tier | Needs | What it asserts | Run when |
|---|---|---|---|
| T0 | nothing (no API key) | config handling, handshake frames and ordering, protocol negotiation, tool-name mangling, schema pass-through, transport faults, connect/read timeouts, process lifecycle — driven through the `claude mcp list`/`get` health check, which connects without the model | every commit, incl. forks |
| T1 | key, pinned cheap model, 1 turn | that a real tool call crosses the client correctly and the result re-enters context | every PR |
| T2 | key, many turns | volume, latency, leak-over-time | nightly |

Rules the suite enforces on itself:

- **No assertion anywhere depends on assistant prose** (`DET-10`). The nonce is
  the only text ever matched, and it is random per run.
- Model-tier argument checks are **invariant-based**: required keys present with
  correct values and JSON types; additional keys are recorded and characterized,
  not failed on.
- Model-tier tests are **forced as hard as the public surface allows** — single
  allowed tool, imperative prompt, pinned model — and then reported as a
  **pass-rate over `-count=N`**, not a binary. A test that is 92% green is
  reported as 92% green; silently retrying until green hides client bugs.
- Timeouts are asserted as **bounds** (`fails within N s`), never exact values,
  so a slow CI runner does not fabricate failures.
- Every run emits a report with tier, pass-rates, pinned versions and **token
  spend**, taken from the `result` event (the brief caps spend at \$50).

The cheap deterministic lever not yet pulled: point `ANTHROPIC_BASE_URL` at a
local stub that returns a canned `tool_use` block, making the entire tool-call
path T0 — free and fully deterministic. Feasibility and risk in `DET-15`.

---

## 3. Test inventory

### Q1 — Connection, startup and authentication

| ID | Test | Tier | Status |
|---|---|---|---|
| CONN-01 | stdio server from inline `--mcp-config` JSON reaches `initialized` | T0 | impl |
| CONN-02 | stdio server from `--mcp-config` file path connects | T0 | impl |
| CONN-03 | `claude mcp add --transport stdio` persists config; `mcp list` shows it | T0 | impl |
| CONN-04 | `claude mcp get <name>` reports command, args, env, transport | T0 | impl |
| CONN-05 | `claude mcp remove` deletes it; `mcp list` no longer shows it | T0 | impl |
| CONN-06 | scope precedence: same server name in local vs project vs user | T0 | plan |
| CONN-07 | project-scoped `.mcp.json` is discovered from cwd | T0 | plan |
| CONN-08 | `.mcp.json` trust gate blocks first use non-interactively (characterize) | T0 | impl |
| CONN-09 | `--strict-mcp-config` ignores ambient user/project config | T0 | impl |
| CONN-10 | `initialize` carries protocolVersion, clientInfo.name/version, capabilities | T0 | impl |
| CONN-11 | client sends `notifications/initialized` after the initialize result | T0 | impl |
| CONN-12 | client calls `tools/list` after init and before any `tools/call` | T0 | impl |
| CONN-13 | negotiation: server answers with an older supported version → proceeds | T0 | impl |
| CONN-14 | negotiation: server answers with an unsupported version → clean fail, no hang | T0 | impl |
| CONN-15 | no `resources/list`/`prompts/list` when server advertises no such capability | T0 | impl |
| CONN-16 | advertised prompts appear as `/mcp__<server>__<prompt>` | T0 | plan |
| CONN-17 | advertised resources are `@`-mentionable | T1 | plan |
| CONN-18 | tools are namespaced `mcp__<server>__<tool>` in the client tool list | T0 | impl |
| CONN-19 | `${VAR}` in config is expanded before spawn | T0 | impl |
| CONN-20 | `${VAR:-default}` falls back when unset | T0 | plan |
| CONN-21 | unresolvable `${VAR}` → actionable error, no spawn | T0 | plan |
| CONN-22 | configured working directory is applied to the spawned server | T0 | plan |
| CONN-23 | streamable HTTP: `Mcp-Session-Id` captured and echoed on later requests | T0 | impl |
| CONN-24 | HTTP: `Accept` includes `application/json` and `text/event-stream` | T0 | impl |
| CONN-25 | legacy SSE transport connects and completes a tool call | T1 | plan |
| CONN-26 | static auth header is sent on `initialize` **and** every later request | T0 | impl |
| CONN-27 | multiple header flags all applied | T0 | plan |
| CONN-28 | `${VAR}` expansion inside a header value | T0 | plan |
| CONN-29 | bearer token is **not** replayed across a cross-origin redirect | T0 | impl |
| CONN-30 | `401` + `WWW-Authenticate` triggers protected-resource metadata discovery | T0 | impl |
| CONN-31 | discovery continues to the authorization-server metadata document | T0 | impl |
| CONN-32 | dynamic client registration request shape (RFC 7591) | T0 | plan |
| CONN-33 | authorization URL carries PKCE `code_challenge`, `S256`, and `state` | T0 | plan |
| CONN-34 | full OAuth code exchange end to end against a fixture authorization server | T0 | plan: looked blocked until recon found `--callback-port` + `claude mcp login`; the consent redirect can be driven directly at a pinned port. Attempt after the depth areas; README states the outcome either way |
| CONN-35 | header/token values absent from stdout, stderr and artifacts | T0 | impl |
| CONN-36 | server process is spawned exactly once per session | T0 | impl |
| CONN-37 | two servers both connect; both tool sets are exposed | T0 | impl |
| CONN-38 | connect timeout fires within bound when server never answers initialize | T0 | impl |
| CONN-39 | version guard: `claude --version` == 2.1.296 (pin did not drift) | T0 | impl |
| CONN-40 | `claude mcp list` health check connects and reports status with no API key | T0 | impl |
| CONN-41 | `claude mcp login <name>` emits the discovery sequence for an HTTP server | T0 | plan |
| CONN-42 | `claude mcp logout <name>` clears stored credentials; next connect sends no token | T0 | plan |
| CONN-43 | WebSocket transport via `add-json` (offered there, absent from `--transport`) | T0 | plan |
| CONN-44 | `reset-project-choices` clears `.mcp.json` approval state | T0 | plan |
| CONN-45 | `--client-id` / `MCP_CLIENT_SECRET` are applied to the OAuth client | T0 | plan |
| CONN-46 | `--mcp-config` is variadic: several config sources merge, later wins on conflict | T0 | impl |
| CONN-47 | `mcp get` on an unapproved `.mcp.json` server reports pending, does not connect | T0 | impl |
| CONN-48 | undocumented `server/discover` pre-probe is sent, and `-32601` is tolerated | T0 | impl |
| CONN-49 | advertised capabilities are exactly `roots.listChanged` + `elicitation.{form,url}` | T0 | impl |
| CONN-50 | session request ids are numeric and monotonic from 0; probe id may be a string | T0 | impl |
| CONN-51 | frame order: discover → initialize → initialized → tools/list | T0 | impl |

### Q2 — Tool calls *(depth area)*

| ID | Test | Tier | Status |
|---|---|---|---|
| TOOL-01 | nonce round trip: run-unique nonce from the tool appears in final output | T1 | impl |
| TOOL-02 | transcript contains exactly one `tools/call` for the target tool | T1 | impl |
| TOOL-03 | `params.name` is the server-side (unmangled) tool name | T1 | impl |
| TOOL-04 | `params.arguments` carries required args with exact expected values | T1 | impl |
| TOOL-05 | client event stream contains a `tool_use` block named `mcp__<server>__<tool>` | T1 | impl |
| TOOL-06 | that `tool_use` id correlates with a returned `tool_result` block | T1 | impl |
| TOOL-07 | three-witness cross-check: client event, server frame and side effect agree | T1 | impl |
| TOOL-08 | side-effect witness: tool writes a file; content matches | T1 | impl |
| TOOL-09 | JSON-RPC request ids are unique within a session | T0 | impl |
| TOOL-10 | text result content is delivered into model context verbatim | T1 | impl |
| TOOL-11 | typed args survive: string, int, bool, enum, array, nested object | T1 | impl |
| TOOL-12 | omitted optional args do not arrive as `null` | T1 | impl |
| TOOL-13 | characterize: are extra/hallucinated args forwarded or stripped | T1 | impl |
| TOOL-14 | two sequential calls in one session are both recorded, in order | T1 | impl |
| TOOL-15 | tools on two different servers route to the correct server | T1 | impl |
| TOOL-16 | identical tool name on two servers resolves unambiguously | T0 | plan |
| TOOL-17 | a tool outside the allow-list is never called | T1 | impl |
| TOOL-18 | structured/JSON result content returned intact | T1 | plan |
| TOOL-19 | image content block in a result is handled | T1 | plan |
| TOOL-20 | embedded-resource content block is handled | T1 | plan |
| TOOL-21 | unicode, emoji and CJK survive args and results unchanged | T1 | impl |
| TOOL-22 | oversized result (≥1 MB) truncation behaviour characterized | T1 | impl |
| TOOL-23 | ANSI/control characters in a result are not interpreted as terminal control | T0 | plan |
| TOOL-24 | 60+ advertised tools are all exposed, none silently dropped | T0 | plan |
| TOOL-25 | multi-kilobyte tool description passes through | T0 | plan |
| TOOL-26 | `tools/list_changed` refreshes the list (client exposes a `RefreshMcpTools` path) | T1 | plan |
| TOOL-27 | parallel tool calls, if the client issues them, are recorded correctly | T1 | plan |
| TOOL-28 | per-call client overhead measured and bounded | T2 | plan |

### Q3 — Errors *(depth area)*

| ID | Test | Tier | Status |
|---|---|---|---|
| ERR-01 | server binary does not exist → clear error, CLI continues, no hang | T0 | impl |
| ERR-02 | server exits non-zero before `initialize` | T0 | impl |
| ERR-03 | server exits 0 immediately | T0 | impl |
| ERR-04 | server never answers `initialize` → connect timeout within bound | T0 | impl |
| ERR-05 | server writes plain log lines to stdout (stdout pollution) | T0 | impl |
| ERR-06 | server writes valid JSON that is not JSON-RPC | T0 | impl |
| ERR-07 | truncated/malformed frame mid-session | T0 | impl |
| ERR-08 | response carrying an unknown id | T0 | plan |
| ERR-09 | unknown notification → ignored, session survives | T0 | impl |
| ERR-10 | unknown fields in `initialize` result → ignored (forward compatibility) | T0 | impl |
| ERR-11 | server crashes after initialize, before `tools/list` | T0 | impl |
| ERR-12 | server crashes mid tool call → error surfaced, session survives | T1 | impl |
| ERR-13 | tool result with `isError: true` → reaches model as a tool error | T1 | impl |
| ERR-14 | `tools/call` answered with JSON-RPC `-32602` invalid params | T1 | impl |
| ERR-15 | `tools/call` answered with `-32601` method not found | T0 | impl |
| ERR-16 | `tools/call` answered with `-32603` internal error | T1 | impl |
| ERR-17 | tool hangs → tool timeout fires, session survives | T1 | impl |
| ERR-18 | result missing `content` / wrong shape → handled cleanly | T1 | impl |
| ERR-19 | non-UTF8 bytes in a result | T0 | plan |
| ERR-20 | HTTP: connection refused, nothing listening | T0 | impl |
| ERR-21 | HTTP: `500` on initialize | T0 | impl |
| ERR-22 | HTTP: `401` with no `WWW-Authenticate` | T0 | impl |
| ERR-23 | HTTP: `404` on an expired session id → client re-initializes | T0 | impl |
| ERR-24 | HTTP: SSE stream closed mid-response | T0 | impl |
| ERR-25 | HTTP: TLS failure / untrusted certificate | T0 | plan |
| ERR-26 | HTTP: response stalls after headers → read timeout | T0 | plan |
| ERR-27 | **isolation**: one broken + one healthy server → healthy tools still work | T1 | impl |
| ERR-28 | no orphan or zombie server processes after exit, on success and failure | T0 | impl |
| ERR-29 | interrupt during an in-flight call terminates child servers | T0 | plan |
| ERR-30 | error text does not leak env values or secrets | T0 | impl |
| ERR-31 | `mcp list` exits 0 despite a failed server — pinned as observed, flagged in README | T0 | impl |
| ERR-32 | malformed `.mcp.json` → actionable parse error, no crash | T0 | impl |

### Q4 — Determinism (suite properties, see §2)

| ID | Test | Tier | Status |
|---|---|---|---|
| DET-01 | the whole T0 tier passes with no API key present | — | impl |
| DET-02 | protocol assertions stable across `-count=10` | T0 | impl |
| DET-03 | `clientInfo` fields identical across runs | T0 | impl |
| DET-04 | tool-name mangling identical across runs | T0 | impl |
| DET-05 | error classification identical across runs per injected fault | T0 | impl |
| DET-06 | model-tier results reported as pass-rate over N, not binary | — | impl |
| DET-07 | flake budget: a model-tier test under threshold is reported flaky, not green | — | impl |
| DET-08 | model pinned by flag and recorded in the run report | — | impl |
| DET-09 | model-tier argument checks are invariant-based, not exact-equality | — | impl |
| DET-10 | no assertion depends on assistant prose (nonce is the only matched text) | — | impl |
| DET-11 | run report records token spend and cost from the `result` event | — | impl |
| DET-12 | hermeticity proven by running the suite twice in one container | — | impl |
| DET-13 | order independence under `-shuffle=on` | — | impl |
| DET-14 | timeout assertions are bounds, never exact durations | — | impl |
| DET-15 | stub-model driver via `ANTHROPIC_BASE_URL` to make Q2 fully T0 | — | plan: highest-value next step; needs a faithful streaming Messages API stub |
| DET-16 | `--restricted` + single allowed tool as a non-determinism reducer for T1 | — | impl |

### Q5 — What else must be true before calling the client working

| ID | Test | Tier | Status |
|---|---|---|---|
| RDY-01 | conformance matrix artifact: observed behaviour vs spec MUST/SHOULD | — | impl |
| RDY-02 | permission gate cannot be bypassed by the server | T1 | impl |
| RDY-03 | injected instructions in a tool **description** are not followed | T1 | plan |
| RDY-04 | injected instructions in a tool **result** are not followed | T1 | plan |
| RDY-05 | shell metacharacters in a server-supplied tool name cause no injection | T0 | plan |
| RDY-06 | client still mediates/approves when a tool arg contains a traversal path | T1 | plan |
| RDY-07 | secrets never land in session transcripts or debug logs | T0 | impl |
| RDY-08 | fd and process counts stable over 50 sequential connect/teardown cycles | T0 | impl |
| RDY-09 | 100 rapid calls complete without degradation | T2 | plan |
| RDY-10 | servers are restarted per session, not leaked across sessions | T0 | plan |
| RDY-11 | status surface reports connected vs failed accurately | T0 | impl |
| RDY-12 | failure is discoverable at all from `mcp list` (output, since not the exit code) | T0 | impl |
| RDY-13 | `--debug-file` output is sufficient to diagnose a failed handshake | T0 | impl |
| RDY-14 | backwards compatibility: older-protocol server remains usable | T0 | impl |
| RDY-15 | forwards compatibility: unknown capability keys ignored | T0 | impl |
| RDY-16 | sampling is **not** advertised; a server requesting it gets a clean refusal | T0 | impl |
| RDY-17 | `roots` is advertised, so `roots/list` from the server must be answered | T0 | impl |
| RDY-18 | `elicitation` is advertised; form and url elicitation behaviour non-interactively | T0 | plan |
| RDY-19 | progress notifications during a long call — characterize | T1 | plan |
| RDY-20 | cancellation on interrupt — characterize | T1 | plan |
| RDY-21 | logging notifications — characterize | T0 | plan |
| RDY-22 | version matrix: green on the pin, diffs reported on newer versions | — | plan |
| RDY-23 | image builds and suite passes on arm64 and amd64 | — | plan |
| RDY-24 | one command from a clean clone with only Docker, git and the key | — | impl |

---

## 4. What is implemented, and what is not

**Depth areas: Q2 and Q3.** Both are fully observable from the instrument, need
no interactive flow, and share one fixture, so the hours buy the most evidence.
Q2 gets the three-witness proof and argument-fidelity matrix; Q3 gets a
parametrized fault catalogue including the two cases that are most often
conflated in MCP clients — a tool that *reports* failure (`isError: true`) and a
tool call that *fails at the protocol level* (a JSON-RPC error object).

**Breadth areas.** Q1 is implemented for stdio and streamable HTTP including
static-header auth and the OAuth *discovery* sequence; legacy SSE and the
interactive half of OAuth are specified, not implemented. Q4 is implemented as
properties of the harness rather than as test cases. Q5 ships the conformance
matrix plus spot tests for the claims that would most embarrass a release —
isolation, process leaks, permission gating, secret hygiene.

**Row counts.** 151 items specified, 105 implemented. That ratio is only
achievable because of two architectural choices, and it is worth being explicit
about why it is not over-promising:

1. **One session settles many rows.** A single stdio happy path yields
   CONN-10/11/12/18/36 and TOOL-01…07 simultaneously — they are independent
   assertions over one recorded transcript, not independent runs.
2. **A fault is a table row, not a new test.** Because the instrument takes
   `-mode`, Q3's 27 implemented rows are substantially *one* Go table-driven
   test with 27 entries. Adding the 28th fault is three lines.

In test-function terms the implemented set is roughly 30 functions. Grouping is
by session cost, not by inventory row. The status column is reconciled against
the finished suite before submission — anything that did not actually land gets
demoted to `plan` rather than left optimistic.

**Deliberately excluded.** Third-party MCP servers (not the SUT, and they make
runs network-dependent); interactive TUI behaviour (no stable headless surface);
load testing beyond the leak checks; anything asserting model answer quality.

## 5. Known limits

- `CONN-34` full OAuth may yet be reachable headlessly via `--callback-port`
  and a fixture authorization server; if it is not, coverage stops at the
  redirect boundary and the README says so plainly.
- Model-tier tests remain probabilistic until `DET-15` lands; they are reported
  as rates, with the deterministic tier carrying the coverage weight.
- Behaviour is pinned to 2.1.296. Newer versions are already published, so the
  suite asserts the pin (`CONN-39`) rather than trusting the image to stay put.
- Characterization rows (`RDY-16`…`RDY-21`, `TOOL-13`, `TOOL-22`) record what the
  client *does*, since the spec leaves these optional. They are documentation
  backed by an executable check, and they are the rows most likely to need
  re-baselining on a version bump.
