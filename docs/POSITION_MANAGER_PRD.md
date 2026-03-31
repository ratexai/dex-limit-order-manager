# Position Manager: Requirements & Architecture

## Product Requirements Document v1.0

---

## 1. Цель

Реализовать CEX-подобное управление позициями с автоматическим
исполнением стоп-лоссов и тейк-профитов на EVM-сетях.
Интеграция в существующий ratex finance layer на Go.

**Ключевое требование:** при достижении ценой уровня лимитки —
исполнение за 1-2 блока, атомарная отмена связанных ордеров.

---

## 2. Пользовательский сценарий

```
Трейдер открывает позицию LONG ETH/USDC по цене 2000:

  ┌─────────────────────────────────────────────────┐
  │                                                 │
  │  TP3: 3000 USDC ─── продать 34% (остаток)      │  ▲ цена
  │  TP2: 2500 USDC ─── продать 33%, SL → TP1      │  │
  │  TP1: 2200 USDC ─── продать 33%, SL → вход     │  │
  │  ════════════════════════════════════════════    │  │ ВХОД: 2000
  │  SL:  1800 USDC ─── продать 100% (всё)         │  │
  │                                                 │
  └─────────────────────────────────────────────────┘

Сценарий A — цена растёт:
  2200 → TP1 срабатывает → продано 0.33 ETH, SL передвинут на 2000
  2500 → TP2 срабатывает → продано 0.33 ETH, SL передвинут на 2200
  3000 → TP3 срабатывает → продано 0.34 ETH, позиция закрыта

Сценарий B — цена падает:
  1800 → SL срабатывает → продано 1.0 ETH, все TP отменены

Сценарий C — частичный рост, потом падение:
  2200 → TP1 срабатывает → продано 0.33 ETH, SL = 2000
  1900 → ничего (SL=2000 не достигнут)
  2000 → SL срабатывает → продано 0.67 ETH (остаток), позиция закрыта
```

---

## 3. Компоненты системы

```
┌─────────────────────────────────────────────────────────────┐
│                     ratex finance layer                      │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐ │
│  │                   Trading Layer                         │ │
│  │                                                         │ │
│  │  ┌─────────────┐  ┌──────────────┐  ┌──────────────┐  │ │
│  │  │  Position    │  │   Keeper     │  │  Execution   │  │ │
│  │  │  Service     │  │   Service    │  │  Engine      │  │ │
│  │  │             │  │              │  │              │  │ │
│  │  │ CRUD        │  │ Price Monitor│  │ DEX Router   │  │ │
│  │  │ позиций     │  │ Trigger      │  │ Tx Builder   │  │ │
│  │  │ + уровней   │  │ Detection    │  │ Gas Manager  │  │ │
│  │  └──────┬──────┘  └──────┬───────┘  └──────┬───────┘  │ │
│  │         │                │                  │          │ │
│  │         ▼                ▼                  ▼          │ │
│  │  ┌─────────────────────────────────────────────────┐   │ │
│  │  │              Position Manager                    │   │ │
│  │  │              (on-chain contract)                  │   │ │
│  │  └─────────────────────────────────────────────────┘   │ │
│  │         │                │                  │          │ │
│  │         ▼                ▼                  ▼          │ │
│  │  ┌───────────┐   ┌───────────┐   ┌────────────────┐  │ │
│  │  │ Chainlink │   │ 1inch     │   │ Uniswap/       │  │ │
│  │  │ Oracle    │   │ Aggregator│   │ Sushiswap etc  │  │ │
│  │  └───────────┘   └───────────┘   └────────────────┘  │ │
│  └────────────────────────────────────────────────────────┘ │
│                                                              │
│  ┌─────────────────┐  ┌──────────────────────────────────┐ │
│  │ Chainlink        │  │  Alert Service                    │ │
│  │ Automation        │  │  (Telegram/webhook уведомления)  │ │
│  │ (fallback keeper)│  │                                    │ │
│  └─────────────────┘  └──────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

---

## 4. Data Model

### 4.1 Position

```
Position {
  id:             uint256        // уникальный ID
  owner:          address        // владелец
  tokenBase:      address        // базовый токен (WETH)
  tokenQuote:     address        // котировочный токен (USDC)
  direction:      enum           // LONG | SHORT
  totalSize:      uint256        // начальный размер (1e18 = 1 ETH)
  remainingSize:  uint256        // текущий остаток
  entryPrice:     uint256        // цена входа (для учёта P&L)
  status:         enum           // ACTIVE | CLOSED | LIQUIDATED
  createdAt:      uint64         // timestamp
  levels:         Level[4]       // SL + TP1 + TP2 + TP3
}
```

### 4.2 Level

```
Level {
  triggerPrice:   uint256        // цена срабатывания (8 decimals)
  portionBps:     uint16         // % от remainingSize (10000 = 100%)
  levelType:      enum           // STOP_LOSS | TAKE_PROFIT
  onTrigger:      Action         // что делать при срабатывании
  status:         enum           // ACTIVE | TRIGGERED | CANCELLED
}
```

### 4.3 Action

```
Action {
  type:           enum           // PARTIAL_CLOSE | FULL_CLOSE
  moveSLTo:       uint256        // передвинуть SL на эту цену (0 = не двигать)
  cancelLevels:   uint8[]        // индексы уровней для отмены
}
```

### 4.4 Пример: LONG ETH/USDC, вход 2000, размер 1 ETH

```
levels[0] = {  // Stop-Loss
  triggerPrice: 1800e8,
  portionBps:   10000,          // 100%
  levelType:    STOP_LOSS,
  onTrigger: {
    type:         FULL_CLOSE,
    moveSLTo:     0,
    cancelLevels: [1, 2, 3]     // отменить все TP
  }
}

