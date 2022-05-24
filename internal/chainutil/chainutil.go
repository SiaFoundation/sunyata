package chainutil

import (
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/consensus"
)

// FindBlockNonce finds a block nonce meeting the target.
func FindBlockNonce(cs consensus.State, h *sunyata.BlockHeader, target sunyata.BlockID) {
	for !h.ID().MeetsTarget(target) {
		h.Nonce++
	}
}

// JustHeaders renters only the headers of each block.
func JustHeaders(blocks []sunyata.Block) []sunyata.BlockHeader {
	headers := make([]sunyata.BlockHeader, len(blocks))
	for i := range headers {
		headers[i] = blocks[i].Header
	}
	return headers
}

// JustTransactions returns only the transactions of each block.
func JustTransactions(blocks []sunyata.Block) [][]sunyata.Transaction {
	txns := make([][]sunyata.Transaction, len(blocks))
	for i := range txns {
		txns[i] = blocks[i].Transactions
	}
	return txns
}

// JustTransactionIDs returns only the transaction ids included in each block.
func JustTransactionIDs(blocks []sunyata.Block) [][]sunyata.TransactionID {
	txns := make([][]sunyata.TransactionID, len(blocks))
	for i := range txns {
		txns[i] = make([]sunyata.TransactionID, len(blocks[i].Transactions))
		for j := range txns[i] {
			txns[i][j] = blocks[i].Transactions[j].ID()
		}
	}
	return txns
}

// JustChainIndexes returns only the chain index of each block.
func JustChainIndexes(blocks []sunyata.Block) []sunyata.ChainIndex {
	cis := make([]sunyata.ChainIndex, len(blocks))
	for i := range cis {
		cis[i] = blocks[i].Index()
	}
	return cis
}

// ChainSim represents a simulation of a blockchain.
type ChainSim struct {
	Genesis consensus.Checkpoint
	Chain   []sunyata.Block
	State   consensus.State

	nonce uint64 // for distinguishing forks

	// for simulating transactions
	pubkey  sunyata.PublicKey
	privkey sunyata.PrivateKey
	outputs []sunyata.OutputElement
}

// Fork forks the current chain.
func (cs *ChainSim) Fork() *ChainSim {
	cs2 := *cs
	cs2.Chain = append([]sunyata.Block(nil), cs2.Chain...)
	cs2.outputs = append([]sunyata.OutputElement(nil), cs2.outputs...)
	cs.nonce += 1 << 48
	return &cs2
}

//MineBlockWithTxns mine a block with the given transaction.
func (cs *ChainSim) MineBlockWithTxns(txns ...sunyata.Transaction) sunyata.Block {
	prev := cs.Genesis.Block.Header
	if len(cs.Chain) > 0 {
		prev = cs.Chain[len(cs.Chain)-1].Header
	}
	b := sunyata.Block{
		Header: sunyata.BlockHeader{
			Height:       prev.Height + 1,
			ParentID:     prev.ID(),
			Nonce:        cs.nonce,
			Timestamp:    prev.Timestamp.Add(time.Second),
			MinerAddress: sunyata.VoidAddress,
		},
		Transactions: txns,
	}
	b.Header.Commitment = cs.State.Commitment(b.Header.MinerAddress, b.Transactions)
	FindBlockNonce(cs.State, &b.Header, sunyata.HashRequiringWork(cs.State.Difficulty))

	sau := consensus.ApplyBlock(cs.State, b)
	cs.State = sau.State
	cs.Chain = append(cs.Chain, b)

	// update our outputs
	for i := range cs.outputs {
		sau.UpdateElementProof(&cs.outputs[i].StateElement)
	}
	for _, out := range sau.NewOutputElements {
		if out.Address == cs.pubkey.Address() {
			cs.outputs = append(cs.outputs, out)
		}
	}

	return b
}

// TxnWithOutputs returns a transaction containing the specified outputs.
// The ChainSim must have funds equal to or exceeding the sum of the outputs.
func (cs *ChainSim) TxnWithOutputs(scos ...sunyata.Output) sunyata.Transaction {
	txn := sunyata.Transaction{
		Outputs:  scos,
		MinerFee: sunyata.NewCurrency64(cs.State.Index.Height),
	}

	totalOut := txn.MinerFee
	for _, sco := range scos {
		totalOut = totalOut.Add(sco.Value)
	}

	// select inputs and compute change output
	var totalIn sunyata.Currency
	for i, out := range cs.outputs {
		txn.Inputs = append(txn.Inputs, sunyata.Input{
			Parent:    out,
			PublicKey: cs.pubkey,
		})
		totalIn = totalIn.Add(out.Value)
		if totalIn.Cmp(totalOut) >= 0 {
			cs.outputs = cs.outputs[i+1:]
			break
		}
	}

	if totalIn.Cmp(totalOut) < 0 {
		panic("insufficient funds")
	} else if totalIn.Cmp(totalOut) > 0 {
		// add change output
		txn.Outputs = append(txn.Outputs, sunyata.Output{
			Address: cs.pubkey.Address(),
			Value:   totalIn.Sub(totalOut),
		})
	}

	// sign
	sigHash := cs.State.InputSigHash(txn)
	for i := range txn.Inputs {
		txn.Inputs[i].Signature = cs.privkey.SignHash(sigHash)
	}
	return txn
}

