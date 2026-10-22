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

## 6 — Model tier

> continue to tests/toolcall, and fix the gaps you stated like the hang_tool,
> when done, we will make the tests and write the README

Produced `internal/claudecli` (stream-json event parser), `harness.Prompt`
(pinned model, narrowed allow-list, per-session cost logging),
`tests/toolcall/toolcall_test.go` (the three-witness proof and argument
fidelity) and `tests/toolcall/toolfaults_test.go` (the eight tool-level faults
the keyless tier cannot reach, plus the isolation test). Both are behind the
`model` build tag.

## 7 — Reviewing the live run

> I exported an api key and ran ./run.sh t1, check the result

The run was still in progress, so its container logs were followed to
completion rather than guessed at. 15 of 16 passed. The failure was diagnosed
from the mounted failure artifact as a real client defect (no bound on a
hanging tool), the connect-timeout contrast was then verified separately at T0
with no key, and `ERR-17` was restructured around the finding.

## 8 — Documentation and reconciliation

> write the README.md covering the architecture the one command run
> instructions the ci roadmap and how each of the five questions is answered
> then check that PLAN.md is updated and reconcile the status so only whats
> actually implemented is marked impl, and make sure PROMPTS.md is updated

Produced `README.md`, `.github/workflows/ci.yml`, and a status reconciliation of
`PLAN.md` driven by a script that compared every row marked `impl` against the
plan IDs actually referenced in code. That audit demoted 18 rows that were
aspirational, promoted 2 that had been implemented without being credited, and
added 2 rows the implementation had invented.

## Method notes

- Recon preceded planning: `claude --help`, `claude mcp --help` and every `mcp`
  subcommand were captured from the pinned 2.1.296 container, and the plan was
  revised against what the binary actually exposes rather than what the public
  docs describe. See `PLAN.md` §0.1.
- The claim that `claude mcp list` health-checks servers (which the whole T0
  tier depends on) was probed with a throwaway fixture server before being
  relied upon, not assumed from the help text.
- Two assumptions in the plan were contradicted by running it, and the plan was
  corrected rather than the tests bent to fit: `--mcp-config` turned out to be
  session-only (so it cannot be what makes T0 hermetic), and the health check
  turned out to have six result states rather than two.
- The instrument was calibrated before being trusted. Sweeping all 38 fault
  modes once caught a bug in the fixture itself — a bare `select{}` tripped Go's
  deadlock detector, so the "hang" modes were killing the process and the client
  was correctly reporting a closed connection rather than a timeout.
- The final status reconciliation was done by script, not by reading, because a
  status column maintained by hand drifts and a drifted one is worse than none.
