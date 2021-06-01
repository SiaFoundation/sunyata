// Package miner provides a basic miner for sunyata, suitable for testing and as
// a basis for more sophisticated implementations.
package miner

import (
	"crypto/rand"
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
	vc    consensus.ValidationContext
	mined int
	rate  float64
}

// Stats reports various mining statistics.
func (m *Miner) Stats() (mined int, rate float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mined, m.rate
}

// MineBlock mines a valid block, using transactions drawn from the txpool.
func (m *Miner) MineBlock() sunyata.Block {
	for {
		// TODO: if the miner and txpool don't have the same tip, we'll
		// construct an invalid block
		txns := m.tp.Transactions()

		m.mu.Lock()
		var weight uint64
		for i := range txns {
			weight += m.vc.TransactionWeight(txns[i])
			if weight > m.vc.MaxBlockWeight() {
				txns = txns[:i]
				break
			}
		}
		proof := consensus.ComputeMultiproof(txns)
		parent := m.vc.Index
		target := sunyata.HashRequiringWork(m.vc.Difficulty)
		addr := m.addr
		commitment := m.vc.Commitment(addr, txns, proof)
		m.mu.Unlock()
		b := sunyata.Block{
			Header: sunyata.BlockHeader{
				Height:       parent.Height + 1,
				ParentID:     parent.ID,
				Timestamp:    sunyata.CurrentTimestamp(),
				Commitment:   commitment,
				MinerAddress: addr,
			},
			Transactions:     txns,
			AccumulatorProof: proof,
		}
		rand.Read(b.Header.Nonce[:])

		// grind
		start := time.Now()
		m.grinder.GrindNonce(&b.Header, target)
		elapsed := time.Since(start)

		// update stats, and check whether the tip has changed since we started
		// grinding; if it has, we need to start over
		m.mu.Lock()
		m.mined++
		m.rate = 1 / elapsed.Seconds()
		tipChanged := m.vc.Index != parent
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
	m.vc = cau.Context
	return nil
}

// ProcessChainRevertUpdate implements chain.Subscriber.
func (m *Miner) ProcessChainRevertUpdate(cru *chain.RevertUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vc = cru.Context
	return nil
}

// New returns a Miner initialized with the provided state.
func New(vc consensus.ValidationContext, addr sunyata.Address, tp *txpool.Pool, ng NonceGrinder) *Miner {
	return &Miner{
		vc:      vc,
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
