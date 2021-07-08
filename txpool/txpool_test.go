package txpool_test

import (
	"testing"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/internal/chainutil"
	"go.sia.tech/sunyata/internal/walletutil"
	"go.sia.tech/sunyata/txpool"
	"go.sia.tech/sunyata/wallet"
)

// TestTPoolFlexibility tests that the txpool can accept transactions that are
// valid for the previous validation context
func TestPoolFlexibility(t *testing.T) {
	sim := chainutil.NewChainSim()

	cm := chain.NewManager(chainutil.NewEphemeralStore(sim.Genesis), sim.Context)
	walletStore := walletutil.NewEphemeralStore()
	wallet := wallet.NewHotWallet(walletStore, wallet.NewSeed())
	tp := txpool.New(sim.Genesis.Context)

	if err := cm.AddSubscriber(walletStore, cm.Tip()); err != nil {
		t.Fatalf("failed to subscribe wallet: %s", err)
	}
	if err := cm.AddSubscriber(tp, cm.Tip()); err != nil {
		t.Fatalf("failed to subscribe txpool: %s", err)
	}

	// Helper function to fund and sign a transaction
	send := func(recip sunyata.Address, amount sunyata.Currency) sunyata.Transaction {
		txn := sunyata.Transaction{
			Outputs: []sunyata.Beneficiary{
				{
					Address: recip,
					Value:   amount,
				},
			},
		}

		added, _, err := wallet.FundTransaction(&txn, amount, nil)
		if err != nil {
			t.Fatalf("failed to fund transaction: %s", err)
		}

		if err := wallet.SignTransaction(&txn, added); err != nil {
			t.Fatalf("failed to sign transaction: %s", err)
		}

		return txn
	}

	// Mine enough blocks to have multiple spendable outputs.
	addr := wallet.NextAddress()
	cm.AddTipBlock(sim.MineBlockWithBeneficiaries(sunyata.Beneficiary{
		Value:   sunyata.BaseUnitsPerCoin.Mul64(50),
		Address: addr,
	}, sunyata.Beneficiary{
		Value:   sunyata.BaseUnitsPerCoin.Mul64(50),
		Address: addr,
	}, sunyata.Beneficiary{
		Value:   sunyata.BaseUnitsPerCoin.Mul64(50),
		Address: addr,
	}))
	cm.AddTipBlock(sim.MineBlock())

	// Create and sign three transactions. One will be spent in the current block
	// to invalidate the merkle proofs of the other transaction. The other will
	// be spent one block later to verify that its proofs can be updated added
	// to the transaction pool. The last will be spent two blocks later to
	// verify that it can no longer be spent since the validation context will
	// be out of scope.
	amount := sunyata.BaseUnitsPerCoin.Mul64(10)
	spent := send(addr, amount)
	held := send(addr, amount)
	held2 := send(addr, amount)

	if err := tp.AddTransaction(spent); err != nil {
		t.Fatalf("failed to add spent transaction: %s", err)
	}

	// Mine a block to invalidate the held transaction's proofs, then add the
	// transaction.
	cm.AddTipBlock(sim.MineBlock())
	if err := tp.AddTransaction(held); err != nil {
		t.Fatalf("failed to add held transaction: %s", err)
	}

	// Mine another block to update the txpool's last applied update, then
	// attempt to add the second held transaction.
	cm.AddTipBlock(sim.MineBlock())
	if err := tp.AddTransaction(held2); err == nil {
		t.Fatalf("expected transaction to contain invalid proofs")
	}

}
