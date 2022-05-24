package consensus

import (
	"crypto/rand"
	"encoding/binary"
	"testing"

	"go.sia.tech/sunyata"
)

func randAddr() (a sunyata.Address) {
	rand.Read(a[:])
	return
}

func randAmount() sunyata.Currency {
	buf := make([]byte, 16)
	rand.Read(buf)
	return sunyata.NewCurrency(
		binary.LittleEndian.Uint64(buf[:8]),
		binary.LittleEndian.Uint64(buf[8:]),
	)
}

func TestApplyBlock(t *testing.T) {
	b := genesisWithOutputs([]sunyata.Output{
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
	}...)
	update1 := GenesisUpdate(b, testingDifficulty)
	acc1 := update1.State.Elements
	origOutputs := update1.NewOutputElements
	if len(origOutputs) != len(b.Transactions[0].Outputs)+1 {
		t.Fatalf("expected %v new outputs, got %v", len(b.Transactions[0].Outputs)+1, len(origOutputs))
	}
	// none of the outputs should be marked as spent
	for _, o := range origOutputs {
		if update1.OutputElementWasSpent(o) {
			t.Error("update should not mark output as spent:", o)
		}
		if acc1.ContainsSpentOutputElement(o) || !acc1.ContainsUnspentOutputElement(o) {
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
		Outputs: []sunyata.Output{{
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
		Transactions: []sunyata.Transaction{txn},
	}

	update2 := ApplyBlock(update1.State, b)
	acc2 := update2.State.Elements
	for i := range origOutputs {
		update2.UpdateElementProof(&origOutputs[i].StateElement)
	}

	// the update should mark each input as spent
	for _, in := range txn.Inputs {
		if !update2.OutputElementWasSpent(in.Parent) {
			t.Error("update should mark input as spent:", in)
		}
	}
	// the new accumulator should contain both the spent and unspent outputs
	for _, o := range origOutputs {
		if update2.OutputElementWasSpent(o) {
			if acc2.ContainsUnspentOutputElement(o) || !acc2.ContainsSpentOutputElement(o) {
				t.Error("accumulator should contain spent output:", o)
			}
		} else {
			if acc2.ContainsSpentOutputElement(o) || !acc2.ContainsUnspentOutputElement(o) {
				t.Error("accumulator should contain unspent output:", o)
			}
		}
	}

	// if we instead revert that block, we should see the inputs being "created"
	// again and the outputs being destroyed
	revertUpdate := RevertBlock(update1.State, b)
	revertAcc := revertUpdate.State.Elements
	if len(revertUpdate.SpentOutputs) != len(txn.Inputs) {
		t.Error("number of spent outputs after revert should equal number of inputs")
	}
	for _, o := range update2.NewOutputElements {
		if !revertUpdate.OutputElementWasRemoved(o) {
			t.Error("output created in reverted block should be marked as removed")
		}
	}
	// update (a copy of) the proofs to reflect the revert
	outputsWithRevert := append([]sunyata.OutputElement(nil), origOutputs...)
	for i := range outputsWithRevert {
		outputsWithRevert[i].MerkleProof = append([]sunyata.Hash256(nil), outputsWithRevert[i].MerkleProof...)
		revertUpdate.UpdateElementProof(&outputsWithRevert[i].StateElement)
	}
	// the reverted proofs should be identical to the proofs prior to b
	for _, o := range outputsWithRevert {
		if update1.OutputElementWasSpent(o) {
			t.Error("update should not mark output as spent:", o)
		}
		if revertAcc.ContainsSpentOutputElement(o) {
			t.Error("output should not be marked as spent:", o)
		}
	}

	// spend one of the outputs whose proof we've been maintaining,
	// using an intermediary transaction to test "ephemeral" outputs
	parentTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{
			{Parent: origOutputs[2]},
		},
		Outputs: []sunyata.Output{{
			Value:   randAmount(),
			Address: randAddr(),
		}},
	}
	childTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent: sunyata.OutputElement{
				StateElement: sunyata.StateElement{
					ID: sunyata.ElementID{
						Source: sunyata.Hash256(parentTxn.ID()),
						Index:  0,
					},
					LeafIndex: sunyata.EphemeralLeafIndex,
				},
				Output: sunyata.Output{
					Value:   randAmount(),
					Address: randAddr(),
				},
			},
		}},
		Outputs: []sunyata.Output{{
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

	update3 := ApplyBlock(update2.State, b)
	acc3 := update3.State.Elements
	for i := range origOutputs {
		update3.UpdateElementProof(&origOutputs[i].StateElement)
	}

	// the update should mark each input as spent
	for _, in := range parentTxn.Inputs {
		if !update3.OutputElementWasSpent(in.Parent) {
			t.Error("update should mark input as spent:", in)
		}
	}
	// the new accumulator should contain both the spent and unspent outputs
	for _, o := range origOutputs {
		if update2.OutputElementWasSpent(o) || update3.OutputElementWasSpent(o) {
			if acc3.ContainsUnspentOutputElement(o) || !acc3.ContainsSpentOutputElement(o) {
				t.Error("accumulator should contain spent output:", o)
			}
		} else {
			if acc3.ContainsSpentOutputElement(o) || !acc3.ContainsUnspentOutputElement(o) {
				t.Error("accumulator should contain unspent output:", o)
			}
		}
	}
}

func TestRevertBlock(t *testing.T) {
	b := genesisWithOutputs([]sunyata.Output{
		{Value: randAmount(), Address: randAddr()},
		{Value: randAmount(), Address: randAddr()},
		{Value: randAmount(), Address: randAddr()},
		{Value: randAmount(), Address: randAddr()},
		{Value: randAmount(), Address: randAddr()},
	}...)
	update1 := GenesisUpdate(b, testingDifficulty)
	origOutputs := update1.NewOutputElements
	if len(origOutputs) != len(b.Transactions[0].Outputs)+1 {
		t.Fatalf("expected %v new outputs, got %v", len(b.Transactions[0].Outputs)+1, len(origOutputs))
	}

	txn := sunyata.Transaction{
		Inputs: []sunyata.Input{
			{Parent: origOutputs[5]},
		},
		Outputs: []sunyata.Output{{
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
		Transactions: []sunyata.Transaction{txn},
	}

	update2 := ApplyBlock(update1.State, b)
	for i := range origOutputs {
		update2.UpdateElementProof(&origOutputs[i].StateElement)
	}

	// revert the block. We should see the inputs being "created" again
	// and the outputs being destroyed
	revertUpdate := RevertBlock(update1.State, b)
	if len(revertUpdate.SpentOutputs) != len(txn.Inputs) {
		t.Error("number of spent outputs after revert should equal number of inputs")
	}
	for _, o := range update2.NewOutputElements {
		if !revertUpdate.OutputElementWasRemoved(o) {
			t.Error("output created in reverted block should be marked as removed")
		}
	}
	// update the proofs to reflect the revert
	for i := range origOutputs {
		revertUpdate.UpdateElementProof(&origOutputs[i].StateElement)
	}
	// the reverted proofs should be identical to the proofs prior to b
	for _, o := range origOutputs {
		if update1.OutputElementWasSpent(o) {
			t.Error("update should not mark output as spent:", o)
		}
		if !update1.State.Elements.ContainsUnspentOutputElement(o) {
			t.Error("output should be in the accumulator, marked as unspent:", o)
		}
	}
}

func BenchmarkApplyBlock(b *testing.B) {
	block := sunyata.Block{
		Transactions: []sunyata.Transaction{{
			Inputs: []sunyata.Input{{
				Parent: sunyata.OutputElement{
					StateElement: sunyata.StateElement{
						LeafIndex: sunyata.EphemeralLeafIndex,
					},
				},
			}},
			Outputs: make([]sunyata.Output, 1000),
		}},
	}
	for i := 0; i < b.N; i++ {
		ApplyBlock(State{}, block)
	}
}
