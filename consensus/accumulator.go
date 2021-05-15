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

func outputLeafHash(o *sunyata.Output, spent bool) sunyata.Hash256 {
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

func outputProofRoot(o *sunyata.Output, spent bool) sunyata.Hash256 {
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

func (sa *StateAccumulator) containsOutput(o *sunyata.Output, spent bool) bool {
	root := outputProofRoot(o, spent)
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
		h := outputLeafHash(&outputs[i], wasSpent(outputs[i].ID))
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

// markInputsSpent recomputes the tree roots in the supplied forest when all of
// the parent Outputs in the supplied txns are "marked as spent," i.e. when
// their leaf hashes are recomputed with the spent flag set. The Merkle proofs
// of each parent Output are also updated, such that outputProofRoot(true)
// returns a root within the new forest for all Outputs. The Outputs are grouped
// by tree and returned for later use.
func (sa *StateAccumulator) markInputsSpent(txns []sunyata.Transaction) [64][]sunyata.Output {
	// Begin by defining a helper function that recursively recomputes subtree
	// roots. The inputs are the leaf indices bounding the subtree and the spent
	// outputs within that subtree. The algorithm is as follows:
	//
	// First, the base case: if the subtree only contains one spent output, then
	// we can recompute its root using that output's leaf hash and pre-existing
	// proof hashes. The only complication here is that we need to trim the set
	// of proof hashes, such that we recompute just the subtree in question
	// rather than the top-level tree root.
	//
	// Second, the recursive case: if the subtree contains multiple outputs,
	// then we need to split, recurse and join. We split our subtree into a left
	// tree and a right tree, grouping the outputs according to which tree they
	// fall under. Then we recurse on each tree to calculate its root, and join
	// the left and right roots to produce the root of the subtree. Here, the
	// complication is that one of the trees may not have any outputs under it!
	// How, then, can we compute its root?
	//
	// The answer is that its root is already present in the Merkle proofs of
	// the outputs under the other tree. Since none of the leaves under the
	// "empty" tree have changed, its root is unchanged also, and this root must
	// be present in the proofs under the other tree. So we augment our
	// recursive case to use these roots directly where applicable.
	//
	// Finally, recall that we have a secondary goal here: rewriting the
	// existing Merkle proofs such that they target the new root. Fortunately,
	// this is easy to accommodate. In our recursive case, after we have
	// recalculated the two tree roots, we simply copy the left-side root into
	// the right-side proofs, and vice versa.
	var recompute func(i, j uint64, outputs []sunyata.Output) sunyata.Hash256
	recompute = func(i, j uint64, outputs []sunyata.Output) sunyata.Hash256 {
		// Define a helper function to split outputs according to a midpoint.
		splitOutputs := func(os []sunyata.Output, mid uint64) (left, right []sunyata.Output) {
			split := sort.Search(len(os), func(i int) bool { return os[i].LeafIndex >= mid })
			return os[:split], os[split:]
		}

		height := bits.TrailingZeros64(j - i) // equivalent to log2(j-i), as j-i is always a power of two
		if len(outputs) == 1 {
			// Base case: there's only one spent output within this subtree.
			// Calculate the new subtree root by hashing the spent leaf with
			// the output's existing proof hashes, stopping when we reach
			// the height of the subtree in question.
			o := outputs[0]
			o.MerkleProof = o.MerkleProof[:height]
			return outputProofRoot(&o, true)
		} else if height == 0 {
			panic("exactly one output should be present in a height-0 subtree")
		}

		// Recursive case: split, recurse, and join.
		mid := (i + j) / 2
		leftOutputs, rightOutputs := splitOutputs(outputs, mid)
		var leftRoot, rightRoot sunyata.Hash256
		if len(leftOutputs) == 0 {
			// None of the leaves under the left-side tree changed, which
			// means we can find its root within the proofs of the
			// rightOutputs. Any proof will work -- the root will be the
			// same in all of them -- but the 0th is guaranteed to exist.
			leftRoot = rightOutputs[0].MerkleProof[height-1]
		} else {
			// At least one leaf under the left-side changed, so we need to
			// recurse to recompute the root. Once we have it, update the
			// proofs of the rightOutputs to use the new root.
			leftRoot = recompute(i, mid, leftOutputs)
			for i := range rightOutputs {
				rightOutputs[i].MerkleProof[height-1] = leftRoot
			}
		}
		if len(rightOutputs) == 0 {
			rightRoot = leftOutputs[0].MerkleProof[height-1]
		} else {
			rightRoot = recompute(mid, j, rightOutputs)
			for i := range leftOutputs {
				leftOutputs[i].MerkleProof[height-1] = rightRoot
			}
		}
		// Join the recomputed roots, yielding the root of the full subtree.
		return merkleNodeHash(leftRoot, rightRoot)
	}

	// Group inputs by which tree in the forest they belong to.
	//
	// TODO: this allocates. The caller should probably provide this memory.
	var groups [64][]sunyata.Output
	for _, txn := range txns {
		for _, in := range txn.Inputs {
			if in.Parent.LeafIndex == sunyata.EphemeralLeafIndex {
				continue // skip ephemeral outputs
			}
			// The recompute function modifies the proofs of outputs directly, so to avoid
			// side effects, we make a copy here.
			in.Parent.MerkleProof = append([]sunyata.Hash256(nil), in.Parent.MerkleProof...)
			groups[len(in.Parent.MerkleProof)] = append(groups[len(in.Parent.MerkleProof)], in.Parent)
			// sanity check
			if !sa.HasTreeAtHeight(len(in.Parent.MerkleProof)) {
				panic("forest missing tree for output proof")
			}
		}
	}
	// Recompute each tree.
	for height, g := range &groups {
		if len(g) == 0 {
			continue
		}

		// Sort the outputs by their leaf index.
		sort.Slice(g, func(i, j int) bool {
			return g[i].LeafIndex < g[j].LeafIndex
		})

		// Determine the range of leaf indices that comprise this tree. We can
		// compute this efficiently by zeroing the least-significant bits of
		// NumLeaves. (Zeroing these bits is equivalent to subtracting the
		// number of leaves in all trees smaller than this one.)
		start := clearBits(sa.NumLeaves, height+1)
		end := clearBits(sa.NumLeaves, height)

		// Recursively recompute the root of the tree.
		sa.Trees[height] = recompute(start, end, g)
	}
	return groups
}
