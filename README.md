# AgentChain

A purpose-built L1 blockchain for AI agents. Fork of [go-ethereum v1.11.6](https://github.com/ethereum/go-ethereum/tree/v1.11.6) with RandomX proof-of-work consensus.

## What is AgentChain?

AgentChain is a standalone blockchain designed so that AI agents can mine, transact, and deploy smart contracts without managing private keys. It replaces Ethereum's Ethash with [RandomX](https://github.com/tevador/RandomX) — a CPU-friendly PoW algorithm — making it possible for any VPS or cloud instance to mine profitably.

The chain includes a custom `agent_*` RPC namespace that handles wallet creation, mining, and transaction signing inside the node itself. An AI agent only needs HTTP access to an RPC endpoint.

## Network Details

| Parameter | Value |
|---|---|
| **Network Name** | AgentChain |
| **Chain ID** | `7331` |
| **Currency** | CRD (18 decimals) |
| **Consensus** | RandomX PoW |
| **Block Time** | ~6 seconds |
| **Block Reward** | 2 CRD |
| **EVM Version** | Berlin |
| **Gas Limit** | 10M - 60M (dynamic) |
| **Min Gas Price** | 1 Gwei |
| **Premine** | Zero |
| **Public RPC** | `http://165.232.86.29:8545` |
| **P2P Port** | `30303` |
| **Explorer** | [agentchain.org/explorer](https://agentchain.org/explorer) |
| **Docs** | [agentchain.org/docs](https://agentchain.org/docs) |

## What Changed from go-ethereum

AgentChain is a fork of go-ethereum v1.11.6. Below is every modification made to the upstream codebase.

### New packages

| Package | Description |
|---|---|
| `consensus/randomx/` | RandomX consensus engine — block sealing, header verification, difficulty adjustment, epoch/cache management, VM pool |
| `consensus/randomx/go-randomx/` | CGo bindings to the RandomX C library (prebuilt static libs for Linux, macOS, Windows) |
| `internal/ethapi/agent_api.go` | Key-free `agent_*` RPC namespace — `agent_createWallet`, `agent_send`, `agent_startMining`, `agent_stopMining`, etc. |

### Modified files (42 files, ~630 insertions, ~220 deletions)

**Consensus swap (Ethash to RandomX):**
- `consensus/ethash/consensus.go` — Disabled Ethash verification (replaced by RandomX)
- `consensus/ethash/difficulty.go` — Replaced difficulty algorithm with 6-second target using LWMA
- `core/block_validator.go` — Adapted block validation for RandomX
- `core/blockchain.go` — Chain engine wiring
- `core/headerchain.go` — Header verification routing
- `miner/miner.go` — Mining engine integration

**Chain parameters:**
- `params/config.go` — AgentChain chain config (Chain ID 7331, Berlin fork, block reward, difficulty)
- `params/bootnodes.go` — AgentChain bootnode list
- `params/network_params.go` — Network parameters
- `params/version.go` — Version string (`1.0.0-agentchain`)

**Transaction model:**
- `core/types/transaction.go` — Legacy transactions only (no EIP-1559), 1 Gwei minimum gas price
- `core/txpool/txpool.go` — Tx pool adapted for legacy-only model
- `core/vm/operations_acl.go` — Berlin-level gas costs

**Node & networking:**
- `cmd/geth/main.go`, `cmd/geth/config.go`, `cmd/utils/flags.go` — CLI flags for RandomX, agent API
- `node/defaults.go` — Default data directory (`agentchain/`)
- `eth/backend.go`, `eth/handler.go` — Engine wiring
- `eth/ethconfig/config.go` — Default config
- `p2p/peer.go` — Peer connection settings

**RPC & API:**
- `internal/ethapi/api.go`, `internal/ethapi/backend.go` — Extended for agent namespace
- `rpc/handler.go` — Agent namespace registration
- `graphql/graphql.go` — Adapted for legacy tx model

**Gas & economics:**
- `consensus/misc/gaslimit.go` — Dynamic gas limit (10M-60M range)
- `core/genesis.go` — Genesis block configuration

### Unchanged

Everything else is unmodified go-ethereum v1.11.6 code. The upstream test suite, devp2p tools, `abigen`, `clef`, `bootnode`, `evm`, and `rlpdump` utilities all remain as-is.

## Building from Source

### Prerequisites

- **Go 1.20+**
- **GCC/G++** (for RandomX CGo bindings)
- Linux, macOS, or Windows (with MinGW-w64)

### Linux / macOS

```bash
git clone https://github.com/AgentChain-dev/agentchain.git
cd agentchain
go build -o geth ./cmd/geth
```

### Windows

Requires MinGW-w64 (GCC toolchain):

```bash
export CGO_ENABLED=1
export CC=gcc CXX=g++
go build -o geth.exe ./cmd/geth
```

## Running a Node

### Connect to AgentChain

```bash
./geth --http --http.addr 0.0.0.0 --http.port 8545 \
  --http.api eth,net,web3,agent,miner,txpool \
  --http.corsdomain "*" --http.vhosts "*" \
  --bootnodes "enode://20cb9c788f661791da5d42291ed3b4230f7550c50e6829375c992343842efa1c6db7137b8c85a16f7b5d0981266579a485c31abd53897beba5c538dfdd3c36cf@165.232.86.29:30303"
```

### Start Mining

Using the `agent_*` API (no private keys needed):

```bash
# Create a wallet
curl -X POST http://localhost:8545 \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"agent_createWallet","params":[],"id":1}'

# Start mining (1 thread)
curl -X POST http://localhost:8545 \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"agent_startMining","params":["0xYOUR_ADDRESS", 1],"id":1}'
```

Or using the traditional geth flags:

```bash
./geth --mine --miner.threads=1 --miner.etherbase=0xYOUR_ADDRESS \
  --http --http.api eth,net,web3,agent,miner,txpool \
  --bootnodes "enode://20cb9c788f661791da5d42291ed3b4230f7550c50e6829375c992343842efa1c6db7137b8c85a16f7b5d0981266579a485c31abd53897beba5c538dfdd3c36cf@165.232.86.29:30303"
```

### Hardware Requirements

| | Minimum | Recommended |
|---|---|---|
| **CPU** | 2 cores | 4+ cores |
| **RAM** | 4 GB | 8+ GB |
| **Storage** | 10 GB | 50 GB SSD |
| **Network** | 5 Mbit/s | 25+ Mbit/s |

RandomX dataset initialization requires ~2 GB of RAM. The chain data is currently small (under 1 GB).

## Agent API

The `agent_*` RPC namespace lets AI agents interact with the blockchain without ever handling private keys:

| Method | Description |
|---|---|
| `agent_createWallet` | Create a new wallet (key stays inside the node) |
| `agent_listWallets` | List all agent wallets |
| `agent_getBalance` | Get CRD balance for an address |
| `agent_send` | Send CRD from an agent wallet |
| `agent_startMining` | Start mining to an address |
| `agent_stopMining` | Stop mining |
| `agent_getMiningStatus` | Check mining status and hashrate |

Full API documentation: [agentchain.org/docs/for-agents/agent-api](https://agentchain.org/docs/for-agents/agent-api)

## Links

- **Website & Docs:** [agentchain.org](https://agentchain.org)
- **Block Explorer:** [agentchain.org/explorer](https://agentchain.org/explorer)
- **Miner App (Desktop):** [AgentChain-dev/agentchain-miner](https://github.com/AgentChain-dev/agentchain-miner)
- **Public RPC:** `http://165.232.86.29:8545`

## License

This project is a fork of [go-ethereum](https://github.com/ethereum/go-ethereum) and inherits its license:

- The library code (everything outside `cmd/`) is licensed under [LGPL-3.0](COPYING.LESSER).
- The binaries (`cmd/`) are licensed under [GPL-3.0](COPYING).

The `consensus/randomx/go-randomx/` package includes bindings based on [core-coin/go-randomx](https://github.com/core-coin/go-randomx), which is also LGPL-3.0 licensed.
