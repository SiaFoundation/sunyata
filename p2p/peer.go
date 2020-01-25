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
	var b msgBuffer
	b.buf.WriteString("空")
	b.buf.WriteByte(m.Version)
	b.write(m.Genesis[:])
	(*msgChainIndex)(&m.Tip).encodeTo(&b)
	b.write(m.Key[:])
	_, err := w.Write(b.buf.Bytes())
	return err
}

func readHandshake(r io.Reader) (peerHandshake, error) {
	buf := make([]byte, 3+1+32+msgChainIndexSize+32)
	if _, err := io.ReadFull(r, buf); err != nil {
		return peerHandshake{}, err
	}
	var b msgBuffer
	b.write(buf)

	var m peerHandshake
	if string(b.buf.Next(3)) != "空" {
		return peerHandshake{}, errors.New("wrong magic bytes")
	}
	m.Version, _ = b.buf.ReadByte()
	b.read(m.Genesis[:])
	(*msgChainIndex)(&m.Tip).decodeFrom(&b)
	b.read(m.Key[:])
	return m, b.err
}

// A Peer is another node on the network that the Syncer is connected to.
type Peer struct {
	conn      net.Conn
	handshake peerHandshake
	in        []Message
	out       []Message
	err       error
	cond      sync.Cond
}

// String returns the peer's address.
func (p *Peer) String() string {
	return p.conn.RemoteAddr().String()
}

func (p *Peer) setErr(err error) error {
	if p.err == nil {
		p.err = err
		p.conn.Close()  // wake p.handleIn
		p.cond.Signal() // wake p.handleOut
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
	for {
		p.conn.SetDeadline(time.Now().Add(time.Hour))
		m, err := readMessage(p.conn)
		if err != nil {
			p.cond.L.Lock()
			p.setErr(fmt.Errorf("couldn't read message: %w", err))
			p.cond.L.Unlock()
			return
		}
		p.cond.L.Lock()
		p.in = append(p.in, m)
		p.cond.L.Unlock()
		wakeSyncer()
	}
}

func (p *Peer) handleOut() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	for p.err == nil {
		if len(p.out) == 0 {
			p.cond.Wait()
			continue
		}
		m := p.out[0]
		p.out = p.out[1:]
		// release mutex during write
		p.cond.L.Unlock()
		err := writeMessage(p.conn, m)
		p.cond.L.Lock()
		if err != nil {
			p.setErr(fmt.Errorf("couldn't write message: %w", err))
		}
	}
}

func (p *Peer) disconnect() {
	p.cond.L.Lock()
	p.setErr(errClosing)
	p.cond.L.Unlock()
}

func (p *Peer) queue(m Message) {
	p.cond.L.Lock()
	p.out = append(p.out, m)
	p.cond.Signal() // wake p.handleOut
	p.cond.L.Unlock()
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
	for _, p := range s.peers {
		p.cond.L.Lock()
		p.out = append(p.out, m)
		p.cond.L.Unlock()
		p.cond.Signal() // wake p.handleOut
	}
}

// relay broadcasts a Message to all connected peers except p.
func (s *Syncer) relay(orig *Peer, m Message) {
	for _, p := range s.peers {
		if p == orig {
			continue
		}
		p.queue(m)
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

func (s *Syncer) nextMessage() (*Peer, Message) {
	for _, p := range s.peers {
		p.cond.L.Lock()
		if len(p.in) > 0 {
			m := p.in[0]
			p.in = p.in[1:]
			p.cond.L.Unlock()
			return p, m
		}
		p.cond.L.Unlock()
	}
	return nil, nil
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

	// TODO: derive shared secret and encrypt connection

	p := &Peer{
		conn:      conn,
		handshake: theirs,
		cond:      sync.Cond{L: new(sync.Mutex)},
	}
	s.peers = append(s.peers, p)
	go p.handleIn(s.cond.Broadcast)
	go p.handleOut()
	return nil
}
