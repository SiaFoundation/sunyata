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

func testingKeypair() (sunyata.PublicKey, ed25519.PrivateKey) {
	var pubkey sunyata.PublicKey
	privkey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	copy(pubkey[:], privkey[32:])
	return pubkey, privkey
}

func genesisWithBeneficiaries(beneficiaries ...sunyata.Beneficiary) sunyata.Block {
	return sunyata.Block{
		Header:       sunyata.BlockHeader{Timestamp: time.Unix(734600000, 0)},
		Transactions: []sunyata.Transaction{{Outputs: beneficiaries}},
	}
}

func signAllInputs(txn *sunyata.Transaction, vc consensus.ValidationContext, priv ed25519.PrivateKey) {
	sigHash := vc.SigHash(*txn)
	for i := range txn.Inputs {
		txn.Inputs[i].Signature = sunyata.SignTransaction(priv, sigHash)
	}
}

// buildTransaction builds a transaction using the provided inputs divided
// between the number of beneficiaries to create ephemeral outputs.
func buildTransaction(pub sunyata.PublicKey, inputs []sunyata.Output, beneficiaries int) (txn sunyata.Transaction, outputs []sunyata.Output) {
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
		txn.Outputs = append(txn.Outputs, sunyata.Beneficiary{
			Value:   value,
			Address: addr,
		})
	}
	txid := txn.ID()
	for i, o := range txn.Outputs {
		outputs = append(outputs, sunyata.Output{
			ID: sunyata.OutputID{
				TransactionID: txid,
				Index:         uint64(i),
			},
			Value:     o.Value,
			Address:   o.Address,
			LeafIndex: sunyata.EphemeralLeafIndex,
		})
	}
	return
}

func BenchmarkTransactions(b *testing.B) {
	b.ResetTimer()
	pub, priv := testingKeypair()
	beneficiaries := make([]sunyata.Beneficiary, 150)
	for i := 0; i < len(beneficiaries); i++ {
		beneficiaries[i].Value = sunyata.BaseUnitsPerCoin
		beneficiaries[i].Address = pub.Address()
	}
	genesis := genesisWithBeneficiaries(beneficiaries...)
	update := consensus.GenesisUpdate(genesis, testingDifficulty)
	vc := update.Context
	outputs := update.NewOutputs[1:]
	tp := txpool.New(vc)
	miner := New(vc, pub.Address(), tp, CPU)

	fundTxn := func(inputs, beneficiaries int) sunyata.Transaction {
		txn, created := buildTransaction(pub, outputs[:inputs], beneficiaries)
		outputs = append(outputs[inputs:], created...)
		signAllInputs(&txn, vc, priv)
		return txn
	}

	for i := 0; i < 1000; i++ {
		if err := tp.AddTransaction(fundTxn(1, 1)); err != nil {
			b.Fatalf("failed to add transaction: %s", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		miner.transactions()
	}
}

func TestMineBlock(t *testing.T) {
	pub, priv := testingKeypair()
	beneficiaries := make([]sunyata.Beneficiary, 5)
	for i := 0; i < len(beneficiaries); i++ {
		beneficiaries[i].Value = sunyata.BaseUnitsPerCoin
		beneficiaries[i].Address = pub.Address()
	}
	genesis := genesisWithBeneficiaries(beneficiaries...)
	update := consensus.GenesisUpdate(genesis, testingDifficulty)
	vc := update.Context
	outputs := update.NewOutputs[1:]
	tp := txpool.New(vc)
	miner := New(vc, pub.Address(), tp, CPU)

	fundTxn := func(inputs, beneficiaries int) sunyata.Transaction {
		txn, created := buildTransaction(pub, outputs[:inputs], beneficiaries)
		outputs = append(outputs[inputs:], created...)
		signAllInputs(&txn, vc, priv)
		return txn
	}

	// add 10 transactions that spend 1 output each. Half of these transactions
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
	if err := vc.ValidateBlock(block); err != nil {
		t.Fatalf("block failed validation: %s", err)
	}

	// create a ransaction that will be dependent on 2 parents.
	if err := tp.AddTransaction(fundTxn(2, 2)); err != nil {
		t.Fatalf("failed to add transaction: %s", err)
	}
	block = miner.MineBlock()
	if len(block.Transactions) != 11 {
		t.Fatalf("expected 11 transactions, got %d", len(block.Transactions))
	}
	if err := vc.ValidateBlock(block); err != nil {
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
	if err := vc.ValidateBlock(block); err != nil {
		t.Fatalf("block failed validation: %s", err)
	}
}
