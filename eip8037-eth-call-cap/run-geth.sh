#!/usr/bin/env bash
# Runs the reproduction against a real geth binary over HTTP JSON-RPC, once per
# chain config (Osaka, Osaka+Amsterdam). geth runs in --dev mode on a custom
# genesis written by `go run . -write-genesis`, with default --rpc.gascap (50M).
#
# Usage: ./run-geth.sh            (clones and builds geth at GETH_COMMIT into WORKDIR)
#        REPRO_GETH=/path/to/geth ./run-geth.sh
set -euo pipefail
cd "$(dirname "$0")"

GETH_COMMIT=f8f9bc574459a1afefac7b63163739910ee0fe62
WORKDIR=${WORKDIR:-/tmp/eip8037-eth-call-cap}
mkdir -p "$WORKDIR"

if [[ -z "${REPRO_GETH:-}" ]]; then
  src="$WORKDIR/go-ethereum"
  if [[ ! -d "$src" ]]; then
    git clone --filter=blob:none -q https://github.com/ethereum/go-ethereum.git "$src"
  fi
  git -C "$src" checkout -q "$GETH_COMMIT"
  REPRO_GETH="$WORKDIR/geth"
  if [[ ! -x "$REPRO_GETH" ]] || [[ "$("$REPRO_GETH" version | sed -n 's/^Git Commit: //p')" != "$GETH_COMMIT" ]]; then
    echo "building geth at $GETH_COMMIT ..." >&2
    (cd "$src" && go build -o "$REPRO_GETH" ./cmd/geth)
  fi
fi

go build -o "$WORKDIR/repro" .
"$WORKDIR/repro" -write-genesis "$WORKDIR/genesis" >&2

out=output-geth.txt
: > "$out"
"$REPRO_GETH" version | sed -n '1,4p' | tee -a "$out"
echo | tee -a "$out"

for fork in osaka amsterdam; do
  datadir=$(mktemp -d "$WORKDIR/datadir-$fork.XXXX")
  log="$WORKDIR/geth-$fork.log"
  "$REPRO_GETH" init --datadir "$datadir" "$WORKDIR/genesis/genesis-$fork.json" > "$log" 2>&1
  "$REPRO_GETH" --dev --dev.period 0 --datadir "$datadir" \
    --http --http.addr 127.0.0.1 --http.port 0 --http.api eth,debug,web3 \
    --authrpc.port 0 --port 0 --ipcdisable --nodiscover >> "$log" 2>&1 &
  pid=$!
  # Stop only the geth we started.
  trap 'kill $pid 2>/dev/null || true' EXIT

  url=""
  for _ in $(seq 1 100); do
    ep=$(grep -o 'HTTP server started *endpoint=[0-9.]*:[0-9]*' "$log" | head -1 | sed 's/.*endpoint=//' || true)
    if [[ -n "$ep" ]]; then url="http://$ep"; break; fi
    kill -0 $pid 2>/dev/null || { echo "geth exited, see $log" >&2; tail -20 "$log" >&2; exit 1; }
    sleep 0.2
  done
  [[ -n "$url" ]] || { echo "geth HTTP endpoint not found, see $log" >&2; exit 1; }

  {
    echo "--- geth ($fork) chain config banner ---"
    grep -E ' - (Osaka|Amsterdam|Bogota): ' "$log" | sed 's/^.*] *//' | head -5 || true
    grep -m1 -o 'Chain ID: .*' "$log" || true
  } | tee -a "$out"
  "$WORKDIR/repro" -rpc "$url" -label "$fork" 2>/dev/null | tee -a "$out"

  kill $pid; wait $pid 2>/dev/null || true
  trap - EXIT
  rm -rf "$datadir"
done
echo "saved to $out (geth logs in $WORKDIR)" >&2
