// Package consensus implements the sunyata consensus algorithms.
package consensus

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"go.sia.tech/sunyata"
)

var (
	// ErrOverweight is returned when a block's weight exceeds MaxBlockWeight.
	ErrOverweight = errors.New("block is too heavy")

	// ErrOverflow is returned when the sum of a transaction's inputs and/or
	// outputs overflows the Currency representation.
	ErrOverflow = errors.New("sum of currency values overflowed")
)

func (s State) medianTimestamp() time.Time {
	presopy := s.PrevTimestamps
	ts := presopy[:s.numTimestamps()]
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	if len(ts)%2 != 0 {
		return ts[len(ts)/2]
	}
	l, r := ts[len(ts)/2-1], ts[len(ts)/2]
	return l.Add(r.Sub(l) / 2)
}

func (s State) validateHeader(h sunyata.BlockHeader) error {
	if h.Height != s.Index.Height+1 {
		return errors.New("wrong height")
	} else if h.ParentID != s.Index.ID {
		return errors.New("wrong parent ID")
	} else if sunyata.WorkRequiredForHash(h.ID()).Cmp(s.Difficulty) < 0 {
		return errors.New("insufficient work")
	} else if h.Timestamp.Before(s.medianTimestamp()) {
		return errors.New("timestamp is too far in the past")
	}
	return nil
}

func (s State) validateCurrencyValues(txn sunyata.Transaction) error {
	// Add up all of the currency values in the transaction and check for
	// overflow. This allows us to freely add any currency values in later
	// validation functions without worrying about overflow.

	var sum sunyata.Currency
	var overflowed bool
	add := func(x sunyata.Currency) {
		if !overflowed {
			sum, overflowed = sum.AddWithOverflow(x)
		}
	}

	for _, in := range txn.Inputs {
		add(in.Parent.Value)
	}
	for i, out := range txn.Outputs {
		if out.Value.IsZero() {
			return fmt.Errorf("output %v has zero value", i)
		}
		add(out.Value)
	}
	add(txn.MinerFee)
	if overflowed {
		return ErrOverflow
	}
	return nil
}

func (s State) validateTimeLocks(txn sunyata.Transaction) error {
	blockHeight := s.Index.Height + 1
	for i, in := range txn.Inputs {
		if in.Parent.MaturityHeight > blockHeight {
			return fmt.Errorf("input %v does not mature until block %v", i, in.Parent.MaturityHeight)
		}
	}
	return nil
}

func (s State) outputsEqualInputs(txn sunyata.Transaction) error {
	var totalIn, totalOut sunyata.Currency
	for _, in := range txn.Inputs {
		totalIn = totalIn.Add(in.Parent.Value)
	}
	for _, out := range txn.Outputs {
		totalOut = totalOut.Add(out.Value)
	}
	totalOut = totalOut.Add(txn.MinerFee)
	if totalIn != totalOut {
		return fmt.Errorf("inputs (%v C) do not equal outputs (%v C)", totalIn, totalOut)
	}
	return nil
}

func (s State) validateStateProofs(txn sunyata.Transaction) error {
	for i, in := range txn.Inputs {
		switch {
		case in.Parent.LeafIndex == sunyata.EphemeralLeafIndex:
			continue
		case s.Elements.ContainsUnspentOutputElement(in.Parent):
			continue
		case s.Elements.ContainsSpentOutputElement(in.Parent):
			return fmt.Errorf("input %v double-spends output %v", i, in.Parent.ID)
		default:
			return fmt.Errorf("input %v spends output (%v) not present in the accumulator", i, in.Parent.ID)
		}
	}
	return nil
}

func (s State) validateSpendPolicies(txn sunyata.Transaction) error {
	sigHash := s.InputSigHash(txn)
	for i, in := range txn.Inputs {
		if in.PublicKey.Address() != in.Parent.Address {
			return fmt.Errorf("input %v claims incorrect pubkey for parent address", i)
		} else if !in.PublicKey.VerifyHash(sigHash, in.Signature) {
			return fmt.Errorf("input %v has invalid signature", i)
		}
	}
	return nil
}

