package sunyata

import (
	"bytes"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"sync"
	"time"
)

// An Encoder writes objects to an underlying stream.
type Encoder struct {
	w   io.Writer
	buf [1024]byte
	n   int
	err error
}

// Flush writes any pending data to the underlying stream. It returns the first
// error encountered by the Encoder.
func (e *Encoder) Flush() error {
	if e.err == nil && e.n > 0 {
		_, e.err = e.w.Write(e.buf[:e.n])
		e.n = 0
	}
	return e.err
}

// Write implements io.Writer.
func (e *Encoder) Write(p []byte) (int, error) {
	lenp := len(p)
	for e.err == nil && len(p) > 0 {
		if e.n == len(e.buf) {
			e.Flush()
		}
		c := copy(e.buf[e.n:], p)
		e.n += c
		p = p[c:]
	}
	return lenp, e.err
}

// WriteBool writes a bool value to the underlying stream.
func (e *Encoder) WriteBool(b bool) {
	var buf [1]byte
	if b {
		buf[0] = 1
	}
	e.Write(buf[:])
}

// WriteUint8 writes a uint8 value to the underlying stream.
func (e *Encoder) WriteUint8(u uint8) {
	e.Write([]byte{u})
}

// WriteUint64 writes a uint64 value to the underlying stream.
func (e *Encoder) WriteUint64(u uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], u)
	e.Write(buf[:])
}

// WritePrefix writes a length prefix to the underlying stream.
func (e *Encoder) WritePrefix(i int) { e.WriteUint64(uint64(i)) }

// WriteTime writes a time.Time value to the underlying stream.
func (e *Encoder) WriteTime(t time.Time) { e.WriteUint64(uint64(t.Unix())) }

// WriteBytes writes a length-prefixed []byte to the underlying stream.
func (e *Encoder) WriteBytes(b []byte) {
	e.WritePrefix(len(b))
	e.Write(b)
}

// WriteString writes a length-prefixed string to the underlying stream.
func (e *Encoder) WriteString(s string) {
	e.WriteBytes([]byte(s))
}

// NewEncoder returns an Encoder that wraps the provided stream.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{
		w: w,
	}
}

// An EncoderTo can encode itself to a stream via an Encoder.
type EncoderTo interface {
	EncodeTo(e *Encoder)
}

// EncodedLen returns the length of v when encoded.
func EncodedLen(v interface{}) int {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	if et, ok := v.(EncoderTo); ok {
		et.EncodeTo(e)
	} else {
		switch v := v.(type) {
		case bool:
			e.WriteBool(v)
		case uint64:
			e.WriteUint64(v)
		case time.Time:
			e.WriteTime(v)
		case []byte:
			e.WritePrefix(len(v))
			e.Write(v)
		default:
			panic(fmt.Sprintf("cannot encode type %T", v))
		}
	}
	_ = e.Flush() // no error possible
	return buf.Len()
}

// A Decoder reads values from an underlying stream. Callers MUST check
// (*Decoder).Err before using any decoded values.
type Decoder struct {
	lr  io.LimitedReader
	buf [64]byte
	err error
}

// SetErr sets the Decoder's error if it has not already been set. SetErr should
// only be called from DecodeFrom methods.
func (d *Decoder) SetErr(err error) {
	if err != nil && d.err == nil {
		d.err = err
		// clear d.buf so that future reads always return zero
		d.buf = [len(d.buf)]byte{}
	}
}

// Err returns the first error encountered during decoding.
func (d *Decoder) Err() error { return d.err }

// Read implements the io.Reader interface. It always returns an error if fewer
// than len(p) bytes were read.
func (d *Decoder) Read(p []byte) (int, error) {
	n := 0
	for len(p[n:]) > 0 && d.err == nil {
		want := len(p[n:])
		if want > len(d.buf) {
			want = len(d.buf)
		}
		var read int
		read, d.err = io.ReadFull(&d.lr, d.buf[:want])
		n += copy(p[n:], d.buf[:read])
	}
	return n, d.err
}

// ReadBool reads a bool value from the underlying stream.
func (d *Decoder) ReadBool() bool {
	d.Read(d.buf[:1])
	switch d.buf[0] {
	case 0:
		return false
	case 1:
		return true
	default:
		d.SetErr(fmt.Errorf("invalid bool value (%v)", d.buf[0]))
		return false
	}
}

// ReadUint8 reads a uint8 value from the underlying stream.
func (d *Decoder) ReadUint8() uint8 {
	d.Read(d.buf[:1])
	return d.buf[0]
}

// ReadUint64 reads a uint64 value from the underlying stream.
func (d *Decoder) ReadUint64() uint64 {
	d.Read(d.buf[:8])
	return binary.LittleEndian.Uint64(d.buf[:8])
}

// ReadPrefix reads a length prefix from the underlying stream. If the length
// exceeds the number of bytes remaining in the stream, ReadPrefix sets d.Err
// and returns 0.
func (d *Decoder) ReadPrefix() int {
	n := d.ReadUint64()
	if n > uint64(d.lr.N) {
		d.SetErr(fmt.Errorf("encoded object contains invalid length prefix (%v elems > %v bytes left in stream)", n, d.lr.N))
		return 0
	}
	return int(n)
}

