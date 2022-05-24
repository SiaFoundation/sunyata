package p2p

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"go.sia.tech/sunyata"
)

// sentinel error for shutdown
var errClosing = errors.New("closing")

var errAlreadyConnected = errors.New("already connected to that peer")

type peerHandshake struct {
	Version uint8
	Genesis sunyata.BlockID
	Tip     sunyata.ChainIndex
	Key     [32]byte
}

func writeHandshake(w io.Writer, m peerHandshake) error {
	e := sunyata.NewEncoder(w)
	e.Write([]byte("空"))
	e.WriteUint8(m.Version)
	m.Genesis.EncodeTo(e)
	m.Tip.EncodeTo(e)
	e.Write(m.Key[:])
	return e.Flush()
}

func readHandshake(r io.Reader) (peerHandshake, error) {
	d := sunyata.NewDecoder(io.LimitedReader{R: r, N: 1e6})
	var magic [3]byte
	d.Read(magic[:])
	if string(magic[:]) != "空" {
		return peerHandshake{}, errors.New("wrong magic bytes")
	}
	var m peerHandshake
	m.Version = d.ReadUint8()
	m.Genesis.DecodeFrom(d)
	m.Tip.DecodeFrom(d)
	d.Read(m.Key[:])
	return m, d.Err()
}

// A Peer is another node on the network that the Syncer is connected to.
type Peer struct {
	conn      net.Conn
	handshake peerHandshake
	in        []taggedMessage
	out       []taggedMessage
	nextID    uint64
	calls     map[uint64]*Call
	err       error
	mu        sync.Mutex
	cond      sync.Cond // shares mu
}

// String returns the peer's address.
func (p *Peer) String() string {
	return p.conn.RemoteAddr().String()
}

func (p *Peer) setErr(err error) error {
	if p.err == nil {
		p.err = err
		for _, c := range p.calls {
			c.err = err
			c.done = true
		}
		p.conn.Close()     // wake p.handleIn
		p.cond.Broadcast() // wake p.handleOut and (Call).Wait
	}
	return p.err
}

func (p *Peer) ban(err error) {
	// TODO: blacklist IP
	p.setErr(fmt.Errorf("banned: %w", err))
}

func (p *Peer) warn(err error) {
	// TODO: currently, peers send too many innocent-but-invalid messages,
	// causing them to be banned during tests (leading to spurious failures)

	// buf := make([]byte, 1)
	// if _, err := rand.Read(buf[:]); err != nil {
	// 	p.setErr(fmt.Errorf("couldn't read entropy: %w", err))
	// 	return
	// }
	// // 1-in-16 chance of banning peer
	// if buf[0] < 16 {
	// 	p.ban(err)
	// }
}

func (p *Peer) handleIn(wakeSyncer func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		// release mutex to read header
		p.mu.Unlock()
		p.conn.SetDeadline(time.Now().Add(time.Hour))
		typ, id, err := readMessageHeader(p.conn)
		p.mu.Lock()
		if err != nil {
			p.setErr(fmt.Errorf("couldn't read message header: %w", err))
			return
		}

		isReq := id&1 == 0
		var m Message
		if isReq {
			m = newMsg(typ)
		} else if c, ok := p.calls[id]; ok {
			m = c.resp
			if typ != msgType(m) {
				c.done = true
				delete(p.calls, id)
				p.setErr(fmt.Errorf("peer sent wrong response type (expected %T, got %T)", msgType(m), typ))
				return
			}
		} else {
			p.setErr(fmt.Errorf("peer sent unsolicited %T", newMsg(typ)))
			return
		}

		// release mutex to read message body
		p.mu.Unlock()
		err = readMessage(p.conn, m)
		p.mu.Lock()
		if err != nil {
			p.setErr(fmt.Errorf("couldn't read %T: %w", m, err))
			return
		}
		if isReq {
			p.in = append(p.in, taggedMessage{id, m})
			wakeSyncer()
		} else {
			c := p.calls[id]
			c.done = true
			delete(p.calls, id)
			p.cond.Broadcast() // wake (Call).Wait
		}
	}
}

func (p *Peer) handleOut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.err == nil {
		// NOTE: since we release the mutex during the write, we must process
		// messages one at a time (rather than iterating over p.out)
		if len(p.out) == 0 {
			p.cond.Wait()
			continue
		}
		tm := p.out[0]
		p.out = p.out[1:]
		p.mu.Unlock()
		err := writeMessage(p.conn, tm)
		p.mu.Lock()
		if err != nil {
			p.setErr(fmt.Errorf("couldn't write %T: %w", tm.m, err))
			return
		}
	}
}

func (p *Peer) disconnect() {
	p.cond.L.Lock()
	p.setErr(errClosing)
	p.cond.L.Unlock()
}

func (p *Peer) newID() uint64 {
	id := p.nextID
	p.nextID += 2
	return id
}

type Call struct {
	id   uint64
	done bool
	resp Message
	err  error
	p    *Peer
}

