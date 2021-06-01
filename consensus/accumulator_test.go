package consensus

import (
	"encoding/binary"
	"math/bits"
	"math/rand"
	"reflect"
	"testing"

	"go.sia.tech/sunyata"
)

func TestBlockRewardValue(t *testing.T) {
	reward := func(height uint64) sunyata.Currency {
		return (&ValidationContext{Index: sunyata.ChainIndex{Height: height - 1}}).BlockReward()
	}

	tests := []struct {
		height uint64
		exp    string
	}{
		{0, "50"},
		{210e3 - 1, "50"},
		{210e3 * 1, "25"},
		{210e3 + 1, "25"},
		{210e3 * 2, "12.5"},
		{210e3 * 3, "6.25"},
		{210e3 * 4, "3.125"},
		{210e3 * 5, "1.563"},
		{210e3 * 6, "0.781"},
		{210e3 * 7, "0.391"},
	}
	for _, test := range tests {
		got := reward(test.height)
		if got.String() != test.exp {
			t.Errorf("expected %v, got %v", test.exp, got)
		}
	}
	// test final reward
	totalHalvings := bits.Len(50 * 1e9)
	finalRewardHeight := uint64(210e3 * totalHalvings)
	if reward(finalRewardHeight - 1).IsZero() {
		t.Errorf("final reward should be non-zero")
	}
	if !reward(finalRewardHeight).IsZero() {
		t.Errorf("reward after final reward height should be zero")
	}
}

