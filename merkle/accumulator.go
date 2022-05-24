package merkle

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/bits"
	"sort"
	"sync"

	"go.sia.tech/sunyata"
)

// Pool for reducing heap allocations when hashing. This is only necessary
// because blake2b.New256 returns a hash.Hash interface, which prevents the
// compiler from doing escape analysis. Can be removed if we switch to an
// implementation whose constructor returns a concrete type.
var hasherPool = &sync.Pool{New: func() interface{} { return sunyata.NewHasher() }}

// from RFC 6961
const leafHashPrefix = 0x00
const nodeHashPrefix = 0x01

// mergeHeight returns the height at which the proof paths of x and y merge.
func mergeHeight(x, y uint64) int { return bits.Len64(x ^ y) }

// clearBits clears the n least significant bits of x.
func clearBits(x uint64, n int) uint64 { return x &^ (1<<n - 1) }

// trailingOnes returns the number of trailing one bits in x.
func trailingOnes(x uint64) int { return bits.TrailingZeros64(x + 1) }

// NodeHash computes the Merkle root of a pair of node hashes.
func NodeHash(left, right sunyata.Hash256) sunyata.Hash256 {
	buf := make([]byte, 1+32+32)
	buf[0] = nodeHashPrefix
	copy(buf[1:], left[:])
	copy(buf[33:], right[:])
	return sunyata.HashBytes(buf)
}

// ProofRoot returns the Merkle root derived from the supplied leaf hash and
// Merkle proof.
func ProofRoot(leafHash sunyata.Hash256, leafIndex uint64, proof []sunyata.Hash256) sunyata.Hash256 {
	root := leafHash
	for i, h := range proof {
		if leafIndex&(1<<i) == 0 {
			root = NodeHash(root, h)
		} else {
			root = NodeHash(h, root)
		}
	}
	return root
}

// An ElementLeaf represents a leaf in the ElementAccumulator Merkle tree.
type ElementLeaf struct {
	sunyata.StateElement
	ElementHash sunyata.Hash256
	Spent       bool
}

// Hash returns the leaf's hash, for direct use in the Merkle tree.
func (l ElementLeaf) Hash() sunyata.Hash256 {
	buf := make([]byte, 1+32+8+1)
	buf[0] = leafHashPrefix
	copy(buf[1:], l.ElementHash[:])
	binary.LittleEndian.PutUint64(buf[33:], l.LeafIndex)
	if l.Spent {
		buf[41] = 1
	}
	return sunyata.HashBytes(buf)
}

// ProofRoot returns the root obtained from the leaf and its proof..
func (l ElementLeaf) ProofRoot() sunyata.Hash256 {
	return ProofRoot(l.Hash(), l.LeafIndex, l.MerkleProof)
}

// OutputLeaf returns the ElementLeaf for an OutputElement.
func OutputLeaf(e sunyata.OutputElement, spent bool) ElementLeaf {
	h := hasherPool.Get().(*sunyata.Hasher)
	defer hasherPool.Put(h)
	h.Reset()
	h.E.WriteString("sunyata/leaf/output")
	e.ID.EncodeTo(h.E)
	e.Output.EncodeTo(h.E)
	h.E.WriteUint64(e.MaturityHeight)
	return ElementLeaf{
		StateElement: e.StateElement,
		ElementHash:  h.Sum(),
		Spent:        spent,
	}
}

// An Accumulator tracks the state of an unbounded number of leaves without
// storing the leaves themselves.
type Accumulator struct {
	// A set of perfect Merkle trees, containing at most one tree at each
	// height. Only the root of each tree is stored.
	Trees     [64]sunyata.Hash256
	NumLeaves uint64
}

// hasTreeAtHeight returns true if the Accumulator contains a tree root at the
// specified height.
func (acc *Accumulator) hasTreeAtHeight(height int) bool {
	return acc.NumLeaves&(1<<height) != 0
}

// EncodeTo implements sunyata.EncoderTo.
func (acc Accumulator) EncodeTo(e *sunyata.Encoder) {
	e.WriteUint64(acc.NumLeaves)
	for i, root := range acc.Trees {
		if acc.hasTreeAtHeight(i) {
			root.EncodeTo(e)
		}
	}
}

// DecodeFrom implements sunyata.DecoderFrom.
func (acc *Accumulator) DecodeFrom(d *sunyata.Decoder) {
	acc.NumLeaves = d.ReadUint64()
	for i := range acc.Trees {
		if acc.hasTreeAtHeight(i) {
			acc.Trees[i].DecodeFrom(d)
		}
	}
}

