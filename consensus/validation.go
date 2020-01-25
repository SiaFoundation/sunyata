// Package consensus implements the sunyata consensus algorithms.
package consensus

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.sia.tech/sunyata"
)

const (
	// MedianTimestampWindow is the number of blocks used when computing the median
	// timestamp.
	MedianTimestampWindow = 11

	maxFutureBlockTime = 2 * time.Hour

	// MaxBlockWeight is the maximum "weight" of a valid block. A block's weight is
	// the sum of the weights of its transactions. The weight of a transaction is
	// given by:
	//
	//   weight := 4*inputs + outputs
	MaxBlockWeight = 100e3
)

var (
	// ErrFutureBlock is returned by AppendHeader if a block's timestamp is too far
	// in the future. The block may be valid at a later time.
	ErrFutureBlock = errors.New("timestamp is too far in the future")

	// ErrOverweight is returned when a block's weight exceeds MaxBlockWeight.
	ErrOverweight = errors.New("block is too heavy")
)

// Pool for reducing heap allocations when hashing. This are only necessary
// because sha512.New512_256 returns a hash.Hash interface, which prevents the
// compiler from doing escape analysis. Can be removed if we switch to an
// implementation whose constructor returns a concrete type.
var hasherPool = &sync.Pool{New: func() interface{} { return sunyata.NewHasher() }}

// TransactionWeight computes the weight of a txn.
func TransactionWeight(txn sunyata.Transaction) uint64 {
	return uint64(4*len(txn.Inputs) + len(txn.Outputs))
}

// BlockWeight computes the combined weight of a block's txns.
func BlockWeight(txns []sunyata.Transaction) uint64 {
	var weight uint64
	for _, txn := range txns {
		weight += TransactionWeight(txn)
	}
	return weight
}

// TransactionsHash computes the hash of a set of Transactions.
func TransactionsHash(txns []sunyata.Transaction) sunyata.Hash256 {
	h := hasherPool.Get().(*sunyata.Hasher)
	defer hasherPool.Put(h)
	h.Reset()

	for _, txn := range txns {
		for _, in := range txn.Inputs {
			h.WriteHash(in.Parent.ID.TransactionID)
			h.WriteUint64(in.Parent.ID.BeneficiaryIndex)
			h.WriteCurrency(in.Parent.Value)
			h.WriteHash(in.Parent.Address)
			h.WriteUint64(in.Parent.Timelock)
			for _, p := range in.Parent.MerkleProof {
				h.WriteHash(p)
			}
			h.WriteUint64(in.Parent.LeafIndex)
			h.WriteHash(in.PublicKey)
			h.Write(in.Signature[:])
		}
		for _, out := range txn.Outputs {
			h.WriteCurrency(out.Value)
			h.WriteHash(out.Address)
		}
		h.WriteCurrency(txn.MinerFee)
	}
	return h.Sum()
}

// ComputeCommitment computes the commitment hash for a block.
func ComputeCommitment(minerAddr sunyata.Address, txns sunyata.Hash256, context sunyata.Hash256) sunyata.Hash256 {
	buf := make([]byte, 32+32+32)
	n := copy(buf[0:], minerAddr[:])
	n += copy(buf[n:], txns[:])
	n += copy(buf[n:], context[:])
	return sunyata.HashBytes(buf)
}

// ValidationContext contains the necessary context to fully validate a block.
type ValidationContext struct {
	Index          sunyata.ChainIndex
	State          StateAccumulator
	TotalWork      sunyata.Work
	Difficulty     sunyata.Work
	LastAdjust     time.Time
	PrevTimestamps [MedianTimestampWindow]time.Time
}

func (vc *ValidationContext) numTimestamps() int {
	if vc.Index.Height+1 < uint64(len(vc.PrevTimestamps)) {
		return int(vc.Index.Height + 1)
	}
	return len(vc.PrevTimestamps)
}

func (vc *ValidationContext) medianTimestamp() time.Time {
	prevCopy := vc.PrevTimestamps
	ts := prevCopy[:vc.numTimestamps()]
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	if len(ts)%2 != 0 {
		return ts[len(ts)/2]
	}
	l, r := ts[len(ts)/2-1], ts[len(ts)/2]
	return l.Add(r.Sub(l) / 2)
}

