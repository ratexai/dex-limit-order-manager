package positionmanager

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// --- Mock Store ---

type mockStore struct {
	mu        sync.RWMutex
	positions map[[16]byte]*Position
}

func newMockStore() *mockStore {
	return &mockStore{positions: make(map[[16]byte]*Position)}
}

func (s *mockStore) Save(_ context.Context, pos *Position) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.positions[pos.ID] = pos
	return nil
}

func (s *mockStore) Get(_ context.Context, id [16]byte) (*Position, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.positions[id]
	if !ok {
		return nil, fmt.Errorf("position not found")
	}
	return p, nil
}

func (s *mockStore) GetByOwner(_ context.Context, owner common.Address, states ...PositionState) ([]*Position, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Position
	stateSet := make(map[PositionState]bool)
	for _, st := range states {
		stateSet[st] = true
	}
	for _, p := range s.positions {
		if p.Owner == owner && (len(stateSet) == 0 || stateSet[p.State]) {
			result = append(result, p)
		}
	}
	return result, nil
}

func (s *mockStore) ListActive(_ context.Context, chainID uint64) ([]*Position, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Position
	for _, p := range s.positions {
		if p.ChainID == chainID && !p.State.IsTerminal() {
			result = append(result, p)
		}
	}
	return result, nil
}

func (s *mockStore) Update(_ context.Context, pos *Position) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.positions[pos.ID] = pos
	return nil
}

func (s *mockStore) Delete(_ context.Context, id [16]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.positions, id)
	return nil
}

// --- Mock FeeProvider ---

type mockFeeProvider struct {
	fee *FeeConfig
}

func (f *mockFeeProvider) GetFee(_ context.Context, _ common.Address) (*FeeConfig, error) {
	return f.fee, nil
}

// --- Mock PriceFeed ---

type mockPriceFeed struct {
	mu      sync.RWMutex
	prices  map[TokenPair]*big.Int
	subs    map[TokenPair][]chan PriceUpdate
}

func newMockPriceFeed() *mockPriceFeed {
	return &mockPriceFeed{
		prices: make(map[TokenPair]*big.Int),
		subs:   make(map[TokenPair][]chan PriceUpdate),
	}
}

func (f *mockPriceFeed) Subscribe(_ context.Context, pair TokenPair) (<-chan PriceUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan PriceUpdate, 32)
	f.subs[pair] = append(f.subs[pair], ch)
	return ch, nil
}

func (f *mockPriceFeed) Latest(pair TokenPair) (*big.Int, int64, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	p, ok := f.prices[pair]
	if !ok {
		return nil, 0, fmt.Errorf("no price")
	}
	return new(big.Int).Set(p), 0, nil
}

func (f *mockPriceFeed) pushPrice(pair TokenPair, price *big.Int) {
	f.mu.Lock()
	f.prices[pair] = price
	subs := make([]chan PriceUpdate, len(f.subs[pair]))
	copy(subs, f.subs[pair])
	f.mu.Unlock()

	update := PriceUpdate{Pair: pair, Price: price, Timestamp: 0}
	for _, ch := range subs {
		select {
		case ch <- update:
		default:
		}
	}
}

// --- Mock ChainClient ---

type mockChainClient struct {
	gasPrice   *big.Int
	gasTipCap  *big.Int
	nonce      uint64
	gasLimit   uint64
	chainID    *big.Int
	sentTxs    []*types.Transaction
	mu         sync.Mutex
	receipts   map[common.Hash]*types.Receipt
}

func newMockChainClient() *mockChainClient {
	return &mockChainClient{
		gasPrice:  big.NewInt(1e9),   // 1 gwei
		gasTipCap: big.NewInt(1e8),   // 0.1 gwei
		gasLimit:  200000,
		chainID:   big.NewInt(1),
		receipts:  make(map[common.Hash]*types.Receipt),
	}
}

func (c *mockChainClient) SendTransaction(_ context.Context, tx *types.Transaction) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sentTxs = append(c.sentTxs, tx)
	// Auto-create a successful receipt.
	c.receipts[tx.Hash()] = &types.Receipt{
		Status: 1,
		TxHash: tx.Hash(),
		Logs:   []*types.Log{},
	}
	return nil
}

func (c *mockChainClient) SuggestGasPrice(_ context.Context) (*big.Int, error) {
	return new(big.Int).Set(c.gasPrice), nil
}

func (c *mockChainClient) SuggestGasTipCap(_ context.Context) (*big.Int, error) {
	return new(big.Int).Set(c.gasTipCap), nil
}

func (c *mockChainClient) PendingNonceAt(_ context.Context, _ common.Address) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.nonce
	return n, nil
}

func (c *mockChainClient) EstimateGas(_ context.Context, _ ethereum.CallMsg) (uint64, error) {
	return c.gasLimit, nil
}

func (c *mockChainClient) CallContract(_ context.Context, _ ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

func (c *mockChainClient) ChainID(_ context.Context) (*big.Int, error) {
	return new(big.Int).Set(c.chainID), nil
}

func (c *mockChainClient) TransactionReceipt(_ context.Context, txHash common.Hash) (*types.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.receipts[txHash]
	if !ok {
		return nil, fmt.Errorf("receipt not found")
	}
	return r, nil
}
