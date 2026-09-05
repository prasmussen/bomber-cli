#!/bin/sh
# Stable, narrowly scoped command entry point for reusable Codex approval.
set -eu
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec python3 "$SCRIPT_DIR/dev.py" "$@"
