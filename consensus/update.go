package consensus

import (
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/merkle"
)

func adjustDifficulty(s *State, h sunyata.BlockHeader) sunyata.Work {
	expectedDelta := s.BlockInterval() * time.Duration(s.DifficultyAdjustmentInterval())
	actualDelta := h.Timestamp.Sub(s.LastAdjust)
	if actualDelta > expectedDelta*4 {
		actualDelta = expectedDelta * 4
	} else if actualDelta < expectedDelta/4 {
		actualDelta = expectedDelta / 4
	}
	return s.Difficulty.Mul64(uint64(expectedDelta)).Div64(uint64(actualDelta))
}

func applyHeader(s *State, h sunyata.BlockHeader) {
	if h.Height == 0 {
		// special handling for GenesisUpdate
		s.PrevTimestamps[0] = h.Timestamp
		s.Index = h.Index()
		return
	}
	s.TotalWork = s.TotalWork.Add(s.Difficulty)
	if h.Height%s.DifficultyAdjustmentInterval() == 0 {
		s.Difficulty = adjustDifficulty(s, h)
		s.LastAdjust = h.Timestamp
	}
	if s.numTimestamps() < len(s.PrevTimestamps) {
		s.PrevTimestamps[s.numTimestamps()] = h.Timestamp
	} else {
		copy(s.PrevTimestamps[:], s.PrevTimestamps[1:])
		s.PrevTimestamps[len(s.PrevTimestamps)-1] = h.Timestamp
	}
	s.Index = h.Index()
}

func updatedInBlock(s State, b sunyata.Block, apply bool) (oes []sunyata.OutputElement, leaves []merkle.ElementLeaf) {
	for _, txn := range b.Transactions {
		for _, in := range txn.Inputs {
			if in.Parent.LeafIndex != sunyata.EphemeralLeafIndex {
				oes = append(oes, in.Parent)
				l := merkle.OutputLeaf(in.Parent, apply)
				// copy proofs so we don't mutate transaction data
				l.MerkleProof = append([]sunyata.Hash256(nil), l.MerkleProof...)
				leaves = append(leaves, l)
			}
		}
	}
	return
}

func createdInBlock(s State, b sunyata.Block) (oes []sunyata.OutputElement) {
	oes = append(oes, sunyata.OutputElement{
		StateElement: sunyata.StateElement{
			ID: b.MinerOutputID(),
		},
		Output: sunyata.Output{
			Value:   s.BlockReward(),
			Address: b.Header.MinerAddress,
		},
		MaturityHeight: s.MaturityHeight(),
	})
	for _, txn := range b.Transactions {
		txid := txn.ID()
		var index uint64
		nextElement := func() sunyata.StateElement {
			index++
			return sunyata.StateElement{
				ID: sunyata.ElementID{
					Source: sunyata.Hash256(txid),
					Index:  index - 1,
				},
			}
		}

		for _, out := range txn.Outputs {
			oes = append(oes, sunyata.OutputElement{
				StateElement: nextElement(),
				Output:       out,
			})
		}
	}

	return
}

// A ApplyUpdate reflects the changes to consensus state resulting from the
// application of a block.
type ApplyUpdate struct {
	merkle.ElementApplyUpdate
	merkle.HistoryApplyUpdate

	State             State
	SpentOutputs      []sunyata.OutputElement
	NewOutputElements []sunyata.OutputElement
}

// OutputElementWasSpent returns true if the OutputElement was spent.
func (au *ApplyUpdate) OutputElementWasSpent(oe sunyata.OutputElement) bool {
	for i := range au.SpentOutputs {
		if au.SpentOutputs[i].LeafIndex == oe.LeafIndex {
			return true
		}
	}
	return false
}

// UpdateTransactionProofs updates the element proofs of a transaction.
func (au *ApplyUpdate) UpdateTransactionProofs(txn *sunyata.Transaction) {
	for i := range txn.Inputs {
		if txn.Inputs[i].Parent.LeafIndex != sunyata.EphemeralLeafIndex {
			au.UpdateElementProof(&txn.Inputs[i].Parent.StateElement)
		}
	}
}

// ApplyBlock integrates a block into the current consensus state, producing an
// ApplyUpdate detailing the resulting changes. The block is assumed to be fully
// validated.
func ApplyBlock(s State, b sunyata.Block) (au ApplyUpdate) {
	if s.Index.Height > 0 && s.Index != b.Header.ParentIndex() {
		panic("consensus: cannot apply non-child block")
	}

	// update elements
	var updated, created []merkle.ElementLeaf
	au.SpentOutputs, updated = updatedInBlock(s, b, true)
	au.NewOutputElements = createdInBlock(s, b)
	spent := make(map[sunyata.ElementID]bool)
	for _, txn := range b.Transactions {
		for _, in := range txn.Inputs {
			if in.Parent.LeafIndex == sunyata.EphemeralLeafIndex {
				spent[in.Parent.ID] = true
			}
		}
	}
	for _, oe := range au.NewOutputElements {
		created = append(created, merkle.OutputLeaf(oe, spent[oe.ID]))
	}
	au.ElementApplyUpdate = s.Elements.ApplyBlock(updated, created)
	for i := range au.NewOutputElements {
		au.NewOutputElements[i].StateElement = created[0].StateElement
		created = created[1:]
	}

	// update history
	au.HistoryApplyUpdate = s.History.ApplyBlock(b.Index())

	// update state
	applyHeader(&s, b.Header)
	au.State = s

	return
}

// GenesisUpdate returns the ApplyUpdate for the genesis block b.
func GenesisUpdate(b sunyata.Block, initialDifficulty sunyata.Work) ApplyUpdate {
	return ApplyBlock(State{
		Difficulty: initialDifficulty,
		LastAdjust: b.Header.Timestamp,
	}, b)
}

// A RevertUpdate reflects the changes to consensus state resulting from the
// removal of a block.
type RevertUpdate struct {
	merkle.ElementRevertUpdate
	merkle.HistoryRevertUpdate

	State             State
	SpentOutputs      []sunyata.OutputElement
	NewOutputElements []sunyata.OutputElement
}

// OutputElementWasRemoved returns true if the specified OutputElement was
// reverted.
func (ru *RevertUpdate) OutputElementWasRemoved(oe sunyata.OutputElement) bool {
	return oe.LeafIndex != sunyata.EphemeralLeafIndex && oe.LeafIndex >= ru.State.Elements.NumLeaves
}

// UpdateTransactionProofs updates the element proofs of a transaction.
func (ru *RevertUpdate) UpdateTransactionProofs(txn *sunyata.Transaction) {
	for i := range txn.Inputs {
		if txn.Inputs[i].Parent.LeafIndex != sunyata.EphemeralLeafIndex {
			ru.UpdateElementProof(&txn.Inputs[i].Parent.StateElement)
		}
	}
}

// RevertBlock produces a RevertUpdate from a block and the State
// prior to that block.
func RevertBlock(s State, b sunyata.Block) (ru RevertUpdate) {
	if b.Header.Height == 0 {
		panic("consensus: cannot revert genesis block")
	} else if s.Index != b.Header.ParentIndex() {
		panic("consensus: cannot revert non-child block")
	}

	ru.State = s
	ru.HistoryRevertUpdate = ru.State.History.RevertBlock(b.Index())
	var updated []merkle.ElementLeaf
	ru.SpentOutputs, updated = updatedInBlock(s, b, false)
	ru.NewOutputElements = createdInBlock(s, b)
	ru.ElementRevertUpdate = ru.State.Elements.RevertBlock(updated)
	return
}
