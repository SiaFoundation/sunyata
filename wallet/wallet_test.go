package wallet_test

import (
	"testing"
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/internal/chainutil"
	"go.sia.tech/sunyata/internal/walletutil"
)

func TestWallet(t *testing.T) {
	sim := chainutil.NewChainSim()

	cm := chain.NewManager(chainutil.NewEphemeralStore(sim.Genesis), sim.State)
	w := walletutil.NewTestingWallet(cm.TipState())
	cm.AddSubscriber(w, cm.Tip())

	// fund the wallet with 100 coins
	ourAddr := w.NewAddress()
	fund := sunyata.Output{Value: sunyata.BaseUnitsPerCoin.Mul64(100), Address: ourAddr}
	if err := cm.AddTipBlock(sim.MineBlockWithOutputs(fund)); err != nil {
		t.Fatal(err)
	}

	// wallet should now have a transaction, one element, and a non-zero balance
	if txns, _ := w.Transactions(time.Time{}, -1); len(txns) != 1 {
		t.Fatal("expected a single transaction, got", txns)
	} else if utxos, _ := w.UnspentOutputElements(); len(utxos) != 1 {
		t.Fatal("expected a single unspent element, got", utxos)
	} else if w.Balance().IsZero() {
		t.Fatal("expected non-zero balance after mining")
	}

	// mine 5 blocks, each containing a transaction that sends some coins to
	// the void and some to ourself
	for i := 0; i < 5; i++ {
		sendAmount := sunyata.BaseUnitsPerCoin.Mul64(7)
		txn := sunyata.Transaction{
			Outputs: []sunyata.Output{{
				Address: sunyata.VoidAddress,
				Value:   sendAmount,
			}},
		}
		if err := w.FundAndSign(&txn); err != nil {
			t.Fatal(err)
		}
		prevBalance := w.Balance()

		if err := cm.AddTipBlock(sim.MineBlockWithTxns(txn)); err != nil {
			t.Fatal(err)
		}

		if !prevBalance.Sub(w.Balance()).Equals(sendAmount) {
			t.Fatal("after send, balance should have decreased accordingly")
		}
	}
}
