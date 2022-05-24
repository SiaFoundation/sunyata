package wallet

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/consensus"
)

// A Seed generates Ed25519 keys deterministically from some initial entropy.
type Seed struct {
	entropy *[16]byte
}

// String implements fmt.Stringer.
func (s Seed) String() string { return hex.EncodeToString(s.entropy[:]) }

// PrivateKey derives the Ed25519 private key for the specified index.
func (s Seed) PrivateKey(index uint64) sunyata.PrivateKey {
	buf := make([]byte, len(s.entropy)+8)
	n := copy(buf, s.entropy[:])
	binary.LittleEndian.PutUint64(buf[n:], index)
	seed := sunyata.HashBytes(buf)
	pk := sunyata.NewPrivateKeyFromSeed(seed[:])
	for i := range seed {
		seed[i] = 0
	}
	return pk
}

// PublicKey derives the sunyata.SiaPublicKey for the specified index.
func (s Seed) PublicKey(index uint64) (pk sunyata.PublicKey) {
	return s.PrivateKey(index).PublicKey()
}

// SeedFromEntropy returns the Seed derived from the supplied entropy.
func SeedFromEntropy(entropy *[16]byte) Seed {
	return Seed{entropy: entropy}
}

// SeedFromString returns the Seed derived from the supplied string.
func SeedFromString(s string) (Seed, error) {
	var entropy [16]byte
	if n, err := hex.Decode(entropy[:], []byte(s)); err != nil {
		return Seed{}, fmt.Errorf("seed string contained invalid characters: %w", err)
	} else if n != 16 {
		return Seed{}, errors.New("invalid seed string length")
	}
	return SeedFromEntropy(&entropy), nil
}

// NewSeed returns a random Seed.
func NewSeed() Seed {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		panic("insufficient system entropy")
	}
	return SeedFromEntropy(&entropy)
}

// A Transaction is an on-chain transaction relevant to a particular wallet,
// paired with useful metadata.
type Transaction struct {
	Raw       sunyata.Transaction
	Index     sunyata.ChainIndex
	ID        sunyata.TransactionID
	Inflow    sunyata.Currency
	Outflow   sunyata.Currency
	Timestamp time.Time
}

// ErrUnknownAddress is returned by Store.AddressInfo for addresses not known to
// the wallet.
var ErrUnknownAddress = errors.New("address not tracked by wallet")

// A Store stores wallet state. Implementations are assumed to be thread safe.
type Store interface {
	SeedIndex() uint64
	Balance() sunyata.Currency
	AddAddress(addr sunyata.Address, index uint64) error
	AddressIndex(addr sunyata.Address) (uint64, error)
	Addresses() ([]sunyata.Address, error)
	UnspentOutputElements() ([]sunyata.OutputElement, error)
	Transactions(since time.Time, max int) ([]Transaction, error)
}

// A TransactionBuilder helps construct transactions.
type TransactionBuilder struct {
	mu    sync.Mutex
	store Store
	used  map[sunyata.ElementID]bool
}

// ReleaseInputs is a helper function that releases the inputs of txn for
// use in other transactions. It should only be called on transactions that are
// invalid or will never be broadcast.
func (tb *TransactionBuilder) ReleaseInputs(txn sunyata.Transaction) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	for _, in := range txn.Inputs {
		delete(tb.used, in.Parent.ID)
	}
}

// ReserveFunds returns inputs worth at least the requested amount. The inputs
// will not be available to future calls to ReserveFunds or Fund unless
// ReleaseInputs is called.
func (tb *TransactionBuilder) ReserveFunds(amount sunyata.Currency, pool []sunyata.Transaction) ([]sunyata.OutputElement, error) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if amount.IsZero() {
		return nil, nil
	}

	// avoid reusing any inputs currently in the transaction pool
	inPool := make(map[sunyata.ElementID]bool)
	for _, ptxn := range pool {
		for _, in := range ptxn.Inputs {
			inPool[in.Parent.ID] = true
		}
	}

	utxos, err := tb.store.UnspentOutputElements()
	if err != nil {
		return nil, err
	}
	var outputSum sunyata.Currency
	var fundingElements []sunyata.OutputElement
	for _, sce := range utxos {
		if tb.used[sce.ID] || inPool[sce.ID] {
			continue
		}
		fundingElements = append(fundingElements, sce)
		outputSum = outputSum.Add(sce.Value)
		if outputSum.Cmp(amount) >= 0 {
			break
		}
	}
	if outputSum.Cmp(amount) < 0 {
		return nil, errors.New("insufficient balance")
	}

	for _, o := range fundingElements {
		tb.used[o.ID] = true
	}
	return fundingElements, nil
}