// MarshalJSON implements json.Marshaler.
func (acc Accumulator) MarshalJSON() ([]byte, error) {
	v := struct {
		NumLeaves uint64            `json:"numLeaves"`
		Trees     []sunyata.Hash256 `json:"trees"`
	}{acc.NumLeaves, []sunyata.Hash256{}}
	for i, root := range acc.Trees {
		if acc.hasTreeAtHeight(i) {
			v.Trees = append(v.Trees, root)
		}
	}
	return json.Marshal(v)
}

// UnmarshalJSON implements json.Unmarshaler.
func (acc *Accumulator) UnmarshalJSON(b []byte) error {
	var v struct {
		NumLeaves uint64
		Trees     []sunyata.Hash256
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	} else if len(v.Trees) != bits.OnesCount64(v.NumLeaves) {
		return errors.New("invalid accumulator encoding")
	}
	acc.NumLeaves = v.NumLeaves
	for i := range acc.Trees {
		if acc.hasTreeAtHeight(i) {
			acc.Trees[i] = v.Trees[0]
			v.Trees = v.Trees[1:]
		}
	}
	return nil
}

// An ElementAccumulator tracks the state of an unbounded number of elements
// without storing the elements themselves.
type ElementAccumulator struct {
	Accumulator
}

func (acc *ElementAccumulator) containsLeaf(l ElementLeaf) bool {
	return acc.hasTreeAtHeight(len(l.MerkleProof)) && acc.Trees[len(l.MerkleProof)] == l.ProofRoot()
}

// ContainsUnspentOutputElement returns true if the accumulator contains sce as an
// unspent output.
func (acc *ElementAccumulator) ContainsUnspentOutputElement(sce sunyata.OutputElement) bool {
	return acc.containsLeaf(OutputLeaf(sce, false))
}

// ContainsSpentOutputElement returns true if the accumulator contains sce as a
// spent output.
func (acc *ElementAccumulator) ContainsSpentOutputElement(sce sunyata.OutputElement) bool {
	return acc.containsLeaf(OutputLeaf(sce, true))
}

// addLeaves adds the supplied leaves to the accumulator, filling in their
// Merkle proofs and returning the new node hashes that extend each existing
// tree.
func (acc *ElementAccumulator) addLeaves(leaves []ElementLeaf) [64][]sunyata.Hash256 {
	initialLeaves := acc.NumLeaves
	var treeGrowth [64][]sunyata.Hash256
	for i := range leaves {
		leaves[i].LeafIndex = acc.NumLeaves
		// TODO: preallocate this more accurately
		leaves[i].MerkleProof = make([]sunyata.Hash256, 0, trailingOnes(acc.NumLeaves))

		// Walk "up" the Forest, merging trees of the same height, but before
		// merging two trees, append each of their roots to the proofs under the
		// opposite tree.
		h := leaves[i].Hash()
		for height := range &acc.Trees {
			if !acc.hasTreeAtHeight(height) {
				// no tree at this height; insert the new tree
				acc.Trees[height] = h
				acc.NumLeaves++
				break
			}
			// Another tree exists at this height. We need to append the root of
			// the "old" (left-hand) tree to the proofs under the "new"
			// (right-hand) tree, and vice veracc. To do this, we seek backwards
			// through the proofs, starting from i, such that the first 2^height
			// proofs we encounter will be under to the right-hand tree, and the
			// next 2^height proofs will be under to the left-hand tree.
			oldRoot := acc.Trees[height]
			startOfNewTree := i - 1<<height
			startOfOldTree := i - 1<<(height+1)
			j := i
			for ; j > startOfNewTree && j >= 0; j-- {
				leaves[j].MerkleProof = append(leaves[j].MerkleProof, oldRoot)
			}
			for ; j > startOfOldTree && j >= 0; j-- {
				leaves[j].MerkleProof = append(leaves[j].MerkleProof, h)
			}
			// Record the left- and right-hand roots in treeGrowth, where
			// applicable.
			curTreeIndex := (acc.NumLeaves + 1) - 1<<height
			prevTreeIndex := (acc.NumLeaves + 1) - 1<<(height+1)
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
			h = NodeHash(oldRoot, h)
		}
	}
	return treeGrowth
}