func TestAccumulator(t *testing.T) {
	randAddr := func() (addr sunyata.Address) {
		rand.Read(addr[:])
		return
	}
	randAmount := func() sunyata.Currency {
		var b [16]byte
		rand.Read(b[:])
		return sunyata.NewCurrency(
			binary.LittleEndian.Uint64(b[:8]),
			binary.LittleEndian.Uint64(b[8:]),
		)
	}
	containsOutput := func(sa StateAccumulator, o sunyata.Output, spent bool) bool {
		root := outputProofRoot(o, spent)
		start, end := bits.TrailingZeros64(sa.NumLeaves), bits.Len64(sa.NumLeaves)
		for i := start; i < end; i++ {
			if sa.HasTreeAtHeight(i) && sa.Trees[i] == root {
				return true
			}
		}
		return false
	}

	b := sunyata.Block{
		Header: sunyata.BlockHeader{
			MinerAddress: randAddr(),
		},
		Transactions: []sunyata.Transaction{{
			Outputs: []sunyata.Beneficiary{
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
			},
		}},
	}
	genesisID := b.ID()
	var state0 ValidationContext
	update1 := ApplyBlock(state0, b)
	origOutputs := update1.NewOutputs
	if len(origOutputs) != len(b.Transactions[0].Outputs)+1 {
		t.Fatalf("expected %v new outputs, got %v", len(b.Transactions[0].Outputs)+1, len(origOutputs))
	}
	// none of the outputs should be marked as spent
	for _, o := range origOutputs {
		if update1.OutputWasSpent(o.LeafIndex) {
			t.Error("update should not mark output as spent:", o)
		}
		if containsOutput(update1.Context.State, o, true) || !containsOutput(update1.Context.State, o, false) {
			t.Error("accumulator should contain unspent output:", o)
		}
	}

	// apply a block that spends some outputs
	txn := sunyata.Transaction{
		Inputs: []sunyata.Input{
			{Parent: origOutputs[6]},
			{Parent: origOutputs[7]},
			{Parent: origOutputs[8]},
			{Parent: origOutputs[9]},
		},
		Outputs: []sunyata.Beneficiary{{
			Value:   randAmount(),
			Address: randAddr(),
		}},
		MinerFee: randAmount(),
	}
	b = sunyata.Block{
		Header: sunyata.BlockHeader{
			Height:       b.Header.Height + 1,
			ParentID:     genesisID,
			MinerAddress: randAddr(),
		},
		Transactions: []sunyata.Transaction{txn},
	}

	update2 := ApplyBlock(update1.Context, b)
	for i := range origOutputs {
		update2.UpdateOutputProof(&origOutputs[i])
	}

	// the update should mark each input as spent
	for _, in := range txn.Inputs {
		if !update2.OutputWasSpent(in.Parent.LeafIndex) {
			t.Error("update should mark input as spent:", in)
		}
	}
	// the new accumulator should contain both the spent and unspent outputs
	for _, o := range origOutputs {
		if update2.OutputWasSpent(o.LeafIndex) {
			if containsOutput(update2.Context.State, o, false) || !containsOutput(update2.Context.State, o, true) {
				t.Error("accumulator should contain spent output:", o)
			}
		} else {
			if containsOutput(update2.Context.State, o, true) || !containsOutput(update2.Context.State, o, false) {
				t.Error("accumulator should contain unspent output:", o)
			}
		}
	}

	// if we reverted that block, we should see the inputs being "created" again
	// and the outputs being destroyed
	revertUpdate := RevertBlock(update1.Context, b)
	if len(revertUpdate.NewOutputs) != len(txn.Inputs) {
		t.Error("number of new outputs after revert should equal number of inputs spent")
	}
	for _, o := range update2.NewOutputs {
		if !revertUpdate.OutputWasRemoved(o.LeafIndex) {
			t.Error("output created in reverted block should be marked as removed")
		}
	}
	// update (a copy of) the proofs to reflect the revert
	outputsWithRevert := append([]sunyata.Output(nil), origOutputs...)
	for i := range outputsWithRevert {
		outputsWithRevert[i].MerkleProof = append([]sunyata.Hash256(nil), outputsWithRevert[i].MerkleProof...)
		revertUpdate.UpdateOutputProof(&outputsWithRevert[i])
	}
	// the reverted proofs should be identical to the proofs prior to b
	for _, o := range outputsWithRevert {
		if update1.OutputWasSpent(o.LeafIndex) {
			t.Error("update should not mark output as spent:", o)
		}
		if containsOutput(update1.Context.State, o, true) {
			t.Error("output should not be marked as spent:", o)
		}
	}

	// spend one of the outputs whose proof we've been maintaining,
	// using an intermediary transaction to test "ephemeral" outputs
	parentTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{
			{Parent: origOutputs[2]},
		},
		Outputs: []sunyata.Beneficiary{{
			Value:   randAmount(),
			Address: randAddr(),
		}},
	}
	childTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent: sunyata.Output{
				ID: sunyata.OutputID{
					TransactionID:    parentTxn.ID(),
					BeneficiaryIndex: 0,
				},
				Value:     randAmount(),
				Address:   randAddr(),
				LeafIndex: sunyata.EphemeralLeafIndex,
			},
		}},
		Outputs: []sunyata.Beneficiary{{
			Value:   randAmount(),
			Address: randAddr(),
		}},
		MinerFee: randAmount(),
	}

	b = sunyata.Block{
		Header: sunyata.BlockHeader{
			Height:       b.Header.Height + 1,
			ParentID:     b.ID(),
			MinerAddress: randAddr(),
		},
		Transactions: []sunyata.Transaction{parentTxn, childTxn},
	}

	update3 := ApplyBlock(update2.Context, b)
	for i := range origOutputs {
		update3.UpdateOutputProof(&origOutputs[i])
	}

	// the update should mark each input as spent
	for _, in := range parentTxn.Inputs {
		if !update3.OutputWasSpent(in.Parent.LeafIndex) {
			t.Error("update should mark input as spent:", in)
		}
	}
	// the new accumulator should contain both the spent and unspent outputs
	for _, o := range origOutputs {
		if update2.OutputWasSpent(o.LeafIndex) || update3.OutputWasSpent(o.LeafIndex) {
			if containsOutput(update3.Context.State, o, false) || !containsOutput(update3.Context.State, o, true) {
				t.Error("accumulator should contain spent output:", o)
			}
		} else {
			if containsOutput(update3.Context.State, o, true) || !containsOutput(update3.Context.State, o, false) {
				t.Error("accumulator should contain unspent output:", o)
			}
		}
	}

	// TODO: we should also be checking childTxn, but we can't check the
	// ephemeral output without knowing its index
}

