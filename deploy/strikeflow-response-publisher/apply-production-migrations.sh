#!/bin/sh
set -eu

# The bounded production gate is implemented in the mainline-aware wrapper.
# Keep this historical entrypoint as a stable operator-facing alias so an
# older runbook command cannot silently run an unbounded migration set. The
# delegated command still requires the explicit --confirm-migrate token.
exec "$(dirname "$0")/apply-mainline-migrations.sh" "$@"
