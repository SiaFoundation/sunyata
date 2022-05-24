package miner

import (
	"crypto/ed25519"
	"testing"
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/consensus"
	"go.sia.tech/sunyata/txpool"
)

var testingDifficulty = sunyata.Work{NumHashes: [32]byte{31: 1}}

func testingKeypair() (sunyata.PublicKey, sunyata.PrivateKey) {
	privkey := sunyata.PrivateKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	return privkey.PublicKey(), privkey
}

func genesisWithBeneficiaries(beneficiaries ...sunyata.Output) sunyata.Block {
	return sunyata.Block{
		Header:       sunyata.BlockHeader{Timestamp: time.Unix(734600000, 0)},
		Transactions: []sunyata.Transaction{{Outputs: beneficiaries}},
	}
}

func signAllInputs(txn *sunyata.Transaction, cs consensus.State, priv sunyata.PrivateKey) {
	sigHash := cs.InputSigHash(*txn)
	for i := range txn.Inputs {
		txn.Inputs[i].Signature = priv.SignHash(sigHash)
	}
}

// buildTransaction builds a transaction using the provided inputs divided
// between the number of beneficiaries to create ephemeral outputs.
func buildTransaction(pub sunyata.PublicKey, inputs []sunyata.OutputElement, beneficiaries int) (txn sunyata.Transaction, outputs []sunyata.OutputElement) {
	var total sunyata.Currency
	for _, input := range inputs {
		total = total.Add(input.Value)
		txn.Inputs = append(txn.Inputs, sunyata.Input{
			Parent:    input,
			PublicKey: pub,
		})
	}

	value := total.Div64(uint64(beneficiaries))
	addr := pub.Address()
	for i := 0; i < beneficiaries; i++ {
		txn.Outputs = append(txn.Outputs, sunyata.Output{
			Value:   value,
			Address: addr,
		})
	}
	txid := txn.ID()
	for i, o := range txn.Outputs {
		outputs = append(outputs, sunyata.OutputElement{
			StateElement: sunyata.StateElement{
				ID: sunyata.ElementID{
					Source: sunyata.Hash256(txid),
					Index:  uint64(i),
				},
				LeafIndex: sunyata.EphemeralLeafIndex,
			},
			Output: sunyata.Output{
				Value:   o.Value,
				Address: o.Address,
			},
		})
	}
	return
}

func TestMineBlock(t *testing.T) {
	pub, priv := testingKeypair()
	beneficiaries := make([]sunyata.Output, 5)
	for i := 0; i < len(beneficiaries); i++ {
		beneficiaries[i].Value = sunyata.BaseUnitsPerCoin
		beneficiaries[i].Address = pub.Address()
	}
	genesis := genesisWithBeneficiaries(beneficiaries...)
	update := consensus.GenesisUpdate(genesis, testingDifficulty)
	cs := update.State
	outputs := update.NewOutputElements[1:]
	tp := txpool.New(cs)
	miner := New(cs, pub.Address(), tp, CPU)

	fundTxn := func(inputs, beneficiaries int) sunyata.Transaction {
		txn, created := buildTransaction(pub, outputs[:inputs], beneficiaries)
		outputs = append(outputs[inputs:], created...)
		signAllInputs(&txn, cs, priv)
		return txn
	}

	// Add 10 transactions that spend 1 output each. Half of these transactions
	// will spend ephemeral outputs.
	for i := 0; i < 10; i++ {
		if err := tp.AddTransaction(fundTxn(1, 1)); err != nil {
			t.Fatalf("failed to add transaction: %s", err)
		}
	}
	block := miner.MineBlock()
	if len(block.Transactions) != 10 {
		t.Fatalf("expected 10 transactions, got %d", len(block.Transactions))
	}
	if err := cs.ValidateBlock(block); err != nil {
		t.Fatalf("block failed validation: %s", err)
	}

	// Create a transaction that will be dependent on 2 parents.
	if err := tp.AddTransaction(fundTxn(2, 2)); err != nil {
		t.Fatalf("failed to add transaction: %s", err)
	}
	block = miner.MineBlock()
	if len(block.Transactions) != 11 {
		t.Fatalf("expected 11 transactions, got %d", len(block.Transactions))
	}
	if err := cs.ValidateBlock(block); err != nil {
		t.Fatalf("block failed validation: %s", err)
	}

	// Create a transaction that will be dependent on all of our previous
	// transactions.
	if err := tp.AddTransaction(fundTxn(len(outputs), 1)); err != nil {
		t.Fatalf("failed to add transaction: %s", err)
	}

	block = miner.MineBlock()
	if len(block.Transactions) != 12 {
		t.Fatalf("expected 12 transactions, got %d", len(block.Transactions))
	}
	if err := cs.ValidateBlock(block); err != nil {
		t.Fatalf("block failed validation: %s", err)
	}
}

func BenchmarkTransactions(b *testing.B) {
	pub, priv := testingKeypair()
	beneficiaries := make([]sunyata.Output, 150)
	for i := 0; i < len(beneficiaries); i++ {
		beneficiaries[i].Value = sunyata.BaseUnitsPerCoin
		beneficiaries[i].Address = pub.Address()
	}
	genesis := genesisWithBeneficiaries(beneficiaries...)
	update := consensus.GenesisUpdate(genesis, testingDifficulty)
	cs := update.State
	outputs := update.NewOutputElements[1:]
	tp := txpool.New(cs)
	miner := New(cs, pub.Address(), tp, CPU)

	fundTxn := func(inputs, beneficiaries int) sunyata.Transaction {
		txn, created := buildTransaction(pub, outputs[:inputs], beneficiaries)
		outputs = append(outputs[inputs:], created...)
		signAllInputs(&txn, cs, priv)
		return txn
	}

	for i := 0; i < 1000; i++ {
		if err := tp.AddTransaction(fundTxn(1, 1)); err != nil {
			b.Fatalf("failed to add transaction: %s", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		miner.txnsForBlock()
	}
}