func TestAccumulatorRevert(t *testing.T) {
	randAddr := func() (addr sunyata.Address) {
		rand.Read(addr[:])
		return
	}
	randAmount := func() sunyata.Currency {
		var b [16]byte
		rand.Read(b[:])
		return sunyata.NewCurrency(
			binary.LittleEndian.Uint64(b[:8]),
			binary.LittleEndian.Uint64(b[8:]),
		)
	}
	containsOutput := func(sa StateAccumulator, o sunyata.Output, spent bool) bool {
		root := outputProofRoot(o, spent)
		start, end := bits.TrailingZeros64(sa.NumLeaves), bits.Len64(sa.NumLeaves)
		for i := start; i < end; i++ {
			if sa.HasTreeAtHeight(i) && sa.Trees[i] == root {
				return true
			}
		}
		return false
	}

	b := sunyata.Block{
		Header: sunyata.BlockHeader{
			MinerAddress: randAddr(),
		},
		Transactions: []sunyata.Transaction{{
			Outputs: []sunyata.Beneficiary{
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
				{Value: randAmount(), Address: randAddr()},
			},
		}},
	}
	genesisID := b.ID()
	var state0 ValidationContext
	update1 := ApplyBlock(state0, b)
	origOutputs := update1.NewOutputs
	if len(origOutputs) != len(b.Transactions[0].Outputs)+1 {
		t.Fatalf("expected %v new outputs, got %v", len(b.Transactions[0].Outputs)+1, len(origOutputs))
	}

	txn := sunyata.Transaction{
		Inputs: []sunyata.Input{
			{Parent: origOutputs[5]},
		},
		Outputs: []sunyata.Beneficiary{{
			Value:   randAmount(),
			Address: randAddr(),
		}},
		MinerFee: randAmount(),
	}
	b = sunyata.Block{
		Header: sunyata.BlockHeader{
			Height:       b.Header.Height + 1,
			ParentID:     genesisID,
			MinerAddress: randAddr(),
		},
		Transactions: []sunyata.Transaction{txn},
	}

	update2 := ApplyBlock(update1.Context, b)
	for i := range origOutputs {
		update2.UpdateOutputProof(&origOutputs[i])
	}

	// revert the block. We should see the inputs being "created" again
	// and the outputs being destroyed
	revertUpdate := RevertBlock(update1.Context, b)
	if len(revertUpdate.NewOutputs) != len(txn.Inputs) {
		t.Error("number of new outputs after revert should equal number of inputs spent")
	}
	for _, o := range update2.NewOutputs {
		if !revertUpdate.OutputWasRemoved(o.LeafIndex) {
			t.Error("output created in reverted block should be marked as removed")
		}
	}
	// update the proofs to reflect the revert
	for i := range origOutputs {
		revertUpdate.UpdateOutputProof(&origOutputs[i])
	}
	// the reverted proofs should be identical to the proofs prior to b
	for _, o := range origOutputs {
		if update1.OutputWasSpent(o.LeafIndex) {
			t.Error("update should not mark output as spent:", o)
		}
		if !containsOutput(update1.Context.State, o, false) {
			t.Error("output should be in the accumulator, marked as unspent:", o)
		}
	}
}

func TestMarkInputsSpent(t *testing.T) {
	outputs := make([]sunyata.Output, 8)
	for i := range outputs {
		outputs[i].LeafIndex = uint64(i)
	}
	var acc StateAccumulator
	acc.addNewOutputs(outputs, func(sunyata.OutputID) bool { return false })

	txns := []sunyata.Transaction{{
		Inputs: []sunyata.Input{
			{Parent: outputs[0]},
			{Parent: outputs[2]},
			{Parent: outputs[3]},
			{Parent: outputs[5]},
			{Parent: outputs[6]},
		},
	}}

	acc.markInputsSpent(txns)

	var acc2 StateAccumulator
	addOutput := func(o sunyata.Output, spent bool) {
		// seek to first open slot, merging nodes as we go
		root := outputLeafHash(o, spent)
		i := 0
		for ; acc2.HasTreeAtHeight(i); i++ {
			root = merkleNodeHash(acc2.Trees[i], root)
		}
		acc2.Trees[i] = root
		acc2.NumLeaves++
	}
	for i, o := range outputs {
		switch i {
		case 0, 2, 3, 5, 6:
			addOutput(o, true)
		default:
			addOutput(o, false)
		}
	}
	for i := range acc2.Trees {
		if acc2.HasTreeAtHeight(i) {
			if !acc2.HasTreeAtHeight(i) {
				t.Fatal("mismatch")
			}
			if acc2.Trees[i] != acc.Trees[i] {
				t.Fatal("mismatch")
			}
		}
	}
}

