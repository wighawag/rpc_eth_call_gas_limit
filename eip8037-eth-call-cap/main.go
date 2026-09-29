// Command repro checks whether go-ethereum's eth_call is limited to ~2^24
// execution gas once Amsterdam (EIP-8037) is active. Nothing in geth is
// modified; every call goes through geth's JSON-RPC server (internal/ethapi).
//
// Modes:
//
//	repro                        in-process: ethclient/simulated (+ a node built
//	                             the same way with the debug API, for traces)
//	repro -rpc URL -label NAME   against a running geth (see run-geth.sh)
//	repro -write-genesis DIR     write genesis-osaka.json / genesis-amsterdam.json
package main

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"

	// Register the native tracers (callTracer). The struct logger is built in.
	_ "github.com/ethereum/go-ethereum/eth/tracers/native"
)

//go:embed build/Burner.bin
var burnerInitHex string

//go:embed build/Burner.bin-runtime
var burnerRuntimeHex string

//go:embed build/Burner.abi
var burnerABIJSON string

// gasProbeRuntime is `GAS PUSH0 MSTORE PUSH1 0x20 PUSH0 RETURN`: it returns the
// gas left at its first instruction (after GAS itself costs 2).
const gasProbeRuntime = "5a5f5260205ff3"

// gasProbeInit deploys gasProbeRuntime:
// PUSH7 <runtime> PUSH0 MSTORE PUSH1 7 PUSH1 25 RETURN
const gasProbeInit = "66" + gasProbeRuntime + "5f52" + "6007" + "6019" + "f3"

const callGas = 50_000_000

// Loop counts for burn(n). Burner costs 21,500 + 120*n gas on Osaka (checked
// at run time by calibrate), so these give ~10M, 16.5M, 17M, 20M and 40M of
// total gas on Osaka. Under Amsterdam the same n costs 6,000 less (EIP-2780
// lowers the intrinsic base from 21,000 to 15,000); execution is identical.
var (
	targets = []string{"10.0M", "16.5M", "17.0M", "20.0M", "40.0M"}
	ns      = []uint64{83154, 137320, 141487, 166487, 333154}
)

const (
	expectedPerIter   = 120
	expectedOsakaBase = 21500
)

var (
	key, _   = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	sender   = crypto.PubkeyToAddress(key.PublicKey)
	burnerAt = common.HexToAddress("0x00000000000000000000000000000000000b0b0b") // override address
	probeAt  = common.HexToAddress("0x00000000000000000000000000000000000fe0fe") // override address
	burnABI  abi.ABI
)

type fork struct {
	name string
	cfg  *params.ChainConfig
}

func forks() []fork {
	// AllDevChainProtocolChanges is the simulated backend's (and geth --dev's)
	// default. At this commit it enables Bogota at 0 but leaves Amsterdam nil,
	// so clear Bogota and choose Amsterdam explicitly.
	osaka := *params.AllDevChainProtocolChanges
	osaka.BogotaTime = nil
	osaka.AmsterdamTime = nil

	ams := osaka
	zero := uint64(0)
	ams.AmsterdamTime = &zero
	return []fork{{"osaka", &osaka}, {"amsterdam", &ams}}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	genesisOut := flag.String("write-genesis", "", "write genesis-<fork>.json files into this directory and exit")
	rpcURL := flag.String("rpc", "", "run against a running node at this JSON-RPC URL (needs eth and debug APIs)")
	label := flag.String("label", "", "label for the -rpc run")
	flag.Parse()

	var err error
	burnABI, err = abi.JSON(strings.NewReader(burnerABIJSON))
	must(err)

	switch {
	case *genesisOut != "":
		writeGenesis(*genesisOut)
	case *rpcURL != "":
		printVersions()
		runAgainstNode(*rpcURL, *label)
	default:
		printVersions()
		runSimulated()
	}
}

func printVersions() {
	if info, ok := debug.ReadBuildInfo(); ok {
		fmt.Printf("client go: %s\n", info.GoVersion)
		for _, d := range info.Deps {
			if d.Path == "github.com/ethereum/go-ethereum" {
				fmt.Printf("client go-ethereum module: %s\n", d.Version)
			}
		}
	}
	fmt.Printf("params.MaxTxGas = %d\n\n", params.MaxTxGas)
}