func (vc *ValidationContext) validateHeader(h sunyata.BlockHeader) error {
	if h.Height != vc.Index.Height+1 {
		return errors.New("wrong height")
	} else if h.ParentID != vc.Index.ID {
		return errors.New("wrong parent ID")
	} else if sunyata.WorkRequiredForHash(h.ID()).Cmp(vc.Difficulty) < 0 {
		return errors.New("insufficient work")
	} else if time.Until(h.Timestamp) > maxFutureBlockTime {
		return ErrFutureBlock
	} else if h.Timestamp.Before(vc.medianTimestamp()) {
		return errors.New("timestamp is too far in the past")
	}
	return nil
}

// ValidateBlock validates b in the context of vc.
func (vc *ValidationContext) ValidateBlock(b sunyata.Block) error {
	h := b.Header
	if err := vc.validateHeader(h); err != nil {
		return err
	} else if ComputeCommitment(h.MinerAddress, TransactionsHash(b.Transactions), vc.Hash()) != h.Commitment {
		return errors.New("commitment hash does not match header")
	} else if err := vc.ValidateTransactionSet(b.Transactions); err != nil {
		return err
	}
	return nil
}

// Hash returns a hash of all of the data in a ValidationContext.
func (vc *ValidationContext) Hash() sunyata.Hash256 {
	h := hasherPool.Get().(*sunyata.Hasher)
	defer hasherPool.Put(h)
	h.Reset()

	h.WriteUint64(vc.Index.Height)
	h.WriteHash(vc.Index.ID)
	h.WriteUint64(vc.State.NumLeaves)
	for i, root := range vc.State.Trees {
		if vc.State.HasTreeAtHeight(i) {
			h.WriteHash(root)
		}
	}
	h.WriteHash(vc.TotalWork.NumHashes)
	h.WriteHash(vc.Difficulty.NumHashes)
	h.WriteTime(vc.LastAdjust)
	for _, ts := range vc.PrevTimestamps {
		h.WriteTime(ts)
	}
	return h.Sum()
}

func (vc *ValidationContext) applyHeader(h sunyata.BlockHeader) {
	blockWork := sunyata.WorkRequiredForHash(h.ID())
	if h.Height > 0 && h.Height%sunyata.DifficultyAdjustmentInterval == 0 {
		vc.Difficulty = sunyata.AdjustDifficulty(vc.Difficulty, h.Timestamp.Sub(vc.LastAdjust))
		vc.LastAdjust = h.Timestamp
	}
	vc.TotalWork = vc.TotalWork.Add(blockWork)
	if vc.numTimestamps() == len(vc.PrevTimestamps) {
		copy(vc.PrevTimestamps[:], vc.PrevTimestamps[1:])
	}
	vc.PrevTimestamps[vc.numTimestamps()-1] = h.Timestamp
	vc.Index = h.Index()
}

func (vc *ValidationContext) containsZeroValuedOutputs(txn sunyata.Transaction) bool {
	for _, out := range txn.Outputs {
		if out.Value.IsZero() {
			return true
		}
	}
	return false
}

func (vc *ValidationContext) validTimeLocks(txn sunyata.Transaction) bool {
	blockHeight := vc.Index.Height + 1
	for _, in := range txn.Inputs {
		if in.Parent.Timelock > blockHeight {
			return false
		}
	}
	return true
}

func (vc *ValidationContext) validPubkeys(txn sunyata.Transaction) bool {
	for _, in := range txn.Inputs {
		if in.PublicKey.Address() != in.Parent.Address {
			return false
		}
	}
	return true
}

func (vc *ValidationContext) outputsEqualInputs(txn sunyata.Transaction) bool {
	var inputSum, outputSum sunyata.Currency
	var overflowed bool
	for _, in := range txn.Inputs {
		inputSum, overflowed = inputSum.AddWithOverflow(in.Parent.Value)
		if overflowed {
			return false
		}
	}
	for _, out := range txn.Outputs {
		outputSum, overflowed = outputSum.AddWithOverflow(out.Value)
		if overflowed {
			return false
		}
	}
	outputSum, overflowed = outputSum.AddWithOverflow(txn.MinerFee)
	return !overflowed && inputSum == outputSum
}

