# Position Manager — Non-Custodial DEX Limit Order Engine

Go library for automated SL/TP position management on EVM DEXes. Executes keeper-driven swaps via Uniswap V3 and PancakeSwap V3 with Permit2 authorization. Zero-gas order management — all SL/TP changes are off-chain DB updates.

## Architecture

```
User's Device (passkey / keychain / external wallet)
       │
       ├── [1] approve(Permit2, MAX)         one-time per token
       └── [2] sign EIP-712 PermitSingle     per position (gasless)
               │
               ▼
       Host App API (prepare/submit pattern)
               │
               ▼
       Position Manager Library (this repo)
       ├── Manager      — CRUD, state machine, permit lifecycle
       ├── TriggerEngine — O(log n) price-based matching
       ├── Executor      — keeper TX submission, gas strategy
       └── PermitManager — EIP-712 validation, Permit2 activation
               │
               ▼
       SwapExecutorV2 Contract (deployed per chain)
       ├── Permit2.transferFrom  → pull user tokens
       ├── Fee deduction         → treasury
       └── Uniswap/PancakeSwap  → tokens to user
```

**Non-custodial:** Tokens transit in a single TX. Swap output goes directly to the user (`recipient = user`). Server never holds user keys.

## Supported Chains

| Chain | DEX | Router | Fee Tiers |
|-------|-----|--------|-----------|
| Ethereum (1) | Uniswap V3 | `0x68b3465833fb72A70ecDF485E0e4C7bD8665Fc45` | 500, 3000, 10000 |
| BSC (56) | PancakeSwap V3 | `0x13f4EA83D0bd40E75C8222255bc855a974568Dd4` | 100, 500, 2500, 10000 |
| Base (8453) | Uniswap V3 | `0x2626664c2603336E57B271c5C0b26F421741e481` | 500, 3000, 10000 |

Permit2: `0x000000000022D473030F116dDEE9F6B43aC78BA3` (canonical, all chains)

## Quick Start

```go
import pm "github.com/ratexai/dex-limit-order-manager/pkg/positionmanager"

mgr, err := pm.New(pm.Config{
    Store:       myStore,            // host provides (PostgreSQL, BoltDB, etc.)
    PriceFeed:   myPriceFeed,        // host provides or use UniswapV3PriceFeed
    FeeProvider: myFeeService,       // host provides (per-user fee tiers)
    Chains: map[uint64]pm.ChainInstance{
        1:    {Client: ethClient, KeeperKey: key, ExecutorAddress: addr, ChainConfig: pm.EthereumDefaults()},
        56:   {Client: bscClient, KeeperKey: key, ExecutorAddress: addr, ChainConfig: pm.BSCDefaults()},
        8453: {Client: baseClient, KeeperKey: key, ExecutorAddress: addr, ChainConfig: pm.BaseDefaults()},
    },
    OnExecution:      func(e pm.ExecutionEvent) { /* log, notify, track referral */ },
    OnError:          func(e pm.ErrorEvent) { /* alert */ },
    OnPermitExpiring: func(e pm.PermitExpiryEvent) { /* notify user to renew */ },
})

go mgr.Run(ctx) // start keeper loop
```

### Open Position (with Permit2)

```go
pos, err := mgr.OpenPosition(ctx, pm.OpenParams{
    Owner:      userAddr,
    TokenBase:  weth,
    TokenQuote: usdc,
    Direction:  pm.Long,
    Size:       big.NewInt(1e18), // 1 ETH
    EntryPrice: big.NewInt(200000000000), // $2000, 8 decimals
    ChainID:    1,
    PoolFee:    3000,
    Levels: []pm.LevelParams{
        {Type: pm.LevelTypeSL, TriggerPrice: big.NewInt(180000000000), PortionBps: 10000},
        {Type: pm.LevelTypeTP, TriggerPrice: big.NewInt(220000000000), PortionBps: 3300, MoveSLTo: big.NewInt(200000000000)},
    },
    // Permit2 — user signed at position creation time
    PermitSignature: sig,
    PermitNonce:     nonce,
    PermitDeadline:  deadline,
    // One-click: frontend silently signed approve TX
    SignedApproveTx: signedApproveTxBytes, // or nil if already approved
})
```

