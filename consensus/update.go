package consensus

import (
	"go.sia.tech/sunyata"
)

// A StateApplyUpdate reflects the changes to consensus state resulting from the
// application of a block.
type StateApplyUpdate struct {
	Context      ValidationContext
	NewOutputs   []sunyata.Output
	spentOutputs [64][]sunyata.Output
	treeGrowth   [64][]sunyata.Hash256
}

// OutputWasSpent returns true if the output with the given leaf index was
// spent.
func (sau *StateApplyUpdate) OutputWasSpent(leafIndex uint64) bool {
	for i := range sau.spentOutputs {
		for _, so := range sau.spentOutputs[i] {
			if so.LeafIndex == leafIndex {
				return true
			}
		}
	}
	return false
}

// UpdateOutputProof updates the Merkle proof of the supplied output to
// incorporate the changes made to the state tree. The output's proof must be
// up-to-date; if it is not, UpdateOutputProof may panic.
func (sau *StateApplyUpdate) UpdateOutputProof(o *sunyata.Output) {
	// A state update can affect an output proof in two ways. First, if an
	// output within the same tree as o was spent, then all of the proof hashes
	// above the "merge point" of o and the spent output will change. Second, if
	// new outputs were added to the Forest, then o may now be within a larger
	// tree, so its proof must be extended to reach the root of this tree.

	// Update the proof to reflect the spent output leaves. What we really care
	// about is the spent output "closest" to ours (where "closest" means the
	// lowest mergeHeight). If we have that output, we can simply copy over all
	// the hashes above their mergeHeight. We'll also need the root of the
	// sibling subtree immediately *prior* to the merge point. Lastly, we need
	// to handle the case where o itself was spent. In that case, we can copy
	// over the entire proof, and there is no subtree prior to the merge point
	// to compute (since the mergeHeight will be 0).
	if spentInTree := sau.spentOutputs[len(o.MerkleProof)]; len(spentInTree) > 0 {
		// find the closest spent output
		best := spentInTree[0]
		bestMergeHeight := mergeHeight(o.LeafIndex, best.LeafIndex)
		for _, so := range spentInTree[1:] {
			if mh := mergeHeight(o.LeafIndex, so.LeafIndex); mh < bestMergeHeight {
				best, bestMergeHeight = so, mh
			}
		}
		if best.LeafIndex == o.LeafIndex {
			// o was spent; copy over the entire updated proof
			copy(o.MerkleProof, best.MerkleProof)
		} else {
			// o was not spent; copy everything above the mergeHeight, then
			// compute the root prior to the mergeHeight. We can do this by
			// artificially truncating the spent output's proof, and then
			// calling ProofRoot.
			copy(o.MerkleProof[bestMergeHeight:], best.MerkleProof[bestMergeHeight:])
			best.MerkleProof = best.MerkleProof[:bestMergeHeight-1]
			o.MerkleProof[bestMergeHeight-1] = outputProofRoot(best, true)
		}
	}
	// Update the proof to incorporate the "growth" of the tree it was in.
	o.MerkleProof = append(o.MerkleProof, sau.treeGrowth[len(o.MerkleProof)]...)
}

func applyHeader(vc ValidationContext, h sunyata.BlockHeader) ValidationContext {
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
	vc.History.AppendLeaf(vc.Index)
	return vc
}

// ApplyBlock integrates a block into the current consensus state, producing
// a StateApplyUpdate detailing the resulting changes. The block is assumed to
// be fully validated.
func ApplyBlock(vc ValidationContext, b sunyata.Block) (sau StateApplyUpdate) {
	sau.Context = applyHeader(vc, b.Header)

	sau.spentOutputs = sau.Context.State.markInputsSpent(b.Transactions, b.AccumulatorProof)

	// create block reward output
	numOutputs := 1
	for i := range b.Transactions {
		numOutputs += len(b.Transactions[i].Outputs)
	}
	sau.NewOutputs = make([]sunyata.Output, 0, numOutputs)
	sau.NewOutputs = append(sau.NewOutputs, sunyata.Output{
		ID: sunyata.OutputID{
			TransactionID:    sunyata.TransactionID(b.ID()),
			BeneficiaryIndex: 0,
		},
		Value:    vc.BlockReward(),
		Address:  b.Header.MinerAddress,
		Timelock: vc.BlockRewardTimelock(),
	})

	// locate ephemeral outputs
	spentMap := make(map[sunyata.OutputID]struct{})
	for _, txn := range b.Transactions {
		for _, in := range txn.Inputs {
			if in.Parent.LeafIndex == sunyata.EphemeralLeafIndex {
				spentMap[in.Parent.ID] = struct{}{}
			}
		}
	}
	wasSpent := func(oid sunyata.OutputID) bool {
		_, ok := spentMap[oid]
		return ok
	}

	// create transaction outputs
	for _, txn := range b.Transactions {
		txid := txn.ID()
		for i, out := range txn.Outputs {
			sau.NewOutputs = append(sau.NewOutputs, sunyata.Output{
				ID: sunyata.OutputID{
					TransactionID:    txid,
					BeneficiaryIndex: uint64(i),
				},
				Value:    out.Value,
				Address:  out.Address,
				Timelock: 0,
			})
		}
	}

	// compute Merkle proofs for each new output
	sau.treeGrowth = sau.Context.State.addNewOutputs(sau.NewOutputs, wasSpent)
	return
}

