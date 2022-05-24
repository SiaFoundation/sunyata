package p2p

import (
	"fmt"
	"io"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/consensus"
	"go.sia.tech/sunyata/merkle"
)

// A Message is a p2p message sent to (or received from) a Peer.
type Message interface {
	sunyata.EncoderTo
	sunyata.DecoderFrom
	MaxLen() int
}

type taggedMessage struct {
	id uint64
	m  Message
}

const (
	typInvalid = iota
	typGetHeaders
	typHeaders
	typGetTransactions
	typTransactions
	typRelayBlock
	typRelayTransactionSet
	typGetCheckpoint
	typCheckpoint
)

func msgType(m Message) uint8 {
	switch m.(type) {
	case *MsgGetHeaders:
		return typGetHeaders
	case *MsgHeaders:
		return typHeaders
	case *MsgGetBlocks:
		return typGetTransactions
	case *MsgBlocks:
		return typTransactions
	case *MsgRelayBlock:
		return typRelayBlock
	case *MsgRelayTransactionSet:
		return typRelayTransactionSet
	case *MsgGetCheckpoint:
		return typGetCheckpoint
	case *MsgCheckpoint:
		return typCheckpoint
	default:
		panic(fmt.Sprintf("unhandled message type: %T", m))
	}
}

func isRelayMessage(m Message) bool {
	switch m.(type) {
	case *MsgRelayBlock:
		return true
	case *MsgRelayTransactionSet:
		return true
	default:
		return false
	}
}

func newMsg(typ uint8) Message {
	switch typ {
	case typGetHeaders:
		return new(MsgGetHeaders)
	case typHeaders:
		return new(MsgHeaders)
	case typGetTransactions:
		return new(MsgGetBlocks)
	case typTransactions:
		return new(MsgBlocks)
	case typRelayBlock:
		return new(MsgRelayBlock)
	case typRelayTransactionSet:
		return new(MsgRelayTransactionSet)
	case typGetCheckpoint:
		return new(MsgGetCheckpoint)
	case typCheckpoint:
		return new(MsgCheckpoint)
	default:
		return nil
	}
}

func readMessageHeader(r io.Reader) (typ uint8, id uint64, err error) {
	// read type and id
	d := sunyata.NewDecoder(io.LimitedReader{R: r, N: 1 + 8})
	typ = d.ReadUint8()
	if typ > typCheckpoint {
		return 0, 0, fmt.Errorf("unrecognized message type (%v)", typ)
	}
	id = d.ReadUint64()
	return typ, id, d.Err()
}

func readMessage(r io.Reader, recv Message) error {
	d := sunyata.NewDecoder(io.LimitedReader{R: r, N: int64(recv.MaxLen())})
	recv.DecodeFrom(d)
	return d.Err()
}

func writeMessage(w io.Writer, tm taggedMessage) error {
	e := sunyata.NewEncoder(w)
	e.WriteUint8(msgType(tm.m))
	e.WriteUint64(tm.id)
	tm.m.EncodeTo(e)
	return e.Flush()
}

// MsgGetHeaders requests a chain of contiguous headers, beginning at the most
// recent index in History known to the peer.
type MsgGetHeaders struct {
	History []sunyata.ChainIndex
}

// EncodeTo implements sunyata.EncoderTo.
func (m *MsgGetHeaders) EncodeTo(e *sunyata.Encoder) {
	e.WritePrefix(len(m.History))
	for i := range m.History {
		m.History[i].EncodeTo(e)
	}
}

// DecodeFrom implements sunyata.DecoderFrom.
func (m *MsgGetHeaders) DecodeFrom(d *sunyata.Decoder) {
	m.History = make([]sunyata.ChainIndex, d.ReadPrefix())
	for i := range m.History {
		m.History[i].DecodeFrom(d)
	}
}

// MaxLen implements p2p.Message.
func (m *MsgGetHeaders) MaxLen() int { return 1e6 }

// MsgHeaders is a response to MsgGetHeaders, containing a chain of contiguous
// headers.
type MsgHeaders struct {
	Headers []sunyata.BlockHeader
}

// EncodeTo implements sunyata.EncoderTo.
func (m *MsgHeaders) EncodeTo(e *sunyata.Encoder) {
	e.WritePrefix(len(m.Headers))
	for i := range m.Headers {
		m.Headers[i].EncodeTo(e)
	}
}

// DecodeFrom implements sunyata.DecoderFrom.
func (m *MsgHeaders) DecodeFrom(d *sunyata.Decoder) {
	m.Headers = make([]sunyata.BlockHeader, d.ReadPrefix())
	for i := range m.Headers {
		m.Headers[i].DecodeFrom(d)
	}
}

// MaxLen implements p2p.Message.
func (m *MsgHeaders) MaxLen() int { return 1e6 }

// MsgGetBlocks requests the referenced blocks.
type MsgGetBlocks struct {
	Blocks []sunyata.ChainIndex
}

