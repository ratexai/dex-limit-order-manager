package positionmanager

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
)

// Store is the persistence interface for positions.
// The host application provides an implementation (BoltDB, PostgreSQL, etc.).
// A reference BoltDB implementation is provided in store_bolt.go.
type Store interface {
	// Save persists a new position.
	Save(ctx context.Context, pos *Position) error

	// Get returns a position by ID.
	Get(ctx context.Context, id [16]byte) (*Position, error)

	// GetByOwner returns positions for a given owner, optionally filtered by state.
	// If no states are provided, all positions are returned.
	GetByOwner(ctx context.Context, owner common.Address, states ...PositionState) ([]*Position, error)

	// ListActive returns all active (non-terminal) positions for a chain.
	ListActive(ctx context.Context, chainID uint64) ([]*Position, error)

	// Update persists changes to an existing position.
	Update(ctx context.Context, pos *Position) error

	// Delete removes a position by ID.
	Delete(ctx context.Context, id [16]byte) error
}
