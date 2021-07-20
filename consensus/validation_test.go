package consensus

import (
	"crypto/ed25519"
	"testing"
	"time"

	"go.sia.tech/sunyata"
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

func signAllInputs(txn *sunyata.Transaction, vc ValidationContext, priv ed25519.PrivateKey) {
	sigHash := vc.SigHash(*txn)
	for i := range txn.Inputs {
		txn.Inputs[i].Signature = sunyata.SignTransaction(priv, sigHash)
	}
}

func TestEphemeralOutputs(t *testing.T) {
	pubkey, privkey := testingKeypair()
	sau := GenesisUpdate(genesisWithBeneficiaries(sunyata.Beneficiary{
		Address: pubkey.Address(),
		Value:   sunyata.BaseUnitsPerCoin,
	}), testingDifficulty)

	// create an ephemeral output
	parentTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    sau.NewOutputs[1],
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Beneficiary{{
			Address: pubkey.Address(),
			Value:   sunyata.BaseUnitsPerCoin,
		}},
	}
	signAllInputs(&parentTxn, sau.Context, privkey)
	ephemeralOutput := sunyata.Output{
		ID: sunyata.OutputID{
			TransactionID:    parentTxn.ID(),
			BeneficiaryIndex: 0,
		},
		Value:     parentTxn.Outputs[0].Value,
		Address:   pubkey.Address(),
		LeafIndex: sunyata.EphemeralLeafIndex,
	}

	// create a transaction that spends the ephemeral output
	childTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    ephemeralOutput,
			PublicKey: pubkey,
		}},
		Outputs: []sunyata.Beneficiary{{
			Address: pubkey.Address(),
			Value:   ephemeralOutput.Value,
		}},
	}
	signAllInputs(&childTxn, sau.Context, privkey)

	// the transaction set should be valid
	err := sau.Context.ValidateTransactionSet([]sunyata.Transaction{parentTxn, childTxn})
	if err != nil {
		t.Fatal(err)
	}

	// change the value of the output and attempt to spend it
	mintTxn := childTxn.DeepCopy()
	mintTxn.Inputs[0].Parent.Value = sunyata.BaseUnitsPerCoin.Mul64(1e6)
	mintTxn.Outputs[0].Value = mintTxn.Inputs[0].Parent.Value
	signAllInputs(&mintTxn, sau.Context, privkey)

	err = sau.Context.ValidateTransactionSet([]sunyata.Transaction{parentTxn, mintTxn})
	if err == nil {
		t.Fatal("ephemeral output with wrong value should be rejected")
	}

	// add another transaction to the set that double-spends the output
	doubleSpendTxn := childTxn.DeepCopy()
	doubleSpendTxn.Outputs[0].Address = sunyata.VoidAddress
	signAllInputs(&doubleSpendTxn, sau.Context, privkey)

	err = sau.Context.ValidateTransactionSet([]sunyata.Transaction{parentTxn, childTxn, doubleSpendTxn})
	if err == nil {
		t.Fatal("ephemeral output double-spend not rejected")
	}
}