// ---------------------------------------------------------------------------
// Genesis files for a real geth node.

func genesisFor(cfg *params.ChainConfig) *core.Genesis {
	alloc := core.SystemContractAllocs()
	alloc[sender] = types.Account{Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))}
	return &core.Genesis{
		Config:     cfg,
		GasLimit:   ethconfig.Defaults.Miner.GasCeil,
		BaseFee:    big.NewInt(params.InitialBaseFee),
		Difficulty: big.NewInt(0),
		Alloc:      alloc,
	}
}

func writeGenesis(dir string) {
	must(os.MkdirAll(dir, 0o755))
	for _, f := range forks() {
		b, err := json.MarshalIndent(genesisFor(f.cfg), "", "  ")
		must(err)
		p := filepath.Join(dir, "genesis-"+f.name+".json")
		must(os.WriteFile(p, append(b, '\n'), 0o644))
		fmt.Println("wrote", p)
	}
}

// ---------------------------------------------------------------------------
// Mode 1: in-process.

func runSimulated() {
	for _, f := range forks() {
		fmt.Printf("=== [%s] Part A: eth_call via ethclient/simulated, RPCGasCap default (%d) ===\n", f.name, ethconfig.Defaults.RPCGasCap)
		sim := simulated.NewBackend(
			types.GenesisAlloc{sender: {Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))}},
			func(_ *node.Config, ec *ethconfig.Config) { ec.Genesis.Config = f.cfg },
		)
		rpcc := rawRPC(sim)
		printForkInfo(rpcc)
		burner := deploy(rpcc, common.FromHex(strings.TrimSpace(burnerInitHex)), func() { sim.Commit() })
		probe := deploy(rpcc, common.FromHex(gasProbeInit), func() { sim.Commit() })
		fmt.Printf("deployed Burner at %s, GasProbe at %s\n", burner.Hex(), probe.Hex())
		callMatrix(rpcc, burner, probe)
		sim.Close()

		fmt.Printf("=== [%s] Part B: debug_traceCall on a node built like ethclient/simulated + tracers API ===\n", f.name)
		n := newTracedNode(f.cfg)
		printForkInfo(n.rpc)
		calibrate(n.rpc, burnerAt, overrides)
		traceMatrix(n.rpc, burnerAt, probeAt, overrides)
		n.close()
	}
}

// rawRPC returns the simulated backend's in-process RPC client, so eth_call
// gets exact JSON params. The concrete client type is
// simClient{*ethclient.Client}; its Client field shadows the promoted
// Client() method, so go through the struct field.
func rawRPC(sim *simulated.Backend) *rpc.Client {
	return reflect.ValueOf(sim.Client()).Field(0).Interface().(*ethclient.Client).Client()
}

// ---------------------------------------------------------------------------
// Mode 2: a running geth.

func runAgainstNode(url, label string) {
	rpcc, err := rpc.Dial(url)
	must(err)
	defer rpcc.Close()
	var ver string
	must(rpcc.Call(&ver, "web3_clientVersion"))
	fmt.Printf("=== [%s] geth over HTTP at %s (%s) ===\n", label, url, ver)
	printForkInfo(rpcc)
	burner := deploy(rpcc, common.FromHex(strings.TrimSpace(burnerInitHex)), nil)
	probe := deploy(rpcc, common.FromHex(gasProbeInit), nil)
	fmt.Printf("deployed Burner at %s, GasProbe at %s (latest block %s)\n", burner.Hex(), probe.Hex(), blockNumber(rpcc))
	calibrate(rpcc, burner, nil)
	callMatrix(rpcc, burner, probe)
	traceMatrix(rpcc, burner, probe, nil)
}

// ---------------------------------------------------------------------------
// Shared RPC helpers.

func gasLabel(withGas bool) string {
	if withGas {
		return "gas=50,000,000"
	}
	return "no gas field"
}

func callArgs(to common.Address, data []byte, withGas bool) map[string]any {
	args := map[string]any{"from": sender, "to": to}
	if data != nil {
		args["input"] = hexutil.Bytes(data)
	}
	if withGas {
		args["gas"] = hexutil.Uint64(callGas)
	}
	return args
}

