package txpool_test

import (
	"testing"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/consensus"
	"go.sia.tech/sunyata/internal/chainutil"
	"go.sia.tech/sunyata/txpool"
)

func TestPoolFlexibility(t *testing.T) {
	sim := chainutil.NewChainSim()

	cm := chain.NewManager(chainutil.NewEphemeralStore(sim.Genesis), sim.State)
	tp := txpool.New(sim.Genesis.State)
	cm.AddSubscriber(tp, cm.Tip())

	// Create three transactions that are valid as of the current block.
	txns := make([]sunyata.Transaction, 3)
	for i := range txns {
		txns[i] = sim.TxnWithOutputs(sunyata.Output{
			Value:   sunyata.BaseUnitsPerCoin.Mul64(1),
			Address: sunyata.VoidAddress,
		})
	}

	// Add the first transaction to the pool; it should be accepted.
	if err := tp.AddTransaction(txns[0]); err != nil {
		t.Fatal("pool rejected control transaction:", err)
	}

	// Mine a block and add the second transaction. Its proofs are now outdated,
	// but only by one block, so the pool should still accept it.
	if err := cm.AddTipBlock(sim.MineBlock()); err != nil {
		t.Fatal(err)
	} else if err := tp.AddTransaction(txns[1]); err != nil {
		t.Fatal("pool rejected slightly outdated transaction:", err)
	}

	// Mine another block and add the third transaction. Its proofs are now
	// outdated by two blocks, so the pool should reject it.
	if err := cm.AddTipBlock(sim.MineBlock()); err != nil {
		t.Fatal(err)
	} else if err := tp.AddTransaction(txns[2]); err == nil {
		t.Fatal("pool did not reject very outdated transaction")
	}
}

func TestEphemeralOutput(t *testing.T) {
	sim := chainutil.NewChainSim()

	cm := chain.NewManager(chainutil.NewEphemeralStore(sim.Genesis), sim.State)
	tp := txpool.New(sim.Genesis.State)
	cm.AddSubscriber(tp, cm.Tip())

	// create parent, child, and grandchild transactions
	privkey := sunyata.GeneratePrivateKey()
	parent := sim.TxnWithOutputs(sunyata.Output{
		Value:   sunyata.BaseUnitsPerCoin.Mul64(10),
		Address: privkey.PublicKey().Address(),
	})
	child := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    parent.EphemeralOutputElement(0),
			PublicKey: privkey.PublicKey(),
		}},
		Outputs: []sunyata.Output{{
			Value:   sunyata.BaseUnitsPerCoin.Mul64(9),
			Address: privkey.PublicKey().Address(),
		}},
		MinerFee: sunyata.BaseUnitsPerCoin.Mul64(1),
	}
	child.Inputs[0].Signature = privkey.SignHash(sim.State.InputSigHash(child))

	grandchild := sunyata.Transaction{
		Inputs: []sunyata.Input{{
			Parent:    child.EphemeralOutputElement(0),
			PublicKey: privkey.PublicKey(),
		}},
		Outputs: []sunyata.Output{{
			Value:   sunyata.BaseUnitsPerCoin.Mul64(7),
			Address: sunyata.VoidAddress,
		}},
		MinerFee: sunyata.BaseUnitsPerCoin.Mul64(2),
	}
	grandchild.Inputs[0].Signature = privkey.SignHash(sim.State.InputSigHash(grandchild))

	// add all three transactions to the pool
	if err := tp.AddTransaction(parent); err != nil {
		t.Fatal(err)
	} else if err := tp.AddTransaction(child); err != nil {
		t.Fatal(err)
	} else if err := tp.AddTransaction(grandchild); err != nil {
		t.Fatal(err)
	} else if len(tp.Transactions()) != 3 {
		t.Fatal("wrong number of transactions in pool:", len(tp.Transactions()))
	}

	// mine a block containing parent and child
	pcState := sim.State
	pcBlock := sim.MineBlockWithTxns(parent, child)
	if err := cm.AddTipBlock(pcBlock); err != nil {
		t.Fatal(err)
	}

	// pool should now contain just grandchild
	if txns := tp.Transactions(); len(txns) != 1 {
		t.Fatal("wrong number of transactions in pool:", len(tp.Transactions()))
	}
	grandchild = tp.Transactions()[0]
	if grandchild.Inputs[0].Parent.LeafIndex == sunyata.EphemeralLeafIndex {
		t.Fatal("grandchild's input should no longer be ephemeral")
	}
	// mine a block containing grandchild
	gState := sim.State
	gBlock := sim.MineBlockWithTxns(grandchild)
	if err := cm.AddTipBlock(gBlock); err != nil {
		t.Fatal(err)
	}

	// revert the grandchild block
	tp.ProcessChainRevertUpdate(&chain.RevertUpdate{
		RevertUpdate: consensus.RevertBlock(gState, gBlock),
		Block:        gBlock,
	})

	// pool should contain the grandchild transaction again
	if len(tp.Transactions()) != 1 {
		t.Fatal("wrong number of transactions in pool:", len(tp.Transactions()))
	}
	grandchild = tp.Transactions()[0]
	if grandchild.Inputs[0].Parent.LeafIndex == sunyata.EphemeralLeafIndex {
		t.Fatal("grandchild's input should not be ephemeral")
	}

	// revert the parent+child block
	tp.ProcessChainRevertUpdate(&chain.RevertUpdate{
		RevertUpdate: consensus.RevertBlock(pcState, pcBlock),
		Block:        pcBlock,
	})

	// pool should contain all three transactions again
	if len(tp.Transactions()) != 3 {
		t.Fatal("wrong number of transactions in pool:", len(tp.Transactions()))
	}
	// grandchild's input should be ephemeral again
	for _, txn := range tp.Transactions() {
		if txn.Outputs[0].Address == sunyata.VoidAddress {
			if txn.Inputs[0].Parent.LeafIndex != sunyata.EphemeralLeafIndex {
				t.Fatal("grandchild's input should be ephemeral again")
			}
			break
		}
	}
}
