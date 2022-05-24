// Package miner provides a basic miner for sunyata, suitable for testing and as
// a basis for more sophisticated implementations.
package miner

import (
	"sync"
	"time"
	"unsafe"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/consensus"
	"go.sia.tech/sunyata/txpool"
)

// A NonceGrinder sets the value of h.Nonce such that h.ID().MeetsTarget(target)
// returns true.
type NonceGrinder interface {
	GrindNonce(h *sunyata.BlockHeader, target sunyata.BlockID)
}

// A Miner mines blocks.
type Miner struct {
	genesis sunyata.Block
	addr    sunyata.Address
	tp      *txpool.Pool
	grinder NonceGrinder

	mu    sync.Mutex
	cs    consensus.State
	mined int
	rate  float64
}

// Stats reports various mining statistics.
func (m *Miner) Stats() (mined int, rate float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mined, m.rate
}

func (m *Miner) txnsForBlock() (blockTxns []sunyata.Transaction) {
	poolTxns := make(map[sunyata.TransactionID]sunyata.Transaction)
	for _, txn := range m.tp.Transactions() {
		poolTxns[txn.ID()] = txn
	}

	// define a helper function that returns the unmet dependencies of a given
	// transaction
	calcDeps := func(txn sunyata.Transaction) (deps []sunyata.Transaction) {
		added := make(map[sunyata.TransactionID]bool)
		var addDeps func(txn sunyata.Transaction)
		addDeps = func(txn sunyata.Transaction) {
			added[txn.ID()] = true
			for _, in := range txn.Inputs {
				parentID := sunyata.TransactionID(in.Parent.ID.Source)
				if parent, inPool := poolTxns[parentID]; inPool && !added[parentID] {
					// NOTE: important that we add the parent's deps before the
					// parent itself
					addDeps(parent)
					deps = append(deps, parent)
				}
			}
			return
		}
		addDeps(txn)
		return
	}

	capacity := m.cs.MaxBlockWeight()
	for _, txn := range poolTxns {
		// prepend the txn with its dependencies
		group := append(calcDeps(txn), txn)
		// if the weight of the group exceeds the remaining capacity of the
		// block, skip it
		groupWeight := m.cs.BlockWeight(group)
		if groupWeight > capacity {
			continue
		}
		// add the group to the block
		blockTxns = append(blockTxns, group...)
		capacity -= groupWeight
		for _, txn := range group {
			delete(poolTxns, txn.ID())
		}
	}

	return
}

// MineBlock mines a valid block, using transactions drawn from the txpool.
func (m *Miner) MineBlock() sunyata.Block {
	for {
		m.mu.Lock()
		parent := m.cs.Index
		target := sunyata.HashRequiringWork(m.cs.Difficulty)
		addr := m.addr
		txns := m.txnsForBlock()
		commitment := m.cs.Commitment(addr, txns)
		m.mu.Unlock()
		b := sunyata.Block{
			Header: sunyata.BlockHeader{
				Height:       parent.Height + 1,
				ParentID:     parent.ID,
				Timestamp:    sunyata.CurrentTimestamp(),
				Commitment:   commitment,
				MinerAddress: addr,
			},
			Transactions: txns,
		}

		// grind
		start := time.Now()
		m.grinder.GrindNonce(&b.Header, target)
		elapsed := time.Since(start)

		// update stats, and check whether the tip has changed since we started
		// grinding; if it has, we need to start over
		m.mu.Lock()
		m.mined++
		m.rate = 1 / elapsed.Seconds()
		tipChanged := m.cs.Index != parent
		m.mu.Unlock()
		if !tipChanged {
			return b
		}
	}
}

// ProcessChainApplyUpdate implements chain.Subscriber.
func (m *Miner) ProcessChainApplyUpdate(cau *chain.ApplyUpdate, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cs = cau.State
	return nil
}

// ProcessChainRevertUpdate implements chain.Subscriber.
func (m *Miner) ProcessChainRevertUpdate(cru *chain.RevertUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cs = cru.State
	return nil
}

// New returns a Miner initialized with the provided state.
func New(cs consensus.State, addr sunyata.Address, tp *txpool.Pool, ng NonceGrinder) *Miner {
	return &Miner{
		cs:      cs,
		addr:    addr,
		tp:      tp,
		grinder: ng,
	}
}

// CPU grinds nonces with a single CPU thread.
var CPU cpuGrinder

type cpuGrinder struct{}

// GrindNonce implements NonceGrinder.
func (cpuGrinder) GrindNonce(h *sunyata.BlockHeader, target sunyata.BlockID) {
	for !h.ID().MeetsTarget(target) {
		// NOTE: an unsafe cast is fine here; we don't care about endianness,
		// only that the nonce is changing
		*(*uint64)(unsafe.Pointer(&h.Nonce))++
	}
}
