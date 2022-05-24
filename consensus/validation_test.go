package consensus

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"go.sia.tech/sunyata"
)

var (
	maxCurrency       = sunyata.NewCurrency(math.MaxUint64, math.MaxUint64)
	testingDifficulty = sunyata.Work{NumHashes: [32]byte{30: 1}}
)

func testingKeypair(seed uint64) (sunyata.PublicKey, sunyata.PrivateKey) {
	var b [32]byte
	binary.LittleEndian.PutUint64(b[:], seed)
	privkey := sunyata.NewPrivateKeyFromSeed(b[:])
	return privkey.PublicKey(), privkey
}

func genesisWithOutputs(scos ...sunyata.Output) sunyata.Block {
	return sunyata.Block{
		Header:       sunyata.BlockHeader{Timestamp: time.Unix(734600000, 0)},
		Transactions: []sunyata.Transaction{{Outputs: scos}},
	}
}

func signAllInputs(txn *sunyata.Transaction, s State, priv sunyata.PrivateKey) {
	sigHash := s.InputSigHash(*txn)
	for i := range txn.Inputs {
		txn.Inputs[i].Signature = priv.SignHash(sigHash)
	}
}

func TestBlockRewardValue(t *testing.T) {
	reward := func(height uint64) sunyata.Currency {
		return (State{Index: sunyata.ChainIndex{Height: height - 1}}).BlockReward()
	}

	baseReward := sunyata.BaseUnitsPerCoin.Mul64(50)
	tests := []struct {
		height uint64
		exp    sunyata.Currency
	}{
		{0, baseReward},
		{1, baseReward},
		{209999, baseReward},
		{210000, baseReward.Div64(2)},
		{210001, baseReward.Div64(2)},
		{1e9, sunyata.ZeroCurrency},
	}
	for _, test := range tests {
		if got := reward(test.height); got != test.exp {
			t.Errorf("expected %v at height %v, got %v", test.exp, test.height/210000, got)
		}
	}
	for i := 1; i < 63; i++ {
		height := uint64(210000 * i)
		exp := baseReward.Div64(1 << i)
		if got := reward(height); got != exp {
			t.Errorf("expected %v at halving #%v, got %v", exp, i, got)
		}
	}
}

func TestEphemeralOutputs(t *testing.T) {
	pubkey, privkey := testingKeypair(0)
	sau := GenesisUpdate(genesisWithOutputs(sunyata.Output{
		Address: pubkey.Address(),
		Value:   sunyata.BaseUnitsPerCoin.Mul64(1),
	}), testingDifficulty)

	// create an ephemeral output
	parentTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    sau.NewOutputElements[1],
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Output{{
			Address: pubkey.Address(),
			Value:   sunyata.BaseUnitsPerCoin.Mul64(1),
		}},
	}
	signAllInputs(&parentTxn, sau.State, privkey)
	ephemeralOutput := sunyata.OutputElement{
		StateElement: sunyata.StateElement{
			ID: sunyata.ElementID{
				Source: sunyata.Hash256(parentTxn.ID()),
				Index:  0,
			},
			LeafIndex: sunyata.EphemeralLeafIndex,
		},
		Output: sunyata.Output{
			Value:   parentTxn.Outputs[0].Value,
			Address: pubkey.Address(),
		},
	}

	// create a transaction that spends the ephemeral output
	childTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    ephemeralOutput,
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Output{{
			Address: pubkey.Address(),
			Value:   ephemeralOutput.Value,
		}},
	}
	signAllInputs(&childTxn, sau.State, privkey)

	// the transaction set should be valid
	if err := sau.State.ValidateTransactionSet([]sunyata.Transaction{parentTxn, childTxn}); err != nil {
		t.Fatal(err)
	}

	// change the value of the output and attempt to spend it
	mintTxn := childTxn.DeepCopy()
	mintTxn.Inputs[0].Parent.Value = sunyata.BaseUnitsPerCoin.Mul64(1e6)
	mintTxn.Outputs[0].Value = mintTxn.Inputs[0].Parent.Value
	signAllInputs(&mintTxn, sau.State, privkey)

	if err := sau.State.ValidateTransactionSet([]sunyata.Transaction{parentTxn, mintTxn}); err == nil {
		t.Fatal("ephemeral output with wrong value should be rejected")
	}

	// add another transaction to the set that double-spends the output
	doubleSpendTxn := childTxn.DeepCopy()
	doubleSpendTxn.Outputs[0].Address = sunyata.VoidAddress
	signAllInputs(&doubleSpendTxn, sau.State, privkey)

	if err := sau.State.ValidateTransactionSet([]sunyata.Transaction{parentTxn, childTxn, doubleSpendTxn}); err == nil {
		t.Fatal("ephemeral output double-spend not rejected")
	}

	invalidTxn := childTxn.DeepCopy()
	invalidTxn.Inputs[0].Parent.Address = sunyata.VoidAddress
	signAllInputs(&invalidTxn, sau.State, privkey)

	if err := sau.State.ValidateTransactionSet([]sunyata.Transaction{parentTxn, invalidTxn}); err == nil {
		t.Fatal("transaction claims wrong address for ephemeral output")
	}
}