// Fund adds inputs worth at least the requested amount to the provided
// transaction. A change output is also added if necessary. The inputs will not
// be available to future calls to ReserveFunds or Fund unless ReleaseInputs is
// called.
func (tb *TransactionBuilder) Fund(cs consensus.State, txn *sunyata.Transaction, amount sunyata.Currency, seed Seed, pool []sunyata.Transaction) ([]sunyata.ElementID, error) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if amount.IsZero() {
		return nil, nil
	}

	// avoid reusing any inputs currently in the transaction pool
	inPool := make(map[sunyata.ElementID]bool)
	for _, ptxn := range pool {
		for _, in := range ptxn.Inputs {
			inPool[in.Parent.ID] = true
		}
	}

	utxos, err := tb.store.UnspentOutputElements()
	if err != nil {
		return nil, err
	}
	var outputSum sunyata.Currency
	var fundingElements []sunyata.OutputElement
	for _, sce := range utxos {
		if tb.used[sce.ID] || inPool[sce.ID] || cs.Index.Height < sce.MaturityHeight {
			continue
		}
		fundingElements = append(fundingElements, sce)
		outputSum = outputSum.Add(sce.Value)
		if outputSum.Cmp(amount) >= 0 {
			break
		}
	}
	if outputSum.Cmp(amount) < 0 {
		return nil, errors.New("insufficient balance")
	} else if outputSum.Cmp(amount) > 0 {
		// generate a change address
		index := tb.store.SeedIndex()
		addr := seed.PublicKey(index).Address()
		if err := tb.store.AddAddress(addr, index); err != nil {
			return nil, err
		}
		txn.Outputs = append(txn.Outputs, sunyata.Output{
			Value:   outputSum.Sub(amount),
			Address: addr,
		})
	}

	toSign := make([]sunyata.ElementID, len(fundingElements))
	for i, sce := range fundingElements {
		index, err := tb.store.AddressIndex(sce.Address)
		if err != nil {
			return nil, err
		}
		txn.Inputs = append(txn.Inputs, sunyata.Input{
			Parent:    sce,
			PublicKey: seed.PublicKey(index),
		})

		toSign[i] = sce.ID
		tb.used[sce.ID] = true
	}

	return toSign, nil
}

// SignTransaction adds a signature to each of the specified inputs using the
// provided seed. If len(toSign) == 0, a signature is added for every input with
// a known key.
func (tb *TransactionBuilder) SignTransaction(cs consensus.State, txn *sunyata.Transaction, toSign []sunyata.ElementID, seed Seed) error {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	sigHash := cs.InputSigHash(*txn)

	if len(toSign) == 0 {
		for _, in := range txn.Inputs {
			if index, err := tb.store.AddressIndex(in.Parent.Address); err == nil {
				in.Signature = seed.PrivateKey(index).SignHash(sigHash)
			} else if err != ErrUnknownAddress {
				return err
			}
		}
		return nil
	}

	inputWithID := func(id sunyata.ElementID) *sunyata.Input {
		for i := range txn.Inputs {
			if in := &txn.Inputs[i]; in.Parent.ID == id {
				return in
			}
		}
		return nil
	}
	for _, id := range toSign {
		in := inputWithID(id)
		if in == nil {
			return errors.New("no input with specified ID")
		}
		index, err := tb.store.AddressIndex(in.Parent.Address)
		if err == ErrUnknownAddress {
			return errors.New("no key for specified input")
		} else if err != nil {
			return err
		}
		in.Signature = seed.PrivateKey(index).SignHash(sigHash)
	}
	return nil
}

// NewTransactionBuilder returns a TransactionBuilder using the provided Store.
func NewTransactionBuilder(store Store) *TransactionBuilder {
	return &TransactionBuilder{
		store: store,
		used:  make(map[sunyata.ElementID]bool),
	}
}