// ValidateTransaction partially validates txn for inclusion in a child block.
// It does not validate ephemeral outputs.
func (s State) ValidateTransaction(txn sunyata.Transaction) error {
	// check proofs first; that way, subsequent checks can assume that all
	// parent StateElements are valid
	if err := s.validateStateProofs(txn); err != nil {
		return err
	}

	if err := s.validateCurrencyValues(txn); err != nil {
		return err
	} else if err := s.validateTimeLocks(txn); err != nil {
		return err
	} else if err := s.outputsEqualInputs(txn); err != nil {
		return err
	} else if err := s.validateSpendPolicies(txn); err != nil {
		return err
	}
	return nil
}

func (s State) validateEphemeralOutputs(txns []sunyata.Transaction) error {
	// skip this check if no ephemeral outputs are present
	for _, txn := range txns {
		for _, in := range txn.Inputs {
			if in.Parent.LeafIndex == sunyata.EphemeralLeafIndex {
				goto validate
			}
		}
	}
	return nil

validate:
	available := make(map[sunyata.ElementID]sunyata.Output)
	for txnIndex, txn := range txns {
		txid := txn.ID()
		var index uint64
		nextID := func() sunyata.ElementID {
			id := sunyata.ElementID{
				Source: sunyata.Hash256(txid),
				Index:  index,
			}
			index++
			return id
		}

		for _, in := range txn.Inputs {
			if in.Parent.LeafIndex == sunyata.EphemeralLeafIndex {
				if out, ok := available[in.Parent.ID]; !ok {
					return fmt.Errorf("transaction set is invalid: transaction %v claims non-existent ephemeral output %v", txnIndex, in.Parent.ID)
				} else if in.Parent.Value != out.Value {
					return fmt.Errorf("transaction set is invalid: transaction %v claims wrong value for ephemeral output %v", txnIndex, in.Parent.ID)
				} else if in.Parent.Address != out.Address {
					return fmt.Errorf("transaction set is invalid: transaction %v claims wrong address for ephemeral output %v", txnIndex, in.Parent.ID)
				}
				delete(available, in.Parent.ID)
			}
		}
		for _, out := range txn.Outputs {
			available[nextID()] = out
		}
	}
	return nil
}

func (s State) noDoubleSpends(txns []sunyata.Transaction) error {
	spent := make(map[sunyata.ElementID]int)
	for i, txn := range txns {
		for _, in := range txn.Inputs {
			if prev, ok := spent[in.Parent.ID]; ok {
				return fmt.Errorf("transaction set is invalid: transaction %v double-spends output %v (previously spent in transaction %v)", i, in.Parent.ID, prev)
			}
			spent[in.Parent.ID] = i
		}
	}
	return nil
}

// ValidateTransactionSet validates txns within the context of s.
func (s State) ValidateTransactionSet(txns []sunyata.Transaction) error {
	if s.BlockWeight(txns) > s.MaxBlockWeight() {
		return ErrOverweight
	} else if err := s.validateEphemeralOutputs(txns); err != nil {
		return err
	} else if err := s.noDoubleSpends(txns); err != nil {
		return err
	}
	for i, txn := range txns {
		if err := s.ValidateTransaction(txn); err != nil {
			return fmt.Errorf("transaction %v is invalid: %w", i, err)
		}
	}
	return nil
}

// ValidateBlock validates b in the context of s.
//
// This function does not check whether the header's timestamp is too far in the
// future. This check should be performed at the time the block is received,
// e.g. in p2p networking code; see MaxFutureTimestamp.
func (s State) ValidateBlock(b sunyata.Block) error {
	h := b.Header
	if err := s.validateHeader(h); err != nil {
		return err
	} else if s.Commitment(h.MinerAddress, b.Transactions) != h.Commitment {
		return errors.New("commitment hash does not match header")
	} else if err := s.ValidateTransactionSet(b.Transactions); err != nil {
		return err
	}
	return nil
}

// MaxFutureTimestamp returns the maximum allowed timestamp for a block.
func (s State) MaxFutureTimestamp(currentTime time.Time) time.Time {
	return currentTime.Add(2 * time.Hour)
}
