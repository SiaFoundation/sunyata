package consensus

import (
	"testing"
	"time"

	"go.sia.tech/sunyata"
)

// copied from chainutil (can't import due to cycle)
func findBlockNonce(sc State, h *sunyata.BlockHeader, target sunyata.BlockID) {
	for !h.ID().MeetsTarget(target) {
		h.Nonce++
	}
}

func mineBlock(s State, parent sunyata.Block, txns ...sunyata.Transaction) sunyata.Block {
	b := sunyata.Block{
		Header: sunyata.BlockHeader{
			Height:    parent.Header.Height + 1,
			ParentID:  parent.Header.ID(),
			Timestamp: parent.Header.Timestamp.Add(time.Second),
		},
		Transactions: txns,
	}
	b.Header.Commitment = s.Commitment(b.Header.MinerAddress, b.Transactions)
	findBlockNonce(s, &b.Header, sunyata.HashRequiringWork(s.Difficulty))
	return b
}

func TestScratchChain(t *testing.T) {
	pubkey, privkey := testingKeypair(0)
	ourAddr := pubkey.Address()

	b := genesisWithOutputs([]sunyata.Output{
		{Value: sunyata.BaseUnitsPerCoin.Mul64(1), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(2), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(3), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(4), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(5), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(6), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(7), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(8), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(9), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(10), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(11), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(12), Address: ourAddr},
		{Value: sunyata.BaseUnitsPerCoin.Mul64(13), Address: ourAddr},
	}...)
	sau := GenesisUpdate(b, testingDifficulty)

	sc := NewScratchChain(sau.State)
	if sc.Base() != sau.State.Index {
		t.Fatal("wrong base:", sc.Base())
	} else if sc.Tip() != sau.State.Index {
		t.Fatal("wrong tip:", sc.Tip())
	} else if sc.UnvalidatedBase() != sau.State.Index {
		t.Fatal("wrong unvalidated base:", sc.UnvalidatedBase())
	}
	var blocks []sunyata.Block
	origOutputs := sau.NewOutputElements
	toSpend := origOutputs[5:10]
	var spendTotal sunyata.Currency
	for _, o := range toSpend {
		spendTotal = spendTotal.Add(o.Value)
	}
	txn := sunyata.Transaction{
		Outputs: []sunyata.Output{{
			Value:   spendTotal.Sub(sunyata.BaseUnitsPerCoin.Mul64(1)),
			Address: ourAddr,
		}},
		MinerFee: sunyata.BaseUnitsPerCoin.Mul64(1),
	}
	for _, o := range toSpend {
		txn.Inputs = append(txn.Inputs, sunyata.Input{
			Parent:    o,
			PublicKey: pubkey,
		})
	}
	signAllInputs(&txn, sau.State, privkey)

	b = mineBlock(sau.State, b, txn)
	if sc.Contains(b.Index()) {
		t.Fatal("scratch chain should not contain the header yet")
	} else if _, err := sc.ApplyBlock(b); err == nil {
		t.Fatal("shouldn't be able to apply a block without a corresponding header")
	} else if err := sc.AppendHeader(b.Header); err != nil {
		t.Fatal(err)
	} else if sc.Tip() != b.Index() {
		t.Fatal("wrong tip:", sc.Tip())
	} else if sc.UnvalidatedBase() != sc.Base() {
		t.Fatal("wrong unvalidated base:", sc.UnvalidatedBase())
	} else if !sc.Contains(b.Index()) {
		t.Fatal("scratch chain should contain the header")
	} else if sc.TotalWork() != testingDifficulty {
		t.Fatal("wrong total work:", sc.TotalWork())
	}
	blocks = append(blocks, b)

	sau = ApplyBlock(sau.State, b)
	sau.UpdateElementProof(&origOutputs[2].StateElement)
	newOutputs := sau.NewOutputElements

	txn = sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    newOutputs[1],
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Output{{
			Value:   newOutputs[1].Value.Sub(sunyata.BaseUnitsPerCoin.Mul64(1)),
			Address: ourAddr,
		}},
		MinerFee: sunyata.BaseUnitsPerCoin.Mul64(1),
	}
	signAllInputs(&txn, sau.State, privkey)

	b = mineBlock(sau.State, b, txn)
	if err := sc.AppendHeader(b.Header); err != nil {
		t.Fatal(err)
	}
	blocks = append(blocks, b)
	sau = ApplyBlock(sau.State, b)
	for i := range origOutputs {
		sau.UpdateElementProof(&origOutputs[i].StateElement)
	}
	toSpend = origOutputs[2:3]
	spendTotal = sunyata.ZeroCurrency
	for _, o := range toSpend {
		spendTotal = spendTotal.Add(o.Value)
	}
	parentTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    toSpend[0],
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Output{{
			Value:   spendTotal,
			Address: ourAddr,
		}},
	}
	signAllInputs(&parentTxn, sau.State, privkey)
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
					Value:   spendTotal,
					Address: ourAddr,
				},
			},
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Output{{
			Value:   spendTotal.Sub(sunyata.BaseUnitsPerCoin.Mul64(1)),
			Address: ourAddr,
		}},
		MinerFee: sunyata.BaseUnitsPerCoin.Mul64(1),
	}
	signAllInputs(&childTxn, sau.State, privkey)

	b = mineBlock(sau.State, b, parentTxn, childTxn)
	if err := sc.AppendHeader(b.Header); err != nil {
		t.Fatal(err)
	}
	blocks = append(blocks, b)

	// should have one unvalidated header for each block
	if sc.FullyValidated() {
		t.Fatal("scratch chain should not be fully validated yet")
	} else if len(sc.Unvalidated()) != len(blocks) {
		t.Fatal("unvalidated headers not equal to blocks")
	}
	for i, index := range sc.Unvalidated() {
		if index != blocks[i].Index() {
			t.Fatal("unvalidated header not equal to block")
		} else if sc.Index(index.Height) != index {
			t.Fatal("inconsistent index:", sc.Index(index.Height), index)
		}
	}

	// validate all blocks
	for _, b := range blocks {
		if _, err := sc.ApplyBlock(b); err != nil {
			t.Fatal(err)
		} else if sc.ValidTip() != b.Index() {
			t.Fatal("wrong valid tip:", sc.ValidTip())
		} else if len(sc.Unvalidated()) > 0 && sc.UnvalidatedBase() != sc.Index(b.Header.Height+1) {
			t.Fatal("wrong unvalidated base:", sc.UnvalidatedBase())
		}
	}
	if !sc.FullyValidated() {
		t.Fatal("scratch chain should be fully validated")
	} else if len(sc.Unvalidated()) != 0 {
		t.Fatal("scratch chain should not have any unvalidated headers")
	}
}