func (c *Call) Wait() (err error) {
	p := c.p
	p.mu.Lock()
	defer p.mu.Unlock()
	for !c.done {
		p.cond.Wait()
	}
	return c.err
}

func (c *Call) Cancel() {
	p := c.p
	p.mu.Lock()
	defer p.mu.Unlock()
	c.err = errors.New("canceled")
	delete(p.calls, c.id)
	p.cond.Broadcast() // wake c.Wait
}

func (p *Peer) RPC(req, resp Message) *Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	reqID := p.newID()
	respID := reqID | 1
	p.out = append(p.out, taggedMessage{reqID, req})
	p.cond.Broadcast() // wake p.handleOut
	call := &Call{
		id:   respID,
		resp: resp,
		p:    p,
	}
	p.calls[respID] = call
	return call
}

// Connect connects to the peer at the specified address, returning an error if
// the dial fails or if a connection to the peer has already been established.
func (s *Syncer) Connect(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("couldn't establish connection to peer: %w", err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	// complete handshake
	s.mu.Lock()
	ours := s.handshake
	ours.Tip = s.cm.Tip()
	s.mu.Unlock()
	if err := writeHandshake(conn, ours); err != nil {
		conn.Close()
		return fmt.Errorf("couldn't send our handshake: %w", err)
	}
	theirs, err := readHandshake(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("couldn't read peer's handshake: %w", err)
	} else if err := s.addPeer(conn, ours, theirs); err != nil {
		conn.Close()
		return err
	}
	// wake syncLoop
	s.cond.Broadcast()
	return nil
}

func (s *Syncer) acceptConnection(conn net.Conn) error {
	s.mu.Lock()
	ours := s.handshake
	ours.Tip = s.cm.Tip()
	s.mu.Unlock()
	theirs, err := readHandshake(conn)
	if err != nil {
		return fmt.Errorf("couldn't read peer's handshake: %w", err)
	} else if theirs.Genesis != ours.Genesis {
		return fmt.Errorf("incompatible peer: different genesis block (ours: %v, theirs: %v)", ours.Genesis, theirs.Genesis)
	} else if theirs.Version != ours.Version {
		return fmt.Errorf("incompatible peer: different protocol version (ours: %v, theirs: %v)", ours.Version, theirs.Version)
	}
	if err := writeHandshake(conn, ours); err != nil {
		return fmt.Errorf("couldn't send our handshake: %w", err)
	}
	if err := s.addPeer(conn, ours, theirs); err != nil {
		return err
	}

	// wake syncLoop
	s.cond.Broadcast()
	return nil
}

// Disconnect disconnects from the peer at the specified address. It returns
// false if no connection was found.
func (s *Syncer) Disconnect(addr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.String() == addr {
			p.disconnect()
			s.removeDisconnected()
			return true
		}
	}
	return false
}

// Peers returns a list of all connected peers.
func (s *Syncer) Peers() []*Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeDisconnected()
	return s.peers
}

// Broadcast broadcasts a Message to all connected peers.
func (s *Syncer) Broadcast(m Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.relay(nil, m)
}

// relay broadcasts a Message to all connected peers except p.
func (s *Syncer) relay(orig *Peer, m Message) {
	for _, p := range s.peers {
		if p != orig {
			p.mu.Lock()
			p.out = append(p.out, taggedMessage{p.newID(), m})
			p.cond.Broadcast() // wake p.handleOut
			p.mu.Unlock()
		}
	}
}

// lazy disconnect helper; called whenever we need the current peer list
func (s *Syncer) removeDisconnected() {
	rem := s.peers[:0]
	for _, p := range s.peers {
		p.cond.L.Lock()
		if p.err == nil {
			rem = append(rem, p)
		}
		p.cond.L.Unlock()
	}
	s.peers = rem
}

func (s *Syncer) nextRequest() (*Peer, taggedMessage) {
	for _, p := range s.peers {
		p.mu.Lock()
		if len(p.in) > 0 {
			m := p.in[0]
			p.in = p.in[1:]
			p.mu.Unlock()
			return p, m
		}
		p.mu.Unlock()
	}
	return nil, taggedMessage{}
}

func (s *Syncer) addPeer(conn net.Conn, ours, theirs peerHandshake) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if theirs.Key == ours.Key {
		return errors.New("refusing to connect to ourself")
	} else if theirs.Genesis != ours.Genesis {
		return fmt.Errorf("incompatible peer: different genesis block (ours: %v, theirs: %v)", ours.Genesis, theirs.Genesis)
	} else if theirs.Version != ours.Version {
		return fmt.Errorf("incompatible peer: different protocol version (ours: %v, theirs: %v)", ours.Version, theirs.Version)
	}
	for _, p := range s.peers {
		if p.handshake.Key == theirs.Key {
			return errAlreadyConnected
		}
	}

	p := &Peer{
		conn:      conn,
		handshake: theirs,
		calls:     make(map[uint64]*Call),
	}
	p.cond.L = &p.mu
	s.peers = append(s.peers, p)
	go p.handleIn(s.cond.Broadcast)
	go p.handleOut()

	return nil
}
