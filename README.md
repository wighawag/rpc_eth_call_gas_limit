# rpc_eth_call_gas_limit

Investigations into how much gas `eth_call` and related RPC simulations can actually use.

## Investigations

- [`eip8037-eth-call-cap/`](eip8037-eth-call-cap/): with Amsterdam (EIP-8037) active, go-ethereum's `eth_call`, `debug_traceCall` and non-strict `eth_simulateV1` get at most `2^24 - intrinsic_gas` of execution gas, even when given 50M. Views that compute more than about 16.76M gas work on Osaka and fail with `out of gas` after the fork. Confirmed on a real geth node over HTTP at commit `f8f9bc57`. See [RESULTS.md](eip8037-eth-call-cap/RESULTS.md) for the numbers and [README.md](eip8037-eth-call-cap/README.md) to run it.

## License

AGPL-3.0-only.
