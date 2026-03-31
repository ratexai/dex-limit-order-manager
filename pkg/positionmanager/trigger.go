package positionmanager

import (
	"math/big"
	"sort"
	"sync"
)

// TriggerEvent represents a level that has been triggered by a price movement.
type TriggerEvent struct {
	PositionID [16]byte
	LevelIndex int
	LevelType  LevelType
	Price      *big.Int // Price that caused the trigger.
	ChainID    uint64
}

// triggerEntry is stored in the sorted index.
type triggerEntry struct {
	price      *big.Int
	positionID [16]byte
	levelIndex int
	levelType  LevelType
}

// TriggerEngine efficiently matches price updates against registered triggers.
// Uses sorted slices with binary search for O(log n) trigger detection.
//
// Per token pair, two sorted slices are maintained:
//   - above: fires when price >= trigger (TP for Long, SL for Short). Sorted ascending.
//   - below: fires when price <= trigger (SL for Long, TP for Short). Sorted descending.
type TriggerEngine struct {
	mu    sync.Mutex
	pairs map[TokenPair]*pairTriggers
}

type pairTriggers struct {
	above []triggerEntry // Sorted ascending by price.
	below []triggerEntry // Sorted ascending by price (we scan from start for <= checks).
}

// NewTriggerEngine creates a new trigger engine.
func NewTriggerEngine() *TriggerEngine {
	return &TriggerEngine{
		pairs: make(map[TokenPair]*pairTriggers),
	}
}

// Register adds a trigger for a position level.
// direction determines which side the trigger goes on:
//   - Long  + SL: below (fires when price drops to trigger)
//   - Long  + TP: above (fires when price rises to trigger)
//   - Short + SL: above (fires when price rises to trigger)
//   - Short + TP: below (fires when price drops to trigger)
func (e *TriggerEngine) Register(pair TokenPair, posID [16]byte, levelIdx int, levelType LevelType, direction Direction, triggerPrice *big.Int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pt := e.getOrCreate(pair)
	entry := triggerEntry{
		price:      new(big.Int).Set(triggerPrice),
		positionID: posID,
		levelIndex: levelIdx,
		levelType:  levelType,
	}

	if shouldTriggerAbove(levelType, direction) {
		pt.above = insertSorted(pt.above, entry)
	} else {
		pt.below = insertSorted(pt.below, entry)
	}
}

// Unregister removes all triggers for a specific position level.
func (e *TriggerEngine) Unregister(pair TokenPair, posID [16]byte, levelIdx int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pt, ok := e.pairs[pair]
	if !ok {
		return
	}

	pt.above = removeEntry(pt.above, posID, levelIdx)
	pt.below = removeEntry(pt.below, posID, levelIdx)
}

// UnregisterPosition removes all triggers for a position.
func (e *TriggerEngine) UnregisterPosition(pair TokenPair, posID [16]byte) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pt, ok := e.pairs[pair]
	if !ok {
		return
	}

	pt.above = removePositionEntries(pt.above, posID)
	pt.below = removePositionEntries(pt.below, posID)
}

// UpdateTriggerPrice changes the trigger price for a specific position level.
func (e *TriggerEngine) UpdateTriggerPrice(pair TokenPair, posID [16]byte, levelIdx int, levelType LevelType, direction Direction, newPrice *big.Int) {
	e.Unregister(pair, posID, levelIdx)
	e.Register(pair, posID, levelIdx, levelType, direction, newPrice)
}

// OnPrice checks if any triggers fire at the given price.
// Returns triggered events and removes them from the index.
// This is O(log n + k) where k is the number of triggered entries.
func (e *TriggerEngine) OnPrice(pair TokenPair, price *big.Int) []TriggerEvent {
	e.mu.Lock()
	defer e.mu.Unlock()

	pt, ok := e.pairs[pair]
	if !ok {
		return nil
	}

	var events []TriggerEvent

	// Check "above" triggers: fire when price >= trigger.
	// These are sorted ascending. All entries where entry.price <= price should fire.
	cutoff := sort.Search(len(pt.above), func(i int) bool {
		return pt.above[i].price.Cmp(price) > 0 // First entry strictly above current price.
	})
	for i := 0; i < cutoff; i++ {
		events = append(events, TriggerEvent{
			PositionID: pt.above[i].positionID,
			LevelIndex: pt.above[i].levelIndex,
			LevelType:  pt.above[i].levelType,
			Price:      new(big.Int).Set(price),
			ChainID:    pair.ChainID,
		})
	}
	pt.above = pt.above[cutoff:] // Remove triggered entries.

	// Check "below" triggers: fire when price <= trigger.
	// These are sorted ascending. All entries where entry.price >= price should fire.
	belowCutoff := sort.Search(len(pt.below), func(i int) bool {
		return pt.below[i].price.Cmp(price) >= 0 // First entry >= current price.
	})
	for i := belowCutoff; i < len(pt.below); i++ {
		events = append(events, TriggerEvent{
			PositionID: pt.below[i].positionID,
			LevelIndex: pt.below[i].levelIndex,
			LevelType:  pt.below[i].levelType,
			Price:      new(big.Int).Set(price),
			ChainID:    pair.ChainID,
		})
	}
	pt.below = pt.below[:belowCutoff] // Remove triggered entries.

	return events
}

// Count returns total number of registered triggers.
func (e *TriggerEngine) Count() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	total := 0
	for _, pt := range e.pairs {
		total += len(pt.above) + len(pt.below)
	}
	return total
}

// --- Helpers ---

func (e *TriggerEngine) getOrCreate(pair TokenPair) *pairTriggers {
	pt, ok := e.pairs[pair]
	if !ok {
		pt = &pairTriggers{}
		e.pairs[pair] = pt
	}
	return pt
}

func shouldTriggerAbove(lt LevelType, dir Direction) bool {
	// TP on Long fires above, SL on Short fires above.
	return (lt == LevelTypeTP && dir == Long) || (lt == LevelTypeSL && dir == Short)
}

// insertSorted inserts an entry into a sorted slice (ascending by price).
func insertSorted(entries []triggerEntry, entry triggerEntry) []triggerEntry {
	i := sort.Search(len(entries), func(j int) bool {
		return entries[j].price.Cmp(entry.price) > 0
	})
	entries = append(entries, triggerEntry{})
	copy(entries[i+1:], entries[i:])
	entries[i] = entry
	return entries
}

func removeEntry(entries []triggerEntry, posID [16]byte, levelIdx int) []triggerEntry {
	n := 0
	for _, e := range entries {
		if e.positionID != posID || e.levelIndex != levelIdx {
			entries[n] = e
			n++
		}
	}
	return entries[:n]
}

func removePositionEntries(entries []triggerEntry, posID [16]byte) []triggerEntry {
	n := 0
	for _, e := range entries {
		if e.positionID != posID {
			entries[n] = e
			n++
		}
	}
	return entries[:n]
}
