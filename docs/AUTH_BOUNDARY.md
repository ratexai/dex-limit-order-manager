# Owner Authentication Boundary

## Overview

The position manager library is designed as a **headless library** that
integrates into a host application (e.g. RateXAI finance layer). This document
clarifies the security boundary between the library and the host regarding
**owner authentication and authorization**.

## Responsibility Matrix

| Concern                        | Responsible | Notes |
|-------------------------------|-------------|-------|
| User authentication (JWT, sessions) | **Host** | Library has no concept of sessions |
| Wallet ownership verification  | **Host** | Verify msg.sender / signatures before calling lib |
| Position ownership checks      | **Host** | Ensure `params.Owner == authenticatedUser` |
| Rate limiting per user         | **Host** | Protect against abuse at API layer |
| Fee tier resolution            | **Host** | Via `FeeProvider` interface |
| Keeper key management          | **Host** | Inject via `Config.Chains[].KeeperKey` |
| On-chain execution             | **Library** | Keeper EOA signs and submits txs |
| Trigger engine logic           | **Library** | Price matching and level management |
| Position state machine         | **Library** | SL/TP lifecycle transitions |
| Nonce management               | **Library** | Per-chain nonce tracking |

## Design Principle: Library Trusts the Host

The library **does not verify** that the caller is authorized to act on behalf
of `params.Owner`. It trusts that the host has already performed authentication
and authorization before calling any library method.

This is intentional:

1. **No network dependencies** — the library doesn't need access to auth
   services, JWT secrets, or session stores.
2. **Flexibility** — the host can use any auth mechanism (Web3 signatures,
   OAuth, API keys, etc.).
3. **Separation of concerns** — the library focuses on trading logic; the host
   handles user identity.

## Host Responsibilities in Detail

### Before `OpenPosition()`
```go
// Host must verify:
// 1. User is authenticated
// 2. User's wallet matches params.Owner
// 3. User has sufficient allowance/balance (optional, contract will revert)
// 4. Rate limiting / anti-spam checks
pos, err := manager.OpenPosition(ctx, params)
```

### Before `CancelPosition()` / `UpdateLevel()` / etc.
```go
// Host must verify:
// 1. User is authenticated
// 2. Position belongs to the authenticated user
pos, _ := manager.GetPosition(ctx, posID)
if pos.Owner != authenticatedUser {
    return ErrUnauthorized
}
err := manager.CancelPosition(ctx, posID)
```

### Keeper Key Security
The keeper private key is provided by the host at initialization. The host is
responsible for:
- Secure key storage (HSM, KMS, encrypted env vars)
- Key rotation procedures
- Monitoring keeper balance for gas
- Alerting on unauthorized usage

## On-Chain Authorization

The `SwapExecutor` smart contract enforces its own authorization:
- Only the designated `keeper` address can call `executeSwap()`
- The contract verifies that the user has granted token allowance
- Fee caps are enforced on-chain (`MAX_FEE_BPS = 500`)

This provides a defense-in-depth layer: even if the library is misused, the
on-chain contract limits the blast radius.

## Summary

```
┌──────────────────────────────────────────────┐
│                  HOST APP                     │
│  ┌─────────────┐  ┌──────────────────────┐   │
│  │ Auth Layer   │  │ API / Gateway        │   │
│  │ (JWT, Web3)  │→ │ (rate limit, authz)  │   │
│  └─────────────┘  └──────────┬───────────┘   │
│                              │                │
│  ┌───────────────────────────▼──────────────┐ │
│  │         Position Manager Library          │ │
│  │  • Trusts host for owner identity         │ │
│  │  • Manages triggers, execution, state     │ │
│  │  • Signs txs with injected keeper key     │ │
│  └───────────────────────────────────────────┘ │
│                              │                │
│  ┌───────────────────────────▼──────────────┐ │
│  │         SwapExecutor Contract             │ │
│  │  • On-chain keeper-only guard             │ │
│  │  • Token allowance checks                 │ │
│  │  • Fee cap enforcement                    │ │
│  └───────────────────────────────────────────┘ │
└──────────────────────────────────────────────┘
```
