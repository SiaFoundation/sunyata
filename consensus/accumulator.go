package consensus

import (
	"encoding/binary"
	"math/bits"
	"sort"

	"go.sia.tech/sunyata"
)

// from RFC 6961
const leafHashPrefix = 0x00
const nodeHashPrefix = 0x01

// mergeHeight returns the height at which the proof paths of x and y merge.
func mergeHeight(x, y uint64) int { return bits.Len64(x ^ y) }

// clearBits clears the n least significant bits of x.
func clearBits(x uint64, n int) uint64 { return x &^ (1<<n - 1) }

// trailingOnes returns the number of trailing one bits in x.
func trailingOnes(x uint64) int { return bits.TrailingZeros64(x + 1) }

func merkleNodeHash(left, right sunyata.Hash256) sunyata.Hash256 {
	buf := make([]byte, 65)
	buf[0] = nodeHashPrefix
	copy(buf[1:], left[:])
	copy(buf[33:], right[:])
	return sunyata.HashBytes(buf)
}

func outputLeafHash(o sunyata.Output, spent bool) sunyata.Hash256 {
	// TODO: which fields should be hashed?
	buf := make([]byte, 1+32+8+8+1)
	n := 0
	buf[n] = leafHashPrefix
	n++
	n += copy(buf[n:], o.ID.TransactionID[:])
	binary.LittleEndian.PutUint64(buf[n:], o.ID.BeneficiaryIndex)
	n += 8
	binary.LittleEndian.PutUint64(buf[n:], o.LeafIndex)
	n += 8
	if spent {
		buf[n] = 0x01
	} else {
		buf[n] = 0x00
	}
	return sunyata.HashBytes(buf)
}

func outputProofRoot(o sunyata.Output, spent bool) sunyata.Hash256 {
	root := outputLeafHash(o, spent)
	for i, h := range o.MerkleProof {
		if o.LeafIndex&(1<<i) == 0 {
			root = merkleNodeHash(root, h)
		} else {
			root = merkleNodeHash(h, root)
		}
	}
	return root
}

func outputsByTree(txns []sunyata.Transaction) [64][]sunyata.Output {
	var trees [64][]sunyata.Output
	for _, txn := range txns {
		for i, in := range txn.Inputs {
			if in.Parent.LeafIndex != sunyata.EphemeralLeafIndex {
				trees[len(in.Parent.MerkleProof)] = append(trees[len(in.Parent.MerkleProof)], txn.Inputs[i].Parent)
			}
		}
	}
	for _, outputs := range trees {
		if len(outputs) > 1 {
			sort.Slice(outputs, func(i, j int) bool {
				return outputs[i].LeafIndex < outputs[j].LeafIndex
			})
		}
	}
	return trees
}

func splitOutputs(outputs []sunyata.Output, mid uint64) (left, right []sunyata.Output) {
	split := sort.Search(len(outputs), func(i int) bool { return outputs[i].LeafIndex >= mid })
	return outputs[:split], outputs[split:]
}

// A StateAccumulator tracks the state of all outputs.
type StateAccumulator struct {
	// A set of perfect Merkle trees, containing at most one tree at each
	// height. Only the root of each tree is stored.
	Trees     [64]sunyata.Hash256
	NumLeaves uint64
}

// HasTreeAtHeight returns true if the StateAccumulator contains a tree root at
// the specified height.
func (sa *StateAccumulator) HasTreeAtHeight(height int) bool {
	return sa.NumLeaves&(1<<height) != 0
}

// ContainsUnspentOutput returns true if o is a valid unspent output in the
// accumulator.
func (sa *StateAccumulator) ContainsUnspentOutput(o sunyata.Output) bool {
	root := outputProofRoot(o, false)
	start, end := bits.TrailingZeros64(sa.NumLeaves), bits.Len64(sa.NumLeaves)
	for i := start; i < end; i++ {
		if sa.HasTreeAtHeight(i) && sa.Trees[i] == root {
			return true
		}
	}
	return false
}

