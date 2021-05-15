package consensus

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"
	"time"

	"go.sia.tech/sunyata"
)

// copied from testutil (can't import due to cycle)
func findBlockNonce(h *sunyata.BlockHeader, target sunyata.BlockID) {
	rand.Read(h.Nonce[:])
	for !h.ID().MeetsTarget(target) {
		binary.LittleEndian.PutUint64(h.Nonce[:], binary.LittleEndian.Uint64(h.Nonce[:])+1)
	}
}

func TestScratchChain(t *testing.T) {
	privkey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	var pubkey sunyata.PublicKey
	copy(pubkey[:], privkey[32:])
	ourAddr := pubkey.Address()
	b := sunyata.Block{
		Header: sunyata.BlockHeader{
			Timestamp:    time.Unix(734600000, 0),
			MinerAddress: ourAddr,
		},
		Transactions: []sunyata.Transaction{{
			Outputs: []sunyata.Beneficiary{
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
			},
		}},
	}
	initialDifficulty := sunyata.Work{NumHashes: [32]byte{31: 1}}
	findBlockNonce(&b.Header, sunyata.HashRequiringWork(initialDifficulty))

	sau := GenesisUpdate(b, initialDifficulty)
	origOutputs := sau.NewOutputs

	sc := NewScratchChain(sau.Context)
	var blocks []sunyata.Block
	toSpend := origOutputs[5:10]
	var spendTotal sunyata.Currency
	for _, o := range toSpend {
		spendTotal = spendTotal.Add(o.Value)
	}
	txn := sunyata.Transaction{
		Outputs: []sunyata.Beneficiary{{
			Value:   spendTotal.Sub(sunyata.BaseUnitsPerCoin),
			Address: ourAddr,
		}},
		MinerFee: sunyata.BaseUnitsPerCoin,
	}
	for _, o := range toSpend {
		txn.Inputs = append(txn.Inputs, sunyata.Input{
			Parent:    o,
			PublicKey: pubkey,
		})
	}
	sigHash := sau.Context.SigHash(txn)
	for i := range txn.Inputs {
		txn.Inputs[i].Signature = sunyata.SignTransaction(privkey, sigHash)
	}

	b = sunyata.Block{
		Header: sunyata.BlockHeader{
			Height:       b.Header.Height + 1,
			ParentID:     b.Header.ID(),
			Timestamp:    b.Header.Timestamp.Add(time.Second),
			MinerAddress: ourAddr,
			Commitment:   sau.Context.Commitment(ourAddr, []sunyata.Transaction{txn}),
		},
		Transactions: []sunyata.Transaction{txn},
	}
	findBlockNonce(&b.Header, sunyata.HashRequiringWork(initialDifficulty))
	bid := b.ID()
	if err := sc.AppendHeader(b.Header); err != nil {
		t.Fatal(err)
	}
	blocks = append(blocks, b)

	sau = ApplyBlock(sau.Context, b)
	sau.UpdateOutputProof(&origOutputs[2])
	newOutputs := sau.NewOutputs

	txn = sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    newOutputs[1],
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Beneficiary{{
			Value:   newOutputs[1].Value.Sub(sunyata.BaseUnitsPerCoin),
			Address: ourAddr,
		}},
		MinerFee: sunyata.BaseUnitsPerCoin,
	}
	sigHash = sau.Context.SigHash(txn)
	for i := range txn.Inputs {
		txn.Inputs[i].Signature = sunyata.SignTransaction(privkey, sigHash)
	}

	b = sunyata.Block{
		Header: sunyata.BlockHeader{
			Height:       b.Header.Height + 1,
			ParentID:     bid,
			Timestamp:    b.Header.Timestamp.Add(time.Second),
			MinerAddress: ourAddr,
			Commitment:   sau.Context.Commitment(ourAddr, []sunyata.Transaction{txn}),
		},
		Transactions: []sunyata.Transaction{txn},
	}
	findBlockNonce(&b.Header, sunyata.HashRequiringWork(initialDifficulty))
	bid = b.ID()
	if err := sc.AppendHeader(b.Header); err != nil {
		t.Fatal(err)
	}
	blocks = append(blocks, b)
	sau = ApplyBlock(sau.Context, b)
	for i := range origOutputs {
		sau.UpdateOutputProof(&origOutputs[i])
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
		Outputs: []sunyata.Beneficiary{{
			Value:   spendTotal,
			Address: ourAddr,
		}},
	}
	childTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent: sunyata.Output{
				ID: sunyata.OutputID{
					TransactionID:    parentTxn.ID(),
					BeneficiaryIndex: 0,
				},
				Value:     spendTotal,
				Address:   ourAddr,
				LeafIndex: sunyata.EphemeralLeafIndex,
			},
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Beneficiary{{
			Value:   spendTotal.Sub(sunyata.BaseUnitsPerCoin),
			Address: ourAddr,
		}},
		MinerFee: sunyata.BaseUnitsPerCoin,
	}
	parentTxn.Inputs[0].Signature = sunyata.SignTransaction(privkey, sau.Context.SigHash(parentTxn))
	childTxn.Inputs[0].Signature = sunyata.SignTransaction(privkey, sau.Context.SigHash(childTxn))

	b = sunyata.Block{
		Header: sunyata.BlockHeader{
			Height:       b.Header.Height + 1,
			ParentID:     b.ID(),
			Timestamp:    b.Header.Timestamp.Add(time.Second),
			MinerAddress: ourAddr,
			Commitment:   sau.Context.Commitment(ourAddr, []sunyata.Transaction{parentTxn, childTxn}),
		},
		Transactions: []sunyata.Transaction{parentTxn, childTxn},
	}
	findBlockNonce(&b.Header, sunyata.HashRequiringWork(initialDifficulty))
	if err := sc.AppendHeader(b.Header); err != nil {
		t.Fatal(err)
	}
	blocks = append(blocks, b)

	// validate all blocks
	for _, b := range blocks {
		if _, err := sc.ApplyBlockTransactions(b.Transactions); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDifficultyAdjustment(t *testing.T) {
	var b sunyata.Block
	b.Header.Timestamp = time.Unix(734600000, 0)
	initialDifficulty := sunyata.Work{NumHashes: [32]byte{31: 4}}
	vc := GenesisUpdate(b, initialDifficulty).Context

	// mine enough blocks to trigger adjustment
	sc := NewScratchChain(vc)
	for i := 0; i < sunyata.DifficultyAdjustmentInterval; i++ {
		b.Header = sunyata.BlockHeader{
			Height:     b.Header.Height + 1,
			ParentID:   b.Header.ID(),
			Timestamp:  b.Header.Timestamp.Add(time.Second),
			Commitment: vc.Commitment(sunyata.VoidAddress, nil),
		}
		findBlockNonce(&b.Header, sunyata.HashRequiringWork(initialDifficulty))
		if err := sc.AppendHeader(b.Header); err != nil {
			t.Fatal(err)
		} else if _, err := sc.ApplyBlockTransactions(nil); err != nil {
			t.Fatal(err)
		}
		vc = ApplyBlock(vc, b).Context
	}

	// difficulty should have changed
	currentDifficulty := sc.tvc.Difficulty
	if currentDifficulty.Cmp(initialDifficulty) <= 0 {
		t.Fatal("difficulty should have increased")
	}

	// mine a block with less than the minimum work; it should be rejected
	b.Header = sunyata.BlockHeader{
		Height:     b.Header.Height + 1,
		ParentID:   b.Header.ID(),
		Timestamp:  b.Header.Timestamp.Add(time.Second),
		Commitment: vc.Commitment(sunyata.VoidAddress, nil),
	}
	for sunyata.WorkRequiredForHash(b.ID()).Cmp(currentDifficulty) >= 0 {
		findBlockNonce(&b.Header, sunyata.HashRequiringWork(initialDifficulty))
	}
	if err := sc.AppendHeader(b.Header); err == nil {
		t.Fatal("expected block to be rejected")
	}
	vc = ApplyBlock(vc, b).Context

	// mine at actual difficulty
	findBlockNonce(&b.Header, sunyata.HashRequiringWork(currentDifficulty))
	if err := sc.AppendHeader(b.Header); err != nil {
		t.Fatal(err)
	} else if _, err := sc.ApplyBlockTransactions(nil); err != nil {
		t.Fatal(err)
	}
	vc = ApplyBlock(vc, b).Context
}
