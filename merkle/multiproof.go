package merkle

import (
	"errors"
	"fmt"
	"math/bits"
	"sort"

	"go.sia.tech/sunyata"
)

func splitLeaves(ls []ElementLeaf, mid uint64) (left, right []ElementLeaf) {
	split := sort.Search(len(ls), func(i int) bool { return ls[i].LeafIndex >= mid })
	return ls[:split], ls[split:]
}

func leavesByTree(txns []sunyata.Transaction) [64][]ElementLeaf {
	var trees [64][]ElementLeaf
	addLeaf := func(l ElementLeaf) {
		trees[len(l.MerkleProof)] = append(trees[len(l.MerkleProof)], l)
	}
	for _, txn := range txns {
		for _, in := range txn.Inputs {
			if in.Parent.LeafIndex != sunyata.EphemeralLeafIndex {
				addLeaf(OutputLeaf(in.Parent, false))
			}
		}
	}
	for _, leaves := range trees {
		sort.Slice(leaves, func(i, j int) bool {
			return leaves[i].LeafIndex < leaves[j].LeafIndex
		})
	}
	return trees
}

// MultiproofSize computes the size of a multiproof for the given transactions.
func MultiproofSize(txns []sunyata.Transaction) int {
	var proofSize func(i, j uint64, leaves []ElementLeaf) int
	proofSize = func(i, j uint64, leaves []ElementLeaf) int {
		height := bits.TrailingZeros64(j - i)
		if len(leaves) == 0 {
			return 1
		} else if height == 0 {
			return 0
		}
		mid := (i + j) / 2
		left, right := splitLeaves(leaves, mid)
		return proofSize(i, mid, left) + proofSize(mid, j, right)
	}

	size := 0
	for height, leaves := range leavesByTree(txns) {
		if len(leaves) == 0 {
			continue
		}
		start := clearBits(leaves[0].LeafIndex, height+1)
		end := start + 1<<height
		size += proofSize(start, end, leaves)
	}
	return size
}

// ComputeMultiproof computes a single Merkle proof for all inputs in txns.
func ComputeMultiproof(txns []sunyata.Transaction) (proof []sunyata.Hash256) {
	var visit func(i, j uint64, leaves []ElementLeaf)
	visit = func(i, j uint64, leaves []ElementLeaf) {
		height := bits.TrailingZeros64(j - i)
		if height == 0 {
			return // fully consumed
		}
		mid := (i + j) / 2
		left, right := splitLeaves(leaves, mid)
		if len(left) == 0 {
			proof = append(proof, right[0].MerkleProof[height-1])
		} else {
			visit(i, mid, left)
		}
		if len(right) == 0 {
			proof = append(proof, left[0].MerkleProof[height-1])
		} else {
			visit(mid, j, right)
		}
	}

	for height, leaves := range leavesByTree(txns) {
		if len(leaves) == 0 {
			continue
		}
		start := clearBits(leaves[0].LeafIndex, height+1)
		end := start + 1<<height
		visit(start, end, leaves)
	}
	return
}

// ExpandMultiproof restores all of the proofs with txns using the supplied
// multiproof, which must be valid. The len of each proof must be the correct
// size.
func ExpandMultiproof(txns []sunyata.Transaction, proof []sunyata.Hash256) {
	var expand func(i, j uint64, leaves []ElementLeaf) sunyata.Hash256
	expand = func(i, j uint64, leaves []ElementLeaf) sunyata.Hash256 {
		height := bits.TrailingZeros64(j - i)
		if len(leaves) == 0 {
			// no leaves in this subtree; must have a proof root
			h := proof[0]
			proof = proof[1:]
			return h
		} else if height == 0 {
			return leaves[0].Hash()
		}
		mid := (i + j) / 2
		left, right := splitLeaves(leaves, mid)
		leftRoot := expand(i, mid, left)
		rightRoot := expand(mid, j, right)
		for i := range right {
			right[i].MerkleProof[height-1] = leftRoot
		}
		for i := range left {
			left[i].MerkleProof[height-1] = rightRoot
		}
		return NodeHash(leftRoot, rightRoot)
	}

	for height, leaves := range leavesByTree(txns) {
		if len(leaves) == 0 {
			continue
		}
		start := clearBits(leaves[0].LeafIndex, height+1)
		end := start + 1<<height
		expand(start, end, leaves)
	}
}

// A CompressedBlock encodes a block in compressed form by merging its
// individual Merkle proofs into a single multiproof.
type CompressedBlock sunyata.Block

