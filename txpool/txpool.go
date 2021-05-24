// Package txpool provides a transaction pool (or "mempool") for sunyata.
package txpool

import (
	"sync"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/consensus"
)

// A Pool holds transactions that may be included in future blocks.
type Pool struct {
	txns map[sunyata.TransactionID]sunyata.Transaction
	vc   consensus.ValidationContext
	mu   sync.Mutex
}

// AddTransaction adds a transaction to the pool. The transaction must be valid
// as of the current chain tip and accumulator state.
func (p *Pool) AddTransaction(txn sunyata.Transaction) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	txid := txn.ID()
	if _, ok := p.txns[txid]; ok {
		return nil // already in pool
	} else if err := p.vc.ValidateTransaction(txn); err != nil {
		return err
	}
	p.txns[txid] = txn
	return nil
}

// Transaction returns the transaction with the specified ID, if it is currently
// in the pool.
func (p *Pool) Transaction(id sunyata.TransactionID) (sunyata.Transaction, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	txn, ok := p.txns[id]
	return txn, ok
}

// Transactions returns the transactions currently in the pool.
func (p *Pool) Transactions() []sunyata.Transaction {
	p.mu.Lock()
	defer p.mu.Unlock()
	txns := make([]sunyata.Transaction, 0, len(p.txns))
	for _, txn := range p.txns {
		txns = append(txns, txn.DeepCopy())
	}
	return txns
}

// ProcessChainApplyUpdate implements chain.Subscriber.
func (p *Pool) ProcessChainApplyUpdate(cau *chain.ApplyUpdate, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// delete confirmed txns
	for _, txn := range cau.Block.Transactions {
		delete(p.txns, txn.ID())
	}

	// update unconfirmed txns
outer:
	for id, txn := range p.txns {
		// if any of the inputs were spent, the txn is now invalid; delete it
		for i := range txn.Inputs {
			if cau.OutputWasSpent(txn.Inputs[i].Parent.LeafIndex) {
				delete(p.txns, id)
				continue outer
			}
		}
		// all inputs still unspent; update proofs
		for i := range txn.Inputs {
			cau.UpdateOutputProof(&txn.Inputs[i].Parent)
		}

		// verify that the transaction is still valid
		//
		// NOTE: in theory we should only need to run height-dependent checks
		// here (e.g. timelocks); but it doesn't hurt to be extra thorough. Easy
		// to remove later if it becomes a bottleneck.
		if err := cau.Context.ValidateTransaction(txn); err != nil {
			delete(p.txns, id)
			continue
		}

		p.txns[id] = txn
	}

	// update validation context
	p.vc = cau.Context
	return nil
}

// ProcessChainRevertUpdate implements chain.Subscriber.
func (p *Pool) ProcessChainRevertUpdate(cru *chain.RevertUpdate) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// put reverted txns back in the pool
	for _, txn := range cru.Block.Transactions {
		p.txns[txn.ID()] = txn.DeepCopy()
	}

	// update unconfirmed txns
outer:
	for id, txn := range p.txns {
		// if any of the inputs no longer exist, the txn is now invalid; delete it
		for i := range txn.Inputs {
			if cru.OutputWasRemoved(txn.Inputs[i].Parent.LeafIndex) {
				delete(p.txns, id)
				continue outer
			}
		}
		// all inputs still unspent; update proofs
		for i := range txn.Inputs {
			cru.UpdateOutputProof(&txn.Inputs[i].Parent)
		}

		// verify that the transaction is still valid
		if err := cru.Context.ValidateTransaction(txn); err != nil {
			delete(p.txns, id)
			continue
		}

		p.txns[id] = txn
	}

	// update validation context
	p.vc = cru.Context
	return nil
}

// New creates a new transaction pool.
func New(vc consensus.ValidationContext) *Pool {
	return &Pool{
		txns: make(map[sunyata.TransactionID]sunyata.Transaction),
		vc:   vc,
	}
}
