#!/bin/bash
# Start/stop dev gateway processes on spare ports (4010-4019), tracked by pid file. Never matches processes by name,
# so a live gateway on 4000 is never touched.
#   devrouter.sh start NAME PORT BIN [extra flags...]   BIN = the it-router binary to run
#   devrouter.sh stop  NAME | stopall | status
D=$(cd "$(dirname "$0")" && pwd); RUN=$D/run; mkdir -p "$RUN"
die() { echo "devrouter: $*" >&2; exit 1; }
stop_one() {
  local n=$1 pf=$RUN/$1.pid pid port l victims=""
  [ -f "$pf" ] || return 0
  read -r pid port < "$pf"
  # what was started may be a wrapper whose child holds the port: kill the pid if it listens, and any listener whose parent is it
  for l in $(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null); do
    if [ "$l" = "$pid" ] || [ "$(ps -o ppid= -p "$l" 2>/dev/null | tr -d ' ')" = "$pid" ]; then victims="$victims $l"; fi
  done
  if [ -n "$victims" ]; then
    kill $pid $victims 2>/dev/null
    for i in 1 2 3 4 5 6 7 8 9 10; do lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1 || break; sleep 0.5; done
    for l in $victims; do kill -0 "$l" 2>/dev/null && kill -9 "$l" 2>/dev/null; done
    echo "stopped $n (pid $pid, port $port)"
  else echo "$n: nothing of mine is listening on $port (pid file removed)"; fi
  rm -f "$pf"
}
case ${1:-} in
  start)
    name=${2:?name}; port=${3:?port}; bin=${4:?bin}; shift 4
    [[ $port =~ ^401[0-9]$ ]] || die "port $port refused: spare ports are 4010-4019 (4000 is the live gateway)"
    lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1 && die "port $port is already in use"
    [ -f "$RUN/$name.pid" ] && die "$name already has a pid file (stop it first)"
    for a in "$@"; do case $a in */Library/Logs/*|*:4000*) die "flag '$a' points at live state";; esac; done
    log=$RUN/$name.jsonl
    cmd=("$bin")
    "${cmd[@]}" "$@" --port "$port" --log "$log" > "$RUN/$name.out" 2>&1 &
    pid=$!; echo "$pid $port" > "$RUN/$name.pid"
    for i in $(seq 1 60); do curl -s -m 1 "http://127.0.0.1:$port/health/liveliness" >/dev/null 2>&1 && { echo "$name up on :$port (pid $pid, log $log)"; exit 0; }; kill -0 $pid 2>/dev/null || { cat "$RUN/$name.out"; rm -f "$RUN/$name.pid"; die "$name exited at start"; }; sleep 0.5; done
    die "$name did not answer in 30s" ;;
  stop) stop_one "${2:?name}" ;;
  stopall) for f in "$RUN"/*.pid; do [ -f "$f" ] && stop_one "$(basename "$f" .pid)"; done; true ;;
  status) for f in "$RUN"/*.pid; do [ -f "$f" ] && echo "$(basename "$f" .pid): $(cat "$f")"; done; true ;;
  *) die "usage: start|stop|stopall|status" ;;
esac