func (vc *ValidationContext) validInputMerkleProofs(txn sunyata.Transaction) bool {
	for _, in := range txn.Inputs {
		if in.Parent.LeafIndex == sunyata.EphemeralLeafIndex {
			// ephemeral outputs do not require Merkle proofs; they are
			// validated in validEphemeralOutputs
			continue
		}
		if !vc.State.containsOutput(&in.Parent, false) {
			return false
		}
	}
	return true
}

func (vc *ValidationContext) validSignatures(txn sunyata.Transaction) bool {
	sigHash := txn.SigHash()
	for _, in := range txn.Inputs {
		if !ed25519.Verify(in.PublicKey[:], sigHash[:], in.Signature[:]) {
			return false
		}
	}
	return true
}

// ValidateTransaction validates txn in the context of the provided chain height
// and accumulator. Any ephemeral outputs are assumed to be valid. To validate
// ephemeral outputs, use ValidateTransactionSet.
func (vc *ValidationContext) ValidateTransaction(txn sunyata.Transaction) error {
	switch {
	case vc.containsZeroValuedOutputs(txn):
		return errors.New("transaction contains zero-valued outputs")
	case !vc.validTimeLocks(txn):
		return errors.New("transaction spends time-locked outputs")
	case !vc.outputsEqualInputs(txn):
		return errors.New("outputs of transaction do not equal its inputs")
	case !vc.validPubkeys(txn):
		return errors.New("transaction contains unlock conditions that do not hash to the correct address")
	case !vc.validInputMerkleProofs(txn):
		return errors.New("transaction contains an invalid Merkle proof")
	case !vc.validSignatures(txn):
		return errors.New("transaction contains an invalid signature")
	}
	return nil
}

func (vc *ValidationContext) validEphemeralOutputs(txns []sunyata.Transaction) error {
	// TODO: this is rather inefficient. Definitely need a better algorithm.
	available := make(map[sunyata.OutputID]struct{})
	for txnIndex, txn := range txns {
		txid := txn.ID()
		for _, in := range txn.Inputs {
			if in.Parent.LeafIndex == sunyata.EphemeralLeafIndex {
				oid := in.Parent.ID
				if _, ok := available[oid]; !ok {
					return fmt.Errorf("transaction set is invalid: transaction %v claims a non-existent ephemeral output", txnIndex)
				}
				delete(available, oid)
			}
		}
		for i := range txn.Outputs {
			oid := sunyata.OutputID{
				TransactionID:    txid,
				BeneficiaryIndex: uint64(i),
			}
			available[oid] = struct{}{}
		}
	}
	return nil
}

func (vc *ValidationContext) noDoubleSpends(txns []sunyata.Transaction) error {
	spent := make(map[uint64]struct{})
	for i := range txns {
		for j := range txns[i].Inputs {
			index := txns[i].Inputs[j].Parent.LeafIndex
			if _, ok := spent[index]; ok && index != sunyata.EphemeralLeafIndex {
				return fmt.Errorf("transaction set is invalid: transaction %v double-spends output %v", i, index)
			}
			spent[index] = struct{}{}
		}
	}
	return nil
}

// ValidateTransactionSet validates txns in their corresponding validation context.
func (vc *ValidationContext) ValidateTransactionSet(txns []sunyata.Transaction) error {
	if BlockWeight(txns) > MaxBlockWeight {
		return ErrOverweight
	}
	if err := vc.validEphemeralOutputs(txns); err != nil {
		return err
	}
	if err := vc.noDoubleSpends(txns); err != nil {
		return err
	}
	for i, txn := range txns {
		if err := vc.ValidateTransaction(txn); err != nil {
			return fmt.Errorf("transaction %v is invalid: %w", i, err)
		}
	}
	return nil
}

// A Checkpoint pairs a block with the context used to validate its children.
type Checkpoint struct {
	Block   sunyata.Block
	Context ValidationContext
}