// EncodeTo implements sunyata.EncoderTo.
func (m *MsgGetBlocks) EncodeTo(e *sunyata.Encoder) {
	e.WritePrefix(len(m.Blocks))
	for i := range m.Blocks {
		m.Blocks[i].EncodeTo(e)
	}
}

// DecodeFrom implements sunyata.DecoderFrom.
func (m *MsgGetBlocks) DecodeFrom(d *sunyata.Decoder) {
	m.Blocks = make([]sunyata.ChainIndex, d.ReadPrefix())
	for i := range m.Blocks {
		m.Blocks[i].DecodeFrom(d)
	}
}

// MaxLen implements p2p.Message.
func (m *MsgGetBlocks) MaxLen() int { return 1e6 }

// MsgBlocks is a response to MsgGetBlocks, containing the requested
// blocks.
type MsgBlocks struct {
	Blocks []sunyata.Block
}

// EncodeTo implements sunyata.EncoderTo.
func (m *MsgBlocks) EncodeTo(e *sunyata.Encoder) {
	e.WritePrefix(len(m.Blocks))
	for i := range m.Blocks {
		(*merkle.CompressedBlock)(&m.Blocks[i]).EncodeTo(e)
	}
}

// DecodeFrom implements sunyata.DecoderFrom.
func (m *MsgBlocks) DecodeFrom(d *sunyata.Decoder) {
	m.Blocks = make([]sunyata.Block, d.ReadPrefix())
	for i := range m.Blocks {
		(*merkle.CompressedBlock)(&m.Blocks[i]).DecodeFrom(d)
	}
}

// MaxLen implements p2p.Message.
func (m *MsgBlocks) MaxLen() int { return 100e6 }

// MsgRelayBlock relays a block.
type MsgRelayBlock struct {
	Block sunyata.Block
}

// EncodeTo implements sunyata.EncoderTo.
func (m *MsgRelayBlock) EncodeTo(e *sunyata.Encoder) {
	(*merkle.CompressedBlock)(&m.Block).EncodeTo(e)
}

// DecodeFrom implements sunyata.DecoderFrom.
func (m *MsgRelayBlock) DecodeFrom(d *sunyata.Decoder) {
	(*merkle.CompressedBlock)(&m.Block).DecodeFrom(d)
}

// MaxLen implements p2p.Message.
func (m *MsgRelayBlock) MaxLen() int { return 10e6 }

// MsgRelayTransactionSet relays a transaction set for inclusion in the txpool.
// All proofs in the set must be up-to-date as of the same block.
type MsgRelayTransactionSet struct {
	Transactions []sunyata.Transaction
}

// EncodeTo implements sunyata.EncoderTo.
func (m *MsgRelayTransactionSet) EncodeTo(e *sunyata.Encoder) {
	e.WritePrefix(len(m.Transactions))
	for i := range m.Transactions {
		m.Transactions[i].EncodeTo(e)
	}
}

// DecodeFrom implements sunyata.DecoderFrom.
func (m *MsgRelayTransactionSet) DecodeFrom(d *sunyata.Decoder) {
	m.Transactions = make([]sunyata.Transaction, d.ReadPrefix())
	for i := range m.Transactions {
		m.Transactions[i].DecodeFrom(d)
	}
}

// MaxLen implements p2p.Message.
func (m *MsgRelayTransactionSet) MaxLen() int { return 2e6 }

// MsgGetCheckpoint requests a Block and its parent State.
type MsgGetCheckpoint struct {
	Index sunyata.ChainIndex
}

// EncodeTo implements sunyata.EncoderTo.
func (m *MsgGetCheckpoint) EncodeTo(e *sunyata.Encoder) {
	m.Index.EncodeTo(e)
}

// DecodeFrom implements sunyata.DecoderFrom.
func (m *MsgGetCheckpoint) DecodeFrom(d *sunyata.Decoder) {
	m.Index.DecodeFrom(d)
}

// MaxLen implements p2p.Message.
func (m *MsgGetCheckpoint) MaxLen() int { return 1e6 }

// MsgCheckpoint is a response to MsgGetCheckpoint, containing the requested
// Block and its parent State.
type MsgCheckpoint struct {
	Block       sunyata.Block
	ParentState consensus.State
}

// EncodeTo implements sunyata.EncoderTo.
func (m *MsgCheckpoint) EncodeTo(e *sunyata.Encoder) {
	(*merkle.CompressedBlock)(&m.Block).EncodeTo(e)
	m.ParentState.EncodeTo(e)
}

// DecodeFrom implements sunyata.DecoderFrom.
func (m *MsgCheckpoint) DecodeFrom(d *sunyata.Decoder) {
	(*merkle.CompressedBlock)(&m.Block).DecodeFrom(d)
	m.ParentState.DecodeFrom(d)
}

// MaxLen implements p2p.Message.
func (m *MsgCheckpoint) MaxLen() int { return 10e6 }
