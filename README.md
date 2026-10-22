# Testing the MCP client in Claude Code

A black-box automated test suite for the MCP client inside **Claude Code
2.1.296**, which runs in Docker as the system under test. No source access, no
internal hooks: the client is driven through its public surfaces — CLI flags,
config files, environment variables — and observed through the wire protocol it
speaks to MCP servers.

**What is under test:** how the client discovers, starts and authenticates to
servers; the frames it puts on the wire and their shape and ordering; how it
maps MCP tools into its own namespace; how it forwards arguments and returns
results into model context; how it behaves when a server, a transport or a tool
call fails; and whether its failures stay contained.

**What is not:** the quality of the model's answers. Nothing here asserts on
prose.

---

## Run it

Requires only Docker and git. An API key is optional.

```bash
./run.sh              # T0: 37 cases, no API key, ~31s
```

That is the whole setup. The image is built on first run (Go, Node, Claude Code
pinned at 2.1.296, the fixture server), and `./run.sh` with no arguments runs
every test that does not need a model.

```bash
export ANTHROPIC_API_KEY=sk-...
./run.sh t1           # model tier: 17 cases, ~$0.02 (measured $0.0013/session)
./run.sh all          # both tiers
./run.sh smoke        # version pin + baseline handshake, verbose
./run.sh modes        # list the 38 fault injection modes
SUITE_SLOW=1 ./run.sh t1    # include the slow characterization tests
FLAKE_RUNS=5 ./run.sh flake # repeat the model tier to measure flakiness
```

Failure artifacts — the fixture's frame transcript, every CLI invocation with
its stdout, stderr, exit code and duration, and the test's environment — land
in `./artifacts/<TestName>/`. Only failures produce them, so a green run stays
quiet.

---

## How it works

### The server is the measuring instrument

The naive way to test this is to prompt Claude and grep its answer. That tests
the model, not the client, and breaks the moment the model rephrases itself.

Instead, `cmd/fixture-mcp` is an MCP server this suite owns. It records every
JSON-RPC frame it exchanges as NDJSON, and it misbehaves on demand through a
`-mode` flag covering 38 named faults. Assertions run against that recording.

It is deliberately hand-rolled rather than built on an MCP SDK: a conformant SDK
will not let you emit a malformed frame, and malformed frames are half of what
the error tests need.

```
  ┌──────────────── docker image · one command ────────────────────────────┐
  │  go test                                                               │
  │    │ fresh HOME + cwd + transcript + nonce per test                    │
  │    ▼                                                                   │
  │  claude ──── stdio / streamable HTTP / legacy SSE ────► fixture-mcp     │
  │  (SUT)  ◄─────────── JSON-RPC 2.0 ──────────────────┘   -mode <fault>  │
  │    │                                                    records frames │
  │    ▼ stream-json                                              │        │
  │  witness A: what the client says        witness B: what actually left  │
  │               └──────────┬─────────────────────────┘                   │
  │                          ▼  + witness C: side effects                  │
  │                  all three agree ⇒ the tool really ran                 │
  └────────────────────────────────────────────────────────────────────────┘
```

### Three witnesses

A tool call is only believed when three independent observations agree:

| | Witness | Proves |
|---|---|---|
| **A** | the client's `stream-json` `tool_use`, and the `tool_result` paired to it by id | the client *says* it called the tool |
| **B** | the fixture's recorded `tools/call`, with the unmangled name and exact arguments | the call actually left the client |
| **C** | a per-run random nonce, present nowhere in the prompt, appearing in the final text | the result re-entered the model's context |

Any one alone is weak. A is a claim. B could happen with the result never
reaching the model. C could in principle be echoed from the prompt — which is
why the nonce is never *in* the prompt. Together they close the loop.

### Two tiers, and why that is the answer to determinism

The client is deterministic. The model in front of it is not. So the suite is
layered to keep the model out of the way of most of the coverage.

| Tier | Needs | Cases | Covers | Run when |
|---|---|---|---|---|
| **T0** | nothing | 37 | config, handshake, negotiation, transports, every fault up to tool discovery, process lifecycle | every commit, including forks |
| **T1** | key, pinned Haiku | 17 | that a real tool call crosses the client correctly, and tool-level faults | every PR |

T0 is possible because of the single most useful thing recon turned up:
**`claude mcp list` performs a real connection and handshake**, with no model
and no API key. Connection, negotiation, transport faults and timeouts are
therefore free to test.

The tiers are enforced, not just documented. The model tier sits behind a
`model` build tag, so no other target can run it. And `harness.Env.WithAPIKey`
defaults to false, skipping the test if a key is needed — so a test cannot reach
the model without asking, which is what keeps T0 keyless by construction rather
than by convention.