func TestScratchChainDifficultyAdjustment(t *testing.T) {
	b := genesisWithOutputs()
	b.Header.Height = (State{}).DifficultyAdjustmentInterval() - 1
	s := GenesisUpdate(b, testingDifficulty).State

	// skip ahead to next adjustment and mine a block
	sc := NewScratchChain(s)
	b = mineBlock(s, b)
	if err := sc.AppendHeader(b.Header); err != nil {
		t.Fatal(err)
	} else if _, err := sc.ApplyBlock(b); err != nil {
		t.Fatal(err)
	}
	s = ApplyBlock(s, b).State

	// difficulty should have changed
	currentDifficulty := sc.ts.Difficulty
	if currentDifficulty.Cmp(testingDifficulty) <= 0 {
		t.Fatal("difficulty should have increased")
	}

	// mine a block with less than the minimum work; it should be rejected
	b = mineBlock(s, b)
	for sunyata.WorkRequiredForHash(b.ID()).Cmp(currentDifficulty) >= 0 {
		b.Header.Nonce++
	}
	if err := sc.AppendHeader(b.Header); err == nil {
		t.Fatal("expected block to be rejected")
	}

	// mine at actual difficulty
	findBlockNonce(s, &b.Header, sunyata.HashRequiringWork(s.Difficulty))
	if err := sc.AppendHeader(b.Header); err != nil {
		t.Fatal(err)
	} else if _, err := sc.ApplyBlock(b); err != nil {
		t.Fatal(err)
	}
	s = ApplyBlock(s, b).State
}
