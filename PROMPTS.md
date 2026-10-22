# PROMPTS

The AI tooling used was Claude Code (Opus) in an interactive session. Prompts
are recorded as given, in order, with a note on what each produced. Transcripts
are omitted per the brief.

## 1 — Analysis before any code

> As part of an interview process, I got a home assignment that I want you to
> read carefully, and for now just analyze it: *(assignment text pasted)*

Produced the framing the suite is built on: that the trap in this assignment is
testing the model instead of the client, and that the fix is to make the MCP
server the measuring instrument. Also produced the tiering idea and the list of
traps (hermetic `HOME`, approval gates blocking headless runs, auto-update
drift).

## 2 — Architecture and plan

> ok first I need to design the black-box test architecture and draft PLAN.md,
> so let's start with that.

Produced `PLAN.md`: scope, the three-witness verification model, the tier table,
and the 147-row inventory.

## 3 — Language decision

> I would prefer if you make it in go

Chosen for static dependency-free fixture binaries, native `-count=N` flake
measurement and `-shuffle=on` order independence, and `t.TempDir()` per-test
hermeticity.

## 4 — Harness implementation

> looks good, let's proceed with building the harness. Implement
> cmd/fixture-mcp, the Dockerfile, and run.sh. Make sure we support stdio/SSE,
> and the -mode flag ready for fault injection (and dont froget server/discover
> probe also make sure run.sh defaults to running T0 headlessly without an api
> key, then do a quick smoke test against claude mcp list to verify the
> baseline

Produced `cmd/fixture-mcp` (stdio, streamable HTTP and legacy SSE transports,
38 named fault modes, NDJSON frame recorder, OAuth discovery endpoints),
`internal/transcript`, `internal/harness`, `tests/protocol/baseline_test.go`,
the `Dockerfile`, `scripts/suite` and `run.sh`.

## 5 — T0 suite and the Q3 table

> ok start writing the go tests, implement the T0 suite and table-driven tests
> for Q3 and Q1 baseline handshake and make sure go test ./... runs cleanly
> inside the container without a key using the fixture's recorded frames for
> assertions lets see the test output when done

Produced `internal/health` (health-check output parser), harness additions for
HTTP/SSE fixtures and multi-server tests, `tests/protocol/connection_test.go`,
`tests/protocol/http_test.go` and `tests/errors/faults_test.go`. Two corrections
came out of running it: `--mcp-config` turned out to be session-only, re-tiering
four rows, and the health check turned out to have more states than the parser
knew.

## Method notes

- Recon preceded planning: `claude --help`, `claude mcp --help` and every `mcp`
  subcommand were captured from the pinned 2.1.296 container, and the plan was
  revised against what the binary actually exposes rather than what the public
  docs describe. See `PLAN.md` §0.1.
- The claim that `claude mcp list` health-checks servers (which the whole T0
  tier depends on) was probed with a throwaway fixture server before being
  relied upon, not assumed from the help text.
