#!/bin/bash
# Installs the gateway from this checkout: builds it-router and installs it, with run.sh, into the gateway directory
# (default ~/.config/litellm, override with IT_GATEWAY_DIR). Whatever it replaces is first copied to
# <dir>/backup-<timestamp>/. It does not restart anything: restarting is a separate, deliberate step (see the README).
#   deploy.sh --dry-run   show what would change, touch nothing
set -euo pipefail
REPO=$(cd "$(dirname "$0")/.." && pwd)
DEST=${IT_GATEWAY_DIR:-$HOME/.config/litellm}
DRY=0; [ "${1:-}" = --dry-run ] && DRY=1
TS=$(date +%Y%m%d-%H%M%S)
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

echo "repo: $REPO"; echo "dest: $DEST"
( cd "$REPO/gateway" && test -z "$(gofmt -l .)" && go vet ./... && go build -o "$TMP/it-router" . )
cp "$REPO/litellm/run.sh" "$TMP/run.sh"
[ -d "$DEST" ] || { echo "dest does not exist: $DEST"; exit 1; }

plan() { # <name> <mode>
  local f=$1
  if [ ! -e "$DEST/$f" ]; then echo "  new      $f"; elif cmp -s "$TMP/$f" "$DEST/$f"; then echo "  same     $f"; else echo "  replace  $f"; fi
}
echo "changes:"; plan it-router; plan run.sh
for f in router.py config.yaml mimir_hooks.py diag_hooks.py; do # the rollback stack: reported, never touched
  if [ -e "$DEST/$f" ] && ! cmp -s "$REPO/litellm/$f" "$DEST/$f"; then echo "  differs  $f (rollback stack, not touched)"; fi
done
[ $DRY = 1 ] && { echo "dry run: nothing changed"; exit 0; }

BK=$DEST/backup-$TS; mkdir -p "$BK"
for f in it-router run.sh; do
  [ -e "$DEST/$f" ] && cp -p "$DEST/$f" "$BK/$f"
  cp "$TMP/$f" "$DEST/$f.new.$$" && chmod +x "$DEST/$f.new.$$" && mv "$DEST/$f.new.$$" "$DEST/$f" # atomic replace
done
echo "installed. Backup of what was replaced: $BK"
echo "Restart is separate: launchctl bootout gui/501/io.cubbit.litellm, wait until 'launchctl print gui/501/io.cubbit.litellm' fails, then launchctl bootstrap gui/501 ~/Library/LaunchAgents/io.cubbit.litellm.plist"