// ReadTime reads a time.Time from the underlying stream.
func (d *Decoder) ReadTime() time.Time {
	return time.Unix(int64(d.ReadUint64()), 0).UTC()
}

// ReadBytes reads a length-prefixed []byte from the underlying stream.
func (d *Decoder) ReadBytes() []byte {
	b := make([]byte, d.ReadPrefix())
	d.Read(b)
	return b
}

// ReadString reads a length-prefixed string from the underlying stream.
func (d *Decoder) ReadString() string {
	return string(d.ReadBytes())
}

// NewDecoder returns a Decoder that wraps the provided stream.
func NewDecoder(lr io.LimitedReader) *Decoder {
	return &Decoder{
		lr: lr,
	}
}

// A DecoderFrom can decode itself from a stream via a Decoder.
type DecoderFrom interface {
	DecodeFrom(d *Decoder)
}

// NewBufDecoder returns a Decoder for the provided byte slice.
func NewBufDecoder(buf []byte) *Decoder {
	return NewDecoder(io.LimitedReader{
		R: bytes.NewReader(buf),
		N: int64(len(buf)),
	})
}

// A Hasher streams objects into an instance of the sunyata hash function.
type Hasher struct {
	h hash.Hash
	E *Encoder
}

// Reset resets the underlying hash digest state.
func (h *Hasher) Reset() { h.h.Reset() }

// Sum returns the digest of the objects written to the Hasher.
func (h *Hasher) Sum() (sum Hash256) {
	_ = h.E.Flush() // no error possible
	h.h.Sum(sum[:0])
	return
}

// NewHasher returns a new Hasher instance.
func NewHasher() *Hasher {
	h := sha512.New512_256()
	e := NewEncoder(h)
	return &Hasher{h, e}
}

// Pool for reducing heap allocations when hashing. This is only necessary
// because sha512.New512_256 returns a hash.Hash interface, which prevents the
// compiler from doing escape analysis. Can be removed if we switch to an
// implementation whose constructor returns a concrete type.
var hasherPool = &sync.Pool{New: func() interface{} { return NewHasher() }}

// implementations of EncoderTo and DecoderFrom for core types

// EncodeTo implements sunyata.EncoderTo.
func (h Hash256) EncodeTo(e *Encoder) { e.Write(h[:]) }

// EncodeTo implements sunyata.EncoderTo.
func (id BlockID) EncodeTo(e *Encoder) { e.Write(id[:]) }

// EncodeTo implements sunyata.EncoderTo.
func (id TransactionID) EncodeTo(e *Encoder) { e.Write(id[:]) }

// EncodeTo implements sunyata.EncoderTo.
func (a Address) EncodeTo(e *Encoder) { e.Write(a[:]) }

// EncodeTo implements sunyata.EncoderTo.
func (pk PublicKey) EncodeTo(e *Encoder) { e.Write(pk[:]) }

// EncodeTo implements sunyata.EncoderTo.
func (s Signature) EncodeTo(e *Encoder) { e.Write(s[:]) }

// EncodeTo implements sunyata.EncoderTo.
func (w Work) EncodeTo(e *Encoder) { e.Write(w.NumHashes[:]) }

// EncodeTo implements sunyata.EncoderTo.
func (c Currency) EncodeTo(e *Encoder) {
	e.WriteUint64(c.Lo)
	e.WriteUint64(c.Hi)
}

// EncodeTo implements sunyata.EncoderTo.
func (index ChainIndex) EncodeTo(e *Encoder) {
	e.WriteUint64(index.Height)
	index.ID.EncodeTo(e)
}

// EncodeTo implements sunyata.EncoderTo.
func (h BlockHeader) EncodeTo(e *Encoder) {
	e.WriteUint64(h.Height)
	h.ParentID.EncodeTo(e)
	e.WriteUint64(h.Nonce)
	e.WriteTime(h.Timestamp)
	h.MinerAddress.EncodeTo(e)
	h.Commitment.EncodeTo(e)
}

// EncodeTo implements sunyata.EncoderTo.
func (id ElementID) EncodeTo(e *Encoder) {
	id.Source.EncodeTo(e)
	e.WriteUint64(id.Index)
}

// EncodeTo implements sunyata.EncoderTo.
func (out Output) EncodeTo(e *Encoder) {
	out.Value.EncodeTo(e)
	out.Address.EncodeTo(e)
}

func (e *Encoder) writeMerkleProof(proof []Hash256) {
	e.WritePrefix(len(proof))
	for _, p := range proof {
		p.EncodeTo(e)
	}
}

// EncodeTo implements sunyata.EncoderTo.
func (se StateElement) EncodeTo(e *Encoder) {
	se.ID.EncodeTo(e)
	e.WriteUint64(se.LeafIndex)
	e.writeMerkleProof(se.MerkleProof)
}