### Hermeticity is load-bearing

Claude Code persists MCP config in `~/.claude.json` keyed by project path, and
project-scoped `.mcp.json` servers sit behind a trust gate. Sharing a `HOME`
between tests makes them pass or fail by ordering, and can hang a
non-interactive run waiting for approval. Every test therefore gets its own
`HOME`, working directory, transcript and nonce via `t.TempDir()`, and tests run
with `-shuffle=on`.

---

## The five questions

### 1. Connection and auth — *breadth*

All three transports connect and are tested: **stdio**, **streamable HTTP** and
**legacy HTTP+SSE**. Configuration is exercised through `claude mcp add/get/
remove` with scope reporting, through a project `.mcp.json`, and with `${VAR}`
expansion verified *from the spawned child's own environment* rather than from
the config file — the fixture reports what it actually received.

The handshake is asserted frame by frame against a recorded baseline:
`server/discover` → `initialize` → `notifications/initialized` → `tools/list`,
including `clientInfo`, the advertised capabilities, and numeric monotonic
request ids.

For auth: a static `Authorization` header is proven present on **every** request
including the very first, before any session exists, and proven to be the *right*
credential by hash. On a `401` with a Bearer challenge the full discovery chain
is asserted in order. Credentials are recorded as scheme + length + truncated
SHA-256 — never in clear — so a test can prove which token was sent while the
artifact stays safe to attach to a CI run.

Not done: the interactive half of OAuth. Recon found `--callback-port` and
`claude mcp login`, which make a headless fixture authorization server
plausible, so this is specified in `PLAN.md` as reachable rather than blocked.

### 2. Tool calls — *depth*

The three-witness model above, plus: the server receives the **unmangled** tool
name, so the `mcp__server__tool` namespacing does not leak onto the wire;
argument fidelity across six JSON types (string, integer, boolean, enum, array,
nested object) with an omitted optional proven absent rather than `null`;
routing between two servers with separate transcripts so mis-routing is visible;
allow-list enforcement proven by an empty transcript; and unicode intact in both
directions.

Argument assertions are invariant-based: types are the contract, and only
unambiguous values are checked. Failing a test because the model paraphrased a
string is not a client bug.

### 3. Errors — *depth*

One table, one row per fault, because the fixture takes a `-mode` flag — adding
the next fault costs three lines, not a new test. Each row asserts the reported
verdict, the cause text, which frames the client did and did not send, and a
duration bound.

T0 covers the server dying before, during and after the handshake; never
answering; polluting stdout; emitting a truncated frame, an unknown id, an
unknown notification, unknown init fields; an unsupported protocol version; HTTP
`500`, `401` and connection-refused; a malformed `.mcp.json`; and orphaned
processes.

T1 covers what the health check cannot reach, because it never issues
`tools/call`: a tool reporting its own failure (`isError: true`) versus a call
failing at the protocol level (a JSON-RPC error object) — distinct code paths
that are often conflated — plus `-32602`, `-32601`, `-32603`, a crash mid-call, a
hanging tool, a malformed result and invalid UTF-8.

The most important row is **ERR-27**: one broken server must not degrade a
working one. A user with five servers configured and one broken must still be
able to work.

### 4. Determinism

Answered structurally rather than by a group of tests — see the tier table
above. The rules the suite holds itself to:

- **Nothing asserts on prose.** The nonce is the only text ever matched, and it
  is random per run. One documented exception: when checking that a failure
  reached the model, the last-resort path scans result text for failure
  keywords, and reports which route it used so the weaker evidence is visible.
- **Timeouts are bounds, never exact durations**, so a slow runner cannot
  fabricate a failure. Where a knob exists it is used instead: `MCP_TIMEOUT=5000`
  makes the connect-timeout test deterministic and fast rather than asserting
  against a ~28s default.
- **Model-tier tests are narrowed as hard as the public surface allows** —
  pinned model, allow-list restricted to the tool under test, imperative prompts
  — and each fault row fails loudly with *"the client never issued tools/call,
  so this row proves nothing"* rather than passing vacuously.
- **Cost is reported per session** so spend is visible and an accidentally
  expensive test is obvious.

Known gap: the suite repeats runs on request (`./run.sh flake`) but does not yet
compute a pass *rate*, so a flaky model-tier test is currently a human reading
per-run lines. That is `DET-06`/`DET-07` in `PLAN.md`, and it is the first thing
I would build next.

### 5. What else has to be true

The suite ships an **observed-behaviour table** (below) because black-box
testing without source means part of the job is discovery, and encoding what you
found as a characterization test is how that discovery survives.