// addNewOutputs adds the supplied outputs to the accumulator, returning the
// new nodes that extend each existing tree. It also fills in the Merkle proof
// of each output.
func (sa *StateAccumulator) addNewOutputs(outputs []sunyata.Output, wasSpent func(sunyata.OutputID) bool) [64][]sunyata.Hash256 {
	initialLeaves := sa.NumLeaves
	var treeGrowth [64][]sunyata.Hash256
	for i := range outputs {
		outputs[i].LeafIndex = sa.NumLeaves
		outputs[i].MerkleProof = make([]sunyata.Hash256, 0, trailingOnes(sa.NumLeaves))

		// Walk "up" the Forest, merging trees of the same height, but before
		// merging two trees, append each of their roots to the proofs under the
		// opposite tree.
		h := outputLeafHash(outputs[i], wasSpent(outputs[i].ID))
		for height := range &sa.Trees {
			if !sa.HasTreeAtHeight(height) {
				// no tree at this height; insert the new tree
				sa.Trees[height] = h
				sa.NumLeaves++
				break
			}
			// Another tree exists at this height. We need to append the root of
			// the "old" (left-hand) tree to the proofs under the "new"
			// (right-hand) tree, and vice versa. To do this, we seek backwards
			// through the proofs, starting from i, such that the first 2^height
			// proofs we encounter will be under to the right-hand tree, and the
			// next 2^height proofs will be under to the left-hand tree.
			oldRoot := sa.Trees[height]
			startOfNewTree := i - 1<<height
			startOfOldTree := i - 1<<(height+1)
			j := i
			for ; j > startOfNewTree && j >= 0; j-- {
				outputs[j].MerkleProof = append(outputs[j].MerkleProof, oldRoot)
			}
			for ; j > startOfOldTree && j >= 0; j-- {
				outputs[j].MerkleProof = append(outputs[j].MerkleProof, h)
			}
			// Record the left- and right-hand roots in treeGrowth, where
			// applicable.
			curTreeIndex := (sa.NumLeaves + 1) - 1<<height
			prevTreeIndex := (sa.NumLeaves + 1) - 1<<(height+1)
			for bit := range treeGrowth {
				if initialLeaves&(1<<bit) == 0 {
					continue
				}
				treeStartIndex := clearBits(initialLeaves, bit+1)
				if treeStartIndex >= curTreeIndex {
					treeGrowth[bit] = append(treeGrowth[bit], oldRoot)
				} else if treeStartIndex >= prevTreeIndex {
					treeGrowth[bit] = append(treeGrowth[bit], h)
				}
			}
			// Merge with the existing tree at this height. Since we're always
			// adding leaves on the right-hand side of the tree, the existing
			// root is always the left-hand sibling.
			h = merkleNodeHash(oldRoot, h)
		}
	}
	return treeGrowth
}

// markInputsSpent recomputes the tree roots of the accumulator when all of the
// parent Outputs in the supplied txns are "marked as spent," i.e. when their
// leaf hashes are recomputed with the spent flag set. The Merkle proofs of each
// parent Output are also updated, such that outputProofRoot(true) returns a
// root within the new accumulator for all Outputs. The Outputs are grouped by
// tree and returned for later use.
func (sa *StateAccumulator) markInputsSpent(txns []sunyata.Transaction, proof []sunyata.Hash256) [64][]sunyata.Output {
	// Define a helper function to recursively recompute subtree roots. As a
	// side effect, we also fill out the individual Merkle proofs for each
	// output.
	var recompute func(i, j uint64, outputs []sunyata.Output) sunyata.Hash256
	recompute = func(i, j uint64, outputs []sunyata.Output) sunyata.Hash256 {
		height := bits.TrailingZeros64(j - i) // equivalent to log2(j-i), as j-i is always a power of two
		if len(outputs) == 0 {
			// no outputs in this subtree; must have a proof root
			h := proof[0]
			proof = proof[1:]
			return h
		} else if height == 0 {
			return outputLeafHash(outputs[0], true)
		}
		mid := (i + j) / 2
		left, right := splitOutputs(outputs, mid)
		leftRoot := recompute(i, mid, left)
		rightRoot := recompute(mid, j, right)
		for i := range right {
			right[i].MerkleProof[height-1] = leftRoot
		}
		for i := range left {
			left[i].MerkleProof[height-1] = rightRoot
		}
		return merkleNodeHash(leftRoot, rightRoot)
	}

	// Group the outputs by their tree (and sort them by leaf index) and use
	// them to recompute the root of each tree.
	groups := outputsByTree(txns)
	for height, outputs := range &groups {
		if len(outputs) == 0 {
			continue
		}

		// Determine the range of leaf indices that comprise this tree. We can
		// compute this efficiently by zeroing the least-significant bits of
		// NumLeaves. (Zeroing these bits is equivalent to subtracting the
		// number of leaves in all trees smaller than this one.)
		start := clearBits(sa.NumLeaves, height+1)
		end := start + 1<<height

		sa.Trees[height] = recompute(start, end, outputs)
	}
	return groups
}