// EncodeTo implements sunyata.EncoderTo.
func (in Input) EncodeTo(e *Encoder) {
	in.Parent.EncodeTo(e)
	in.PublicKey.EncodeTo(e)
	in.Signature.EncodeTo(e)
}

// EncodeTo implements sunyata.EncoderTo.
func (oe OutputElement) EncodeTo(e *Encoder) {
	oe.StateElement.EncodeTo(e)
	oe.Output.EncodeTo(e)
	e.WriteUint64(oe.MaturityHeight)
}

// EncodeTo implements sunyata.EncoderTo.
func (txn Transaction) EncodeTo(e *Encoder) {
	const version = 1
	e.WriteUint8(version)

	e.WritePrefix(len(txn.Inputs))
	for _, in := range txn.Inputs {
		in.EncodeTo(e)
	}
	e.WritePrefix(len(txn.Outputs))
	for _, out := range txn.Outputs {
		out.EncodeTo(e)
	}
	txn.MinerFee.EncodeTo(e)
}

// DecodeFrom implements sunyata.DecoderFrom.
func (h *Hash256) DecodeFrom(d *Decoder) { d.Read(h[:]) }

// DecodeFrom implements sunyata.DecoderFrom.
func (id *BlockID) DecodeFrom(d *Decoder) { d.Read(id[:]) }

// DecodeFrom implements sunyata.DecoderFrom.
func (id *TransactionID) DecodeFrom(d *Decoder) { d.Read(id[:]) }

// DecodeFrom implements sunyata.DecoderFrom.
func (a *Address) DecodeFrom(d *Decoder) { d.Read(a[:]) }

// DecodeFrom implements sunyata.DecoderFrom.
func (pk *PublicKey) DecodeFrom(d *Decoder) { d.Read(pk[:]) }

// DecodeFrom implements sunyata.DecoderFrom.
func (s *Signature) DecodeFrom(d *Decoder) { d.Read(s[:]) }

// DecodeFrom implements sunyata.DecoderFrom.
func (w *Work) DecodeFrom(d *Decoder) { d.Read(w.NumHashes[:]) }

// DecodeFrom implements sunyata.DecoderFrom.
func (c *Currency) DecodeFrom(d *Decoder) {
	c.Lo = d.ReadUint64()
	c.Hi = d.ReadUint64()
}

// DecodeFrom implements sunyata.DecoderFrom.
func (index *ChainIndex) DecodeFrom(d *Decoder) {
	index.Height = d.ReadUint64()
	index.ID.DecodeFrom(d)
}

// DecodeFrom implements sunyata.DecoderFrom.
func (h *BlockHeader) DecodeFrom(d *Decoder) {
	h.Height = d.ReadUint64()
	h.ParentID.DecodeFrom(d)
	h.Nonce = d.ReadUint64()
	h.Timestamp = d.ReadTime()
	h.MinerAddress.DecodeFrom(d)
	h.Commitment.DecodeFrom(d)
}

// DecodeFrom implements sunyata.DecoderFrom.
func (id *ElementID) DecodeFrom(d *Decoder) {
	id.Source.DecodeFrom(d)
	id.Index = d.ReadUint64()
}

// DecodeFrom implements sunyata.DecoderFrom.
func (out *Output) DecodeFrom(d *Decoder) {
	out.Value.DecodeFrom(d)
	out.Address.DecodeFrom(d)
}

func (d *Decoder) readMerkleProof() []Hash256 {
	proof := make([]Hash256, d.ReadPrefix())
	for i := range proof {
		proof[i].DecodeFrom(d)
	}
	return proof
}

// DecodeFrom implements sunyata.DecoderFrom.
func (se *StateElement) DecodeFrom(d *Decoder) {
	se.ID.DecodeFrom(d)
	se.LeafIndex = d.ReadUint64()
	se.MerkleProof = d.readMerkleProof()
}

// DecodeFrom implements sunyata.DecoderFrom.
func (in *Input) DecodeFrom(d *Decoder) {
	in.Parent.DecodeFrom(d)
	in.PublicKey.DecodeFrom(d)
	in.Signature.DecodeFrom(d)
}

// DecodeFrom implements sunyata.DecoderFrom.
func (oe *OutputElement) DecodeFrom(d *Decoder) {
	oe.StateElement.DecodeFrom(d)
	oe.Output.DecodeFrom(d)
	oe.MaturityHeight = d.ReadUint64()
}

// DecodeFrom implements sunyata.DecoderFrom.
func (txn *Transaction) DecodeFrom(d *Decoder) {
	if version := d.ReadUint8(); version != 1 {
		d.SetErr(fmt.Errorf("unsupported transaction version (%v)", version))
		return
	}

	txn.Inputs = make([]Input, d.ReadPrefix())
	for i := range txn.Inputs {
		txn.Inputs[i].DecodeFrom(d)
	}
	txn.Outputs = make([]Output, d.ReadPrefix())
	for i := range txn.Outputs {
		txn.Outputs[i].DecodeFrom(d)
	}
	txn.MinerFee.DecodeFrom(d)
}
