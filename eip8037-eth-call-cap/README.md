# eth_call execution gas under Amsterdam (EIP-8037): reproduction

Checks whether go-ethereum's `eth_call` can only spend about 2^24 gas on computation once Amsterdam (EIP-8037) is active, even when the call is given 50M gas. Findings: `RESULTS.md`.

Drafts meant to be posted elsewhere (the GitHub issue, forum and chat messages, and the instructions for posting them) live in `outbox/`, which is git ignored.

## Run

Three ways, from most realistic to most self-contained. All give the same numbers.

**1. Real geth binary over HTTP** (`output-geth.txt`):

```sh
./run-geth.sh
```

Clones go-ethereum at the commit below into `/tmp/eip8037-eth-call-cap` (override with `WORKDIR=...`), builds `cmd/geth` (a few GB of RAM; prefix with `unbounded` if the build is killed), writes one genesis per config, and for each one runs `geth init` then `geth --dev --dev.period 0 --http --http.api eth,debug,web3` on that datadir with default `--rpc.gascap` (50M). The client deploys the contracts with real transactions and runs the whole suite over HTTP, then the script stops that geth process (and only that one). The fork banner from geth's log is included in the output. To reuse an existing binary: `REPRO_GETH=/path/to/geth ./run-geth.sh`.

**2. In-process** (`output.txt`), no geth binary needed:

```sh
./run.sh
```

Uses `ethclient/simulated` for the `eth_call` matrix, and for traces a node built the same way plus the `debug` API (the simulated backend does not register it). Contracts are placed with state overrides for the traces.

**3. Stock `geth --dev` and curl** (smallest, used in the issue):

```sh
geth --dev --http --override.amsterdam 0   # in another terminal; drop the flag to compare
./curl-repro.sh http://127.0.0.1:8545
```

Note that the stock dev chain at this commit also has Bogota active at 0; modes 1 and 2 use a genesis without Bogota.

Requirements: Go >= 1.25, network access to the Go module proxy and GitHub, `curl` for mode 3. The compiled contract is checked in under `build/`; to recompile it (needs `nix`): `./run.sh --recompile`.

## Versions used

- go-ethereum commit `f8f9bc574459a1afefac7b63163739910ee0fe62` (module version `v1.17.7-0.20260929081437-f8f9bc574459`, binary `Geth/v1.17.7-unstable-f8f9bc57-20260929`), unmodified
- Go `go1.26.7 linux/amd64`
- solc `0.8.33+commit.64118f21`, `--optimize --evm-version cancun`

## What it does

Two chain configs, both copied from `params.AllDevChainProtocolChanges` (the simulated backend's and `geth --dev`'s default):

- **osaka**: every fork up to and including Osaka active at genesis (`BogotaTime` and `AmsterdamTime` nil).
- **amsterdam**: the same plus `AmsterdamTime = 0`.

In mode 2 the config is injected through the option hook of `simulated.NewBackend` (`func(*node.Config, *ethconfig.Config)`), by replacing `ethConf.Genesis.Config`. In mode 1 the same configs are written as genesis files (`go run . -write-genesis DIR`).

Contracts:

- `contracts/Burner.sol`: `burn(uint256 n)` is a `pure` loop that hashes 64 bytes of scratch memory `n` times. No state access, no memory growth, so gas is exactly linear in `n` (120 gas per iteration). An earlier version using `keccak256(abi.encode(h, i))` grew memory every iteration and was quadratic, so it was replaced.
- GasProbe (raw bytecode `5a5f5260205ff3`, i.e. `GAS PUSH0 MSTORE PUSH1 0x20 PUSH0 RETURN`): returns the gas left at its first instruction, which shows directly through `eth_call` how much execution gas the call started with.

Per config, the suite:

1. checks the calibration with `debug_traceCall` (`gasUsed(n) = 21,500 + 120*n` on Osaka), which is what the fixed `n` values (83,154 / 137,320 / 141,487 / 166,487 / 333,154 for ~10M / 16.5M / 17M / 20M / 40M) are derived from;
2. calls GasProbe with `eth_call`, with `gas = 50,000,000` and with no `gas` field;
3. runs `eth_call`, `eth_simulateV1` (`validation: false`) and `eth_estimateGas` for each `n`;
4. runs `debug_traceCall` for each `n` with the struct logger (limited to the first opcode, to read the starting gas) and with `callTracer`.