// updateLeaves overwrites the specified leaves in the accumulator. It updates
// the Merkle proofs of each leaf, and returns the leaves (grouped by tree) for
// later use.
func (acc *ElementAccumulator) updateLeaves(leaves []ElementLeaf) [64][]ElementLeaf {
	var recompute func(i, j uint64, leaves []ElementLeaf) sunyata.Hash256
	recompute = func(i, j uint64, leaves []ElementLeaf) sunyata.Hash256 {
		height := bits.TrailingZeros64(j - i) // equivalent to log2(j-i), as j-i is always a power of two
		if len(leaves) == 1 && height == 0 {
			return leaves[0].Hash()
		}
		mid := (i + j) / 2
		left, right := splitLeaves(leaves, mid)
		var leftRoot, rightRoot sunyata.Hash256
		if len(left) == 0 {
			leftRoot = right[0].MerkleProof[height-1]
		} else {
			leftRoot = recompute(i, mid, left)
			for i := range right {
				right[i].MerkleProof[height-1] = leftRoot
			}
		}
		if len(right) == 0 {
			rightRoot = left[0].MerkleProof[height-1]
		} else {
			rightRoot = recompute(mid, j, right)
			for i := range left {
				left[i].MerkleProof[height-1] = rightRoot
			}
		}
		return NodeHash(leftRoot, rightRoot)
	}

	// Group leaves by tree, and sort them by leaf index.
	var trees [64][]ElementLeaf
	sort.Slice(leaves, func(i, j int) bool {
		if len(leaves[i].MerkleProof) != len(leaves[j].MerkleProof) {
			return len(leaves[i].MerkleProof) < len(leaves[j].MerkleProof)
		}
		return leaves[i].LeafIndex < leaves[j].LeafIndex
	})
	for len(leaves) > 0 {
		i := 0
		for i < len(leaves) && len(leaves[i].MerkleProof) == len(leaves[0].MerkleProof) {
			i++
		}
		trees[len(leaves[0].MerkleProof)] = leaves[:i]
		leaves = leaves[i:]
	}

	// Recompute the root of each tree with updated leaves, and fill in the
	// proof of each leaf.
	for height, leaves := range &trees {
		if len(leaves) == 0 {
			continue
		}
		// Determine the range of leaf indices that comprise this tree. We can
		// compute this efficiently by zeroing the least-significant bits of
		// NumLeaves. (Zeroing these bits is equivalent to subtracting the
		// number of leaves in all trees smaller than this one.)
		start := clearBits(acc.NumLeaves, height+1)
		end := start + 1<<height
		acc.Trees[height] = recompute(start, end, leaves)
	}
	return trees
}

// ApplyBlock applies the supplied leaves to the accumulator, modifying it and
// producing an update.
func (acc *ElementAccumulator) ApplyBlock(updated, added []ElementLeaf) (eau ElementApplyUpdate) {
	eau.updated = acc.updateLeaves(updated)
	eau.treeGrowth = acc.addLeaves(added)
	return eau
}

// RevertBlock produces an update from the supplied leaves. The accumulator is
// not modified.
func (acc *ElementAccumulator) RevertBlock(updated []ElementLeaf) (eru ElementRevertUpdate) {
	eru.numLeaves = acc.NumLeaves
	for _, l := range updated {
		eru.updated[len(l.MerkleProof)] = append(eru.updated[len(l.MerkleProof)], l)
	}
	return
}

func updateProof(e *sunyata.StateElement, updated *[64][]ElementLeaf) {
	// find the "closest" updated object (the one with the lowest mergeHeight)
	updatedInTree := updated[len(e.MerkleProof)]
	if len(updatedInTree) == 0 {
		return
	}
	best := updatedInTree[0]
	for _, ul := range updatedInTree[1:] {
		if mergeHeight(e.LeafIndex, ul.LeafIndex) < mergeHeight(e.LeafIndex, best.LeafIndex) {
			best = ul
		}
	}

	if best.LeafIndex == e.LeafIndex {
		// copy over the updated proof in its entirety
		copy(e.MerkleProof, best.MerkleProof)
	} else {
		// copy over the updated proof above the mergeHeight
		mh := mergeHeight(e.LeafIndex, best.LeafIndex)
		copy(e.MerkleProof[mh:], best.MerkleProof[mh:])
		// at the merge point itself, compute the updated sibling hash
		e.MerkleProof[mh-1] = ProofRoot(best.Hash(), best.LeafIndex, best.MerkleProof[:mh-1])
	}
}

// An ElementApplyUpdate reflects the changes to an ElementAccumulator resulting
// from the application of a block.
type ElementApplyUpdate struct {
	updated    [64][]ElementLeaf
	treeGrowth [64][]sunyata.Hash256
}

// UpdateElementProof updates the Merkle proof of the supplied element to
// incorporate the changes made to the accumulator. The element's proof must be
// up-to-date; if it is not, UpdateElementProof may panic.
func (eau *ElementApplyUpdate) UpdateElementProof(e *sunyata.StateElement) {
	if e.LeafIndex == sunyata.EphemeralLeafIndex {
		panic("cannot update an ephemeral element")
	}
	updateProof(e, &eau.updated)
	e.MerkleProof = append(e.MerkleProof, eau.treeGrowth[len(e.MerkleProof)]...)
}

