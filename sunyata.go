// Package sunyata defines the basic types and functions of the sunyata
// system.
package sunyata

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"math"
	"math/big"
	"math/bits"
	"sync"
	"time"
)

// EphemeralLeafIndex is used as the LeafIndex of Outputs that are created and
// spent within the same block. Such outputs do not require a proof of
// existence. They are, however, assigned a proper index and are incorporated
// into the state accumulator when the block is processed.
const EphemeralLeafIndex = math.MaxUint64

// A Hash256 is a generic 256-bit cryptographic hash.
type Hash256 [32]byte

// HashBytes computes the hash of b using sunyata's hash function.
func HashBytes(b []byte) Hash256 { return sha512.Sum512_256(b) }

// An Address is the hash of a public key.
type Address Hash256

// VoidAddress is an address whose signing key does not exist. Sending coins to
// this address ensures that they will never be recoverable by anyone.
var VoidAddress Address

// A BlockID uniquely identifies a block.
type BlockID Hash256

// MeetsTarget returns true if bid is not greater than t.
func (bid BlockID) MeetsTarget(t BlockID) bool {
	return bytes.Compare(bid[:], t[:]) <= 0
}

// A ChainIndex pairs a block's height with its ID.
//
// Using either value alone can be problematic: if only the height is known, it
// could refer to multiple blocks on different chains; if only the ID is known,
// efficient random access to blocks (e.g. within a slice or a flat file) is not
// possible without a supplementary lookup table.
type ChainIndex struct {
	Height uint64
	ID     BlockID
}

// A PublicKey is an ed25519 public key.
type PublicKey [32]byte

// Address returns the address corresponding to a public key.
func (pk PublicKey) Address() Address { return Address(HashBytes(pk[:])) }

// A TransactionID uniquely identifies a transaction.
type TransactionID Hash256

// An OutputID uniquely identifies an Output.
type OutputID struct {
	TransactionID    TransactionID
	BeneficiaryIndex uint64
}

// An Output is a volume of currency that is created and spent as an atomic
// unit. Every Output has an associated Address; spending the Output requires
// revealing the PublicKey that is the preimage of the Address. An auxilliary
// condition is the Timelock, which renders the Output unspendable until the
// specified chain height is reached. Finally, the utreexo model requires the
// spender to prove that the Output is spendable, i.e. that it is within the set
// of spendable outputs; to this end, each Output contains a Merkle proof and
// some associated leaf data.
type Output struct {
	ID          OutputID
	Value       Currency
	Address     Address
	Timelock    uint64
	MerkleProof []Hash256
	LeafIndex   uint64
}

// An InputSignature signs a transaction input.
type InputSignature [64]byte

// SignTransaction signs sigHash with privateKey, producing an InputSignature.
func SignTransaction(privateKey ed25519.PrivateKey, sigHash Hash256) (is InputSignature) {
	copy(is[:], ed25519.Sign(privateKey, sigHash[:]))
	return
}

// An Input spends its parent Output by revealing its public key and signing the
// transaction.
type Input struct {
	Parent    Output
	PublicKey PublicKey
	Signature InputSignature
}

// A Beneficiary is the recipient of some of the value spent in a transaction.
type Beneficiary struct {
	Value   Currency
	Address Address
}

// A Transaction transfers value by consuming existing Outputs and creating new
// Outputs.
type Transaction struct {
	Inputs   []Input
	Outputs  []Beneficiary
	MinerFee Currency
}

// ID returns the hash of all block-independent data in the transaction.
func (txn *Transaction) ID() TransactionID {
	h := hasherPool.Get().(*Hasher)
	defer hasherPool.Put(h)
	h.Reset()
	// Input IDs, Outputs, and MinerFee are sufficient to uniquely identify a
	// transaction
	for i := range txn.Inputs {
		h.WriteHash(txn.Inputs[i].Parent.ID.TransactionID)
		h.WriteUint64(txn.Inputs[i].Parent.ID.BeneficiaryIndex)
	}
	for i := range txn.Outputs {
		h.WriteCurrency(txn.Outputs[i].Value)
		h.WriteHash(txn.Outputs[i].Address)
	}
	h.WriteCurrency(txn.MinerFee)
	return TransactionID(h.Sum())
}

