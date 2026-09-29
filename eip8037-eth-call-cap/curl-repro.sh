#!/usr/bin/env bash
# Minimal repro with stock `geth --dev` and curl, no deployment needed (the
# contracts are injected with eth_call state overrides).
#
#   geth --dev --http                           # Osaka (+Bogota), the default dev chain
#   geth --dev --http --override.amsterdam 0    # same, plus Amsterdam
#   ./curl-repro.sh [http://127.0.0.1:8545]
set -euo pipefail
cd "$(dirname "$0")"
URL=${1:-http://127.0.0.1:8545}
BURNER=0x$(tr -d '[:space:]' < build/Burner.bin-runtime)

rpc() { curl -s -H 'content-type: application/json' -d "$1" "$URL"; echo; }

echo "# GasProbe (GAS PUSH0 MSTORE PUSH1 0x20 PUSH0 RETURN), gas=50,000,000: gas left at first opcode"
rpc '{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x000000000000000000000000000000000000fe0f","gas":"0x2faf080"},"latest",{"0x000000000000000000000000000000000000fe0f":{"code":"0x5a5f5260205ff3"}}]}'

for n in 137320 141487; do
  # burn(uint256) selector 0x42966c68; 137320 iterations ~16.5M gas, 141487 ~17.0M gas
  data=0x42966c68$(printf '%064x' "$n")
  echo "# burn($n), gas=50,000,000"
  rpc '{"jsonrpc":"2.0","id":2,"method":"eth_call","params":[{"to":"0x000000000000000000000000000000000000b0b0","input":"'"$data"'","gas":"0x2faf080"},"latest",{"0x000000000000000000000000000000000000b0b0":{"code":"'"$BURNER"'"}}]}'
done
