#!/bin/zsh
# Starts the gateway the launchd job keeps alive. Everything is resolved from this script's own directory, so a copy
# under test never touches the live files; the port, log and Mimir endpoint come from the environment.
#
# IT_ROUTER_PORT (4000), IT_ROUTER_LOG, IT_MIMIR_URL override the defaults.
# launchd does not inherit the shell env, so MIMIR_API_KEY falls back to the opencode key file.
DIR=${0:A:h}
PORT=${IT_ROUTER_PORT:-4000}
export MIMIR_API_KEY="${MIMIR_API_KEY:-$(cat "$HOME/.config/opencode/mimir-key" 2>/dev/null)}"
ARGS=(--port "$PORT")
[ -n "$IT_MIMIR_URL" ] && ARGS+=(--mimir-url "$IT_MIMIR_URL")
[ -n "$IT_ROUTER_LOG" ] && ARGS+=(--log "$IT_ROUTER_LOG")

[ -x "$DIR/it-router" ] || { echo "run.sh: $DIR/it-router is missing: run gateway/deploy.sh" >&2; exit 78; }
exec "$DIR/it-router" "${ARGS[@]}" "$@"