// DeepCopy returns a copy of txn that does not alias any of its memory.
func (txn *Transaction) DeepCopy() Transaction {
	c := *txn
	c.Inputs = append([]Input(nil), c.Inputs...)
	for i := range c.Inputs {
		c.Inputs[i].Parent.MerkleProof = append([]Hash256(nil), c.Inputs[i].Parent.MerkleProof...)
	}
	c.Outputs = append([]Beneficiary(nil), c.Outputs...)
	return c
}

// A BlockHeader contains a Block's non-transaction data.
type BlockHeader struct {
	Height       uint64
	ParentID     BlockID
	Nonce        [8]byte
	Timestamp    time.Time
	MinerAddress Address
	Commitment   Hash256
}

// Index returns the header's chain index.
func (h BlockHeader) Index() ChainIndex {
	return ChainIndex{
		Height: h.Height,
		ID:     h.ID(),
	}
}

// ParentIndex returns the index of the header's parent.
func (h BlockHeader) ParentIndex() ChainIndex {
	return ChainIndex{
		Height: h.Height - 1,
		ID:     h.ParentID,
	}
}

// ID returns a hash that uniquely identifies a block.
func (h BlockHeader) ID() BlockID {
	buf := make([]byte, 8+8+32)
	copy(buf[:], h.Nonce[:])
	binary.LittleEndian.PutUint64(buf[8:], uint64(h.Timestamp.Unix()))
	copy(buf[16:], h.Commitment[:])
	return BlockID(HashBytes(buf))
}

// CurrentTimestamp returns the current time, rounded to the nearest second.
func CurrentTimestamp() time.Time { return time.Now().Round(time.Second) }

// A Block is a set of Transactions, along with a header and proof data.
type Block struct {
	Header           BlockHeader
	Transactions     []Transaction
	AccumulatorProof []Hash256
}

// ID returns a hash that uniquely identifies a block. It is equivalent to
// b.Header.ID().
func (b *Block) ID() BlockID { return b.Header.ID() }

// Index returns the block's chain index. It is equivalent to b.Header.Index().
func (b *Block) Index() ChainIndex { return b.Header.Index() }

// Work represents a quantity of work.
type Work struct {
	// The representation is the expected number of hashes required to produce a
	// given hash, in big-endian order.
	//
	// TODO: no reason not to use little-endian here, except that it would make
	// it harder to convert to/from big.Int. If we replace big.Int with custom
	// division code, then we can switch to little-endian.
	NumHashes [32]byte
}

// Add returns w+v.
func (w Work) Add(v Work) Work {
	var r Work
	var sum, c uint64
	for i := 24; i >= 0; i -= 8 {
		wi := binary.BigEndian.Uint64(w.NumHashes[i:])
		vi := binary.BigEndian.Uint64(v.NumHashes[i:])
		sum, c = bits.Add64(wi, vi, c)
		binary.BigEndian.PutUint64(r.NumHashes[i:], sum)
	}
	return r
}

// Cmp compares two work values.
func (w Work) Cmp(v Work) int {
	return bytes.Compare(w.NumHashes[:], v.NumHashes[:])
}

// WorkRequiredForHash estimates how much work was required to produce the given
// id. Note that the mapping is not injective; many different ids may require
// the same expected amount of Work.
func WorkRequiredForHash(id BlockID) Work {
	if id == (BlockID{}) {
		// This should never happen as long as inputs are properly validated and
		// the laws of physics are intact.
		panic("impossibly good BlockID")
	}
	// As a special case, this hash requires the maximum possible amount of
	// Work. (Otherwise, the division would produce 2^256, which overflows our
	// representation.)
	if id == ([32]byte{31: 1}) {
		return Work{
			NumHashes: [32]byte{
				0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
				0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
			},
		}
	}

	// To get the expected number of hashes required, simply divide 2^256 by id.
	//
	// TODO: write a zero-alloc uint256 division instead of using big.Int
	maxTarget := new(big.Int).Lsh(big.NewInt(1), 256)
	idInt := new(big.Int).SetBytes(id[:])
	quo := maxTarget.Div(maxTarget, idInt).Bytes()
	var w Work
	copy(w.NumHashes[32-len(quo):], quo)
	return w
}

