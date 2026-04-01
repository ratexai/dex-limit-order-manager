# Position Manager v3: Go Library + Thin Executor

## Context

Реализация position manager'а как **Go библиотеки** (`pkg/positionmanager/`),
которая встраивается в существующий RateXAI finance layer. Finance layer —
агрегатор API: маркет ордера, on-chain кошельки, рефка, комиссии.
Position Manager — один из модулей.

Требования:
- **Go library** — чистые интерфейсы, dependency injection, без собственного HTTP/main
- **Без SwapVM, без 1inch** — прямые свапы на Uniswap V3
- **Non-custodial** — юзер хранит токены, keeper не может украсть
- **Гибкое управление ордерами** — SL movement = UPDATE в БД (0 gas)
- **Сети**: Ethereum (P0) + Base (P0)
- **Масштаб**: десятки тысяч ордеров
- **Комиссии** — per-user fee tier, вычитается до свапа

## Ключевое преимущество нового подхода

**Управление ордерами стоит 0 gas.** Все SL/TP уровни живут в off-chain БД.
Передвинуть SL с 1800 на 2000 = UPDATE в BoltDB. Не нужны pre-signed ордера,
не нужна отмена on-chain, не нужна подпись юзера. Keeper просто исполняет
свап когда цена дошла до триггера.

---

## Архитектура: Library Design

```
┌─────────────────────────────────────────────────────────────────┐
│                RateXAI Finance Layer (host app)                  │
│                                                                  │
│  ┌──────────┐  ┌──────────┐  ┌─────────┐  ┌───────────────┐  │
│  │ REST API │  │ Wallets  │  │ Referral│  │ Other modules │  │
│  │ (host)   │  │ (host)   │  │ (host)  │  │               │  │
│  └────┬─────┘  └──────────┘  └─────────┘  └───────────────┘  │
│       │                                                        │
│       ▼                                                        │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │  pkg/positionmanager  (наша библиотека)                  │  │
│  │                                                          │  │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐              │  │
│  │  │ Manager  │→ │ Trigger  │→ │ Executor │              │  │
│  │  │          │  │ Engine   │  │          │              │  │
│  │  └──────────┘  └──────────┘  └──────────┘              │  │
│  │       │              ↑              │                    │  │
│  │       ▼              │              ▼                    │  │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐              │  │
│  │  │ Store    │  │ PriceFeed│  │ Chain    │              │  │
│  │  │(interface)│ │(interface)│ │(interface)│              │  │
│  │  └──────────┘  └──────────┘  └──────────┘              │  │
│  └─────────────────────────────────────────────────────────┘  │
│       │              │              │                          │
│       ▼              ▼              ▼                          │
│  Host provides implementations:                                │
│  - Store → BoltDB / PostgreSQL / whatever host uses            │
│  - PriceFeed → host's existing price service                   │
│  - ChainClient → host's existing RPC/wallet infra              │
└─────────────────────────────────────────────────────────────────┘
```

### Принцип: dependency injection через интерфейсы

Библиотека **НЕ** владеет:
- HTTP сервером (host app предоставляет роуты)
- RPC соединениями (host app передаёт `*ethclient.Client`)
- БД (host app передаёт реализацию `Store` interface)
- Price feed (host app передаёт реализацию `PriceFeed` interface)
- Ключами keeper'а (host app передаёт `*ecdsa.PrivateKey` или signer interface)

Библиотека **владеет**:
- Trigger engine (sorted index, matching logic)
- Position state machine (CRUD, transitions)
- Execution logic (build calldata, gas strategy)
- Fee calculation

### Использование из host app:

```go
import pm "github.com/ratexai/dex-limit-order-manager/pkg/positionmanager"

// Host предоставляет зависимости
mgr, err := pm.New(pm.Config{
    Store:       myBoltStore,           // или PostgreSQL, или что угодно
    PriceFeed:   myPriceFeedService,    // host's existing price feed
    ChainClient: myEthClient,           // host's existing RPC
    KeeperKey:   myKeeperPrivateKey,    // keeper EOA
    Chains: map[uint64]pm.ChainConfig{
        1:    pm.EthereumDefaults(),    // с preset'ами
        8453: pm.BaseDefaults(),
    },
    FeeProvider: myFeeService,    // host's fee/tier logic
    OnExecution: func(event pm.ExecutionEvent) {
        // event.Fee содержит FeeResult: totalFee, platformShare, referralShare, referrer
        // host записывает: P&L, реферальный долг, аналитика, уведомления
    },
})

// Запуск keeper loop (в горутине host'а)
go mgr.Run(ctx)

// Из REST API handler'а host'а:
pos, err := mgr.OpenPosition(ctx, pm.OpenParams{...})
err = mgr.UpdateLevel(ctx, posID, levelIdx, newTriggerPrice)
err = mgr.CancelPosition(ctx, posID)
positions, err := mgr.ListPositions(ctx, owner, pm.StateActive)
```

---

## On-Chain: Thin SwapExecutor (~50 строк Solidity)

Единственный контракт, который нужно задеплоить. Минимальный, аудируемый за часы.

```solidity
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

interface ISwapRouter {
    struct ExactInputSingleParams {
        address tokenIn;
        address tokenOut;
        uint24 fee;
        address recipient;
        uint256 amountIn;
        uint256 amountOutMinimum;
        uint160 sqrtPriceLimitX96;
    }
    function exactInputSingle(ExactInputSingleParams calldata) external returns (uint256);
}

contract SwapExecutor is ReentrancyGuard {
    using SafeERC20 for IERC20;

    address public immutable keeper;
    address public immutable swapRouter;     // Uniswap V3 SwapRouter02
    address public immutable feeCollector;   // куда идёт комиссия

    uint256 public constant MAX_FEE_BPS = 500; // макс 5% (защита от ошибки)

    constructor(address _keeper, address _swapRouter, address _feeCollector) {
        keeper = _keeper;
        swapRouter = _swapRouter;
        feeCollector = _feeCollector;
    }

    /// @notice Execute swap on behalf of user with platform fee.
    /// @param feeBps Platform fee in basis points (100 = 1%). 0 = no fee.
    ///        Fee is deducted from amountIn BEFORE the swap.
    ///        Different users have different fee tiers — keeper passes the rate.
    function executeSwap(
        address user,
        address tokenIn,
        address tokenOut,
        uint24  poolFee,
        uint256 amountIn,
        uint256 minAmountOut,
        uint16  feeBps         // комиссия платформы (bps), задаётся per-swap
    ) external nonReentrant returns (uint256 amountOut) {
        require(msg.sender == keeper, "unauthorized");
        require(feeBps <= MAX_FEE_BPS, "fee too high");

        // Pull tokens from user
        IERC20(tokenIn).safeTransferFrom(user, address(this), amountIn);

        // Deduct platform fee before swap
        uint256 feeAmount;
        uint256 swapAmount = amountIn;
        if (feeBps > 0) {
            feeAmount = amountIn * feeBps / 10000;
            swapAmount = amountIn - feeAmount;
            IERC20(tokenIn).safeTransfer(feeCollector, feeAmount);
        }

        // Approve router
        IERC20(tokenIn).forceApprove(swapRouter, swapAmount);

        // Swap — result goes directly to user
        amountOut = ISwapRouter(swapRouter).exactInputSingle(
            ISwapRouter.ExactInputSingleParams({
                tokenIn:           tokenIn,
                tokenOut:          tokenOut,
                fee:               poolFee,
                recipient:         user,      // ← токены НАПРЯМУЮ юзеру
                amountIn:          swapAmount,
                amountOutMinimum:  minAmountOut,
                sqrtPriceLimitX96: 0
            })
        );
    }
}
```

### Модель комиссии (полная)

**Принцип**: контракт минимален (берёт fee, отправляет на treasury). Вся логика
тиров, рефералки, выплат — off-chain через `FeeProvider` interface.