func burnData(n uint64) []byte {
	data, err := burnABI.Pack("burn", new(big.Int).SetUint64(n))
	must(err)
	return data
}

func ethCall(c *rpc.Client, to common.Address, data []byte, withGas bool) ([]byte, error) {
	var out hexutil.Bytes
	err := c.Call(&out, "eth_call", callArgs(to, data, withGas), "latest")
	return out, err
}

func errCode(err error) int {
	if re, ok := err.(rpc.Error); ok {
		return re.ErrorCode()
	}
	return 0
}

func blockNumber(c *rpc.Client) string {
	var n hexutil.Uint64
	must(c.Call(&n, "eth_blockNumber"))
	return fmt.Sprint(uint64(n))
}

func printForkInfo(c *rpc.Client) {
	var resp struct {
		Current struct {
			ActivationTime uint64                     `json:"activationTime"`
			ForkID         string                     `json:"forkId"`
			Precompiles    map[string]json.RawMessage `json:"precompiles"`
		} `json:"current"`
		Next json.RawMessage `json:"next"`
	}
	if err := c.Call(&resp, "eth_config"); err != nil {
		fmt.Printf("eth_config: error %v\n", err)
		return
	}
	fmt.Printf("eth_config current: forkId=%s activationTime=%d precompiles=%d next=%s\n",
		resp.Current.ForkID, resp.Current.ActivationTime, len(resp.Current.Precompiles), string(resp.Next))
}