levels[1] = {  // Take-Profit 1
  triggerPrice: 2200e8,
  portionBps:   3300,           // 33%
  levelType:    TAKE_PROFIT,
  onTrigger: {
    type:         PARTIAL_CLOSE,
    moveSLTo:     2000e8,       // SL → breakeven
    cancelLevels: []
  }
}

levels[2] = {  // Take-Profit 2
  triggerPrice: 2500e8,
  portionBps:   5000,           // 50% от остатка (≈33% от начала)
  levelType:    TAKE_PROFIT,
  onTrigger: {
    type:         PARTIAL_CLOSE,
    moveSLTo:     2200e8,       // SL → TP1
    cancelLevels: []
  }
}

levels[3] = {  // Take-Profit 3
  triggerPrice: 3000e8,
  portionBps:   10000,          // 100% от остатка (≈34% от начала)
  levelType:    TAKE_PROFIT,
  onTrigger: {
    type:         FULL_CLOSE,
    moveSLTo:     0,
    cancelLevels: [0]           // отменить SL
  }
}
```

---

## 5. On-Chain контракт: PositionManager

### 5.1 Интерфейс

```
openPosition(params)        → positionId
  - Создать позицию с 4 уровнями
  - Трансфер токенов от owner в контракт (custody)
  - Approve DEX router на эти токены

execute(positionId, levelIndex, oracleData, swapCalldata)
  - Вызывается keeper-ом или Chainlink Automation
  - Проверяет: oracle price достигла triggerPrice
  - Считает amount = remainingSize * portionBps / 10000
  - Свапает через DEX aggregator (1inch calldata)
  - Выполняет Action (отмена уровней, передвижение SL)
  - Отправляет результат owner-у
  - Атомарно: всё или ничего

updateLevel(positionId, levelIndex, newTriggerPrice)
  - Только owner
  - Изменить цену срабатывания (передвинуть SL/TP вручную)

closePosition(positionId)
  - Только owner
  - Экстренное закрытие: вернуть все токены owner-у
  - Отменить все уровни

getPosition(positionId) → Position
  - View: текущее состояние позиции
```

### 5.2 Модель хранения токенов

```
Вариант A: Custodial (контракт хранит токены)
  + Атомарное исполнение, не нужен approve от юзера в момент swap
  + Keeper может исполнить без участия owner
  - Юзер не контролирует токены пока позиция открыта
  - Контракт = точка отказа (если взломан)

Вариант B: Non-custodial (approve-based)
  + Юзер хранит токены
  - Юзер может случайно потратить/перевести залоченные токены
  - Нужен постоянный approve на контракт

РЕКОМЕНДАЦИЯ: Вариант A (custodial) для MVP
  Причина: надёжность исполнения важнее.
  Юзер вносит токены → контракт гарантирует исполнение.
```

### 5.3 Oracle: проверка цены

```
execute() проверяет цену через Chainlink Oracle:

  LONG + STOP_LOSS:   oraclePrice <= triggerPrice
  LONG + TAKE_PROFIT: oraclePrice >= triggerPrice
  SHORT + STOP_LOSS:  oraclePrice >= triggerPrice
  SHORT + TAKE_PROFIT:oraclePrice <= triggerPrice