**On-chain (контракт)**:
- `feeBps` передаётся keeper'ом per-swap (keeper знает тариф юзера)
- Комиссия вычитается из `amountIn` ДО свапа → идёт на `feeCollector` (treasury)
- `MAX_FEE_BPS = 500` (5%) — hard cap, защита от ошибки
- `feeCollector` — immutable, задаётся при деплое

**Off-chain (библиотека + host)**:
```go
// pkg/positionmanager/fees.go

// FeeProvider — host app реализует этот интерфейс.
// Библиотека вызывает его перед каждым свапом.
type FeeProvider interface {
    // GetFee returns the fee config for a specific user.
    // Host determines the tier based on user's tariff, volume, etc.
    GetFee(ctx context.Context, user common.Address) (*FeeConfig, error)
}

type FeeConfig struct {
    FeeBps        uint16          // комиссия платформы (bps). 100 = 1%
    ReferrerShare uint16          // доля реферера от комиссии (bps). 3000 = 30% от fee
    Referrer      common.Address  // адрес реферера (zero = нет реферера)
}

// FeeResult — результат после исполнения свапа, для учёта.
type FeeResult struct {
    TotalFee      *big.Int        // сколько взяли комиссии (in tokenIn)
    PlatformShare *big.Int        // доля платформы
    ReferralShare *big.Int        // доля реферера (для выплаты)
    Referrer      common.Address
}
```

**Поток**:
1. Trigger → Executor готовит свап
2. Executor вызывает `FeeProvider.GetFee(user)` → получает `FeeConfig{FeeBps: 80, ReferrerShare: 3000, Referrer: 0xABC}`
3. Executor вызывает `SwapExecutor.executeSwap(..., feeBps=80)` → 0.8% уходит на treasury
4. Executor вычисляет `FeeResult`: totalFee=0.8%, platformShare=70% от fee, referralShare=30% от fee
5. Executor вызывает `OnExecution` callback → host получает `FeeResult` и записывает:
   - Комиссия для P&L учёта
   - Реферальный долг к выплате для 0xABC
6. Host app выплачивает рефералку отдельно (batch, weekly, whatever)

