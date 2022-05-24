package walletutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/consensus"
	"go.sia.tech/sunyata/wallet"
)

// EphemeralStore implements wallet.Store in memory.
type EphemeralStore struct {
	mu        sync.Mutex
	tip       sunyata.ChainIndex
	seedIndex uint64
	addrs     map[sunyata.Address]uint64
	outputs   []sunyata.OutputElement
	txns      []wallet.Transaction
}

// SeedIndex implements wallet.Store.
func (s *EphemeralStore) SeedIndex() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seedIndex
}

// Balance implements wallet.Store.
func (s *EphemeralStore) Balance() (bal sunyata.Currency) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, out := range s.outputs {
		if out.MaturityHeight < s.tip.Height {
			bal = bal.Add(out.Value)
		}
	}
	return
}

// AddAddress implements wallet.Store.
func (s *EphemeralStore) AddAddress(addr sunyata.Address, index uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addrs[addr] = index
	if index >= s.seedIndex {
		s.seedIndex = index + 1
	}
	return nil
}

// AddressIndex implements wallet.Store.
func (s *EphemeralStore) AddressIndex(addr sunyata.Address) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.addrs[addr]
	if !ok {
		return 0, wallet.ErrUnknownAddress
	}
	return index, nil
}

// Addresses implements wallet.Store.
func (s *EphemeralStore) Addresses() ([]sunyata.Address, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	addrs := make([]sunyata.Address, 0, len(s.addrs))
	for addr := range s.addrs {
		addrs = append(addrs, addr)
	}
	return addrs, nil
}

// UnspentOutputElements implements wallet.Store.
func (s *EphemeralStore) UnspentOutputElements() ([]sunyata.OutputElement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var elems []sunyata.OutputElement
	for _, out := range s.outputs {
		out.MerkleProof = append([]sunyata.Hash256(nil), out.MerkleProof...)
		elems = append(elems, out)
	}
	return elems, nil
}

// Transactions returns all transactions relevant to the wallet, ordered
// oldest-to-newest.
func (s *EphemeralStore) Transactions(since time.Time, max int) ([]wallet.Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var txns []wallet.Transaction
	for _, txn := range s.txns {
		if len(txns) == max {
			break
		} else if txn.Timestamp.After(since) {
			txns = append(txns, txn)
		}
	}
	return txns, nil
}

// ProcessChainApplyUpdate implements chain.Subscriber.
func (s *EphemeralStore) ProcessChainApplyUpdate(cau *chain.ApplyUpdate, mayCommit bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// delete spent elements
	rem := s.outputs[:0]
	for _, out := range s.outputs {
		if !cau.OutputElementWasSpent(out) {
			rem = append(rem, out)
		}
	}
	s.outputs = rem

	// update proofs for our elements
	for i := range s.outputs {
		cau.UpdateElementProof(&s.outputs[i].StateElement)
	}

	// add new elements
	for _, o := range cau.NewOutputElements {
		if _, ok := s.addrs[o.Address]; ok {
			s.outputs = append(s.outputs, o)
		}
	}

	// add relevant transactions
	for _, txn := range cau.Block.Transactions {
		// a transaction is relevant if any of its inputs or outputs reference a
		// wallet-controlled address
		var inflow, outflow sunyata.Currency
		for _, out := range txn.Outputs {
			if _, ok := s.addrs[out.Address]; ok {
				inflow = inflow.Add(out.Value)
			}
		}
		for _, in := range txn.Inputs {
			if _, ok := s.addrs[in.Parent.Address]; ok {
				outflow = outflow.Add(in.Parent.Value)
			}
		}
		if !inflow.IsZero() || !outflow.IsZero() {
			s.txns = append(s.txns, wallet.Transaction{
				Raw:       txn.DeepCopy(),
				Index:     cau.State.Index, // same as cau.Block.Index()
				ID:        txn.ID(),
				Inflow:    inflow,
				Outflow:   outflow,
				Timestamp: cau.Block.Header.Timestamp,
			})
		}
	}

	s.tip = cau.State.Index
	return nil
}

// ProcessChainRevertUpdate implements chain.Subscriber.
func (s *EphemeralStore) ProcessChainRevertUpdate(cru *chain.RevertUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// delete removed elements
	rem := s.outputs[:0]
	for _, o := range s.outputs {
		if !cru.OutputElementWasRemoved(o) {
			rem = append(rem, o)
		}
	}
	s.outputs = rem

	// re-add elements that were spent in the reverted block
	for _, o := range cru.SpentOutputs {
		if _, ok := s.addrs[o.Address]; ok {
			o.MerkleProof = append([]sunyata.Hash256(nil), o.MerkleProof...)
			s.outputs = append(s.outputs, o)
		}
	}

	// update proofs for our elements
	for i := range s.outputs {
		cru.UpdateElementProof(&s.outputs[i].StateElement)
	}

	// delete transactions originating in this block
	index := cru.Block.Index()
	for i, txn := range s.txns {
		if txn.Index == index {
			s.txns = s.txns[:i]
			break
		}
	}

	s.tip = cru.State.Index
	return nil
}

// NewEphemeralStore returns a new EphemeralStore.
func NewEphemeralStore(tip sunyata.ChainIndex) *EphemeralStore {
	return &EphemeralStore{
		tip:   tip,
		addrs: make(map[sunyata.Address]uint64),
	}
}

// JSONStore implements wallet.Store in memory, backed by a JSON file.
type JSONStore struct {
	*EphemeralStore
	dir string
}

type jsonPersistData struct {
	Tip            sunyata.ChainIndex
	SeedIndex      uint64
	Addrs          map[sunyata.Address]uint64
	OutputElements []sunyata.OutputElement
	Transactions   []wallet.Transaction
}

