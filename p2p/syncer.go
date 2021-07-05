package p2p

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/txpool"
)

// A Syncer maintains peer connections, synchronizes a chain and a transaction
// pool, and relays new blocks and transactions.
type Syncer struct {
	l  net.Listener
	cm *chain.Manager
	tp *txpool.Pool

	mu        sync.Mutex
	cond      sync.Cond // shares mu
	peers     []*Peer
	handshake peerHandshake
	err       error
}

func (s *Syncer) setErr(err error) error {
	if s.err == nil {
		s.err = err
		for _, p := range s.peers {
			p.disconnect()
		}
		s.cond.Broadcast() // wake s.Run
		s.l.Close()        // wake s.listen
	}
	return s.err
}

func (s *Syncer) handleMessage(p *Peer, msg Message) {
	switch msg := msg.(type) {
	case *MsgGetHeaders:
		s.processMsgGetHeaders(p, msg)
	case *MsgHeaders:
		s.processMsgHeaders(p, msg)
	case *MsgGetBlocks:
		s.processMsgGetBlocks(p, msg)
	case *MsgBlocks:
		s.processMsgBlocks(p, msg)
	case *MsgRelayBlock:
		s.processMsgRelayBlock(p, msg)
	case *MsgRelayTransactionSet:
		s.processMsgRelayTransactionSet(p, msg)
	case *MsgGetCheckpoint:
		s.processMsgGetCheckpoint(p, msg)
	case *MsgCheckpoint:
		// we only receive this message in DownloadCheckpoint
		p.warn(errors.New("unsolicited checkpoint"))
	default:
		panic(fmt.Sprintf("unhandled message type (%T)", msg))
	}
}

func (s *Syncer) processMsgGetCheckpoint(p *Peer, msg *MsgGetCheckpoint) {
	// peer is requesting a checkpoint

	b, err := s.cm.Block(msg.Index)
	if errors.Is(err, chain.ErrPruned) {
		return // nothing we can do
	} else if errors.Is(err, chain.ErrUnknownIndex) {
		p.warn(err)
		return
	} else if err != nil {
		s.setErr(fmt.Errorf("%T: couldn't load block: %w", msg, err))
		return
	}
	vc, err := s.cm.ValidationContext(b.Header.ParentIndex())
	if errors.Is(err, chain.ErrPruned) {
		return
	} else if err != nil {
		s.setErr(fmt.Errorf("%T: couldn't load validation context: %w", msg, err))
		return
	}

	p.queue(&MsgCheckpoint{
		Block:         b,
		ParentContext: vc,
	})
}

func (s *Syncer) processMsgGetHeaders(p *Peer, msg *MsgGetHeaders) {
	// peer is requesting headers in bulk

	sort.Slice(msg.History, func(i, j int) bool {
		return msg.History[i].Height > msg.History[j].Height
	})
	headers, err := s.cm.HeadersForHistory(make([]sunyata.BlockHeader, 2000), msg.History)
	if err != nil {
		s.setErr(fmt.Errorf("%T: couldn't load headers: %w", msg, err))
		return
	}
	if len(headers) > 0 {
		p.queue(&MsgHeaders{Headers: headers})
	}
}

func (s *Syncer) processMsgHeaders(p *Peer, msg *MsgHeaders) {
	// peer is sending us headers, a subset of which should attach to one of our
	// known chains.

	if len(msg.Headers) == 0 {
		p.ban(errors.New("empty headers message"))
		return
	} else if msg.Headers[0].Height == 0 {
		p.ban(errors.New("headers message should never contain genesis header"))
		return
	}

	newBest, err := s.cm.AddHeaders(msg.Headers)
	if errors.Is(err, chain.ErrUnknownIndex) {
		// NOTE: attempting to synchronize again would be a bad idea: it could
		// easily lead to an infinite loop. Instead, just ignore these headers.
		return
	} else if err != nil {
		s.setErr(fmt.Errorf("%T: couldn't add headers: %w", msg, err))
		return
	}
	if newBest == nil {
		// request the next set of headers
		last := []sunyata.ChainIndex{msg.Headers[len(msg.Headers)-1].Index()}
		p.queue(&MsgGetHeaders{History: last})
		return
	}

	// we now have a new best chain (assuming its transaction are valid);
	// request those transactions
	blocks := newBest.Unvalidated()
	if len(blocks) > 10 {
		blocks = blocks[:10]
	}
	p.queue(&MsgGetBlocks{Blocks: blocks})
}

func (s *Syncer) processMsgGetBlocks(p *Peer, msg *MsgGetBlocks) {
	// peer is requesting blocks

	if len(msg.Blocks) == 0 {
		p.ban(fmt.Errorf("empty %T", msg))
		return
	}

	var blocks []sunyata.Block
	for _, index := range msg.Blocks {
		b, err := s.cm.Block(index)
		if errors.Is(err, chain.ErrPruned) {
			break // nothing we can do
		} else if errors.Is(err, chain.ErrUnknownIndex) {
			p.warn(fmt.Errorf("peer requested blocks we don't have"))
			break
		} else if err != nil {
			s.setErr(fmt.Errorf("%T: couldn't load transactions: %w", msg, err))
			return
		}
		blocks = append(blocks, b)
	}
	if len(blocks) > 0 {
		p.queue(&MsgBlocks{Blocks: blocks})
	}
}

