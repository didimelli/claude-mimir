#!/bin/zsh
# Starts the gateway (the launchd job runs this). Everything is resolved from this script's own directory, so a copy
# under test never touches the live files.
#
# Configuration comes from the environment. launchd passes almost none, so $DIR/env (KEY=VALUE lines, keep it mode 600)
# is read first if it exists: MIMIR_API_KEY, MIMIR_ANTHROPIC_URL, and optionally IT_ROUTER_PORT (4000), IT_ROUTER_LOG.
# Without MIMIR_API_KEY, the key is read from MIMIR_API_KEY_FILE (default ~/.config/opencode/mimir-key).
DIR=${0:A:h}
if [ -f "$DIR/env" ]; then set -a; source "$DIR/env"; set +a; fi
export MIMIR_API_KEY="${MIMIR_API_KEY:-$(cat "${MIMIR_API_KEY_FILE:-$HOME/.config/opencode/mimir-key}" 2>/dev/null)}"
ARGS=(--port "${IT_ROUTER_PORT:-4000}")
[ -n "$IT_ROUTER_LOG" ] && ARGS+=(--log "$IT_ROUTER_LOG")

[ -x "$DIR/it-router" ] || { echo "run.sh: $DIR/it-router is missing: run deploy/deploy.sh" >&2; exit 78; }
exec "$DIR/it-router" "${ARGS[@]}" "$@"