// MineBlockWithOutputs mines a block with a transaction containing the
// specified outputs. The ChainSim must have funds equal to or exceeding the sum
// of the outputs.
func (cs *ChainSim) MineBlockWithOutputs(outs ...sunyata.Output) sunyata.Block {
	txn := sunyata.Transaction{
		Outputs:  outs,
		MinerFee: sunyata.NewCurrency64(cs.State.Index.Height),
	}

	totalOut := txn.MinerFee
	for _, b := range outs {
		totalOut = totalOut.Add(b.Value)
	}

	// select inputs and compute change output
	var totalIn sunyata.Currency
	for i, out := range cs.outputs {
		txn.Inputs = append(txn.Inputs, sunyata.Input{
			Parent:    out,
			PublicKey: cs.pubkey,
		})
		totalIn = totalIn.Add(out.Value)
		if totalIn.Cmp(totalOut) >= 0 {
			cs.outputs = cs.outputs[i+1:]
			break
		}
	}

	if totalIn.Cmp(totalOut) < 0 {
		panic("insufficient funds")
	} else if totalIn.Cmp(totalOut) > 0 {
		// add change output
		txn.Outputs = append(txn.Outputs, sunyata.Output{
			Address: cs.pubkey.Address(),
			Value:   totalIn.Sub(totalOut),
		})
	}

	// sign and mine
	sigHash := cs.State.InputSigHash(txn)
	for i := range txn.Inputs {
		txn.Inputs[i].Signature = cs.privkey.SignHash(sigHash)
	}
	return cs.MineBlockWithTxns(txn)
}

// MineBlock mine an empty block.
func (cs *ChainSim) MineBlock() sunyata.Block {
	// simulate chain activity by sending our existing outputs to new addresses
	var txns []sunyata.Transaction
	for _, out := range cs.outputs {
		txn := sunyata.Transaction{
			Inputs: []sunyata.Input{{
				Parent:    out,
				PublicKey: cs.pubkey,
			}},
			Outputs: []sunyata.Output{
				{Address: cs.pubkey.Address(), Value: out.Value.Sub(sunyata.NewCurrency64(cs.State.Index.Height + 1))},
				{Address: sunyata.Address{byte(cs.nonce >> 48), byte(cs.nonce >> 56), 1, 2, 3}, Value: sunyata.NewCurrency64(1)},
			},
			MinerFee: sunyata.NewCurrency64(cs.State.Index.Height),
		}
		sigHash := cs.State.InputSigHash(txn)
		for i := range txn.Inputs {
			txn.Inputs[i].Signature = cs.privkey.SignHash(sigHash)
		}

		txns = append(txns, txn)
	}
	cs.outputs = cs.outputs[:0]
	return cs.MineBlockWithTxns(txns...)
}

// MineBlocks mine a number of blocks.
func (cs *ChainSim) MineBlocks(n int) []sunyata.Block {
	blocks := make([]sunyata.Block, n)
	for i := range blocks {
		blocks[i] = cs.MineBlock()
	}
	return blocks
}

// NewChainSim returns a new ChainSim useful for simulating forks.
func NewChainSim() *ChainSim {
	// gift ourselves some coins in the genesis block
	privkey := sunyata.GeneratePrivateKey()
	pubkey := privkey.PublicKey()
	ourAddr := pubkey.Address()
	gift := make([]sunyata.Output, 10)
	for i := range gift {
		gift[i] = sunyata.Output{
			Address: ourAddr,
			Value:   sunyata.BaseUnitsPerCoin.Mul64(10 * uint64(i+1)),
		}
	}
	genesisTxns := []sunyata.Transaction{{Outputs: gift}}
	genesis := sunyata.Block{
		Header: sunyata.BlockHeader{
			Timestamp: time.Unix(734600000, 0).UTC(),
		},
		Transactions: genesisTxns,
	}
	sau := consensus.GenesisUpdate(genesis, sunyata.Work{NumHashes: [32]byte{31: 4}})
	var outputs []sunyata.OutputElement
	for _, out := range sau.NewOutputElements {
		if out.Address == pubkey.Address() {
			outputs = append(outputs, out)
		}
	}
	return &ChainSim{
		Genesis: consensus.Checkpoint{
			Block: genesis,
			State: sau.State,
		},
		State:   sau.State,
		privkey: privkey,
		pubkey:  pubkey,
		outputs: outputs,
	}
}
