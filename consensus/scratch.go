package consensus

import (
	"errors"

	"go.sia.tech/sunyata"
)

// A ScratchChain processes a potential extension or fork of the best chain,
// first validating its headers, then its transactions.
type ScratchChain struct {
	base    sunyata.ChainIndex
	headers []sunyata.BlockHeader
	// for validating headers
	hvc ValidationContext
	// for validating transactions
	tvc ValidationContext
}

// AppendHeader validates the supplied header and appends it to the chain.
// Headers must be appended before their transactions can be filled in with
// AppendBlockTransactions.
func (sc *ScratchChain) AppendHeader(h sunyata.BlockHeader) error {
	if err := sc.hvc.validateHeader(h); err != nil {
		return err
	}
	sc.hvc.applyHeader(h)
	sc.headers = append(sc.headers, h)
	return nil
}

// ApplyBlockTransactions fills in the transactions of the next empty entry in
// the chain. The block's validated header must already exist in the chain.
func (sc *ScratchChain) ApplyBlockTransactions(txns []sunyata.Transaction) (Checkpoint, error) {
	if sc.tvc.Index.Height+1 > sc.hvc.Index.Height {
		return Checkpoint{}, errors.New("more blocks than headers")
	}
	b := sunyata.Block{
		Header:       sc.headers[sc.tvc.Index.Height-sc.Base().Height],
		Transactions: txns,
	}
	if err := sc.tvc.ValidateBlock(b); err != nil {
		return Checkpoint{}, err
	}

	sc.tvc = ApplyBlock(sc.tvc, b).Context
	return Checkpoint{
		Block:   b,
		Context: sc.tvc,
	}, nil
}

// Index returns the header at the specified height. The returned header may or
// may not have a corresponding validated block.
func (sc *ScratchChain) Index(height uint64) sunyata.ChainIndex {
	return sc.headers[height-sc.Base().Height-1].Index()
}

// Base returns the base of the header chain, i.e. the parent of the first
// header.
func (sc *ScratchChain) Base() sunyata.ChainIndex {
	return sc.base
}

// Tip returns the tip of the header chain, which may or may not have a
// corresponding validated block.
func (sc *ScratchChain) Tip() sunyata.ChainIndex {
	return sc.hvc.Index
}

// UnvalidatedBase returns the base of the unvalidated header chain, i.e. the
// lowest index for which there is no validated block. If all of the blocks have
// been validated, UnvalidatedBase panics.
func (sc *ScratchChain) UnvalidatedBase() sunyata.ChainIndex {
	if sc.tvc.Index.Height == sc.base.Height {
		return sc.base
	}
	return sc.Index(sc.tvc.Index.Height + 1)
}

// ValidTip returns the tip of the validated header chain, i.e. the highest
// index for which there is a known validated block.
func (sc *ScratchChain) ValidTip() sunyata.ChainIndex {
	return sc.tvc.Index
}

// FullyValidated is equivalent to sc.Tip() == sc.ValidTip().
func (sc *ScratchChain) FullyValidated() bool {
	return sc.tvc.Index == sc.hvc.Index
}

// TotalWork returns the total work of the header chain.
func (sc *ScratchChain) TotalWork() sunyata.Work {
	return sc.hvc.TotalWork
}

// Contains returns whether the chain contains the specified index. It does not
// indicate whether the transactions for that index have been validated.
func (sc *ScratchChain) Contains(index sunyata.ChainIndex) bool {
	if !(sc.Base().Height < index.Height && index.Height <= sc.Tip().Height) {
		return false
	}
	return sc.Index(index.Height) == index
}

// Unvalidated returns the indexes of all the unvalidated blocks in the chain.
func (sc *ScratchChain) Unvalidated() []sunyata.ChainIndex {
	headers := sc.headers[sc.tvc.Index.Height-sc.Base().Height:]
	indices := make([]sunyata.ChainIndex, len(headers))
	for i := range indices {
		indices[i] = headers[i].Index()
	}
	return indices
}

// NewScratchChain initializes a ScratchChain with the provided validation
// context.
func NewScratchChain(vc ValidationContext) *ScratchChain {
	return &ScratchChain{
		base: vc.Index,
		hvc:  vc,
		tvc:  vc,
	}
}