Implemented beyond the first four questions: isolation between servers,
orphan-process cleanup, permission gating, capability honesty (the client must
not be interrogated for features it never advertised), backwards compatibility
with an older protocol version and forwards compatibility with unknown fields.

Specified and not implemented, in rough priority order: prompt injection via
tool descriptions and results, secret-leakage assertions, the `roots/list` and
`elicitation` round trips (the fixture has modes for both), progress
notifications — now concretely testable because the client sends a
`progressToken` — resource and prompt surfaces, a version matrix, and
multi-architecture verification.

---

## Observed behaviour of the client

Established empirically against 2.1.296. These are the rows most likely to need
re-baselining on a version bump.

| Area | Observed |
|---|---|
| Handshake order | `server/discover` → `initialize` → `notifications/initialized` → `tools/list` |
| Protocol version | `initialize` negotiates `2025-11-25`; the discover pre-probe advertises `2026-07-28` |
| `server/discover` | Undocumented pre-probe, string id, `io.modelcontextprotocol/*` `_meta`. A `-32601` refusal is tolerated and the client falls back cleanly |
| `clientInfo` | `claude-code` / "Claude Code" / `2.1.296`, plus description and websiteUrl |
| Client capabilities | `roots.listChanged`, `elicitation.{form,url}`. **No `sampling`** |
| Request ids | Numeric and monotonic from 0; the discover probe uses a string id |
| Tool namespacing | `mcp__<server>__<tool>` to the model; the server receives the unmangled name |
| `tools/call` `_meta` | `claudecode/toolUseId` and a `progressToken` |
| Health states | `✔ Connected`, `✘ Failed to connect — <cause>`, `! Connected · tools fetch failed`, `! Needs authentication`, `⏸ Pending approval`, disabled. **Two contain the word "Connected"** |
| `mcp list` exit code | **Always 0**, even on total connection failure |
| `mcp get` exit code | 1 for an unknown server — a different contract from `list` |
| Connect timeout | ~28s by default; `MCP_TIMEOUT` is honoured (5000 → 3s, 60000 → 57s) |
| Tool-call timeout | **None by default** — see findings |
| Stream robustness | stdout log lines, non-JSON-RPC JSON, a truncated frame, an unknown id, an unknown notification and unknown `initialize` fields are all tolerated |
| Unsupported version | Refused by name; `tools/list` is never attempted |
| Crash after handshake | Reported as the partial state, not as a failure |
| OAuth on `401` | Discovery runs during an ordinary health check: `/.well-known/oauth-protected-resource/mcp` (RFC 9728 path-inserted) → root form → `/.well-known/oauth-authorization-server`. A challenge does not change the verdict |
| HTTP envelope | `Accept` carries both `application/json` and `text/event-stream`; the credential is on every request including the first; `Mcp-Session-Id` is echoed after `initialize` |
| Permissions | `--disallowedTools` removes the tool from the model's visible set entirely. `--allowedTools` does **not** narrow it — all 21 built-ins stay advertised |
| Tool catalogue | 64 advertised tools accepted without loss |
| `.mcp.json` | Unapproved servers report pending and are **not connected to** — zero frames |
| Malformed `.mcp.json` | Diagnostic line, exit 0, no crash |
| Isolation | A broken server does not degrade a healthy sibling; init reports `{broken: failed, healthy: connected}` |

---

## Findings worth acting on

**1. A hanging tool is never bounded.** A server that receives `tools/call` and
never answers leaves the session running indefinitely — still going after four
minutes, with the transcript proving the call was delivered and unanswered, and
it had to be killed. The connect path bounds itself and `MCP_TIMEOUT` moves it,
so the asymmetry is specific to tool calls. `ERR-17` now tests that
`MCP_TOOL_TIMEOUT` contains the problem; `ERR-17b` is an opt-in characterization
asserting the missing default is *still* missing, so a future fix fails the test
and asks to be re-baselined.

**2. Malformed tool results are accepted silently.** A result with no `content`
field, and a result containing invalid UTF-8, both produce no error anywhere —
no `is_error`, nothing in the result event, no mention in the text. The session
completes as though the call succeeded. A tool failure presented as success is
worse than a loud one, because the model builds on it.

**3. `claude mcp list` always exits 0.** Failure is in the output but not the
exit code, so any CI health gate built on it is blind. This is pinned as an
observation in `ERR-31`, which fails if the behaviour changes in either
direction.

---

## CI roadmap

The T0 tier is already CI-shaped: no key, no secrets, no network beyond the
image build, 31s, order-shuffled, and a meaningful exit code. `.github/workflows/
ci.yml` is included and reads as intended, but has **not been exercised** — this
repo has no remote.

What running it properly would take, in order of value:

1. **Two jobs, split on the secret.** T0 runs on every push and pull request,
   including from forks, because it needs no key. T1 runs only where the secret
   is available — on push to the default branch, or on a PR carrying a label —
   so a fork PR cannot exfiltrate the key and cannot spend money.
2. **Machine-readable results.** Add `gotestsum` to the image and wrap the
   `go test` call to emit `--junitfile`, so failures surface per-test in the PR
   rather than in a log. About five lines in the `Dockerfile` and `scripts/suite`.
3. **Artifact upload on failure.** `./artifacts/` already contains exactly what
   is needed; it just needs an `actions/upload-artifact` step with `if:
   failure()`.
4. **Layer caching.** The image takes a few minutes to build cold, almost all of
   it the 256 MB Claude Code binary. `docker/build-push-action` with
   `cache-from: type=gha` keeps that off the critical path; pinning
   `CLAUDE_VERSION` means the layer is stable until the pin moves.
5. **Flake policy before the model tier gates anything.** T1 is probabilistic by
   nature. Until `DET-06`/`DET-07` compute a pass rate, T1 should be
   non-blocking: report it, do not fail the build on it. The honest version is a
   nightly `FLAKE_RUNS=10 ./run.sh flake` whose rate is tracked over time, with a
   threshold agreed before it becomes a gate.
6. **Architecture.** The image is built and verified on `arm64` locally; CI
   runners are usually `amd64`. The base images are multi-arch, so this should
   just work, but `RDY-23` exists because "should" is not "does".
7. **Version matrix.** `CLAUDE_VERSION` is already a build arg, so a matrix over
   several releases comes nearly free and turns the characterization rows into a
   diff of client behaviour across versions — which is where this suite becomes
   most useful (`RDY-22`).
8. **Cost control.** T1 is about $0.02 per run at current size. Worth a budget
   assertion in the run report before anyone wires it to every commit.

---

## Repo layout

```
run.sh                    the one command
Dockerfile                pins Claude Code 2.1.296, updater disabled
scripts/suite             tier → go test mapping, inside the image
cmd/fixture-mcp/          the instrument: transports, 38 fault modes, recorder
internal/
  harness/                hermetic env, CLI runner, session runner, artifacts
  transcript/             queries over recorded frames (witness B)
  claudecli/              stream-json event parser (witness A)
  health/                 health-check output parser
tests/
  protocol/               T0 — Q1 connection, handshake, transports, HTTP/auth
  errors/                 T0 — Q3 fault catalogue
  toolcall/               T1 — Q2 three witnesses + tool-level faults (model tag)
PLAN.md                   153 rows, what is implemented and what is not
PROMPTS.md                the prompts used
```

---

## Limits

- The model tier is probabilistic. It is narrowed as far as the public surface
  allows and reports what it spends, but it is not deterministic, and the
  suite's weight is deliberately on the keyless tier.
- `--mcp-config` and `--strict-mcp-config` are session-only flags, rejected by
  the `mcp` subcommands, so the rows that depend on them are T1 and unimplemented.
- `TOOL-22` (oversized results) is **inconclusive**. It logged a result
  flattened to 1 and 1536 bytes against 1 MiB sent, but the 1-byte figure is
  most likely this suite's own content flattening rendering a non-text block as
  nothing. No truncation behaviour is claimed; the test now logs the raw
  envelope so the next run settles it.
- Behaviour is pinned to 2.1.296, and newer versions are already published. The
  suite asserts the pin rather than trusting the image to stay put.
- The biggest single improvement available is a stub model server behind
  `ANTHROPIC_BASE_URL` returning a canned `tool_use`, which would move the whole
  of Q2 into the deterministic tier. Specified as `DET-15`.

---

## How each requirement in the brief is met

| Requirement | Where |
|---|---|
| Plan first, one line each, covering all five questions | `PLAN.md` §3 — 153 rows across Q1–Q5 |
| Tests answer at least two questions in depth | Q2 (`tests/toolcall`) and Q3 (`tests/errors` + `tests/toolcall`) |
| Claude Code runs in Docker as the SUT | `Dockerfile`, pinned to 2.1.296 with the updater disabled |
| Starts with one command, no other setup | `./run.sh` — Docker and git only; the default tier needs no key |
| API key from `ANTHROPIC_API_KEY` | Read by `run.sh`, passed through only when set |
| Buildable for CI on every change | T0 is already CI-shaped; roadmap above; `.github/workflows/ci.yml` included |
| `PLAN.md` with what was implemented | Status column per row, with qualifiers where a row is partial |
| `PROMPTS.md` | Every prompt given, in order, with what it produced |
| `README.md`: how to run, choices, requirements | This file |
