#!/usr/bin/env bash
# The one command. Builds the image and runs a tier inside it.
#
#   ./run.sh            # T0: no API key needed
#   ./run.sh smoke      # baseline handshake, verbose
#   ./run.sh t1         # model tier, needs ANTHROPIC_API_KEY
#   ./run.sh all
#
# Nothing is required on the host but Docker and git.
set -euo pipefail
cd "$(dirname "$0")"

IMAGE="${IMAGE:-mcp-client-tests}"
TARGET="${1:-t0}"
shift || true

mkdir -p artifacts

echo "== building $IMAGE (first build pulls Go and Node and installs Claude Code) =="
docker build --progress=plain -t "$IMAGE" . 2>&1 | grep -Ev '^#[0-9]+ (sha256|extracting|transferring)' || true

RUN_ARGS=(--rm -v "$PWD/artifacts:/artifacts")

# The key is passed only when present, so the default target genuinely runs
# without one and cannot quietly depend on it.
if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
  RUN_ARGS+=(-e "ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}")
fi
if [ -n "${FLAKE_RUNS:-}" ]; then
  RUN_ARGS+=(-e "FLAKE_RUNS=${FLAKE_RUNS}")
fi

exec docker run "${RUN_ARGS[@]}" "$IMAGE" "$TARGET" "$@"