func TestValidateTransaction(t *testing.T) {
	// This test constructs a complex transaction and then corrupts it in
	// various ways to produce validation errors.

	// create genesis block with multiple outputs
	pubkey, privkey := testingKeypair(0)
	genesisBlock := sunyata.Block{
		Header: sunyata.BlockHeader{Timestamp: time.Unix(734600000, 0)},
		Transactions: []sunyata.Transaction{{
			Outputs: []sunyata.Output{
				{
					Address: pubkey.Address(),
					Value:   sunyata.BaseUnitsPerCoin.Mul64(11),
				},
				{
					Address: pubkey.Address(),
					Value:   sunyata.BaseUnitsPerCoin.Mul64(11),
				},
				{
					Address: pubkey.Address(),
					Value:   maxCurrency,
				},
			},
		}},
	}
	sau := GenesisUpdate(genesisBlock, testingDifficulty)
	spentSC := sau.NewOutputElements[1]
	unspentSC := sau.NewOutputElements[2]
	overflowSC := sau.NewOutputElements[3]

	// mine a block, then construct a setup transaction that spends some of the
	// outputs
	b := mineBlock(sau.State, genesisBlock)
	if err := sau.State.ValidateBlock(b); err != nil {
		t.Fatal(err)
	}
	sau = ApplyBlock(sau.State, b)
	sau.UpdateElementProof(&spentSC.StateElement)
	sau.UpdateElementProof(&unspentSC.StateElement)
	resolveTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    spentSC,
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Output{{
			Address: sunyata.VoidAddress,
			Value:   spentSC.Value,
		}},
	}
	signAllInputs(&resolveTxn, sau.State, privkey)
	b = mineBlock(sau.State, b, resolveTxn)
	if err := sau.State.ValidateBlock(b); err != nil {
		t.Fatal(err)
	}
	sau = ApplyBlock(sau.State, b)
	sau.UpdateElementProof(&spentSC.StateElement)
	sau.UpdateElementProof(&unspentSC.StateElement)
	s := sau.State

	// finally, create the valid transaction, which spends the remaining outputs
	txn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    unspentSC,
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Output{{
			Address: sunyata.VoidAddress,
			Value:   sunyata.BaseUnitsPerCoin.Mul64(9),
		}},
		MinerFee: sunyata.BaseUnitsPerCoin.Mul64(2),
	}
	signAllInputs(&txn, s, privkey)
	if err := s.ValidateTransaction(txn); err != nil {
		t.Fatal(err)
	}

	// corrupt the transaction in various ways to trigger validation errors
	tests := []struct {
		desc    string
		corrupt func(*sunyata.Transaction)
	}{
		{
			"zero-valued output",
			func(txn *sunyata.Transaction) {
				txn.Outputs[0].Value = sunyata.ZeroCurrency
			},
		},
		{
			"input address does not match spend policy",
			func(txn *sunyata.Transaction) {
				txn.Inputs[0].PublicKey[0] ^= 1
			},
		},
		{
			"outputs that do not equal inputs",
			func(txn *sunyata.Transaction) {
				txn.Outputs[0].Value = txn.Outputs[0].Value.Div64(2)
			},
		},
		{
			"inputs that overflow",
			func(txn *sunyata.Transaction) {
				txn.Inputs = append(txn.Inputs, sunyata.Input{
					Parent:    overflowSC,
					PublicKey: pubkey,
				})
				signAllInputs(txn, s, privkey)
			},
		},
		{
			"outputs that overflow",
			func(txn *sunyata.Transaction) {
				txn.Outputs = append(txn.Outputs, sunyata.Output{
					Value: maxCurrency,
				})
			},
		},
		{
			"miner fee that overflows",
			func(txn *sunyata.Transaction) {
				txn.MinerFee = maxCurrency
			},
		},
		{
			"non-existent output",
			func(txn *sunyata.Transaction) {
				txn.Inputs[0].Parent.ID = sunyata.ElementID{}
			},
		},
		{
			"double-spent output",
			func(txn *sunyata.Transaction) {
				txn.Inputs[0].Parent = spentSC
			},
		},
		{
			"invalid signature",
			func(txn *sunyata.Transaction) {
				txn.Inputs[0].Signature[0] ^= 1
			},
		},
	}
	for _, test := range tests {
		corruptTxn := txn.DeepCopy()
		test.corrupt(&corruptTxn)
		if err := s.ValidateTransaction(corruptTxn); err == nil {
			t.Fatalf("accepted transaction with %v", test.desc)
		}
	}
}