func (s *JSONStore) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	js, _ := json.MarshalIndent(jsonPersistData{
		Tip:            s.tip,
		SeedIndex:      s.seedIndex,
		Addrs:          s.addrs,
		OutputElements: s.outputs,
		Transactions:   s.txns,
	}, "", "  ")

	// atomic save
	dst := filepath.Join(s.dir, "wallet.json")
	f, err := os.OpenFile(dst+"_tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0660)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(js); err != nil {
		return err
	} else if f.Sync(); err != nil {
		return err
	} else if f.Close(); err != nil {
		return err
	} else if err := os.Rename(dst+"_tmp", dst); err != nil {
		return err
	}
	return nil
}

func (s *JSONStore) load(tip sunyata.ChainIndex) (sunyata.ChainIndex, error) {
	var p jsonPersistData
	if js, err := os.ReadFile(filepath.Join(s.dir, "wallet.json")); os.IsNotExist(err) {
		// set defaults
		s.tip = tip
		s.addrs = make(map[sunyata.Address]uint64)
		return tip, nil
	} else if err != nil {
		return sunyata.ChainIndex{}, err
	} else if err := json.Unmarshal(js, &p); err != nil {
		return sunyata.ChainIndex{}, err
	}
	s.tip = tip
	s.seedIndex = p.SeedIndex
	s.addrs = p.Addrs
	s.outputs = p.OutputElements
	s.txns = p.Transactions
	return p.Tip, nil
}

// AddAddress implements wallet.Store.
func (s *JSONStore) AddAddress(addr sunyata.Address, index uint64) error {
	s.EphemeralStore.AddAddress(addr, index)
	return s.save()
}

// ProcessChainApplyUpdate implements chain.Subscriber.
func (s *JSONStore) ProcessChainApplyUpdate(cau *chain.ApplyUpdate, mayCommit bool) error {
	s.EphemeralStore.ProcessChainApplyUpdate(cau, mayCommit)
	if mayCommit {
		return s.save()
	}
	return nil
}

// NewJSONStore returns a new JSONStore.
func NewJSONStore(dir string, tip sunyata.ChainIndex) (*JSONStore, sunyata.ChainIndex, error) {
	s := &JSONStore{
		EphemeralStore: NewEphemeralStore(tip),
		dir:            dir,
	}
	tip, err := s.load(tip)
	if err != nil {
		return nil, sunyata.ChainIndex{}, err
	}
	return s, tip, nil
}

// A TestingWallet is a simple hot wallet, useful for sending and receiving
// transactions in a testing environment.
type TestingWallet struct {
	mu sync.Mutex
	*EphemeralStore
	Seed wallet.Seed
	tb   *wallet.TransactionBuilder
	cs   consensus.State
}

// Balance returns the wallet's balance.
func (w *TestingWallet) Balance() sunyata.Currency {
	return w.EphemeralStore.Balance()
}

// Address returns an address controlled by the wallet.
func (w *TestingWallet) Address() sunyata.Address {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Seed.PublicKey(w.SeedIndex()).Address()
}

// NewAddress derives a new address and adds it to the wallet.
func (w *TestingWallet) NewAddress() sunyata.Address {
	w.mu.Lock()
	defer w.mu.Unlock()
	addr := w.Seed.PublicKey(w.SeedIndex()).Address()
	w.AddAddress(addr, w.SeedIndex())
	return addr
}

// FundTransaction funds the provided transaction, adding a change output if
// necessary.
func (w *TestingWallet) FundTransaction(txn *sunyata.Transaction, amount sunyata.Currency, pool []sunyata.Transaction) ([]sunyata.ElementID, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	toSign, err := w.tb.Fund(w.cs, txn, amount, w.Seed, pool)
	return toSign, func() { w.tb.ReleaseInputs(*txn) }, err
}

// SignTransaction funds the provided transaction, adding a change output if
// necessary.
func (w *TestingWallet) SignTransaction(cs consensus.State, txn *sunyata.Transaction, toSign []sunyata.ElementID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tb.SignTransaction(cs, txn, toSign, w.Seed)
}

// FundAndSign funds and signs the provided transaction, adding a change output
// if necessary.
func (w *TestingWallet) FundAndSign(txn *sunyata.Transaction) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var amount sunyata.Currency
	for _, sco := range txn.Outputs {
		amount = amount.Add(sco.Value)
	}
	amount = amount.Add(txn.MinerFee)
	for _, sci := range txn.Inputs {
		amount = amount.Sub(sci.Parent.Value)
	}

	toSign, err := w.tb.Fund(w.cs, txn, amount, w.Seed, nil)
	if err != nil {
		return err
	}
	return w.tb.SignTransaction(w.cs, txn, toSign, w.Seed)
}

// ProcessChainApplyUpdate implements chain.Subscriber.
func (w *TestingWallet) ProcessChainApplyUpdate(cau *chain.ApplyUpdate, mayCommit bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.EphemeralStore.ProcessChainApplyUpdate(cau, mayCommit)
	w.cs = cau.State
	return nil
}

// ProcessChainRevertUpdate implements chain.Subscriber.
func (w *TestingWallet) ProcessChainRevertUpdate(cru *chain.RevertUpdate) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.EphemeralStore.ProcessChainRevertUpdate(cru)
	w.cs = cru.State
	return nil
}

// NewTestingWallet creates a TestingWallet with the provided consensus state
// and an ephemeral store.
func NewTestingWallet(cs consensus.State) *TestingWallet {
	store := NewEphemeralStore(cs.Index)
	return &TestingWallet{
		EphemeralStore: store,
		Seed:           wallet.NewSeed(),
		tb:             wallet.NewTransactionBuilder(store),
		cs:             cs,
	}
}