// ComputeMultiproof computes a single Merkle proof for all inputs in txns.
func ComputeMultiproof(txns []sunyata.Transaction) (proof []sunyata.Hash256) {
	var visit func(i, j uint64, outputs []sunyata.Output)
	visit = func(i, j uint64, outputs []sunyata.Output) {
		height := bits.TrailingZeros64(j - i)
		if height == 0 {
			return // fully consumed
		}
		mid := (i + j) / 2
		left, right := splitOutputs(outputs, mid)
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

	for height, outputs := range outputsByTree(txns) {
		if len(outputs) == 0 {
			continue
		}
		start := clearBits(outputs[0].LeafIndex, height+1)
		end := start + 1<<height
		visit(start, end, outputs)
	}
	return
}

// ExpandMultiproof restores all of the input proofs in txns using the supplied
// multiproof. The len of each input proof must be the correct size.
func ExpandMultiproof(txns []sunyata.Transaction, proof []sunyata.Hash256) bool {
	var expand func(i, j uint64, outputs []sunyata.Output) sunyata.Hash256
	expand = func(i, j uint64, outputs []sunyata.Output) sunyata.Hash256 {
		height := bits.TrailingZeros64(j - i)
		if len(outputs) == 0 {
			// no outputs in this subtree; must have a proof root
			h := proof[0]
			proof = proof[1:]
			return h
		} else if height == 0 {
			return outputLeafHash(outputs[0], false)
		}
		mid := (i + j) / 2
		left, right := splitOutputs(outputs, mid)
		leftRoot := expand(i, mid, left)
		rightRoot := expand(mid, j, right)
		for i := range right {
			right[i].MerkleProof[height-1] = leftRoot
		}
		for i := range left {
			left[i].MerkleProof[height-1] = rightRoot
		}
		return merkleNodeHash(leftRoot, rightRoot)
	}

	// helper for computing valid proof size
	var proofSize func(i, j uint64, outputs []sunyata.Output) int
	proofSize = func(i, j uint64, outputs []sunyata.Output) int {
		height := bits.TrailingZeros64(j - i)
		if len(outputs) == 0 {
			return 1
		} else if height == 0 {
			return 0
		}
		mid := (i + j) / 2
		left, right := splitOutputs(outputs, mid)
		return proofSize(i, mid, left) + proofSize(mid, j, right)
	}

	for height, outputs := range outputsByTree(txns) {
		if len(outputs) == 0 {
			continue
		}
		start := clearBits(outputs[0].LeafIndex, height+1)
		end := start + 1<<height
		if len(proof) < proofSize(start, end, outputs) {
			return false
		}
		expand(start, end, outputs)
	}

	return len(proof) == 0
}

func (sa *StateAccumulator) validProof(txns []sunyata.Transaction, proof []sunyata.Hash256) bool {
	var root func(i, j uint64, outputs []sunyata.Output) sunyata.Hash256
	root = func(i, j uint64, outputs []sunyata.Output) sunyata.Hash256 {
		height := bits.TrailingZeros64(j - i)
		if len(outputs) == 0 {
			// we should have a proof root for all empty subtrees; if not,
			// return a hash that is guaranteed to result in the wrong root
			if len(proof) == 0 {
				return sunyata.Hash256{}
			}
			h := proof[0]
			proof = proof[1:]
			return h
		} else if height == 0 {
			return outputLeafHash(outputs[0], false)
		}
		mid := (i + j) / 2
		left, right := splitOutputs(outputs, mid)
		return merkleNodeHash(root(i, mid, left), root(mid, j, right))
	}

	for height, outputs := range outputsByTree(txns) {
		if len(outputs) == 0 {
			continue
		}
		start := clearBits(outputs[0].LeafIndex, height+1)
		end := start + 1<<height
		if root(start, end, outputs) != sa.Trees[height] {
			return false
		}
	}
	// there should be no excess proof roots
	return len(proof) == 0
}

func merkleHistoryLeafHash(index sunyata.ChainIndex) sunyata.Hash256 {
	buf := make([]byte, 1+8+32)
	buf[0] = leafHashPrefix
	binary.LittleEndian.PutUint64(buf[1:], index.Height)
	copy(buf[9:], index.ID[:])
	return sunyata.HashBytes(buf)
}

// A HistoryAccumulator is a Merkle tree of sequential ChainIndexes.
type HistoryAccumulator struct {
	// same design as StateAccumulator
	Trees     [64]sunyata.Hash256
	NumLeaves uint64
}

// HasTreeAtHeight returns true if the HistoryAccumulator contains a tree root at
// the specified height.
func (ha *HistoryAccumulator) HasTreeAtHeight(height int) bool {
	return ha.NumLeaves&(1<<height) != 0
}

// AppendLeaf appends an index to the accumulator.
func (ha *HistoryAccumulator) AppendLeaf(index sunyata.ChainIndex) {
	h := merkleHistoryLeafHash(index)
	i := 0
	for ; ha.HasTreeAtHeight(i); i++ {
		h = merkleNodeHash(ha.Trees[i], h)
	}
	ha.Trees[i] = h
	ha.NumLeaves++
}
