package chainutil

import (
	"crypto/ed25519"
	"encoding/binary"
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/consensus"
)

func FindBlockNonce(h *sunyata.BlockHeader, target sunyata.BlockID) {
	for !h.ID().MeetsTarget(target) {
		binary.LittleEndian.PutUint64(h.Nonce[:], binary.LittleEndian.Uint64(h.Nonce[:])+1)
	}
}

func JustHeaders(blocks []sunyata.Block) []sunyata.BlockHeader {
	headers := make([]sunyata.BlockHeader, len(blocks))
	for i := range headers {
		headers[i] = blocks[i].Header
	}
	return headers
}

func JustTransactions(blocks []sunyata.Block) [][]sunyata.Transaction {
	txns := make([][]sunyata.Transaction, len(blocks))
	for i := range txns {
		txns[i] = blocks[i].Transactions
	}
	return txns
}

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

func JustChainIndexes(blocks []sunyata.Block) []sunyata.ChainIndex {
	cis := make([]sunyata.ChainIndex, len(blocks))
	for i := range cis {
		cis[i] = blocks[i].Index()
	}
	return cis
}

type ChainSim struct {
	Genesis consensus.Checkpoint
	Chain   []sunyata.Block
	Context consensus.ValidationContext

	nonce [8]byte // for distinguishing forks

	// for simulating transactions
	pubkey  sunyata.PublicKey
	privkey ed25519.PrivateKey
	outputs []sunyata.Output
}

func (cs *ChainSim) Fork() *ChainSim {
	cs2 := *cs
	cs2.Chain = append([]sunyata.Block(nil), cs2.Chain...)
	cs2.outputs = append([]sunyata.Output(nil), cs2.outputs...)
	if cs.nonce[7]++; cs.nonce[7] == 0 {
		cs.nonce[6]++
	}
	return &cs2
}

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
		Transactions:     txns,
		AccumulatorProof: consensus.ComputeMultiproof(txns),
	}
	b.Header.Commitment = cs.Context.Commitment(b.Header.MinerAddress, b.Transactions, b.AccumulatorProof)
	FindBlockNonce(&b.Header, sunyata.HashRequiringWork(cs.Context.Difficulty))

	sau := consensus.ApplyBlock(cs.Context, b)
	cs.Context = sau.Context
	cs.Chain = append(cs.Chain, b)

	// update our outputs
	for i := range cs.outputs {
		sau.UpdateOutputProof(&cs.outputs[i])
	}
	for _, out := range sau.NewOutputs {
		if out.Address == cs.pubkey.Address() {
			cs.outputs = append(cs.outputs, out)
		}
	}

	return b
}

func (cs *ChainSim) MineBlockWithBeneficiaries(bs ...sunyata.Beneficiary) sunyata.Block {
	txn := sunyata.Transaction{
		Outputs:  bs,
		MinerFee: sunyata.NewCurrency64(cs.Context.Index.Height),
	}

	totalOut := txn.MinerFee
	for _, b := range bs {
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
		txn.Outputs = append(txn.Outputs, sunyata.Beneficiary{
			Address: cs.pubkey.Address(),
			Value:   totalIn.Sub(totalOut),
		})
	}

	// sign and mine
	sigHash := cs.Context.SigHash(txn)
	for i := range txn.Inputs {
		txn.Inputs[i].Signature = sunyata.SignTransaction(cs.privkey, sigHash)
	}
	return cs.MineBlockWithTxns(txn)
}

func (cs *ChainSim) MineBlock() sunyata.Block {
	// simulate chain activity by sending our existing outputs to new addresses
	var txns []sunyata.Transaction
	for _, out := range cs.outputs {
		txn := sunyata.Transaction{
			Inputs: []sunyata.Input{{
				Parent:    out,
				PublicKey: cs.pubkey,
			}},
			Outputs: []sunyata.Beneficiary{
				{Address: cs.pubkey.Address(), Value: out.Value.Sub(sunyata.NewCurrency64(cs.Context.Index.Height + 1))},
				{Address: sunyata.Address{cs.nonce[6], cs.nonce[7], 1, 2, 3}, Value: sunyata.NewCurrency64(1)},
			},
			MinerFee: sunyata.NewCurrency64(cs.Context.Index.Height),
		}
		sigHash := cs.Context.SigHash(txn)
		for i := range txn.Inputs {
			txn.Inputs[i].Signature = sunyata.SignTransaction(cs.privkey, sigHash)
		}

		txns = append(txns, txn)
	}
	cs.outputs = cs.outputs[:0]
	return cs.MineBlockWithTxns(txns...)
}

func (cs *ChainSim) MineBlocks(n int) []sunyata.Block {
	blocks := make([]sunyata.Block, n)
	for i := range blocks {
		blocks[i] = cs.MineBlock()
	}
	return blocks
}

func NewChainSim() *ChainSim {
	// gift ourselves some coins in the genesis block
	privkey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	var pubkey sunyata.PublicKey
	copy(pubkey[:], privkey[32:])
	ourAddr := pubkey.Address()
	gift := make([]sunyata.Beneficiary, 10)
	for i := range gift {
		gift[i] = sunyata.Beneficiary{
			Address: ourAddr,
			Value:   sunyata.BaseUnitsPerCoin.Mul64(10 * uint64(i+1)),
		}
	}
	genesisTxns := []sunyata.Transaction{{Outputs: gift}}
	genesis := sunyata.Block{
		Header: sunyata.BlockHeader{
			Timestamp: time.Unix(734600000, 0),
		},
		Transactions: genesisTxns,
	}
	sau := consensus.GenesisUpdate(genesis, sunyata.Work{NumHashes: [32]byte{31: 4}})
	var outputs []sunyata.Output
	for _, out := range sau.NewOutputs {
		if out.Address == pubkey.Address() {
			outputs = append(outputs, out)
		}
	}
	return &ChainSim{
		Genesis: consensus.Checkpoint{
			Block:   genesis,
			Context: sau.Context,
		},
		Context: sau.Context,
		privkey: privkey,
		pubkey:  pubkey,
		outputs: outputs,
	}
}
