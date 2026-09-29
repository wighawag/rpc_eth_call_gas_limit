# Results

go-ethereum `f8f9bc574459a1afefac7b63163739910ee0fe62` (unmodified), Go 1.26.7, default `RPCGasCap` of 50,000,000. How to run: `README.md`.

The same suite was run three ways, and every number below is identical across them:

- a real `geth` binary built from that commit, `geth --dev` on a custom genesis, called over HTTP (`output-geth.txt`);
- in process, through `ethclient/simulated` plus a node built the same way with the `debug` API (`output.txt`);
- stock `geth --dev --override.amsterdam 0` with curl and state overrides (`curl-repro.sh`; GasProbe and the 16.5M / 17M points only).

## Setup check

- `params.MaxTxGas` = 16,777,216 (2^24).
- The real geth logs its fork schedule at start-up: the Osaka run shows `Osaka: @0` only, the Amsterdam run shows `Osaka: @0` and `Amsterdam: @0`.
- Both setups report different fork IDs through `eth_config`: `0x9bd14a56` (Osaka) and `0x2ed4abd3` (Osaka+Amsterdam), so the Amsterdam config is really in effect. The Amsterdam intrinsic cost change (EIP-2780, base 15,000 instead of 21,000) is also visible in every gas figure below, which is further confirmation.
- Burner cost, calibrated with `debug_traceCall` on Osaka: `gasUsed(n) = 21,500 + 120*n`, exactly linear. Under Amsterdam every figure is 6,000 lower (intrinsic 15,228 instead of 21,228 for this calldata); the execution part is identical.

## eth_call matrix (step 4)

"Work" is the total gas the call consumes when it succeeds, measured with `debug_traceCall` (struct logger `gas` field) on the respective config. For failing Amsterdam runs it is the gas the same call uses on Osaka minus the 6,000 intrinsic difference, i.e. what it would need.

| target | n | work (Osaka) | work (Amsterdam) | Osaka, gas=50M | Osaka, no gas | Amsterdam, gas=50M | Amsterdam, no gas |
|---|---|---|---|---|---|---|---|
| 10M | 83,154 | 9,999,992 | 9,993,992 | ok | ok | ok | ok |
| 16.5M | 137,320 | 16,499,912 | 16,493,912 | ok | ok | ok | ok |
| 17M | 141,487 | 16,999,952 | 16,993,952 | ok | ok | `out of gas` (-32000) | `out of gas` (-32000) |
| 20M | 166,487 | 19,999,952 | 19,993,952 | ok | ok | `out of gas` (-32000) | `out of gas` (-32000) |
| 40M | 333,154 | 39,999,992 | 39,993,992 | ok | ok | `out of gas` (-32000) | `out of gas` (-32000) |

Successful calls return the same `h` on both configs. The exact JSON-RPC error for every failing call is `out of gas`, code `-32000`. Every failing Amsterdam call burns exactly 16,777,216 gas (struct logger and `callTracer` both report `gasUsed = 16777216`).

Related RPCs, same targets:

| RPC | Osaka | Amsterdam |
|---|---|---|
| `eth_simulateV1` (`validation: false`), gas=50M | all succeed, gasUsed as in the table | 10M, 16.5M succeed; 17M+ `status=0`, gasUsed 16,777,216, error `out of gas` (-32015) |
| `eth_simulateV1` (`validation: false`), no gas | all succeed | same, but the error reads `out of gas (gas limit was capped by the RPC server's global gas cap)` |
| `debug_traceCall` (struct logger, callTracer) | all succeed | 17M+ fail with `out of gas`; `callTracer` still reports `gas: 50000000` for the top frame |
| `eth_estimateGas`, no gas | 10M: 10,080,524; 16.5M: 16,632,031; 17M+: `gas required exceeds allowance (16777216)` | 10M: 10,074,477; 16.5M: 16,625,983; 17M+: `gas required exceeds allowance (50000000)` |

The Osaka `eth_estimateGas` cap at 2^24 is intended (gasestimator caps `hi` at `MaxTxGas` on Osaka pre-Amsterdam, because the estimate is for a transaction). It is listed only for completeness.

## Mechanism (step 5)

The GasProbe contract returns `GAS` at its first opcode. Called with `eth_call` (same values on the real geth over HTTP and in process):

| config | gas field | GAS at first opcode | = |
|---|---|---|---|
| Osaka | 50,000,000 | 49,978,998 | 50,000,000 - 21,000 (intrinsic) - 2 (GAS) |
| Osaka | none | 49,978,998 | same (RPCGasCap default 50M applied) |
| Osaka+Amsterdam | 50,000,000 | 16,762,214 | 16,777,216 (2^24) - 15,000 (intrinsic) - 2 (GAS) |
| Osaka+Amsterdam | none | 16,762,214 | same |

`debug_traceCall` with the struct logger agrees: the first opcode of GasProbe has `gas = 16,762,216` under Amsterdam (49,979,000 on Osaka). For `burn(n)` the first opcode has `gas = 16,761,988` under Amsterdam, which is 2^24 minus the intrinsic 15,228 (15,000 + calldata), versus 49,978,772 = 50,000,000 - 21,228 on Osaka. So under Amsterdam the call starts with `MaxTxGas - intrinsic` of execution gas; the other 33,222,784 of the 50M goes to the EIP-8037 state-gas reservoir, which pure computation cannot use. This matches `initRuntimeGasBudget` (`core/state_transition.go` L502-L507), which applies `min(MaxTxGas - intrinsicGas, evmGas)` under Amsterdam with no check of `SkipTransactionChecks`, while `preCheck` (L561-L564) does skip the Osaka cap for `eth_call`.

## Conclusion

The hypothesis is confirmed, on a real geth node over HTTP as well as in process. At this commit, with Amsterdam active, `eth_call` (and `debug_traceCall` and non-strict `eth_simulateV1`, which share the state transition) gets at most `2^24 - intrinsic_gas` of execution gas no matter how much gas is requested: with 50M given explicitly or through the default `RPCGasCap`, a pure computation of 16.49M succeeds and one of 16.99M fails with `out of gas`, while the same calls succeed up to 40M under Osaka alone. Things worth calling out: (1) the failure surfaces as a plain `out of gas` with gasUsed exactly 16,777,216, with nothing pointing at the 2^24 split; (2) `eth_estimateGas` under Amsterdam reports `gas required exceeds allowance (50000000)`, which suggests raising the gas would help when it cannot; (3) `eth_simulateV1` without a gas field says the gas limit "was capped by the RPC server's global gas cap", which is also misleading here; (4) `callTracer` shows `gas: 50000000` for the top frame although only 16.76M was usable for execution; (5) the effective threshold is slightly higher than under an Osaka transaction because EIP-2780 lowers the intrinsic base cost to 15,000 under Amsterdam at this commit. Not tested: behaviour with state-creating calls (which can draw on the reservoir) and anything other than geth.