### Zero-Gas SL Movement

```go
// Move SL from $1800 to $2000 — just a DB update, 0 gas
err := mgr.UpdateLevel(ctx, posID, 0, big.NewInt(200000000000))
```

### Market Swap (Permit2 SignatureTransfer)

```go
result, err := mgr.MarketSwap(ctx, pm.MarketSwapParams{
    Owner:           userAddr,
    TokenIn:         weth,
    TokenOut:        usdc,
    AmountIn:        big.NewInt(5e17),
    ChainID:         56,
    PoolFee:         2500,
    SlippageBps:     50,
    PermitSignature: sig,
    PermitNonce:     nonce,
    PermitDeadline:  deadline,
    SignedApproveTx: signedApproveTxBytes,
})
```

## Execution Modes

| Mode | Token Pull Method | Use Case |
|------|------------------|----------|
| `ExecModeLegacy` | Direct `ERC20.transferFrom` | Backward compat (user approved SwapExecutor) |
| `ExecModePermit2Allowance` | `Permit2.transferFrom` | Multi-level positions (SL/TP) |
| `ExecModePermit2Signature` | `Permit2.permitTransferFrom` | Single-use market swaps |

## Project Structure

```
contracts/
  SwapExecutor.sol          Legacy executor (backward compat)
  SwapExecutorV2.sol        Permit2-integrated executor + native ETH

pkg/positionmanager/        All library code in one package
  manager.go                Manager — CRUD, Run(), state machine, permit lifecycle
  trigger.go                TriggerEngine — sorted index, O(log n) matching
  executor.go               On-chain TX execution, gas strategy, nonce management
  permit.go                 EIP-712 typed data, ecrecover, permit validation
  permit2_abi.go            Permit2 + SwapExecutorV2 ABI bindings
  executor_abi.go           SwapExecutor V1 ABI bindings
  types.go                  Position, Level, OpenParams, enums
  config.go                 ChainConfig, EthereumDefaults, BSCDefaults, BaseDefaults
  events.go                 ExecutionEvent, ErrorEvent, PermitExpiryEvent
  fees.go                   FeeProvider interface, fee calculation
  store.go                  Store interface (host implements)
  pricefeed.go              PriceFeed interface
  pricefeed_uniswapv3.go    Reference impl: Uniswap V3 slot0 + TWAP
  chain.go                  ChainClient interface (go-ethereum compatible)
  circuitbreaker.go         Circuit breaker for executor resilience
  ratelimiter.go            RPC rate limiter (token bucket)
  metrics.go                MetricsCollector interface
  bps.go                    Basis points arithmetic

docs/
  POSITION_MANAGER_V3.md    Full architecture document
  AUTH_BOUNDARY.md           Security responsibility matrix
```

## Key Design Decisions

- **Library, not service** — integrates into host app via interfaces, no HTTP server
- **Permit2 over unlimited approve** — per-position amount caps, 50x blast radius reduction on key compromise
- **AllowanceTransfer for positions** — one signature covers all SL/TP levels, preserves zero-gas SL movement
- **SignatureTransfer for market swaps** — single-use, short deadline (5 min)
- **One-click UX** — frontend silently signs approve + Permit2 (passkey/keychain), keeper pays all gas
- **V3 now, V4 ready** — SwapExecutorV2 targets V3 routers. V4 support via SwapExecutorV4 when liquidity shifts

## Dependencies

```
github.com/ethereum/go-ethereum v1.14.12  (only external dependency)
```

Solidity: OpenZeppelin v5.x (SafeERC20, ReentrancyGuard)

## Security Model

See [AUTH_BOUNDARY.md](docs/AUTH_BOUNDARY.md) for the full responsibility matrix.

- **Host** handles: user auth (JWT), wallet ownership, rate limiting, keeper key management
- **Library** handles: trigger engine, execution, state machine, permit validation
- **On-chain** enforces: keeper-only guard, fee cap (5%), token allowance checks, reentrancy protection

## License

MIT
