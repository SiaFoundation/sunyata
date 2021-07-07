package txpool_test

import (
	"testing"
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/consensus"
	"go.sia.tech/sunyata/internal/chainutil"
	"go.sia.tech/sunyata/internal/walletutil"
	"go.sia.tech/sunyata/miner"
	"go.sia.tech/sunyata/txpool"
	"go.sia.tech/sunyata/wallet"
)

// TestTPoolFlexibility tests that the txpool can accept transactions that are
// valid for the previous validation context
func TestTPoolFlexibility(t *testing.T) {
	walletStore := walletutil.NewEphemeralStore()
	wallet := wallet.NewHotWallet(walletStore, wallet.NewSeed())
	genesisBlock := sunyata.Block{
		Header: sunyata.BlockHeader{
			Timestamp: time.Now().Add(-1 * time.Hour),
		},
		Transactions: []sunyata.Transaction{
			{
				Outputs: []sunyata.Beneficiary{
					{
						Address: wallet.NextAddress(),
						Value:   sunyata.BaseUnitsPerCoin.Mul64(100),
					},
				},
			},
		},
	}
	genesis := consensus.Checkpoint{
		Block:   genesisBlock,
		Context: consensus.GenesisUpdate(genesisBlock, sunyata.Work{NumHashes: [32]byte{31: 1}}).Context,
	}
	chainStore := chainutil.NewEphemeralStore(genesis)
	cm := chain.NewManager(chainStore, genesis.Context)
	tp := txpool.New(genesis.Context)

	if err := cm.AddSubscriber(walletStore, genesis.Context.Index); err != nil {
		t.Fatalf("failed to subscribe wallet: %s", err)
	}

	m := miner.New(genesis.Context, wallet.NextAddress(), tp, miner.CPU)
	if err := cm.AddSubscriber(tp, cm.Tip()); err != nil {
		t.Fatalf("failed to subscribe txpool: %s", err)
	}
	if err := cm.AddSubscriber(m, cm.Tip()); err != nil {
		t.Fatalf("failed to subscribe miner: %s", err)
	}

	// Helper function to miner blocks using our miner.
	mineBlock := func(n int) {
		for i := 0; i < n; i++ {
			b := m.MineBlock()
			if err := cm.AddTipBlock(b); err != nil {
				t.Fatalf("failed to mine block: %s", err)
			}
		}
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
	mineBlock(200)

	// Create and sign three transactions. One will be spent in the current block
	// to invalidate the merkle proofs of the other transaction. The other will
	// be spent one block later to verify that its proofs can be updated added
	// to the transaction pool. The last will be spent two blocks later to
	// verify that it can no longer be spent since the validation context will
	// be out of scope.
	amount := sunyata.BaseUnitsPerCoin.Mul64(10)
	spent := send(wallet.NextAddress(), amount)
	held := send(wallet.NextAddress(), amount)
	held2 := send(wallet.NextAddress(), amount)

	if err := tp.AddTransaction(spent); err != nil {
		t.Fatalf("failed to add spent transaction: %s", err)
	}

	// Mine a block to invalidate the held transaction's proofs, then add the
	// transaction.
	mineBlock(1)
	if err := tp.AddTransaction(held); err != nil {
		t.Fatalf("failed to add held transaction: %s", err)
	}

	// Mine another block to update the txpool's last applied update, then
	// attempt to add the second held transaction.
	mineBlock(1)
	if err := tp.AddTransaction(held2); err == nil {
		t.Fatalf("expected transaction to contain invalid proofs")
	}

}