// An ElementRevertUpdate reflects the changes to an ElementAccumulator
// resulting from the removal of a block.
type ElementRevertUpdate struct {
	updated   [64][]ElementLeaf
	numLeaves uint64
}

// UpdateElementProof updates the Merkle proof of the supplied element to
// incorporate the changes made to the accumulator. The element's proof must be
// up-to-date; if it is not, UpdateElementProof may panic.
func (eru *ElementRevertUpdate) UpdateElementProof(e *sunyata.StateElement) {
	if e.LeafIndex == sunyata.EphemeralLeafIndex {
		panic("cannot update an ephemeral element")
	} else if e.LeafIndex >= eru.numLeaves {
		panic("cannot update an element that is not present in the accumulator")
	}
	if mh := mergeHeight(eru.numLeaves, e.LeafIndex); mh <= len(e.MerkleProof) {
		e.MerkleProof = e.MerkleProof[:mh-1]
	}
	updateProof(e, &eru.updated)
}

func historyLeafHash(index sunyata.ChainIndex) sunyata.Hash256 {
	buf := make([]byte, 1+8+32)
	buf[0] = leafHashPrefix
	binary.LittleEndian.PutUint64(buf[1:], index.Height)
	copy(buf[9:], index.ID[:])
	return sunyata.HashBytes(buf)
}

func historyProofRoot(index sunyata.ChainIndex, proof []sunyata.Hash256) sunyata.Hash256 {
	return ProofRoot(historyLeafHash(index), index.Height, proof)
}

// A HistoryAccumulator tracks the state of all ChainIndexs in a chain without
// storing the full sequence of indexes itself.
type HistoryAccumulator struct {
	Accumulator
}

// Contains returns true if the accumulator contains the given index.
func (acc *HistoryAccumulator) Contains(index sunyata.ChainIndex, proof []sunyata.Hash256) bool {
	return acc.hasTreeAtHeight(len(proof)) && acc.Trees[len(proof)] == historyProofRoot(index, proof)
}

// ApplyBlock integrates a ChainIndex into the accumulator, producing a
// HistoryApplyUpdate.
func (acc *HistoryAccumulator) ApplyBlock(index sunyata.ChainIndex) (hau HistoryApplyUpdate) {
	h := historyLeafHash(index)
	i := 0
	for ; acc.hasTreeAtHeight(i); i++ {
		hau.proof = append(hau.proof, acc.Trees[i])
		hau.growth = append(hau.growth, h)
		h = NodeHash(acc.Trees[i], h)
	}
	acc.Trees[i] = h
	acc.NumLeaves++
	return
}

// RevertBlock produces a HistoryRevertUpdate from a ChainIndex.
func (acc *HistoryAccumulator) RevertBlock(index sunyata.ChainIndex) HistoryRevertUpdate {
	return HistoryRevertUpdate{index}
}

// A HistoryApplyUpdate reflects the changes to a HistoryAccumulator resulting
// from the application of a block.
type HistoryApplyUpdate struct {
	proof  []sunyata.Hash256
	growth []sunyata.Hash256
}

// HistoryProof returns a history proof for the applied block. To prevent
// aliasing, it always returns new memory.
func (hau *HistoryApplyUpdate) HistoryProof() []sunyata.Hash256 {
	return append([]sunyata.Hash256(nil), hau.proof...)
}

// UpdateProof updates the supplied history proof to incorporate changes made to
// the chain history. The proof must be up-to-date; if it is not, UpdateProof
// may panic.
func (hau *HistoryApplyUpdate) UpdateProof(proof *[]sunyata.Hash256) {
	if len(hau.growth) > len(*proof) {
		*proof = append(*proof, hau.growth[len(*proof)])
		*proof = append(*proof, hau.proof[len(*proof):]...)
	}
}

// A HistoryRevertUpdate reflects the changes to a HistoryAccumulator resulting
// from the removal of a block.
type HistoryRevertUpdate struct {
	index sunyata.ChainIndex
}

// UpdateProof updates the supplied history proof to incorporate the changes
// made to the chain history. The proof must be up-to-date; if it is not,
// UpdateWindowProof may panic.
func (hru *HistoryRevertUpdate) UpdateProof(height uint64, proof *[]sunyata.Hash256) {
	if mh := mergeHeight(hru.index.Height, height); mh <= len(*proof) {
		*proof = (*proof)[:mh-1]
	}
}
