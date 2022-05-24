package consensus

import (
	"sync"
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/merkle"
)

// Pool for reducing heap allocations when hashing. This is only necessary
// because blake2b.New256 returns a hash.Hash interface, which prevents the
// compiler from doing escape analysis. Can be removed if we switch to an
// implementation whose constructor returns a concrete type.
var hasherPool = &sync.Pool{New: func() interface{} { return sunyata.NewHasher() }}

// State represents the full state of the chain as of a particular block.
type State struct {
	Index          sunyata.ChainIndex
	Elements       merkle.ElementAccumulator
	History        merkle.HistoryAccumulator
	TotalWork      sunyata.Work
	Difficulty     sunyata.Work
	LastAdjust     time.Time
	PrevTimestamps [11]time.Time
}

// EncodeTo implements sunyata.EncoderTo.
func (s State) EncodeTo(e *sunyata.Encoder) {
	s.Index.EncodeTo(e)
	s.Elements.EncodeTo(e)
	s.History.EncodeTo(e)
	s.TotalWork.EncodeTo(e)
	s.Difficulty.EncodeTo(e)
	e.WriteTime(s.LastAdjust)
	for _, ts := range s.PrevTimestamps {
		e.WriteTime(ts)
	}
}

// DecodeFrom implements sunyata.DecoderFrom.
func (s *State) DecodeFrom(d *sunyata.Decoder) {
	s.Index.DecodeFrom(d)
	s.Elements.DecodeFrom(d)
	s.History.DecodeFrom(d)
	s.TotalWork.DecodeFrom(d)
	s.Difficulty.DecodeFrom(d)
	s.LastAdjust = d.ReadTime()
	for i := range s.PrevTimestamps {
		s.PrevTimestamps[i] = d.ReadTime()
	}
}

func (s State) numTimestamps() int {
	if s.Index.Height+1 < uint64(len(s.PrevTimestamps)) {
		return int(s.Index.Height + 1)
	}
	return len(s.PrevTimestamps)
}

// BlockInterval is the expected wall clock time between consecutive blocks.
func (s State) BlockInterval() time.Duration {
	return 10 * time.Minute
}

// DifficultyAdjustmentInterval is the number of blocks between adjustments to
// the block mining target.
func (s State) DifficultyAdjustmentInterval() uint64 {
	return 2016
}

// BlockReward returns the reward for mining a child block.
func (s State) BlockReward() sunyata.Currency {
	r := sunyata.BaseUnitsPerCoin.Mul64(50)
	n := (s.Index.Height + 1) / 210000
	return sunyata.NewCurrency(r.Lo>>n|r.Hi<<(64-n), r.Hi>>n) // r >> n
}

// MaturityHeight is the height at which various outputs created in the child
// block will "mature" (become spendable).
func (s State) MaturityHeight() uint64 {
	return (s.Index.Height + 1) + 144
}

// MaxBlockWeight is the maximum "weight" of a valid child block.
func (s State) MaxBlockWeight() uint64 {
	return 1_000_000
}

// TransactionWeight computes the weight of a txn.
func (s State) TransactionWeight(txn sunyata.Transaction) uint64 {
	return uint64(4*len(txn.Inputs) + len(txn.Outputs))
}

// BlockWeight computes the combined weight of a block's txns.
func (s State) BlockWeight(txns []sunyata.Transaction) uint64 {
	var weight uint64
	for _, txn := range txns {
		weight += s.TransactionWeight(txn)
	}
	return weight
}

// Commitment computes the commitment hash for a child block.
func (s State) Commitment(minerAddr sunyata.Address, txns []sunyata.Transaction) sunyata.Hash256 {
	h := hasherPool.Get().(*sunyata.Hasher)
	defer hasherPool.Put(h)
	h.Reset()

	// hash the state
	s.EncodeTo(h.E)
	stateHash := h.Sum()

	// hash the transactions
	h.Reset()
	h.E.WritePrefix(len(txns))
	for _, txn := range txns {
		txn.ID().EncodeTo(h.E)
	}
	txnsHash := h.Sum()

	// concatenate the hashes and the miner address
	h.Reset()
	h.E.WriteString("sunyata/commitment")
	stateHash.EncodeTo(h.E)
	minerAddr.EncodeTo(h.E)
	txnsHash.EncodeTo(h.E)
	return h.Sum()
}

// InputSigHash returns the hash that must be signed for each transaction input.
func (s State) InputSigHash(txn sunyata.Transaction) sunyata.Hash256 {
	// NOTE: This currently covers exactly the same fields as txn.ID(), and for
	// similar reasons.
	h := hasherPool.Get().(*sunyata.Hasher)
	defer hasherPool.Put(h)
	h.Reset()
	h.E.WriteString("sunyata/sig/transactioninput")
	h.E.WritePrefix(len(txn.Inputs))
	for _, in := range txn.Inputs {
		in.Parent.ID.EncodeTo(h.E)
	}
	h.E.WritePrefix(len(txn.Outputs))
	for _, out := range txn.Outputs {
		out.EncodeTo(h.E)
	}
	txn.MinerFee.EncodeTo(h.E)
	return h.Sum()
}

// A Checkpoint pairs a block with its resulting chain state.
type Checkpoint struct {
	Block sunyata.Block
	State State
}
