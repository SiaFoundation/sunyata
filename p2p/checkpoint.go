package p2p

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/consensus"
)

// DownloadCheckpoint connects to addr and downloads the checkpoint at the
// specified index.
func DownloadCheckpoint(ctx context.Context, addr string, genesisID sunyata.BlockID, index sunyata.ChainIndex) (consensus.Checkpoint, error) {
	// construct handshake
	ours := peerHandshake{
		Version: 1,
		Genesis: genesisID,
		Tip:     sunyata.ChainIndex{Height: 0, ID: genesisID},
	}
	if _, err := rand.Read(ours.Key[:]); err != nil {
		return consensus.Checkpoint{}, fmt.Errorf("couldn't generate encryption key: %w", err)
	}

	// dial peer
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return consensus.Checkpoint{}, fmt.Errorf("couldn't establish connection to peer: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	// perform handshake
	if err := writeHandshake(conn, ours); err != nil {
		return consensus.Checkpoint{}, fmt.Errorf("couldn't send our handshake: %w", err)
	}
	theirs, err := readHandshake(conn)
	if err != nil {
		return consensus.Checkpoint{}, fmt.Errorf("couldn't read peer's handshake: %w", err)
	} else if theirs.Genesis != ours.Genesis {
		return consensus.Checkpoint{}, fmt.Errorf("incompatible peer: different genesis block (ours: %v, theirs: %v)", ours.Genesis, theirs.Genesis)
	} else if theirs.Version != ours.Version {
		return consensus.Checkpoint{}, fmt.Errorf("incompatible peer: different protocol version (ours: %v, theirs: %v)", ours.Version, theirs.Version)
	}

	// request checkpoint
	p := &Peer{
		conn:      conn,
		handshake: theirs,
		calls:     make(map[uint32]*Call),
	}
	p.cond.L = &p.mu
	defer p.disconnect()
	go p.handleIn(func() {})
	go p.handleOut()

	var resp MsgCheckpoint
	if err := p.RPC(&MsgGetCheckpoint{Index: index}, &resp).Wait(); err != nil {
		return consensus.Checkpoint{}, fmt.Errorf("RPC failed: %w", err)
	}

	// validate response
	if resp.Block.Index() != index || resp.Block.Header.ParentIndex() != resp.ParentContext.Index {
		return consensus.Checkpoint{}, errors.New("wrong checkpoint header")
	}
	commitment := resp.ParentContext.Commitment(resp.Block.Header.MinerAddress, resp.Block.Transactions)
	if commitment != resp.Block.Header.Commitment {
		return consensus.Checkpoint{}, errors.New("wrong checkpoint commitment")
	}

	return consensus.Checkpoint{
		Block:   resp.Block,
		Context: consensus.ApplyBlock(resp.ParentContext, resp.Block).Context,
	}, nil
}