func (s *Syncer) processMsgBlocks(p *Peer, msg *MsgBlocks) {
	// peer is sending us the blocks we requested; they should match up exactly
	// with one of our existing scratch chains.

	sc, err := s.cm.AddBlocks(msg.Blocks)
	if errors.Is(err, chain.ErrUnknownIndex) {
		p.warn(fmt.Errorf("%T: non-attaching blocks: %w", msg, err))
		return
	} else if err != nil {
		s.setErr(fmt.Errorf("%T: couldn't add blocks: %w", msg, err))
		return
	}
	if blocks := sc.Unvalidated(); len(blocks) > 0 {
		// request the next set of blocks
		if len(blocks) > 10 {
			blocks = blocks[:10]
		}
		p.queue(&MsgGetBlocks{Blocks: blocks})
		return
	}

	// request the next set of headers
	tip := []sunyata.ChainIndex{sc.ValidTip()}
	p.queue(&MsgGetHeaders{History: tip})
}

func (s *Syncer) processMsgRelayBlock(p *Peer, msg *MsgRelayBlock) {
	// peer is relaying a block

	err := s.cm.AddTipBlock(msg.Block)
	if errors.Is(err, chain.ErrKnownBlock) {
		// avoid relaying a block multiple times
		return
	} else if errors.Is(err, chain.ErrUnknownIndex) {
		// block does not attach to our tip; request a full history
		history, err := s.cm.History()
		if err != nil {
			s.setErr(fmt.Errorf("%T: couldn't construct history: %w", msg, err))
			return
		}
		p.queue(&MsgGetHeaders{History: history})
		return
	} else if err != nil {
		// TODO: this is likely a validation error, not a fatal internal error
		s.setErr(fmt.Errorf("%T: couldn't add tip block: %w", msg, err))
		return
	}

	s.relay(p, msg)
}

func (s *Syncer) processMsgRelayTransactionSet(p *Peer, msg *MsgRelayTransactionSet) {
	// peer is relaying a set of transactions for inclusion in the txpool

	allValid := true
	for _, txn := range msg.Transactions {
		if err := s.tp.AddTransaction(txn); err != nil {
			p.warn(fmt.Errorf("%T: invalid txn: %w", msg, err))
			allValid = false
		}
	}

	if allValid {
		s.relay(p, msg)
	}
}

// Run initiates communication with peers. This is a blocking call; it processes
// messages until the Syncer is closed or encounters a fatal error.
func (s *Syncer) Run() error {
	go s.acceptLoop()
	go s.messageLoop()
	go s.syncLoop()

	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	for s.err == nil {
		s.cond.Wait()
	}
	if s.err != errClosing {
		return s.err
	}
	return nil
}

func (s *Syncer) acceptLoop() {
	for {
		conn, err := s.l.Accept()
		if err != nil {
			s.cond.L.Lock()
			s.setErr(err)
			s.cond.L.Unlock()
			s.cond.Broadcast() // wake s.Run
			return
		}
		go func(conn net.Conn) {
			if err := s.acceptConnection(conn); err != nil {
				conn.Close()
				// TODO: maybe set peer.err instead of logging here?
			}
		}(conn)
	}
}

func (s *Syncer) messageLoop() {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	for s.err == nil {
		s.removeDisconnected()
		p, msg := s.nextMessage()
		if msg == nil {
			s.cond.Wait()
			continue
		}
		s.handleMessage(p, msg)
	}
}

func (s *Syncer) syncLoop() {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	seen := make(map[sunyata.ChainIndex]bool)
	for s.err == nil {
		// initiate header sync with any new peers
		for _, p := range s.peers {
			if !seen[p.handshake.Tip] {
				history, err := s.cm.History()
				if err != nil {
					s.setErr(err)
					return
				}
				p.queue(&MsgGetHeaders{History: history})
				seen[p.handshake.Tip] = true
			}
		}

		s.cond.Wait()
	}
}

// Addr returns the address that the Syncer listens on.
func (s *Syncer) Addr() string {
	return s.l.Addr().String()
}

// Close closes all active connections and stops the listener goroutine.
func (s *Syncer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.setErr(errClosing); err != errClosing {
		return err
	}
	return nil
}

// NewSyncer returns a Syncer for the provided chain manager and transaction
// pool, listening on the provided address.
func NewSyncer(addr string, genesisID sunyata.BlockID, cm *chain.Manager, tp *txpool.Pool) (*Syncer, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("couldn't establish listener: %w", err)
	}
	handshake := peerHandshake{
		Version: 1,
		Genesis: genesisID,
		Tip:     cm.Tip(),
	}
	if _, err := rand.Read(handshake.Key[:]); err != nil {
		return nil, fmt.Errorf("couldn't generate encryption key: %w", err)
	}
	s := &Syncer{
		l:         l,
		cm:        cm,
		tp:        tp,
		handshake: handshake,
	}
	s.cond.L = &s.mu
	return s, nil
}
