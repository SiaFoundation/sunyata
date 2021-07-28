package walletutil

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/consensus"
	"go.sia.tech/sunyata/wallet"
)

// EphemeralStore implements Store in memory.
type EphemeralStore struct {
	mu        sync.Mutex
	addrs     map[sunyata.Address]uint64
	outputs   []sunyata.Output
	txns      []wallet.Transaction
	vc        consensus.ValidationContext
	seedIndex uint64
}

// SeedIndex implements Store.
func (s *EphemeralStore) SeedIndex() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seedIndex
}

// Context implements Store.
func (s *EphemeralStore) Context() consensus.ValidationContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vc
}

// AddAddress implements Store.
func (s *EphemeralStore) AddAddress(addr sunyata.Address, index uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addrs[addr] = index
	if index >= s.seedIndex {
		s.seedIndex = index + 1
	}
	return nil
}

// AddressIndex implements Store.
func (s *EphemeralStore) AddressIndex(addr sunyata.Address) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.addrs[addr]
	return index, ok
}

// SpendableOutputs implements Store.
func (s *EphemeralStore) SpendableOutputs() []sunyata.Output {
	s.mu.Lock()
	defer s.mu.Unlock()
	var spendable []sunyata.Output
	for _, o := range s.outputs {
		if o.Timelock <= s.vc.Index.Height {
			o.MerkleProof = append([]sunyata.Hash256(nil), o.MerkleProof...)
			spendable = append(spendable, o)
		}
	}
	return spendable
}

// Transactions returns all transactions relevant to the wallet, ordered
// oldest-to-newest.
func (s *EphemeralStore) Transactions() []wallet.Transaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]wallet.Transaction(nil), s.txns...)
}

// ProcessChainApplyUpdate implements chain.Subscriber.
func (s *EphemeralStore) ProcessChainApplyUpdate(cau *chain.ApplyUpdate, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// delete spent outputs
	remOutputs := s.outputs[:0]
	for _, o := range s.outputs {
		if !cau.OutputWasSpent(o) {
			remOutputs = append(remOutputs, o)
		}
	}
	s.outputs = remOutputs

	// update proofs for our outputs
	for i := range s.outputs {
		cau.UpdateOutputProof(&s.outputs[i])
	}

	// add new outputs
	for _, o := range cau.NewOutputs {
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
				Raw:     txn.DeepCopy(),
				Index:   cau.Context.Index, // same as cau.Block.Index()
				ID:      txn.ID(),
				Inflow:  inflow,
				Outflow: outflow,
			})
		}
	}

	s.vc = cau.Context
	return nil
}

// ProcessChainRevertUpdate implements chain.Subscriber.
func (s *EphemeralStore) ProcessChainRevertUpdate(cru *chain.RevertUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// delete removed outputs
	remOutputs := s.outputs[:0]
	for _, o := range s.outputs {
		if !cru.OutputWasRemoved(o) {
			remOutputs = append(remOutputs, o)
		}
	}
	s.outputs = remOutputs

	// re-add outputs that were spent in the reverted block
	for _, o := range cru.SpentOutputs {
		if _, ok := s.addrs[o.Address]; ok {
			o.MerkleProof = append([]sunyata.Hash256(nil), o.MerkleProof...)
			s.outputs = append(s.outputs, o)
		}
	}

	// update proofs for our outputs
	for i := range s.outputs {
		cru.UpdateOutputProof(&s.outputs[i])
	}

	// delete transactions originating in this block
	index := cru.Block.Index()
	for i, txn := range s.txns {
		if txn.Index == index {
			s.txns = s.txns[:i]
			break
		}
	}

	s.vc = cru.Context
	return nil
}

// NewEphemeralStore returns a new EphemeralStore.
func NewEphemeralStore() *EphemeralStore {
	return &EphemeralStore{
		addrs: make(map[sunyata.Address]uint64),
	}
}

// JSONStore implements Store in memory, backed by a JSON file.
type JSONStore struct {
	*EphemeralStore
	dir string
}

type persistData struct {
	Tip          sunyata.ChainIndex
	SeedIndex    uint64
	Addrs        map[string]uint64
	Outputs      []sunyata.Output
	Transactions []wallet.Transaction
}

func (s *JSONStore) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	addrs := make(map[string]uint64, len(s.addrs))
	for k, v := range s.addrs {
		addrs[hex.EncodeToString(k[:])] = v
	}
	js, _ := json.MarshalIndent(persistData{
		Tip:          s.vc.Index,
		SeedIndex:    s.seedIndex,
		Addrs:        addrs,
		Outputs:      s.outputs,
		Transactions: s.txns,
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

func (s *JSONStore) load(vc consensus.ValidationContext) (sunyata.ChainIndex, error) {
	var p persistData
	if js, err := os.ReadFile(filepath.Join(s.dir, "wallet.json")); os.IsNotExist(err) {
		// set defaults
		s.addrs = make(map[sunyata.Address]uint64)
		s.vc = vc
		return vc.Index, nil
	} else if err != nil {
		return sunyata.ChainIndex{}, err
	} else if err := json.Unmarshal(js, &p); err != nil {
		return sunyata.ChainIndex{}, err
	}
	s.addrs = make(map[sunyata.Address]uint64, len(p.Addrs))
	for k, v := range p.Addrs {
		var addr sunyata.Address
		hex.Decode(addr[:], []byte(k))
		s.addrs[addr] = v
	}
	s.outputs = p.Outputs
	s.txns = p.Transactions
	s.vc = vc
	s.seedIndex = p.SeedIndex
	return p.Tip, nil
}

// AddAddress implements Store.
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

// ProcessChainRevertUpdate implements chain.Subscriber.
func (s *JSONStore) ProcessChainRevertUpdate(cru *chain.RevertUpdate) error {
	return s.EphemeralStore.ProcessChainRevertUpdate(cru)
}

// NewJSONStore returns a new JSONStore.
func NewJSONStore(dir string, vc consensus.ValidationContext) (*JSONStore, sunyata.ChainIndex, error) {
	s := &JSONStore{
		EphemeralStore: NewEphemeralStore(),
		dir:            dir,
	}
	tip, err := s.load(vc)
	if err != nil {
		return nil, sunyata.ChainIndex{}, err
	}
	return s, tip, nil
}