**Почему рефералка off-chain, а не в контракте**:
- Рефералка не нужна атомарно (можно выплачивать batch'ами)
- Тиры, процент реферера, минимальные пороги — всё меняется, держать в контракте = redeploy
- Treasury аккумулирует ВСЮ комиссию, рефералы получают долю из treasury
- Проще аудит контракта (он не знает про рефералов)
- Host app полностью контролирует бизнес-логику выплат

**Примеры тиров**:
| Тариф | feeBps | Referrer share | Пример |
|-------|--------|----------------|--------|
| Free | 100 (1%) | 30% от fee | Swap 1 ETH → fee 0.01 ETH → ref 0.003 ETH |
| Pro | 50 (0.5%) | 25% | Swap 1 ETH → fee 0.005 ETH → ref 0.00125 ETH |
| VIP | 20 (0.2%) | 20% | Swap 1 ETH → fee 0.002 ETH → ref 0.0004 ETH |

Тиры хранятся в host app БД, не в библиотеке. `FeeProvider` — мост между host логикой и библиотекой.

### Почему это безопасно (non-custodial):
- Keeper может вызвать ТОЛЬКО `executeSwap` → ТОЛЬКО через Uniswap V3 router
- `swapRouter` — immutable, зашит при деплое, нельзя поменять
- `recipient = user` — результат свапа идёт НАПРЯМУЮ юзеру, не на контракт
- Контракт не хранит токены (транзитно, в рамках одной TX)
- `ReentrancyGuard` — защита от reentrancy
- `SafeERC20` — защита от кривых ERC20

### Адреса SwapRouter02:
- **Ethereum**: `0x68b3465833fb72A70ecDF485E0e4C7bD8665Fc45`
- **Base**: `0x2626664c2603336E57B271c5C0b26F421741e481`

### User setup (одноразово):
1. `token.approve(SwapExecutor, type(uint256).max)` — один раз на токен

---

## Public API (интерфейсы библиотеки)

### Manager — главный объект
```go
// pkg/positionmanager/manager.go
type Manager struct { /* internal */ }

func New(cfg Config) (*Manager, error)
func (m *Manager) Run(ctx context.Context) error           // keeper loop (blocking)
func (m *Manager) Stop()

// Position CRUD
func (m *Manager) OpenPosition(ctx context.Context, p OpenParams) (*Position, error)
func (m *Manager) GetPosition(ctx context.Context, id uuid.UUID) (*Position, error)
func (m *Manager) ListPositions(ctx context.Context, owner common.Address, state ...PositionState) ([]*Position, error)
func (m *Manager) CancelPosition(ctx context.Context, id uuid.UUID) error

// Level management (0 gas — off-chain only)
func (m *Manager) UpdateLevel(ctx context.Context, posID uuid.UUID, levelIdx int, newTriggerPrice *big.Int) error
func (m *Manager) AddLevel(ctx context.Context, posID uuid.UUID, level Level) error
func (m *Manager) RemoveLevel(ctx context.Context, posID uuid.UUID, levelIdx int) error

// Market orders (immediate execution)
func (m *Manager) MarketSwap(ctx context.Context, p MarketSwapParams) (*SwapResult, error)
```

### Interfaces — host app реализует
```go
// pkg/positionmanager/store.go
type Store interface {
    Save(ctx context.Context, pos *Position) error
    Get(ctx context.Context, id uuid.UUID) (*Position, error)
    GetByOwner(ctx context.Context, owner common.Address, states ...PositionState) ([]*Position, error)
    ListActive(ctx context.Context, chainID uint64) ([]*Position, error)
    Update(ctx context.Context, pos *Position) error
    Delete(ctx context.Context, id uuid.UUID) error
}

// pkg/positionmanager/pricefeed.go
type PriceFeed interface {
    Subscribe(ctx context.Context, pair TokenPair) (<-chan PriceUpdate, error)
    Latest(pair TokenPair) (*big.Int, int64, error)  // price, timestamp
}

// pkg/positionmanager/chain.go — абстракция over go-ethereum
type ChainClient interface {
    SendTransaction(ctx context.Context, tx *types.Transaction) error
    SuggestGasPrice(ctx context.Context) (*big.Int, error)
    PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
    EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error)
    CallContract(ctx context.Context, call ethereum.CallMsg, block *big.Int) ([]byte, error)
    ChainID(ctx context.Context) (*big.Int, error)
}
// Nota: *ethclient.Client уже реализует этот интерфейс — host может передать напрямую.
```

### Config
```go
type Config struct {
    Store       Store
    PriceFeed   PriceFeed
    FeeProvider FeeProvider                 // host's fee/tier/referral logic
    Chains      map[uint64]ChainInstance    // chainID → chain-specific config + client
    OnExecution func(ExecutionEvent)         // callback: fee result, P&L, referral tracking
    OnError     func(error)
}

type ChainInstance struct {
    Client          ChainClient
    KeeperKey       *ecdsa.PrivateKey       // keeper EOA для этой сети
    ExecutorAddress common.Address           // SwapExecutor contract address
    ChainConfig                              // gas params, slippage, etc.
}
```

---

## Off-Chain: Data Model

### Position
```go
// pkg/position/types.go
type Position struct {
    ID            uuid.UUID
    Owner         common.Address
    TokenBase     common.Address     // WETH
    TokenQuote    common.Address     // USDC
    Direction     Direction          // Long | Short
    TotalSize     *big.Int           // начальный размер (wei)
    RemainingSize *big.Int           // текущий остаток
    EntryPrice    *big.Int           // 8 decimals
    State         PositionState      // Active | PartialClosed | Closed | Cancelled
    ChainID       uint64
    PoolFee       uint24             // Uniswap fee tier (500, 3000, 10000)
    // FeeBps НЕ хранится — запрашивается динамически через FeeProvider перед каждым свапом
    Levels        []Level            // SL + TP1 + TP2 + TP3 (variable count)
    CreatedAt     int64
    UpdatedAt     int64
}
```

### Level
```go
type Level struct {
    Index        int
    Type         LevelType      // SL | TP
    TriggerPrice *big.Int       // 8 decimals
    PortionBps   uint16         // 3300 = 33%, 10000 = 100%
    Status       LevelStatus    // Active | Triggered | Cancelled
    MoveSLTo     *big.Int       // после этого TP — куда двигать SL (0 = не двигать)
    CancelOnFire []int          // индексы уровней для отмены при срабатывании
    // Результат исполнения
    ExecTxHash   common.Hash
    ExecPrice    *big.Int
    ExecAmount   *big.Int
    ExecAt       int64
}
```

### Ключевое отличие от SwapVM-подхода:
- **Нет SignedOrder** — ордера не подписываются, не хранятся on-chain
- **Нет bitIndex** — нет invalidation
- **SL movement** = просто `level.TriggerPrice = newPrice` в БД
- **Добавить/удалить уровень** = добавить/удалить запись в БД
- **Количество уровней** — не ограничено 4мя, можно 10, 20, сколько угодно

---

## Процесс SL Movement (ZERO gas)

```
Было:  SL=1800, TP1=2200, TP2=2500, TP3=3000  (4 уровня в БД)

Цена достигла 2200 → TP1 сработал:
1. Executor: вызов SwapExecutor.executeSwap(user, WETH, USDC, 0.33 ETH, minOut)
2. Manager: level[1].Status = Triggered
3. Manager: level[0].TriggerPrice = 2000  ← просто UPDATE в БД, 0 gas!
4. Manager: position.RemainingSize -= 0.33 ETH
5. Trigger Engine: обновить индекс для SL

Стало: SL=2000, TP2=2500, TP3=3000 (SL передвинут, TP1 исполнен)
```

Сравни с SwapVM: там нужно cancel SL-0 on-chain + activate SL-1 on-chain = 2 extra TX + gas.

---

## Компоненты (файловая структура)

```
contracts/
  SwapExecutor.sol              — тонкий executor (~70 LOC)
  test/SwapExecutor.t.sol       — Foundry тесты

pkg/positionmanager/            — ВСЯ БИБЛИОТЕКА В ОДНОМ ПАКЕТЕ
  // === Public API ===
  manager.go                    — New(), Run(), OpenPosition(), UpdateLevel(), Cancel()
  config.go                     — Config, ChainConfig, defaults (EthereumDefaults, BaseDefaults)
  types.go                      — Position, Level, Direction, State, enums
  events.go                     — ExecutionEvent, callback types

  // === Interfaces (host implements) ===
  store.go                      — Store interface (Save, Get, List, Update, Delete)
  pricefeed.go                  — PriceFeed interface (Subscribe, Latest)
  chain.go                      — ChainClient interface (SendTx, EstimateGas, etc.)

  // === Internal ===
  trigger.go                    — Trigger engine (sorted index, O(log n) matching)
  executor.go                   — Swap execution (build calldata, gas, nonce)
  executor_abi.go               — SwapExecutor contract ABI
  fees.go                       — Fee calculation logic

  // === Reference implementations (optional, host can use or ignore) ===
  store_bolt.go                 — BoltDB Store implementation
  pricefeed_uniswapv3.go        — Uniswap V3 pool-based PriceFeed (primary, recommended)

  // === Tests ===
  manager_test.go
  trigger_test.go
  executor_test.go

pkg/swapvm/                     — (EXISTING, не трогаем)
```

### Почему один пакет, а не несколько:
- Проще импортировать: `import pm "...pkg/positionmanager"`
- Нет circular dependencies
- Интерфейсы рядом с кодом который их использует
- Reference implementations можно вынести позже если вырастут

---

## Price Feed: Uniswap V3 напрямую (без Chainlink)

**Chainlink не нужен.** Мы свапаем на Uniswap → Uniswap и есть source of truth.

### Подход: slot0() + Swap events

```go
// Reference implementation: pkg/positionmanager/pricefeed_uniswapv3.go

// 1. Подписка на новые блоки через WebSocket
heads := make(chan *types.Header)
sub, _ := wsClient.SubscribeNewHead(ctx, heads)

// 2. На каждом блоке — читаем slot0() из Uniswap V3 пула
for header := range heads {
    sqrtPriceX96, tick, _, _, _, _, _ := pool.Slot0(nil)
    price := sqrtPriceX96ToPrice(sqrtPriceX96, token0Decimals, token1Decimals)
    priceChan <- PriceUpdate{Pair: pair, Price: price, Block: header.Number}
}

// Конвертация sqrtPriceX96 в цену:
// price = (sqrtPriceX96 / 2^96)^2 × 10^(token0Decimals - token1Decimals)
```

### Почему не Chainlink:
| | Uniswap V3 slot0 | Chainlink |
|---|---|---|
| Latency | Мгновенно (eth_call) | 30+ сек staleness |
| Источник | ТОТ ЖЕ пул где свапаем | Внешний оракул |
| Зависимости | 0 (только RPC) | Chainlink контракты |
| Стоимость | 0 (eth_call free) | 0 (read free) |
| Base chain | Полная поддержка | Ограниченные пары |

### Anti-manipulation (без Chainlink):
- **TWAP**: Uniswap V3 имеет встроенный TWAP oracle — `pool.observe([300, 0])` даёт 5-мин TWAP
- **Circuit breaker**: если spot отклоняется от TWAP >5% → пауза, не триггерим
- **Flash loan protection**: TWAP нельзя сманипулировать в одном блоке

### Интерфейс (host может подставить свой):
```go
type PriceFeed interface {
    Subscribe(ctx context.Context, pair TokenPair) (<-chan PriceUpdate, error)
    Latest(pair TokenPair) (*big.Int, int64, error)
}

type PriceUpdate struct {
    Pair      TokenPair
    Price     *big.Int    // 8 decimals
    Block     uint64
    Timestamp int64
}
```

Host app может использовать нашу reference implementation или подставить свою
(например если у них уже есть price service).

---

## Масштабирование (10K–50K ордеров)

| Аспект | Решение | Perf |
|--------|---------|------|
| Trigger matching | Sorted slice + binary search per pair | O(log n) per price update |
| Memory | ~0.5 KB/position × 50K = 25MB | Легко в RAM |
| Price feed | 1 WebSocket per chain | Минимум connections |
| DB writes | BoltDB batch writes | ~100K writes/sec |
| TX throughput | Worker pool (4 ETH, 8 Base) | 5-10 tx/block/keeper |
| Burst (mass SL) | Multiple keeper EOAs, round-robin | 30+ tx/block |

### Оптимизация Trigger Engine

```go
// Для каждой торговой пары — два sorted slice:
type PairTriggers struct {
    // Fires when price >= trigger (TP for Long, SL for Short)
    Above []TriggerEntry  // sorted ascending by price
    // Fires when price <= trigger (SL for Long, TP for Short)
    Below []TriggerEntry  // sorted descending by price
}

// На price update:
// Binary search → O(log n) чтобы найти первый сработавший
// Потом линейно собрать все сработавшие → O(k)
// Итого: O(log n + k) где k = кол-во сработавших
```

Для 50K ордеров / 100 пар = 500 ордеров/пара. Binary search = ~9 сравнений. Микросекунды.

---

## Безопасность

### On-Chain (контракт)
| Гарантия | Как обеспечена |
|----------|----------------|
| Keeper не может украсть токены | `recipient = user` всегда. Router immutable. Только `executeSwap` |
| Slippage protection | `minAmountOut` проверяется Uniswap router'ом |
| Reentrancy | `ReentrancyGuard` от OpenZeppelin |
| Кривые ERC20 | `SafeERC20` |
| Контракт не хранит токены | Транзитно: transferFrom → approve → swap в одной TX |

### Off-Chain (keeper)
| Угроза | Митигация |
|--------|-----------|
| Keeper key compromised | Multisig ownership, rate limiting per user, max amount per swap |
| Юзер потратил токены | Balance check перед swap. Periodic monitoring → alert |
| Юзер отозвал approve | Allowance check перед swap → alert |
| Flash loan manipulation | TWAP 5 мин через Uniswap V3 `observe()` вместо spot. Circuit breaker при >5% spike за блок |
| MEV (sandwich) | Ethereum: Flashbots private TX. Base: sequencer-ordered. `minAmountOut` всегда |
| Keeper downtime | systemd watchdog, health endpoint, alert |
| Gas spike на SL | Aggressive gas (2.5x base fee), max cap per chain |
| Price gap через SL | SL всё равно исполняется по рыночной цене. `minAmountOut` защищает |
| Double execution | Check `level.Status == Active` перед каждым исполнением + mutex |
| DB corruption | BoltDB transactions, periodic backup snapshots |

### Сравнение с подходами:

| | Thin Executor (наш) | SwapVM pre-signed | Custom PositionManager.sol |
|---|---|---|---|
| Аудит | Часы (50 LOC) | Не нужен (чужой контракт) | Недели (500+ LOC) |
| Гибкость ордеров | Бесконечная (off-chain) | Ограничена подписями | Ограничена контрактом |
| Gas на изменение ордера | 0 | ~50K (cancel + new) | ~30K (updateLevel) |
| Зависимости | Uniswap V3 only | 1inch SwapVM | Chainlink + Uniswap + custom |
| Custody risk | Minimal (transit only) | Non-custodial (approvals) | Custodial (контракт хранит) |

---

## Конфигурация по сетям

```go
// pkg/keeper/config.go
var Configs = map[uint64]ChainConfig{
    1: {  // Ethereum
        Name:            "ethereum",
        BlockTime:       12 * time.Second,
        SwapRouter:      "0x68b3465833fb72A70ecDF485E0e4C7bD8665Fc45",
        ExecutorWorkers: 4,
        SLGasMultiplier: 2.5,
        TPGasMultiplier: 1.3,
        SLSlippageBps:   200,  // 2%
        TPSlippageBps:   50,   // 0.5%
        MaxGasGwei:      200,
        UseFlashbots:    true,
    },
    8453: {  // Base
        Name:            "base",
        BlockTime:       2 * time.Second,
        SwapRouter:      "0x2626664c2603336E57B271c5C0b26F421741e481",
        ExecutorWorkers: 8,
        SLGasMultiplier: 2.0,
        TPGasMultiplier: 1.5,
        SLSlippageBps:   200,
        TPSlippageBps:   50,
        MaxGasGwei:      1,
        UseFlashbots:    false,
    },
}
```

---

## Market Orders

Немедленное исполнение — тот же `SwapExecutor.executeSwap()`, просто без trigger engine:

1. API получает запрос на market order
2. Executor: получить текущую цену из price feed
3. Рассчитать `minAmountOut` с учётом slippage (0.5%)
4. Вызвать `SwapExecutor.executeSwap()` напрямую
5. Вернуть результат

---

## Жизненный цикл позиции

```
1. ОТКРЫТИЕ
   User → API: POST /positions {token, direction, size, SL, TP1, TP2, TP3}
   Manager: validate → create Position with 4 Levels → save to BoltDB
   Trigger Engine: register 4 triggers
   ← Return position ID

2. TP1 СРАБОТАЛ (цена >= 2200)
   Price Feed → Trigger Engine: price crossed 2200
   Executor: SwapExecutor.executeSwap(user, WETH, USDC, 0.33 ETH, minOut)
   Manager:
     - level[1].Status = Triggered
     - level[0].TriggerPrice = 2000  ← SL moved (0 gas!)
     - remainingSize -= 0.33 ETH
     - state = PartialClosed
   Trigger Engine: update SL trigger price

3. SL СРАБОТАЛ (цена <= 2000)
   Executor: SwapExecutor.executeSwap(user, WETH, USDC, 0.67 ETH, minOut)
   Manager:
     - level[0].Status = Triggered
     - cancel all remaining TP levels
     - state = Closed
   Trigger Engine: remove all triggers for this position

4. ОТМЕНА ЮЗЕРОМ
   User → API: DELETE /positions/:id
   Manager: mark all levels Cancelled, state = Cancelled
   Trigger Engine: remove all triggers
   ← No on-chain TX needed!
```

---

## Фазы реализации

### Phase 1: Core Library (3 недели)

**Неделя 1: Contract + Types + Store**
- [ ] `contracts/SwapExecutor.sol` — executor с комиссией
- [ ] `contracts/test/SwapExecutor.t.sol` — Foundry fork-тесты
- [ ] `pkg/positionmanager/types.go` — Position, Level, enums, params
- [ ] `pkg/positionmanager/store.go` — Store interface
- [ ] `pkg/positionmanager/store_bolt.go` — BoltDB reference impl
- [ ] `pkg/positionmanager/config.go` — Config, ChainConfig, defaults
- [ ] `pkg/positionmanager/fees.go` — fee calculation

**Неделя 2: Trigger Engine + Executor**
- [ ] `pkg/positionmanager/trigger.go` — sorted index + O(log n) matching
- [ ] `pkg/positionmanager/executor.go` — build calldata, gas, nonce, execute
- [ ] `pkg/positionmanager/executor_abi.go` — SwapExecutor ABI binding
- [ ] `pkg/positionmanager/pricefeed.go` — PriceFeed interface
- [ ] `pkg/positionmanager/chain.go` — ChainClient interface
- [ ] `pkg/positionmanager/trigger_test.go` — unit тесты trigger
- [ ] `pkg/positionmanager/executor_test.go`

**Неделя 3: Manager + Integration**
- [ ] `pkg/positionmanager/manager.go` — New(), Run(), CRUD, state machine
- [ ] `pkg/positionmanager/events.go` — ExecutionEvent, callbacks
- [ ] `pkg/positionmanager/manager_test.go` — unit + integration тесты
- [ ] `pkg/positionmanager/pricefeed_uniswapv3.go` — reference Uniswap V3 price feed (slot0 + TWAP)
- [ ] Deploy SwapExecutor на Sepolia + Base Sepolia
- [ ] E2E тест с mock dependencies

### Phase 2: Production (3 недели)
- [ ] TWAP validation в trigger engine (anti-manipulation)
- [ ] Flashbots integration в executor (Ethereum)
- [ ] Balance/allowance monitoring (periodic check в Run loop)
- [ ] Circuit breaker (price deviation detection)
- [ ] Market order support (`MarketSwap`)
- [ ] Deploy SwapExecutor на Ethereum mainnet + Base mainnet

### Phase 3: Scale (2-3 недели)
- [ ] Multiple keeper EOAs (round-robin для burst SL execution)
- [ ] Trailing stop-loss support
- [ ] Performance тесты (50K positions)
- [ ] Graceful shutdown, state recovery on restart

---

## Зависимости

```
github.com/ethereum/go-ethereum  v1.14.12   (existing)
go.etcd.io/bbolt                 v1.3.9     (reference Store impl, optional)
github.com/google/uuid           v1.6.0     (position IDs)
```

**Solidity:**
```
@openzeppelin/contracts  v5.x  (SafeERC20, ReentrancyGuard)
forge-std                       (тесты)
```

Библиотека имеет **одну обязательную зависимость**: go-ethereum.
BoltDB и uuid — optional (reference implementations).

---

## Verification

1. **Unit tests**: position state machine, trigger engine sorted index, amount calculations
2. **Foundry fork tests**: SwapExecutor на Ethereum fork (real Uniswap pools)
3. **Integration test**: keeper + mock price feed → verify trigger → verify on-chain swap
4. **E2E on testnet**: Sepolia — open position, manipulate price, verify SL/TP execution
5. **Load test**: 10K positions, synthetic price feed, measure trigger latency (<1ms target)
6. **Security**: audit SwapExecutor (50 LOC), verify `recipient=user` always
