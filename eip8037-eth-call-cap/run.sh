#!/usr/bin/env bash
# Runs the reproduction. Needs Go >= 1.25 and network access for the Go module proxy.
# Pass --recompile to rebuild build/Burner.* with solc via nix first.
set -euo pipefail
cd "$(dirname "$0")"
if [[ "${1:-}" == "--recompile" ]]; then
  nix shell nixpkgs#solc -c solc --evm-version cancun --optimize --bin --bin-runtime --abi contracts/Burner.sol -o build --overwrite
fi
# geth logs go to stderr; the report goes to stdout.
go run . 2>/dev/null | tee output.txt