// GenesisUpdate returns the StateApplyUpdate for the genesis block b.
func GenesisUpdate(b sunyata.Block, initialDifficulty sunyata.Work) StateApplyUpdate {
	return ApplyBlock(ValidationContext{
		Difficulty: initialDifficulty,
		LastAdjust: b.Header.Timestamp,
	}, b)
}

// A StateRevertUpdate reflects the changes to consensus state resulting from the
// removal of a block.
type StateRevertUpdate struct {
	Context    ValidationContext
	NewOutputs []sunyata.Output
}

// OutputWasRemoved returns true if the Output with the specified index was
// reverted.
func (sru *StateRevertUpdate) OutputWasRemoved(leafIndex uint64) bool {
	return leafIndex >= sru.Context.State.NumLeaves
}

// UpdateOutputProof updates the Merkle proof of the supplied output to
// incorporate the changes made to the state tree. The output's proof must be
// up-to-date; if it is not, UpdateOutputProof may panic.
func (sru *StateRevertUpdate) UpdateOutputProof(o *sunyata.Output) {
	// The algorithm here is, unsurprisingly, the inverse of
	// (StateApplyUpdate).UpdateOutputProof. First we trim off any hashes that
	// were appended by treeGrowth when we applied. Then we find the closest
	// NewOutput and copy its proof hashes above the mergeHeight.
	//
	// The structure is slightly different because StateRevertUpdate only has
	// NewOutputs, unlike StateApplyUpdate which has spentOutputs. This may
	// change in the future.
	trim := mergeHeight(sru.Context.State.NumLeaves, o.LeafIndex) - 1
	if trim < len(o.MerkleProof) {
		o.MerkleProof = o.MerkleProof[:trim]
	}
	if len(sru.NewOutputs) == 0 {
		return
	}
	// locate closest NewOutput and copy its proof
	best := sru.NewOutputs[0]
	bestMergeHeight := mergeHeight(o.LeafIndex, best.LeafIndex)
	for _, so := range sru.NewOutputs[1:] {
		if mh := mergeHeight(o.LeafIndex, so.LeafIndex); mh < bestMergeHeight {
			best, bestMergeHeight = so, mh
		}
	}
	if bestMergeHeight > len(o.MerkleProof) {
		return // no NewOutput in same tree as o, so nothing to change
	}
	if best.LeafIndex == o.LeafIndex {
		// o was un-spent; copy over the entire updated proof
		copy(o.MerkleProof, best.MerkleProof)
	} else {
		// o was not un-spent; copy everything above the mergeHeight, then
		// compute the root prior to the mergeHeight. We can do this by
		// artificially truncating the spent output's proof, and then calling
		// ProofRoot.
		copy(o.MerkleProof[bestMergeHeight:], best.MerkleProof[bestMergeHeight:])
		best.MerkleProof = best.MerkleProof[:bestMergeHeight-1]
		o.MerkleProof[bestMergeHeight-1] = outputProofRoot(best, false)
	}
}

// RevertBlock produces a StateRevertUpdate from a block and the
// ValidationContext prior to that block.
func RevertBlock(vc ValidationContext, b sunyata.Block) StateRevertUpdate {
	var numOutputs int
	for i := range b.Transactions {
		numOutputs += len(b.Transactions[i].Inputs)
	}
	newOutputs := make([]sunyata.Output, 0, numOutputs)
	for _, txn := range b.Transactions {
		for _, in := range txn.Inputs {
			newOutputs = append(newOutputs, in.Parent)
		}
	}
	return StateRevertUpdate{
		Context:    vc,
		NewOutputs: newOutputs,
	}
}