// EncodeTo implements sunyata.EncoderTo.
func (b CompressedBlock) EncodeTo(e *sunyata.Encoder) {
	const version = 1
	e.WriteUint8(version)

	b.Header.EncodeTo(e)
	e.WritePrefix(len(b.Transactions))
	for _, txn := range b.Transactions {
		(compressedTransaction)(txn).EncodeTo(e)
	}
	for _, p := range ComputeMultiproof(b.Transactions) {
		p.EncodeTo(e)
	}
}

// DecodeFrom implements sunyata.DecoderFrom.
func (b *CompressedBlock) DecodeFrom(d *sunyata.Decoder) {
	if version := d.ReadUint8(); version != 1 {
		d.SetErr(fmt.Errorf("unsupported block version (%v)", version))
		return
	}

	b.Header.DecodeFrom(d)
	b.Transactions = make([]sunyata.Transaction, d.ReadPrefix())
	for i := range b.Transactions {
		(*compressedTransaction)(&b.Transactions[i]).DecodeFrom(d)
	}
	// MultiproofSize will panic on invalid inputs, so return early if we've
	// already encountered an error
	if d.Err() != nil {
		return
	}
	proof := make([]sunyata.Hash256, MultiproofSize(b.Transactions))
	for i := range proof {
		proof[i].DecodeFrom(d)
	}
	ExpandMultiproof(b.Transactions, proof)
}

// helper sunyata for compressed encoding

type compressedStateElement sunyata.StateElement

func (se compressedStateElement) EncodeTo(e *sunyata.Encoder) {
	se.ID.EncodeTo(e)
	e.WriteUint64(se.LeafIndex)
	e.WritePrefix(len(se.MerkleProof)) // omit proof data
}

func (se *compressedStateElement) DecodeFrom(d *sunyata.Decoder) {
	se.ID.DecodeFrom(d)
	se.LeafIndex = d.ReadUint64()
	se.MerkleProof = make([]sunyata.Hash256, d.ReadPrefix()) // omit proof data
	if len(se.MerkleProof) >= 64 {
		d.SetErr(errors.New("impossibly-large MerkleProof"))
	}
}

type compressedOutputElement sunyata.OutputElement

func (sce compressedOutputElement) EncodeTo(e *sunyata.Encoder) {
	(compressedStateElement)(sce.StateElement).EncodeTo(e)
	sce.Output.EncodeTo(e)
	e.WriteUint64(sce.MaturityHeight)
}

func (sce *compressedOutputElement) DecodeFrom(d *sunyata.Decoder) {
	(*compressedStateElement)(&sce.StateElement).DecodeFrom(d)
	sce.Output.DecodeFrom(d)
	sce.MaturityHeight = d.ReadUint64()
}

type compressedInput sunyata.Input

func (in compressedInput) EncodeTo(e *sunyata.Encoder) {
	(compressedOutputElement)(in.Parent).EncodeTo(e)
	in.PublicKey.EncodeTo(e)
	in.Signature.EncodeTo(e)
}

func (in *compressedInput) DecodeFrom(d *sunyata.Decoder) {
	(*compressedOutputElement)(&in.Parent).DecodeFrom(d)
	in.PublicKey.DecodeFrom(d)
	in.Signature.DecodeFrom(d)
}

type compressedTransaction sunyata.Transaction

func (txn compressedTransaction) EncodeTo(e *sunyata.Encoder) {
	const version = 1
	e.WriteUint8(version)

	e.WritePrefix(len(txn.Inputs))
	for _, in := range txn.Inputs {
		(compressedInput)(in).EncodeTo(e)
	}
	e.WritePrefix(len(txn.Outputs))
	for _, out := range txn.Outputs {
		out.EncodeTo(e)
	}
	txn.MinerFee.EncodeTo(e)
}

func (txn *compressedTransaction) DecodeFrom(d *sunyata.Decoder) {
	if version := d.ReadUint8(); version != 1 {
		d.SetErr(fmt.Errorf("unsupported transaction version (%v)", version))
		return
	}

	txn.Inputs = make([]sunyata.Input, d.ReadPrefix())
	for i := range txn.Inputs {
		(*compressedInput)(&txn.Inputs[i]).DecodeFrom(d)
	}
	txn.Outputs = make([]sunyata.Output, d.ReadPrefix())
	for i := range txn.Outputs {
		txn.Outputs[i].DecodeFrom(d)
	}
	txn.MinerFee.DecodeFrom(d)
}