Допуск: ±0.5% от triggerPrice (configurable)
  чтобы избежать отказов из-за проскальзывания oracle
```

---

## 6. Keeper Service (Go, в ratex)

### 6.1 Архитектура

```
┌─ Keeper Service ──────────────────────────────────────┐
│                                                        │
│  ┌──────────────────────────────────────────────────┐ │
│  │ Price Feed (goroutine)                            │ │
│  │                                                    │ │
│  │ Sources (приоритет):                               │ │
│  │   1. WebSocket DEX price stream (Uniswap, etc)    │ │
│  │   2. Chainlink latestRoundData() polling          │ │
│  │   3. 1inch Spot Price API                          │ │
│  │                                                    │ │
│  │ Output: map[tokenPair]Price (обновление каждый блок)│ │
│  └────────────────────┬─────────────────────────────┘ │
│                       │                                │
│                       ▼                                │
│  ┌──────────────────────────────────────────────────┐ │
│  │ Trigger Detector (goroutine per chain)            │ │
│  │                                                    │ │
│  │ for each block:                                    │ │
│  │   prices := priceFeed.Latest()                    │ │
│  │   for _, pos := range activePositions:             │ │
│  │     for i, level := range pos.Levels:              │ │
│  │       if shouldTrigger(level, prices, pos.Dir):    │ │
│  │         triggerChan <- TriggerEvent{pos, i}        │ │
│  │                                                    │ │
│  └────────────────────┬─────────────────────────────┘ │
│                       │                                │
│                       ▼                                │
│  ┌──────────────────────────────────────────────────┐ │
│  │ Executor (goroutine pool, concurrency=N)          │ │
│  │                                                    │ │
│  │ 1. Fetch best swap route from 1inch API           │ │
│  │ 2. Build execute() calldata                        │ │
│  │ 3. Estimate gas, set priority fee                  │ │
│  │ 4. Sign & broadcast tx                             │ │
│  │ 5. Wait for confirmation (1-2 blocks)              │ │
│  │ 6. Update position cache                           │ │
│  │ 7. Send notification to user                       │ │
│  │                                                    │ │
│  │ On failure:                                        │ │
│  │   - Retry with higher gas (up to 3x)              │ │
│  │   - Alert if still failing                         │ │
│  │                                                    │ │
│  └──────────────────────────────────────────────────┘ │
│                                                        │
│  ┌──────────────────────────────────────────────────┐ │
│  │ Position Cache (in-memory + periodic sync)        │ │
│  │                                                    │ │
│  │ - Загрузка active positions из контракта при старте│ │
│  │ - Обновление по событиям (PositionOpened, etc)    │ │
│  │ - Periodic full sync каждые 5 минут               │ │
│  └──────────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────┘
```

### 6.2 Приоритеты исполнения

```
STOP_LOSS  → priority: CRITICAL (gas boost 2x, retry aggressive)
TAKE_PROFIT → priority: NORMAL  (standard gas, retry gentle)
```

### 6.3 Gas Management

```
Base fee estimation:
  gasPrice = baseFee * 1.25 + priorityFee

For STOP_LOSS:
  gasPrice = baseFee * 2.0 + priorityFee * 3   // агрессивно
  gasLimit = estimatedGas * 1.5                  // с запасом

For TAKE_PROFIT:
  gasPrice = baseFee * 1.25 + priorityFee       // стандартно
  gasLimit = estimatedGas * 1.2
```

---

## 7. Chainlink Automation (fallback)

### 7.1 Зачем

Если Go keeper упал, Chainlink Automation подхватывает исполнение.
Задержка: 10-30 сек (хуже keeper-а, но лучше чем ничего).

### 7.2 Интерфейс

```
contract PositionManager is AutomationCompatibleInterface {

  function checkUpkeep(bytes calldata)
    external view returns (bool upkeepNeeded, bytes memory performData)
  {
    // Перебрать active positions
    // Проверить oracle prices
    // Вернуть первый triggered level
  }

  function performUpkeep(bytes calldata performData) external {
    // Декодировать positionId + levelIndex
    // Получить swap route (или использовать fallback route)
    // Вызвать execute()
  }
}
```

### 7.3 Ограничение

Chainlink Automation не может вызывать внешние API (1inch)
для получения оптимального маршрута. Варианты:

```
A. Использовать прямой swap через Uniswap router (без агрегации)
   - Хуже цена, но гарантированно работает
   - Slippage tolerance: 1-2%

