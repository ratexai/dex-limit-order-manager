# SwapVM Limit Orders: Technical Specification

## Overview

This document provides the complete technical specification for integrating
1inch SwapVM limit orders into our Go financial layer. SwapVM replaces the
legacy Limit Order Protocol v4 with a bytecode-programmable VM that executes
swap strategies on-chain.

**Contract address (all chains):** `0x8fdd04dbf6111437b44bbca99c28882434e0958f`

**Supported chains:** Ethereum, Base, Optimism, Polygon, Arbitrum, Avalanche,
BSC, Linea, Sonic, Unichain, Gnosis, zkSync

**Router for limit orders:** `LimitSwapVMRouter`

---

## 1. Core Data Structures

### 1.1 Order (Solidity)

```solidity
struct Order {
    address maker;       // 20 bytes - liquidity provider
    MakerTraits traits;  // uint256  - packed bitfield (see 1.2)
    bytes data;          // variable - hook data + program bytecode
}
```

### 1.2 MakerTraits Bitfield (uint256)

```
Bit 255:     SHOULD_UNWRAP_WETH
Bit 254:     USE_AQUA_INSTEAD_OF_SIGNATURE
Bit 253:     ALLOW_ZERO_AMOUNT_IN
Bit 252:     HAS_PRE_TRANSFER_IN_HOOK
Bit 251:     HAS_POST_TRANSFER_IN_HOOK
Bit 250:     HAS_PRE_TRANSFER_OUT_HOOK
Bit 249:     HAS_POST_TRANSFER_OUT_HOOK
Bit 248:     PRE_TRANSFER_IN_HOOK_HAS_TARGET
Bit 247:     POST_TRANSFER_IN_HOOK_HAS_TARGET
Bit 246:     PRE_TRANSFER_OUT_HOOK_HAS_TARGET
Bit 245:     POST_TRANSFER_OUT_HOOK_HAS_TARGET
Bits 244-225: Reserved
Bits 224-160: Order data slice indices (4 x uint16 = 64 bits)
Bits 159-0:   Receiver address (address(0) = maker)
```

**Slice indices** (bits 224-160, as uint64):
- `[0:15]`  = end of PreTransferInHook data
- `[16:31]` = end of PostTransferInHook data
- `[32:47]` = end of PreTransferOutHook data
- `[48:63]` = end of PostTransferOutHook data

Program bytecode starts at `sliceIndex[3]` through `data.length`.

**Simple limit order (no hooks):** all hook bits = 0, all slice indices = 0,
`order.data` = raw program bytecode, receiver = `address(0)` (defaults to maker).

### 1.3 TakerTraits (uint176 = 22 bytes header)

Wire format of `takerTraitsAndData`:
```
[20 bytes: sliceIndices (uint160)] [2 bytes: flags (uint16)] [variable: takerData]
```

**Flags:**
```
Bit 0 (0x0001): IS_EXACT_IN
Bit 1 (0x0002): SHOULD_UNWRAP_WETH
Bit 2 (0x0004): HAS_PRE_TRANSFER_IN_CALLBACK
Bit 3 (0x0008): HAS_PRE_TRANSFER_OUT_CALLBACK
Bit 4 (0x0010): IS_STRICT_THRESHOLD
Bit 5 (0x0020): IS_FIRST_TRANSFER_FROM_TAKER
Bit 6 (0x0040): USE_TRANSFER_FROM_AND_AQUA_PUSH
```

**Slice indices** (uint160, 10 x uint16):
```
Index 0: End of Threshold (0 or 32 bytes)
Index 1: End of To (0 or 20 bytes)
Index 2: End of Deadline (0 or 5 bytes, uint40)
Index 3: End of PreTransferInHookData
Index 4: End of PostTransferInHookData
Index 5: End of PreTransferOutHookData
Index 6: End of PostTransferOutHookData
Index 7: End of PreTransferInCallbackData
Index 8: End of PreTransferOutCallbackData
Index 9: End of InstructionsArgs
```

Everything after index 9 through end of takerData = **Signature**.

### 1.4 SwapRegisters (VM state)

```
balanceIn       uint256   // balance of tokenIn (set by Balances instruction)
balanceOut      uint256   // balance of tokenOut
amountIn        uint256   // computed input amount
amountOut       uint256   // computed output amount
amountNetPulled uint256   // net pulled from maker (Aqua accounting)
```

---

## 2. EIP-712 Signing