// HashRequiringWork returns the best BlockID that the given amount of Work
// would be expected to produce. Note that many different BlockIDs may require
// the same amount of Work; this function returns the lowest of them.
func HashRequiringWork(w Work) BlockID {
	if w.NumHashes == ([32]byte{}) {
		panic("no hash requires zero work")
	}
	// As a special case, 1 Work produces this hash. (Otherwise, the division
	// would produce 2^256, which overflows our representation.)
	if w.NumHashes == ([32]byte{31: 1}) {
		return BlockID{
			0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
			0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		}
	}
	maxTarget := new(big.Int).Lsh(big.NewInt(1), 256)
	workInt := new(big.Int).SetBytes(w.NumHashes[:])
	quo := maxTarget.Div(maxTarget, workInt).Bytes()
	var id BlockID
	copy(id[32-len(quo):], quo)
	return id
}

// BlockInterval is the expected wall clock time between consecutive blocks.
const BlockInterval = 10 * time.Minute

// DifficultyAdjustmentInterval is the number of blocks between adjustments to
// the block mining target.
const DifficultyAdjustmentInterval = 2016

// AdjustDifficulty computes the amount of Work required for the next difficulty
// adjustment interval, given the amount of Work performed in the previous
// interval and the duration of that interval.
func AdjustDifficulty(w Work, interval time.Duration) Work {
	if interval.Round(time.Second) != interval {
		// developer error; interval should be the difference between two Unix
		// timestamps
		panic("interval not rounded to nearest second")
	}
	const maxInterval = BlockInterval * DifficultyAdjustmentInterval * 4
	const minInterval = BlockInterval * DifficultyAdjustmentInterval / 4
	if interval > maxInterval {
		interval = maxInterval
	} else if interval < minInterval {
		interval = minInterval
	}
	workInt := new(big.Int).SetBytes(w.NumHashes[:])
	workInt.Mul(workInt, big.NewInt(int64(BlockInterval*DifficultyAdjustmentInterval)))
	workInt.Div(workInt, big.NewInt(int64(interval)))
	quo := workInt.Bytes()
	copy(w.NumHashes[32-len(quo):], quo)
	return w
}

// A Hasher hashes data using sunyata's hash function.
type Hasher struct {
	h   hash.Hash
	buf [64]byte
}

// Reset resets the underlying hasher.
func (h *Hasher) Reset() {
	h.h.Reset()
}

// Write implements io.Writer.
func (h *Hasher) Write(p []byte) (int, error) {
	buf := bytes.NewBuffer(p)
	for buf.Len() > 0 {
		n := copy(h.buf[:], buf.Next(len(h.buf)))
		h.h.Write(h.buf[:n])
	}
	return len(p), nil
}

// WriteHash writes a generic hash to the underlying hasher.
func (h *Hasher) WriteHash(p [32]byte) {
	copy(h.buf[:], p[:])
	h.h.Write(h.buf[:32])
}

// WriteUint64 writes a uint64 value to the underlying hasher.
func (h *Hasher) WriteUint64(u uint64) {
	binary.LittleEndian.PutUint64(h.buf[:8], u)
	h.h.Write(h.buf[:8])
}

// WriteTime writes a time.Time value to the underlying hasher.
func (h *Hasher) WriteTime(t time.Time) {
	h.WriteUint64(uint64(t.Unix()))
}

// WriteCurrency writes a Currency value to the underlying hasher.
func (h *Hasher) WriteCurrency(c Currency) {
	binary.LittleEndian.PutUint64(h.buf[:8], c.Lo)
	binary.LittleEndian.PutUint64(h.buf[8:], c.Hi)
	h.h.Write(h.buf[:16])
}

// Sum returns the hash of the data written to the underlying hasher.
func (h *Hasher) Sum() Hash256 {
	var sum Hash256
	h.h.Sum(sum[:0])
	return sum
}

// NewHasher returns a Hasher instance for sunyata's hash function.
func NewHasher() *Hasher { return &Hasher{h: sha512.New512_256()} }