B. Предварительно записывать fallback route в контракт
   - Keeper обновляет маршруты периодически
   - Chainlink использует последний сохранённый
```

**Рекомендация:** Вариант A для MVP.

---

## 8. Поддерживаемые сети (MVP)

| Сеть | Блок | Приоритет | Причина |
|------|-------|----------|---------|
| Arbitrum | 0.25s | P0 | Быстрее всех, дешёвый газ |
| Base | 2s | P0 | Большая ликвидность, дешёвый газ |
| Polygon | 2s | P1 | Широкая аудитория |
| Ethereum | 12s | P2 | Дорогой газ, но самая глубокая ликвидность |

---

## 9. API (для фронтенда/терминала)

```
POST /positions
  body: { tokenBase, tokenQuote, direction, size, levels[] }
  → { positionId, txHash }

GET /positions/:id
  → { position with current status, P&L }

GET /positions?owner=0x...&status=active
  → [ positions[] ]

PATCH /positions/:id/levels/:index
  body: { triggerPrice }
  → { txHash }

DELETE /positions/:id
  → { txHash }  // emergency close

GET /positions/:id/history
  → [ { levelIndex, triggeredAt, amountSwapped, priceExecuted, txHash } ]
```

---

## 10. Events (on-chain, для индексации)

```
PositionOpened(positionId, owner, tokenBase, tokenQuote, size, direction)
LevelTriggered(positionId, levelIndex, amountSwapped, executionPrice, txHash)
LevelUpdated(positionId, levelIndex, oldPrice, newPrice)
LevelCancelled(positionId, levelIndex, reason)
PositionClosed(positionId, totalPnL)
```

---

## 11. Безопасность

### 11.1 Контракт

- Reentrancy guard на execute()
- Только авторизованные keepers могут вызывать execute()
  (whitelist: наш EOA + Chainlink forwarder)
- Owner может только: updateLevel, closePosition
- Slippage protection: минимальный amountOut проверяется в контракте
- Oracle freshness check: отклонять stale prices (> 60 сек)
- Max position size per user (configurable)

### 11.2 Keeper

- Private key в HSM / AWS KMS (не в .env)
- Rate limiting: max 10 executions per block per chain
- Circuit breaker: остановка при аномалиях (oracle deviation > 10%)
- Health check endpoint для мониторинга
- Duplicate trigger prevention (idempotent execution)

### 11.3 Финансовые риски

- Проскальзывание: slippage tolerance 0.5% (TP), 2% (SL)
- Gas spike: max gas price cap per chain
- Oracle manipulation: использовать TWAP, не spot price
- Flash loan attacks: минимальная задержка между открытием и исполнением

---

## 12. Фазы реализации

### Phase 1: MVP (2-3 недели)

```
[ ] PositionManager контракт (Solidity)
    [ ] openPosition / execute / closePosition
    [ ] Oracle integration (Chainlink)
    [ ] Прямой Uniswap swap (без агрегатора)
    [ ] Тесты на Foundry (fork-тесты)

[ ] Keeper Service (Go, в ratex)
    [ ] Price monitor (Chainlink polling)
    [ ] Trigger detection
    [ ] Executor с прямым Uniswap swap
    [ ] Position cache

[ ] Deploy на Arbitrum testnet
[ ] E2E тест: открыть позицию, дождаться trigger, проверить исполнение
```

### Phase 2: Production (2-3 недели)

```
[ ] 1inch Aggregator интеграция (лучшие цены)
[ ] Chainlink Automation fallback
[ ] Multi-chain deploy (Arbitrum + Base)
[ ] Gas optimization
[ ] Monitoring + alerting
[ ] REST API для фронтенда
```

### Phase 3: Advanced (3-4 недели)

```
[ ] Trailing stop-loss
[ ] Conditional orders (если BTC > X, то SL ETH → Y)
[ ] Batch position management
[ ] SwapVM интеграция для пассивных лимиток
[ ] P&L tracking + analytics
[ ] Ethereum mainnet deploy
```

---

## 13. Зависимости

| Компонент | Технология | Версия |
|-----------|-----------|--------|
| Контракт | Solidity | 0.8.24+ |
| Билд | Foundry | latest |
| Oracle | Chainlink Price Feeds | v3 |
| DEX (MVP) | Uniswap V3 Router | SwapRouter02 |
| DEX (Prod) | 1inch Aggregation API | v6 |
| Keeper | Go (ratex) | 1.22+ |
| Automation | Chainlink Automation | v2.1 |
| Тесты | Foundry fork tests | Arbitrum fork |