func TestMultiproof(t *testing.T) {
	outputs := make([]sunyata.Output, 8)
	leaves := make([]sunyata.Hash256, len(outputs))
	for i := range outputs {
		outputs[i].LeafIndex = uint64(i)
		outputs[i].ID.BeneficiaryIndex = uint64(i)
		leaves[i] = outputLeafHash(outputs[i], false)
	}
	node01 := merkleNodeHash(leaves[0], leaves[1])
	node23 := merkleNodeHash(leaves[2], leaves[3])
	node45 := merkleNodeHash(leaves[4], leaves[5])
	node67 := merkleNodeHash(leaves[6], leaves[7])
	node03 := merkleNodeHash(node01, node23)
	node47 := merkleNodeHash(node45, node67)
	outputs[0].MerkleProof = []sunyata.Hash256{leaves[1], node23, node47}
	outputs[1].MerkleProof = []sunyata.Hash256{leaves[0], node23, node47}
	outputs[2].MerkleProof = []sunyata.Hash256{leaves[3], node01, node47}
	outputs[3].MerkleProof = []sunyata.Hash256{leaves[2], node01, node47}
	outputs[4].MerkleProof = []sunyata.Hash256{leaves[5], node67, node03}
	outputs[5].MerkleProof = []sunyata.Hash256{leaves[4], node67, node03}
	outputs[6].MerkleProof = []sunyata.Hash256{leaves[7], node45, node03}
	outputs[7].MerkleProof = []sunyata.Hash256{leaves[6], node45, node03}

	tests := []struct {
		inputs []int
		proof  []sunyata.Hash256
	}{
		{
			inputs: []int{0},
			proof:  []sunyata.Hash256{leaves[1], node23, node47},
		},
		{
			inputs: []int{1, 2, 3},
			proof:  []sunyata.Hash256{leaves[0], node47},
		},
		{
			inputs: []int{7, 6, 0, 2, 3},
			proof:  []sunyata.Hash256{leaves[1], node45},
		},
		{
			inputs: []int{7, 6, 5, 4, 3, 2, 1, 0},
			proof:  nil,
		},
	}
	for _, test := range tests {
		txns := []sunyata.Transaction{{Inputs: make([]sunyata.Input, len(test.inputs))}}
		for i, j := range test.inputs {
			txns[0].Inputs[i].Parent = outputs[j]
		}

		old := txns[0].DeepCopy()
		// compute multiproof
		proof := ComputeMultiproof(txns)
		if !reflect.DeepEqual(proof, test.proof) {
			t.Error("wrong proof generated")
		}
		for _, txn := range txns {
			for i := range txn.Inputs {
				txn.Inputs[i].Parent.MerkleProof = make([]sunyata.Hash256, len(txn.Inputs[i].Parent.MerkleProof))
			}
		}
		// expand multiproof and check roundtrip
		ExpandMultiproof(txns, proof)
		if !reflect.DeepEqual(txns[0], old) {
			t.Fatal("\n", txns[0], "\n", old)
		}
	}
}

func BenchmarkOutputLeaf(b *testing.B) {
	var o sunyata.Output
	o.MerkleProof = make([]sunyata.Hash256, 25)
	for i := 0; i < b.N; i++ {
		outputProofRoot(o, true)
	}
}

func BenchmarkApplyBlock(b *testing.B) {
	block := sunyata.Block{
		Transactions: []sunyata.Transaction{{
			Inputs: []sunyata.Input{{
				Parent: sunyata.Output{
					LeafIndex: sunyata.EphemeralLeafIndex,
				},
			}},
			Outputs: make([]sunyata.Beneficiary, 1000),
		}},
	}
	for i := 0; i < b.N; i++ {
		ApplyBlock(ValidationContext{}, block)
	}
}

func BenchmarkMarkInputs(b *testing.B) {
	outputs := make([]sunyata.Output, 1000)
	for i := range outputs {
		outputs[i].LeafIndex = uint64(i)
	}
	var acc StateAccumulator
	acc.addNewOutputs(outputs, func(sunyata.OutputID) bool { return false })
	proofs := make([][]sunyata.Hash256, len(outputs))
	for i := range proofs {
		proofs[i] = append([]sunyata.Hash256(nil), outputs[i].MerkleProof...)
	}
	indices := rand.Perm(len(outputs))[:len(outputs)/2]
	txns := []sunyata.Transaction{{
		Inputs: make([]sunyata.Input, len(indices)),
	}}
	for i, j := range indices {
		txns[0].Inputs[i].Parent = outputs[j]
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// reset everything
		b.StopTimer()
		acc2 := acc
		for i, j := range indices {
			copy(txns[0].Inputs[i].Parent.MerkleProof, proofs[j])
		}
		b.StartTimer()

		acc2.markInputsSpent(txns)
	}
}
