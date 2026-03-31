# SwapVM Limit Orders — Developer Notes

## Что это

Go-клиент для создания лимитных ордеров через 1inch SwapVM — замену Limit Order Protocol v4.
Контракт: `0x8fdd04dbf6111437b44bbca99c28882434e0958f` (12 сетей, единый адрес).

## Структура

```
pkg/swapvm/
  types/           — Order, MakerTraits, TakerTraits, ABI
  program/         — Билдер байткода (опкоды VM)
  signer/          — EIP-712 подпись ордеров
  client.go        — Высокоуровневый API
docs/
  SWAPVM_LIMIT_ORDERS_SPEC.md — Полная техспецификация
```

## Быстрый старт

```go
client, _ := swapvm.NewClient(ctx, "https://rpc.example.com")

// Создать лимитный ордер: продать 1000 USDC за 0.5 WETH
order, sig, _ := client.CreateLimitOrder(ctx, makerKey, swapvm.LimitOrderParams{
    TokenA:  USDC,
    TokenB:  WETH,
    AmountA: big.NewInt(1000e6),  // 1000 USDC (rate = AmountB/AmountA)
    AmountB: big.NewInt(5e17),    // 0.5 WETH
    BitIndex: 42,                  // уникальный ID для отмены
    Deadline: 1711929600,          // unix timestamp
})

// Превью
amountIn, amountOut, _ := client.Quote(ctx, order, USDC, WETH, amount, true, sig)

// Исполнить (от лица тейкера)
tx, _ := client.Swap(ctx, takerKey, order, USDC, WETH, amount, true, threshold, sig)

// Отменить
tx, _ = client.CancelBitOrder(ctx, makerKey, 42)
```

## Ключевые моменты

1. **Approvals** — мейкер апрувит tokenOut, тейкер апрувит tokenIn на адрес контракта
2. **Подпись** — EIP-712, domain `name="SwapVM"`, `version="1.0.0"` (читать через `eip712Domain()`)
3. **Программа** — байткод из опкодов: `[opcode 1B][argsLen 1B][args NB]`
4. **Шаблоны ордеров**:
   - One-time: `InvalidateBit` → `StaticBalances` → `LimitSwap`
   - Partial fill: `StaticBalances` → `LimitSwap` → `InvalidateTokenOut`
   - С дедлайном: `Deadline` → `InvalidateBit` → `StaticBalances` → `LimitSwap`
5. **Отмена**: `invalidateBit(bitIndex)` для one-time, `invalidateTokenOut(orderHash, token)` для partial

## Безопасность

- Используем ТОЛЬКО проверенные опкоды: 13, 17, 18-22, 30
- Избегаем fee-on-transfer токенов
- Всегда ставим `IS_FIRST_TRANSFER_FROM_TAKER` и `IS_STRICT_THRESHOLD`
- `quote()` — НЕ view, вызывать через `eth_call`
- Открытые баги (Dutch auction, oracle) нас НЕ затрагивают
