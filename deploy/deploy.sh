#!/bin/bash
# Installs the gateway from this checkout: builds it-router and installs it, with run.sh, into the gateway directory
# (default ~/.config/it-gateway, override with IT_GATEWAY_DIR; created if missing). Whatever it replaces is first copied to
# <dir>/backup-<timestamp>/. It does not restart anything: restarting is a separate, deliberate step (see the README).
#   deploy.sh --dry-run   show what would change, touch nothing
set -euo pipefail
REPO=$(cd "$(dirname "$0")/.." && pwd)
DEST=${IT_GATEWAY_DIR:-$HOME/.config/it-gateway}
DRY=0; [ "${1:-}" = --dry-run ] && DRY=1
TS=$(date +%Y%m%d-%H%M%S)
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

echo "repo: $REPO"; echo "dest: $DEST"
( cd "$REPO" && test -z "$(gofmt -l .)" && go vet ./... && go build -o "$TMP/it-router" . )
cp "$REPO/deploy/run.sh" "$TMP/run.sh"
[ -d "$DEST" ] || echo "dest does not exist yet: it is created"

plan() { # <name> <mode>
  local f=$1
  if [ ! -e "$DEST/$f" ]; then echo "  new      $f"; elif cmp -s "$TMP/$f" "$DEST/$f"; then echo "  same     $f"; else echo "  replace  $f"; fi
}
echo "changes:"; plan it-router; plan run.sh
if [ -z "${MIMIR_ANTHROPIC_URL:-}" ] && ! grep -qs '^MIMIR_ANTHROPIC_URL=.' "$DEST/env"; then
  echo "WARNING: MIMIR_ANTHROPIC_URL is set neither in $DEST/env nor here: the gateway will exit at start-up (see .env.example)"
fi
[ $DRY = 1 ] && { echo "dry run: nothing changed"; exit 0; }

mkdir -p "$DEST"; BK=$DEST/backup-$TS
for f in it-router run.sh; do
  if [ -e "$DEST/$f" ]; then mkdir -p "$BK"; cp -p "$DEST/$f" "$BK/$f"; fi
  cp "$TMP/$f" "$DEST/$f.new.$$" && chmod +x "$DEST/$f.new.$$" && mv "$DEST/$f.new.$$" "$DEST/$f" # atomic replace
done
if [ -d "$BK" ]; then echo "installed. Backup of what was replaced: $BK"; else echo "installed (nothing to back up)"; fi
echo "Restart is separate: launchctl bootout gui/$(id -u)/io.cubbit.it-router, wait until 'launchctl print gui/$(id -u)/io.cubbit.it-router' fails, then launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.cubbit.it-router.plist"