### 2.1 TypeHash

```
ORDER_TYPEHASH = keccak256(
    "Order(address maker,uint256 traits,bytes data)"
)
```

### 2.2 Struct Hash

```
structHash = keccak256(abi.encode(
    ORDER_TYPEHASH,
    order.maker,
    uint256(order.traits),
    keccak256(order.data)
))
```

### 2.3 Order Hash (EIP-712)

```
orderHash = keccak256("\x19\x01" || domainSeparator || structHash)
```

### 2.4 Domain Separator

```
domainSeparator = keccak256(abi.encode(
    keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"),
    keccak256(bytes(name)),     // LimitSwapVMRouter deploy param
    keccak256(bytes(version)),  // LimitSwapVMRouter deploy param
    chainId,
    contractAddress             // 0x8fdd04dbf6111437b44bbca99c28882434e0958f
))
```

> **Note:** In tests the domain uses `name="SwapVM"`, `version="1.0.0"`.
> For production, read the actual values from the contract's
> `eip712Domain()` function (EIP-5267) on each chain to be safe.

### 2.5 Signature

Standard ECDSA over `orderHash`:
- Output: `r (32 bytes) || s (32 bytes) || v (1 byte)` = 65 bytes
- Also supports EIP-1271 (smart contract wallets)

---

## 3. Bytecode Program Format

Each instruction:
```
[1 byte: opcode] [1 byte: argsLength] [argsLength bytes: args]
```

VM executes sequentially from PC=0. Max jump target: 65535 (uint16).

### 3.1 Limit Order Opcodes

| Opcode | Name                    | Args                                               |
|--------|-------------------------|-----------------------------------------------------|
| 17     | `_staticBalancesXD`     | uint16 tokensCount + (20*N) tokens + (32*N) balances |
| 18     | `_invalidateBit1D`      | uint32 bitIndex (one-time order)                     |
| 19     | `_invalidateTokenIn1D`  | none (partial fill by tokenIn)                       |
| 20     | `_invalidateTokenOut1D` | none (partial fill by tokenOut)                      |
| 21     | `_limitSwap1D`          | 1 byte: bool makerDirectionLt                        |
| 22     | `_limitSwapOnlyFull1D`  | 1 byte: bool makerDirectionLt                        |
| 23     | `_requireMinRate1D`     | uint64 rateLt + uint64 rateGt                        |
| 30     | `_salt`                 | arbitrary bytes (hash uniqueness)                    |
| 31     | `_flatFeeAmountInXD`    | uint32 feeBps (1e9 = 100%)                           |
| 13     | `_deadline`             | uint40 timestamp                                     |

### 3.2 Limit Swap Math

`_limitSwap1D` with balances set by `_staticBalancesXD`:

```
Rate = balanceOut / balanceIn

ExactIn:  amountOut = floor(amountIn * balanceOut / balanceIn)
ExactOut: amountIn  = ceil(amountOut * balanceIn / balanceOut)
```

`_limitSwapOnlyFull1D` (opcode 22) — only allows full fills:
```
ExactIn:  requires amountIn == balanceIn,  then amountOut = balanceOut
ExactOut: requires amountOut == balanceOut, then amountIn  = balanceIn
```

Rounding always favors the maker.

---

## 4. Program Templates

### 4.1 One-Time Limit Order

```
_invalidateBit1D(bitIndex)        // one-time execution guard
_staticBalancesXD(tokens, bals)   // set exchange rate
_limitSwap1D(makerDirectionLt)    // compute swap amounts
```

### 4.2 Partial Fill Limit Order

```
_staticBalancesXD(tokens, bals)   // set exchange rate
_limitSwap1D(makerDirectionLt)    // compute swap amounts
_invalidateTokenOut1D()           // track partial fills
```

### 4.3 Limit Order with Deadline

```
_deadline(timestamp)              // revert after expiry
_invalidateBit1D(bitIndex)        // one-time guard
_staticBalancesXD(tokens, bals)   // set exchange rate
_limitSwap1D(makerDirectionLt)    // compute swap amounts
```

### 4.4 Limit Order with Min Rate + Fee

```
_flatFeeAmountInXD(feeBps)        // MUST be before swap instruction
  _deadline(timestamp)
  _invalidateBit1D(bitIndex)
  _staticBalancesXD(tokens, bals)
  _requireMinRate1D(rateLt, rateGt)
  _limitSwap1D(makerDirectionLt)
```

