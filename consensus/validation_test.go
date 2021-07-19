package consensus

import (
	"crypto/ed25519"
	"testing"
	"time"

	"go.sia.tech/sunyata"
)

func genesisUpdate(pub sunyata.PublicKey, beneficiaries ...sunyata.Beneficiary) (sunyata.Block, StateApplyUpdate) {
	b := sunyata.Block{
		Header: sunyata.BlockHeader{
			Timestamp:    time.Unix(734600000, 0),
			MinerAddress: pub.Address(),
		},
		Transactions: []sunyata.Transaction{{
			Outputs: beneficiaries,
		}},
	}
	initialDifficulty := sunyata.Work{NumHashes: [32]byte{31: 1}}
	findBlockNonce(&b.Header, sunyata.HashRequiringWork(initialDifficulty))

	return b, GenesisUpdate(b, initialDifficulty)
}

func signTxn(txn *sunyata.Transaction, vc ValidationContext, priv ed25519.PrivateKey) {
	sigHash := vc.SigHash(*txn)
	for i := range txn.Inputs {
		txn.Inputs[i].Signature = sunyata.SignTransaction(priv, sigHash)
	}
}

func TestSpendInvalidEphemeralOutput(t *testing.T) {
	var pubkey sunyata.PublicKey
	privkey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	copy(pubkey[:], privkey[32:])

	_, sau := genesisUpdate(pubkey, sunyata.Beneficiary{
		Address: pubkey.Address(),
		Value:   sunyata.BaseUnitsPerCoin,
	})

	// create a setup transaction to create an ephemeral output.
	setupTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{
			{
				Parent:    sau.NewOutputs[1],
				PublicKey: pubkey,
			},
		},
		Outputs: []sunyata.Beneficiary{
			{
				Address: pubkey.Address(),
				Value:   sunyata.BaseUnitsPerCoin,
			},
		},
	}
	signTxn(&setupTxn, sau.Context, privkey)

	// Build and sign a transaction that spends the ephemeral output, but changes its
	// value; minting new tokens.
	newValue := sunyata.BaseUnitsPerCoin.Mul64(1e6)
	outputID := sunyata.OutputID{
		TransactionID:    setupTxn.ID(),
		BeneficiaryIndex: 0,
	}
	ephemeralTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{
			{
				Parent: sunyata.Output{
					ID: outputID,
					// change the value of the output from 1 to 1e6
					Value:     newValue,
					Address:   pubkey.Address(),
					LeafIndex: sunyata.EphemeralLeafIndex,
				},
				PublicKey: pubkey,
			},
		},
		Outputs: []sunyata.Beneficiary{
			{
				Address: pubkey.Address(),
				Value:   newValue,
			},
		},
	}
	signTxn(&ephemeralTxn, sau.Context, privkey)

	err := sau.Context.ValidateTransactionSet([]sunyata.Transaction{
		setupTxn,
		ephemeralTxn,
	})
	if err == nil {
		t.Fatal("changed ephemeral output value")
	}
}

func TestDoubleSpendEphemeralOutput(t *testing.T) {
	var pubkey sunyata.PublicKey
	privkey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	copy(pubkey[:], privkey[32:])

	_, sau := genesisUpdate(pubkey, sunyata.Beneficiary{
		Address: pubkey.Address(),
		Value:   sunyata.BaseUnitsPerCoin,
	})

	// create a setup transaction to create an ephemeral output.
	setupTxn := sunyata.Transaction{
		Inputs: []sunyata.Input{
			{
				Parent:    sau.NewOutputs[1],
				PublicKey: pubkey,
			},
		},
		Outputs: []sunyata.Beneficiary{
			{
				Address: pubkey.Address(),
				Value:   sunyata.BaseUnitsPerCoin,
			},
		},
	}
	signTxn(&setupTxn, sau.Context, privkey)

	outputID := sunyata.OutputID{
		TransactionID:    setupTxn.ID(),
		BeneficiaryIndex: 0,
	}
	// Build two transactions that spend the ephemeral output, but different
	// beneficiaries.
	txn1 := sunyata.Transaction{
		Inputs: []sunyata.Input{
			{
				Parent: sunyata.Output{
					ID:        outputID,
					Value:     setupTxn.Outputs[0].Value,
					Address:   pubkey.Address(),
					LeafIndex: sunyata.EphemeralLeafIndex,
				},
				PublicKey: pubkey,
			},
		},
		Outputs: []sunyata.Beneficiary{
			{
				Address: pubkey.Address(),
				Value:   setupTxn.Outputs[0].Value,
			},
		},
	}
	txn2 := sunyata.Transaction{
		Inputs: []sunyata.Input{
			{
				Parent: sunyata.Output{
					ID:        outputID,
					Value:     setupTxn.Outputs[0].Value,
					Address:   pubkey.Address(),
					LeafIndex: sunyata.EphemeralLeafIndex,
				},
				PublicKey: pubkey,
			},
		},
		Outputs: []sunyata.Beneficiary{
			{
				Value: setupTxn.Outputs[0].Value,
			},
		},
	}
	signTxn(&txn1, sau.Context, privkey)
	signTxn(&txn2, sau.Context, privkey)

	err := sau.Context.ValidateTransactionSet([]sunyata.Transaction{
		setupTxn,
		txn1,
		txn2,
	})
	if err == nil {
		t.Fatal("double spent ephemeral output")
	}
}