// Pool for reducing heap allocations when hashing. This is only necessary
// because sha512.New512_256 returns a hash.Hash interface, which prevents the
// compiler from doing escape analysis. Can be removed if we switch to an
// implementation whose constructor returns a concrete type.
var hasherPool = &sync.Pool{New: func() interface{} { return NewHasher() }}

// Implementations of fmt.Stringer and json.(Un)marshaler

func stringerHex(prefix string, data []byte) string {
	return prefix + ":" + hex.EncodeToString(data[:])
}

func marshalJSONHex(prefix string, data []byte) ([]byte, error) {
	return []byte(`"` + stringerHex(prefix, data) + `"`), nil
}

func unmarshalJSONHex(dst []byte, prefix string, data []byte) error {
	_, err := hex.Decode(dst, bytes.TrimPrefix(bytes.Trim(data, `"`), []byte(prefix+":")))
	if err != nil {
		return fmt.Errorf("decoding %v:<hex> failed: %w", prefix, err)
	}
	return nil
}

// String implements fmt.Stringer.
func (h Hash256) String() string { return stringerHex("h", h[:]) }

// MarshalJSON implements json.Marshaler.
func (h Hash256) MarshalJSON() ([]byte, error) { return marshalJSONHex("h", h[:]) }

// UnmarshalJSON implements json.Unmarshaler.
func (h *Hash256) UnmarshalJSON(b []byte) error { return unmarshalJSONHex(h[:], "h", b) }

// String implements fmt.Stringer.
func (ci ChainIndex) String() string {
	// use the 4 least-significant bytes of ID -- in a mature chain, the
	// most-significant bytes will be zeros
	return fmt.Sprintf("%v::%x", ci.Height, ci.ID[len(ci.ID)-4:])
}

// String implements fmt.Stringer.
func (a Address) String() string { return stringerHex("addr", a[:]) }

// MarshalJSON implements json.Marshaler.
func (a Address) MarshalJSON() ([]byte, error) { return marshalJSONHex("addr", a[:]) }

// UnmarshalJSON implements json.Unmarshaler.
func (a *Address) UnmarshalJSON(b []byte) error { return unmarshalJSONHex(a[:], "addr", b) }

// String implements fmt.Stringer.
func (bid BlockID) String() string { return stringerHex("bid", bid[:]) }

// MarshalJSON implements json.Marshaler.
func (bid BlockID) MarshalJSON() ([]byte, error) { return marshalJSONHex("bid", bid[:]) }

// UnmarshalJSON implements json.Unmarshaler.
func (bid *BlockID) UnmarshalJSON(b []byte) error { return unmarshalJSONHex(bid[:], "bid", b) }

// String implements fmt.Stringer.
func (pk PublicKey) String() string { return stringerHex("ed25519", pk[:]) }

// MarshalJSON implements json.Marshaler.
func (pk PublicKey) MarshalJSON() ([]byte, error) { return marshalJSONHex("ed25519", pk[:]) }

// UnmarshalJSON implements json.Unmarshaler.
func (pk *PublicKey) UnmarshalJSON(b []byte) error { return unmarshalJSONHex(pk[:], "ed25519", b) }

// String implements fmt.Stringer.
func (tid TransactionID) String() string { return stringerHex("txid", tid[:]) }

// MarshalJSON implements json.Marshaler.
func (tid TransactionID) MarshalJSON() ([]byte, error) { return marshalJSONHex("txid", tid[:]) }

// UnmarshalJSON implements json.Unmarshaler.
func (tid *TransactionID) UnmarshalJSON(b []byte) error { return unmarshalJSONHex(tid[:], "txid", b) }

// String implements fmt.Stringer.
func (is InputSignature) String() string { return stringerHex("sig", is[:]) }

// MarshalJSON implements json.Marshaler.
func (is InputSignature) MarshalJSON() ([]byte, error) { return marshalJSONHex("sig", is[:]) }

// UnmarshalJSON implements json.Unmarshaler.
func (is *InputSignature) UnmarshalJSON(b []byte) error { return unmarshalJSONHex(is[:], "sig", b) }

// String implements fmt.Stringer.
func (w Work) String() string { return new(big.Int).SetBytes(w.NumHashes[:]).String() }