// deploy sends a signed creation transaction over JSON-RPC. commit (if not
// nil) seals a block, as the simulated backend needs; otherwise the receipt is
// polled (geth --dev seals on demand).
func deploy(c *rpc.Client, code []byte, commit func()) common.Address {
	var (
		nonce   hexutil.Uint64
		chainID hexutil.Big
		est     hexutil.Uint64
		head    struct {
			BaseFee *hexutil.Big `json:"baseFeePerGas"`
		}
	)
	must(c.Call(&nonce, "eth_getTransactionCount", sender, "pending"))
	must(c.Call(&chainID, "eth_chainId"))
	must(c.Call(&head, "eth_getBlockByNumber", "latest", false))
	must(c.Call(&est, "eth_estimateGas", map[string]any{"from": sender, "input": hexutil.Bytes(code)}, "latest"))
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID.ToInt(),
		Nonce:     uint64(nonce),
		GasTipCap: big.NewInt(params.GWei),
		GasFeeCap: new(big.Int).Add(new(big.Int).Mul(head.BaseFee.ToInt(), big.NewInt(2)), big.NewInt(params.GWei)),
		Gas:       uint64(est) * 2,
		Data:      code,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID.ToInt()), key)
	must(err)
	raw, err := signed.MarshalBinary()
	must(err)
	var hash common.Hash
	must(c.Call(&hash, "eth_sendRawTransaction", hexutil.Bytes(raw)))
	if commit != nil {
		commit()
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var rcpt *types.Receipt
		// geth may answer "transaction indexing is in progress" right after
		// start-up; treat any error as "not yet" until the deadline.
		err := c.Call(&rcpt, "eth_getTransactionReceipt", hash)
		if err == nil && rcpt != nil {
			if rcpt.Status != types.ReceiptStatusSuccessful {
				panic("deploy failed")
			}
			return rcpt.ContractAddress
		}
		if time.Now().After(deadline) {
			panic(fmt.Sprintf("deploy: no receipt after 30s (last error: %v)", err))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// callMatrix: GasProbe via eth_call, then eth_call, eth_simulateV1 and
// eth_estimateGas for every target, with gas=50M and with no gas field.
func callMatrix(c *rpc.Client, burner, probe common.Address) {
	for _, withGas := range []bool{true, false} {
		res, err := ethCall(c, probe, nil, withGas)
		if err != nil {
			fmt.Printf("GasProbe eth_call (%s): error %v\n", gasLabel(withGas), err)
			continue
		}
		fmt.Printf("GasProbe eth_call (%s): GAS at first opcode = %d\n", gasLabel(withGas), new(big.Int).SetBytes(res).Uint64())
	}

	fmt.Println("| target (Osaka work) | n | gas field | eth_call result | error | returned h |")
	fmt.Println("|---|---|---|---|---|---|")
	for i, t := range targets {
		for _, withGas := range []bool{true, false} {
			res, err := ethCall(c, burner, burnData(ns[i]), withGas)
			if err != nil {
				fmt.Printf("| %s | %d | %s | FAIL | `%s` (code %d) | |\n", t, ns[i], gasLabel(withGas), err.Error(), errCode(err))
			} else {
				fmt.Printf("| %s | %d | %s | ok | | 0x%s... |\n", t, ns[i], gasLabel(withGas), hex.EncodeToString(res)[:16])
			}
		}
	}

	for i, t := range targets {
		for _, withGas := range []bool{true, false} {
			var out []struct {
				Calls []struct {
					Status  hexutil.Uint64 `json:"status"`
					GasUsed hexutil.Uint64 `json:"gasUsed"`
					Error   *struct {
						Message string `json:"message"`
						Code    int    `json:"code"`
					} `json:"error"`
				} `json:"calls"`
			}
			err := c.Call(&out, "eth_simulateV1", map[string]any{
				"blockStateCalls": []any{map[string]any{"calls": []any{callArgs(burner, burnData(ns[i]), withGas)}}},
				"validation":      false,
			}, "latest")
			switch {
			case err != nil:
				fmt.Printf("eth_simulateV1 %s (n=%d, %s): rpc error `%v` (code %d)\n", t, ns[i], gasLabel(withGas), err, errCode(err))
			case len(out) == 0 || len(out[0].Calls) == 0:
				fmt.Printf("eth_simulateV1 %s: empty result\n", t)
			default:
				call := out[0].Calls[0]
				msg := ""
				if call.Error != nil {
					msg = fmt.Sprintf(" error `%s` (code %d)", call.Error.Message, call.Error.Code)
				}
				fmt.Printf("eth_simulateV1 %s (n=%d, %s): status=%d gasUsed=%d%s\n", t, ns[i], gasLabel(withGas), uint64(call.Status), uint64(call.GasUsed), msg)
			}
		}
	}

	for i, t := range targets {
		var est hexutil.Uint64
		if err := c.Call(&est, "eth_estimateGas", callArgs(burner, burnData(ns[i]), false), "latest"); err != nil {
			fmt.Printf("eth_estimateGas %s (n=%d): error `%v` (code %d)\n", t, ns[i], err, errCode(err))
		} else {
			fmt.Printf("eth_estimateGas %s (n=%d): %d\n", t, ns[i], uint64(est))
		}
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// Traces.

var overrides = map[string]any{
	burnerAt.Hex(): map[string]any{"code": "0x" + strings.TrimSpace(burnerRuntimeHex)},
	probeAt.Hex():  map[string]any{"code": "0x" + gasProbeRuntime},
}

type structResult struct {
	Gas         uint64 `json:"gas"`
	Failed      bool   `json:"failed"`
	ReturnValue string `json:"returnValue"`
	StructLogs  []struct {
		Pc  uint64 `json:"pc"`
		Op  string `json:"op"`
		Gas uint64 `json:"gas"`
	} `json:"structLogs"`
}

// traceStruct runs debug_traceCall with the default struct logger, limited to
// the first opcode so the output stays small.
func traceStruct(c *rpc.Client, to common.Address, data []byte, withGas bool, ov map[string]any) (*structResult, error) {
	cfg := map[string]any{"disableStack": true, "disableStorage": true, "limit": 1}
	if ov != nil {
		cfg["stateOverrides"] = ov
	}
	var res structResult
	err := c.Call(&res, "debug_traceCall", callArgs(to, data, withGas), "latest", cfg)
	return &res, err
}

type callFrame struct {
	Gas     hexutil.Uint64 `json:"gas"`
	GasUsed hexutil.Uint64 `json:"gasUsed"`
	Error   string         `json:"error"`
}

func traceCallTracer(c *rpc.Client, to common.Address, data []byte, withGas bool, ov map[string]any) (*callFrame, error) {
	cfg := map[string]any{"tracer": "callTracer"}
	if ov != nil {
		cfg["stateOverrides"] = ov
	}
	var res callFrame
	err := c.Call(&res, "debug_traceCall", callArgs(to, data, withGas), "latest", cfg)
	return &res, err
}

// calibrate measures gasUsed(n) = base + perIter*n with debug_traceCall and
// checks it against the constants the n values were derived from.
func calibrate(c *rpc.Client, burner common.Address, ov map[string]any) {
	used := func(k uint64) uint64 {
		r, err := traceStruct(c, burner, burnData(k), true, ov)
		must(err)
		if r.Failed {
			panic("calibration call failed")
		}
		return r.Gas
	}
	g1, g2, g3 := used(1000), used(2000), used(3000)
	perIter := (g3 - g2) / 1000
	base := g2 - 2000*perIter
	fmt.Printf("calibration (debug_traceCall): gasUsed(n) = %d + %d*n", base, perIter)
	if g2-g1 != g3-g2 {
		fmt.Printf(" [WARNING: non-linear %d %d %d]", g1, g2, g3)
	}
	if perIter != expectedPerIter {
		fmt.Printf(" [WARNING: expected %d per iteration]", expectedPerIter)
	}
	fmt.Printf(" (Osaka reference: %d + %d*n)\n", expectedOsakaBase, expectedPerIter)
}

func traceMatrix(c *rpc.Client, burner, probe common.Address, ov map[string]any) {
	for _, withGas := range []bool{true, false} {
		r, err := traceStruct(c, probe, nil, withGas, ov)
		if err != nil || len(r.StructLogs) == 0 {
			fmt.Printf("structLogger GasProbe (%s): error %v\n", gasLabel(withGas), err)
			continue
		}
		first := r.StructLogs[0]
		fmt.Printf("structLogger GasProbe (%s): first op %s pc=%d gas=%d; returned GAS=%d; total gasUsed=%d\n",
			gasLabel(withGas), first.Op, first.Pc, first.Gas, new(big.Int).SetBytes(common.FromHex(r.ReturnValue)).Uint64(), r.Gas)
	}

	fmt.Println("| target (Osaka work) | n | gas field | structLogger first-op gas | structLogger gasUsed | failed | callTracer gas | callTracer gasUsed | callTracer error |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|")
	for i, t := range targets {
		data := burnData(ns[i])
		for _, withGas := range []bool{true, false} {
			sr, err := traceStruct(c, burner, data, withGas, ov)
			if err != nil || len(sr.StructLogs) == 0 {
				fmt.Printf("| %s | %d | %s | trace error `%v` |\n", t, ns[i], gasLabel(withGas), err)
				continue
			}
			cr, err := traceCallTracer(c, burner, data, withGas, ov)
			if err != nil {
				fmt.Printf("| %s | %d | %s | trace error `%v` |\n", t, ns[i], gasLabel(withGas), err)
				continue
			}
			fmt.Printf("| %s | %d | %s | %d | %d | %v | %d | %d | %s |\n", t, ns[i], gasLabel(withGas),
				sr.StructLogs[0].Gas, sr.Gas, sr.Failed, uint64(cr.Gas), uint64(cr.GasUsed), cr.Error)
		}
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// In-process node with the debug API (Part B of mode 1).

type tracedNode struct {
	stack *node.Node
	rpc   *rpc.Client
}

func newTracedNode(cfg *params.ChainConfig) *tracedNode {
	// Mirrors simulated.NewBackend / newWithNode at this commit, plus the
	// tracers API (registered by cmd/utils for a real geth node).
	nodeConf := node.DefaultConfig
	nodeConf.DataDir = ""
	nodeConf.P2P = p2p.Config{NoDiscovery: true}
	stack, err := node.New(&nodeConf)
	must(err)

	ethConf := ethconfig.Defaults
	ethConf.Genesis = genesisFor(cfg)
	ethConf.SyncMode = ethconfig.FullSync
	ethConf.TxPool.NoLocals = true
	ethConf.LogNoHistory = true
	backend, err := eth.New(stack, &ethConf)
	must(err)
	stack.RegisterAPIs(tracers.APIs(backend.APIBackend))
	must(stack.Start())
	return &tracedNode{stack: stack, rpc: stack.Attach()}
}

func (n *tracedNode) close() { n.rpc.Close(); n.stack.Close() }