func TestValidateTransactionSet(t *testing.T) {
	pubkey, privkey := testingKeypair(0)
	genesisBlock := genesisWithOutputs(sunyata.Output{
		Address: pubkey.Address(),
		Value:   sunyata.BaseUnitsPerCoin.Mul64(1),
	})
	sau := GenesisUpdate(genesisBlock, testingDifficulty)
	s := sau.State

	txn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    sau.NewOutputElements[1],
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Output{{
			Address: pubkey.Address(),
			Value:   sau.NewOutputElements[1].Value,
		}},
	}
	signAllInputs(&txn, s, privkey)

	if err := sau.State.ValidateTransactionSet([]sunyata.Transaction{txn, txn}); err == nil {
		t.Fatal("accepted transaction set with repeated txn")
	}

	doubleSpendSCTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    sau.NewOutputElements[1],
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Output{{
			Address: pubkey.Address(),
			Value:   sau.NewOutputElements[1].Value,
		}},
	}
	signAllInputs(&doubleSpendSCTxn, s, privkey)

	if err := sau.State.ValidateTransactionSet([]sunyata.Transaction{txn, doubleSpendSCTxn}); err == nil {
		t.Fatal("accepted transaction set with double-spent output")
	}

	// overfill set with copies of txn
	w := sau.State.TransactionWeight(txn)
	txns := make([]sunyata.Transaction, (sau.State.MaxBlockWeight()/w)+1)
	for i := range txns {
		txns[i] = txn
	}
	if err := sau.State.ValidateTransactionSet(txns); err == nil {
		t.Fatal("accepted overweight transaction set")
	}
}

func TestValidateBlock(t *testing.T) {
	pubkey, privkey := testingKeypair(0)
	genesis := genesisWithOutputs(sunyata.Output{
		Address: pubkey.Address(),
		Value:   sunyata.BaseUnitsPerCoin.Mul64(1),
	}, sunyata.Output{
		Address: pubkey.Address(),
		Value:   sunyata.BaseUnitsPerCoin.Mul64(1),
	})
	sau := GenesisUpdate(genesis, testingDifficulty)
	s := sau.State

	// Mine a block with a few transactions. We are not testing transaction
	// validity here, but the block should still be valid.
	txns := []sunyata.Transaction{
		{
			Inputs: []sunyata.Input{{
				Parent:    sau.NewOutputElements[1],
				PublicKey: pubkey,
			}},
			Outputs: []sunyata.Output{{
				Address: sunyata.VoidAddress,
				Value:   sau.NewOutputElements[1].Value,
			}},
		},
		{
			Inputs: []sunyata.Input{{
				Parent:    sau.NewOutputElements[2],
				PublicKey: pubkey,
			}},
			MinerFee: sau.NewOutputElements[2].Value,
		},
	}
	signAllInputs(&txns[0], s, privkey)
	signAllInputs(&txns[1], s, privkey)
	b := mineBlock(s, genesis, txns...)
	if err := s.ValidateBlock(b); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc    string
		corrupt func(*sunyata.Block)
	}{
		{
			"incorrect header block height",
			func(b *sunyata.Block) {
				b.Header.Height = 999
			},
		},
		{
			"incorrect header parent ID",
			func(b *sunyata.Block) {
				b.Header.ParentID[0] ^= 1
			},
		},
		{
			"long-past header timestamp",
			func(b *sunyata.Block) {
				b.Header.Timestamp = b.Header.Timestamp.Add(-24 * time.Hour)
			},
		},
		{
			"invalid commitment (different miner address)",
			func(b *sunyata.Block) {
				b.Header.MinerAddress[0] ^= 1
			},
		},
		{
			"invalid commitment (different transactions)",
			func(b *sunyata.Block) {
				b.Transactions = b.Transactions[1:]
			},
		},
	}
	for _, test := range tests {
		corruptBlock := b
		test.corrupt(&corruptBlock)
		if err := s.ValidateBlock(corruptBlock); err == nil {
			t.Fatalf("accepted block with %v", test.desc)
		}
	}
}
