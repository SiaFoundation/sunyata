package consensus

import (
	"encoding/binary"
	"math/bits"
	"math/rand"
	"testing"

	"go.sia.tech/sunyata"
)

func TestBlockRewardValue(t *testing.T) {
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
		got := BlockRewardValue(test.height)
		if got.String() != test.exp {
			t.Errorf("expected %v, got %v", test.exp, got)
		}
	}
	// test final reward
	totalHalvings := bits.Len(50 * 1e9)
	finalRewardHeight := uint64(210e3 * totalHalvings)
	if BlockRewardValue(finalRewardHeight - 1).IsZero() {
		t.Errorf("final reward should be non-zero")
	}
	if !BlockRewardValue(finalRewardHeight).IsZero() {
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
		if update1.Context.State.containsOutput(&o, true) || !update1.Context.State.containsOutput(&o, false) {
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
			if update2.Context.State.containsOutput(&o, false) || !update2.Context.State.containsOutput(&o, true) {
				t.Error("accumulator should contain spent output:", o)
			}
		} else {
			if update2.Context.State.containsOutput(&o, true) || !update2.Context.State.containsOutput(&o, false) {
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
		if update1.Context.State.containsOutput(&o, true) {
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
			if update3.Context.State.containsOutput(&o, false) || !update3.Context.State.containsOutput(&o, true) {
				t.Error("accumulator should contain spent output:", o)
			}
		} else {
			if update3.Context.State.containsOutput(&o, true) || !update3.Context.State.containsOutput(&o, false) {
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
		if !update1.Context.State.containsOutput(&o, false) {
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
	addOutput := func(o *sunyata.Output, spent bool) {
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
			addOutput(&o, true)
		default:
			addOutput(&o, false)
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

func BenchmarkOutputLeaf(b *testing.B) {
	var o sunyata.Output
	o.MerkleProof = make([]sunyata.Hash256, 25)
	for i := 0; i < b.N; i++ {
		outputProofRoot(&o, true)
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