> Note: `_flatFeeAmountInXD` wraps remaining program via `ctx.runLoop()`,
> so it must be placed BEFORE all other instructions.

---

## 5. Token Transfer Flow

### 5.1 Approvals Required

| Party  | Approves         | For Token | To Contract |
|--------|------------------|-----------|-------------|
| Maker  | `tokenOut`       | unlimited | SwapVM addr |
| Taker  | `tokenIn`        | unlimited | SwapVM addr |

### 5.2 Transfer Order

Controlled by `IS_FIRST_TRANSFER_FROM_TAKER` (taker flag bit 5):

**If set (recommended):**
1. Taker sends `amountIn` of `tokenIn` to maker's receiver
2. Maker sends `amountOut` of `tokenOut` to taker's `to` address

**If not set:**
1. Maker sends first
2. Taker sends second

### 5.3 WETH Unwrapping

- Maker flag bit 255: unwrap received WETH to ETH
- Taker flag bit 1: unwrap received WETH to ETH

---

## 6. Order Invalidation

### 6.1 Bit Invalidator (One-Time)

- Storage: `mapping(maker => mapping(slotIndex => bitmap))`
- `slotIndex = bitIndex >> 8`
- `bit = 1 << (bitIndex & 0xFF)`
- Each bit can be used once; reverts if already set

### 6.2 Token Invalidator (Partial Fills)

- Storage: `mapping(maker => mapping(orderHash => mapping(token => filled)))`
- Tracks cumulative filled amount
- Reverts when `prefilled + amount > balance`

### 6.3 Manual Cancellation

Maker can call directly on SwapVM contract:
- `invalidateBit(bitIndex)` - cancel one-time order
- `invalidateTokenIn(orderHash, tokenIn)` - cancel partial fill order
- `invalidateTokenOut(orderHash, tokenOut)` - cancel partial fill order

---

## 7. Contract ABI (Relevant Functions)

```solidity
// Read-only: preview swap amounts
function quote(
    Order calldata order,
    address tokenIn,
    address tokenOut,
    uint256 amount,
    bytes calldata takerTraitsAndData
) external view returns (uint256 amountIn, uint256 amountOut);

// Execute swap
function swap(
    Order calldata order,
    address tokenIn,
    address tokenOut,
    uint256 amount,
    bytes calldata takerTraitsAndData
) external returns (uint256 amountIn, uint256 amountOut);

// Compute EIP-712 hash
function hash(Order calldata order) external view returns (bytes32);

// Cancellation
function invalidateBit(uint256 bitIndex) external;
function invalidateTokenIn(bytes32 orderHash, address tokenIn) external;
function invalidateTokenOut(bytes32 orderHash, address tokenOut) external;
```

---

## 8. Security Considerations

### 8.1 Known Open Issues (as of March 2026)

1. **Division-by-zero in DutchAuction** - not relevant for simple limit orders
2. **Decimal normalization in OraclePriceAdjuster** - not relevant
3. **Underflow in BaseFeeAdjuster** - not relevant
4. **Fee-on-transfer token mismatch** - avoid fee-on-transfer tokens

### 8.2 Safety Rules for Our Implementation

- Use only `_staticBalancesXD` + `_limitSwap1D` + invalidators (audited subset)
- Always set `IS_FIRST_TRANSFER_FROM_TAKER` flag
- Always set `IS_STRICT_THRESHOLD` with a threshold value
- Validate `makerDirectionLt` matches actual token address ordering
- Read `domainSeparator` from contract, never hardcode
- Verify `quote()` before submitting `swap()` transactions
- Set reasonable `deadline` on all orders

### 8.3 Reentrancy Protection

SwapVM uses per-orderHash transient locks. Each orderHash can only be entered
once per transaction. Safe against reentrancy.

---

## 9. Go Implementation Architecture

```
pkg/swapvm/
  types/          # Order, MakerTraits, TakerTraits structs
    order.go      # Order struct and ABI encoding
    maker_traits.go  # MakerTraits bitfield builder
    taker_traits.go  # TakerTraits bitfield + data builder
  program/        # Bytecode program builder
    builder.go    # Fluent API for building programs
    opcodes.go    # Opcode constants and encoders
  signer/         # EIP-712 signing
    eip712.go     # TypeHash, domain separator, signing
  client.go       # High-level: create, sign, quote, swap, cancel
  abi.go          # Contract ABI bindings
```
