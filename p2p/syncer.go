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

func (s *Syncer) handleRequest(p *Peer, msg Message) Message {
	switch msg := msg.(type) {
	case *MsgGetHeaders:
		return s.handleMsgGetHeaders(p, msg)
	case *MsgGetBlocks:
		return s.handleMsgGetBlocks(p, msg)
	case *MsgGetCheckpoint:
		return s.handleMsgGetCheckpoint(p, msg)
	case *MsgRelayBlock:
		return s.handleMsgRelayBlock(p, msg)
	case *MsgRelayTransactionSet:
		return s.handleMsgRelayTransactionSet(p, msg)
	default:
		return nil
	}
}

func (s *Syncer) handleMsgGetHeaders(p *Peer, msg *MsgGetHeaders) Message {
	// peer is requesting headers in bulk

	sort.Slice(msg.History, func(i, j int) bool {
		return msg.History[i].Height > msg.History[j].Height
	})
	headers, err := s.cm.HeadersForHistory(make([]sunyata.BlockHeader, 2000), msg.History)
	if err != nil {
		s.setErr(fmt.Errorf("%T: couldn't load headers: %w", msg, err))
		return nil
	}
	return &MsgHeaders{Headers: headers}
}

func (s *Syncer) handleMsgGetBlocks(p *Peer, msg *MsgGetBlocks) Message {
	// peer is requesting blocks

	if len(msg.Blocks) == 0 {
		p.ban(fmt.Errorf("empty %T", msg))
		return nil
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
			return nil
		}
		blocks = append(blocks, b)
	}
	return &MsgBlocks{Blocks: blocks}
}

func (s *Syncer) handleMsgGetCheckpoint(p *Peer, msg *MsgGetCheckpoint) *MsgCheckpoint {
	// peer is requesting a checkpoint

	b, err := s.cm.Block(msg.Index)
	if errors.Is(err, chain.ErrPruned) {
		return nil // nothing we can do
	} else if errors.Is(err, chain.ErrUnknownIndex) {
		p.warn(err)
		return nil
	} else if err != nil {
		s.setErr(fmt.Errorf("%T: couldn't load block: %w", msg, err))
		return nil
	}
	vc, err := s.cm.ValidationContext(b.Header.ParentIndex())
	if errors.Is(err, chain.ErrPruned) {
		return nil
	} else if err != nil {
		s.setErr(fmt.Errorf("%T: couldn't load validation context: %w", msg, err))
		return nil
	}

	return &MsgCheckpoint{Block: b, ParentContext: vc}
}

func (s *Syncer) handleMsgRelayBlock(p *Peer, msg *MsgRelayBlock) *MsgRelayBlock {
	// peer is relaying a block

	err := s.cm.AddTipBlock(msg.Block)
	if errors.Is(err, chain.ErrKnownBlock) {
		// don't relay a block multiple times
		return nil
	} else if errors.Is(err, chain.ErrUnknownIndex) {
		// TODO: mark this index as a "sync target" and wake s.syncLoop
		return msg
	} else if err != nil {
		// TODO: this is likely a validation error, not a fatal internal error
		s.setErr(fmt.Errorf("%T: couldn't add tip block: %w", msg, err))
		return nil
	}
	return msg
}

func (s *Syncer) handleMsgRelayTransactionSet(p *Peer, msg *MsgRelayTransactionSet) *MsgRelayTransactionSet {
	// peer is relaying a set of transactions for inclusion in the txpool

	for _, txn := range msg.Transactions {
		if err := s.tp.AddTransaction(txn); err != nil {
			p.warn(fmt.Errorf("%T: invalid txn: %w", msg, err))
			return nil
		}
	}
	return msg
}

// Run initiates communication with peers. This is a blocking call; it processes
// messages until the Syncer is closed or encounters a fatal error.
func (s *Syncer) Run() error {
	go s.acceptLoop()
	go s.requestLoop()
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

func (s *Syncer) requestLoop() {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	for s.err == nil {
		s.removeDisconnected()
		p, tm := s.nextRequest()
		if p == nil {
			s.cond.Wait()
			continue
		}
		resp := s.handleRequest(p, tm.m)
		if resp == nil {
			continue // bad request; handleRequest is responsible for warning/banning the peer
		} else if isRelayMessage(resp) {
			s.relay(p, resp)
		} else {
			p.mu.Lock()
			p.out = append(p.out, taggedMessage{tm.id | 1, resp})
			p.cond.Broadcast() // wake p.handleOut
			p.mu.Unlock()
		}
	}
}

func (s *Syncer) syncLoop() {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	seen := make(map[sunyata.ChainIndex]bool)
	for s.err == nil {
		// initiate header sync with any new peers
		s.removeDisconnected()
		for _, p := range s.peers {
			if !seen[p.handshake.Tip] {
				seen[p.handshake.Tip] = true
				s.syncToTarget(p, p.handshake.Tip)
			}
		}

		s.cond.Wait()
	}
}

func (s *Syncer) syncToTarget(p *Peer, target sunyata.ChainIndex) {
	// helper functions for calling RPCs
	getHeaders := func(history []sunyata.ChainIndex) []sunyata.BlockHeader {
		req := &MsgGetHeaders{History: history}
		var resp MsgHeaders
		// unlock during call
		s.cond.L.Unlock()
		err := p.RPC(req, &resp).Wait()
		s.cond.L.Lock()
		if err != nil {
			p.disconnect()
			return nil
		} else if len(resp.Headers) > 0 && resp.Headers[0].Height == 0 {
			p.ban(errors.New("headers message should never contain genesis header"))
			return nil
		}
		return resp.Headers
	}
	getBlocks := func(blocks []sunyata.ChainIndex) []sunyata.Block {
		req := &MsgGetBlocks{Blocks: blocks}
		var resp MsgBlocks
		s.cond.L.Unlock()
		err := p.RPC(req, &resp).Wait()
		s.cond.L.Lock()
		if err != nil {
			p.disconnect()
			return nil
		}
		return resp.Blocks
	}

	// exchange history
	history, err := s.cm.History()
	if err != nil {
		s.setErr(err)
		return
	}
	for {
		headers := getHeaders(history)
		if len(headers) == 0 {
			return
		}
		sc, err := s.cm.AddHeaders(headers)
		if errors.Is(err, chain.ErrUnknownIndex) {
			// NOTE: attempting to synchronize again would be a bad idea: it could
			// easily lead to an infinite loop. Instead, just ignore these headers.
			return
		} else if err != nil {
			s.setErr(fmt.Errorf("syncLoop: couldn't add headers: %w", err))
			return
		}
		if sc == nil {
			// this chain is still not the best known; request more headers
			history = []sunyata.ChainIndex{headers[len(headers)-1].Index()}
			continue
		}

		// we now have a new best chain, assuming its transaction are valid;
		// request those transactions
		for !sc.FullyValidated() {
			unvalidated := sc.Unvalidated()
			if len(unvalidated) > 10 {
				unvalidated = unvalidated[:10]
			}
			blocks := getBlocks(unvalidated)
			if blocks == nil {
				return
			}
			sc, err = s.cm.AddBlocks(blocks)
			if errors.Is(err, chain.ErrUnknownIndex) {
				p.warn(fmt.Errorf("syncLoop: non-attaching blocks: %w", err))
				return
			} else if err != nil {
				s.setErr(fmt.Errorf("syncLoop: couldn't add blocks: %w", err))
				return
			}
		}
		// if we reached the target index, we're done; otherwise, request more headers
		if sc.Contains(target) {
			return
		}
		history = []sunyata.ChainIndex{sc.ValidTip()}
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
