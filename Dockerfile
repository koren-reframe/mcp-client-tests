# The whole suite runs in this image: Claude Code (the system under test), the
# fixture MCP server (the instrument), and the Go test runner. Nothing is
# needed on the host but Docker, git and optionally an API key.

# --- stage 1: compile the instrument --------------------------------------
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/fixture-mcp ./cmd/fixture-mcp

# --- stage 2: runtime ------------------------------------------------------
FROM golang:1.25-bookworm

# Node is here only because Claude Code is distributed as an npm package that
# wraps a native binary. Copying from the official image keeps the version
# pinned without adding a third-party apt source.
COPY --from=node:22-bookworm-slim /usr/local/bin/node /usr/local/bin/node
COPY --from=node:22-bookworm-slim /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -sf /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && apt-get update \
 && apt-get install -y --no-install-recommends procps lsof ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Pinned deliberately: newer versions are already published upstream, and an
# image that drifts stops testing the stated system under test. CONN-39
# asserts this value at runtime so drift fails loudly rather than silently.
ARG CLAUDE_VERSION=2.1.296

# Keep the client from updating itself or making calls the tests did not ask
# for. These are belt and braces; the runtime assertion is the real guard.
ENV DISABLE_AUTOUPDATER=1 \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    DISABLE_BUG_COMMAND=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_VERSION} \
 && claude --version

COPY --from=build /out/fixture-mcp /usr/local/bin/fixture-mcp

WORKDIR /suite
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY tests ./tests
COPY scripts/suite /usr/local/bin/suite
RUN chmod +x /usr/local/bin/suite

ENV FIXTURE_BIN=/usr/local/bin/fixture-mcp \
    ARTIFACT_DIR=/artifacts \
    GOCACHE=/tmp/gocache \
    GOFLAGS=-mod=mod

# Warm the build cache so a test run is not paying for compilation. Compiling
# the test binaries without running them is enough.
RUN go build ./... && go test -run='^$' ./tests/... >/dev/null

ENTRYPOINT ["/usr/local/bin/suite"]
CMD ["t0"]
