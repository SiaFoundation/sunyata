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
	if err := writeMessage(conn, &MsgGetCheckpoint{Index: index}); err != nil {
		return consensus.Checkpoint{}, fmt.Errorf("couldn't write message: %w", err)
	}

	// read response
	//
	// NOTE: peer might send us relay or sync messages, so wait for up to three
	// messages before giving up
	var msg *MsgCheckpoint
	for i := 0; ; i++ {
		m, err := readMessage(conn)
		if err != nil {
			return consensus.Checkpoint{}, fmt.Errorf("couldn't read message: %w", err)
		}
		var ok bool
		msg, ok = m.(*MsgCheckpoint)
		if ok {
			break
		} else if i > 2 {
			return consensus.Checkpoint{}, fmt.Errorf("peer sent wrong message type (%T)", m)
		}
	}

	// validate response
	if msg.Block.Index() != index || msg.Block.Header.ParentIndex() != msg.ParentContext.Index {
		return consensus.Checkpoint{}, errors.New("wrong checkpoint header")
	}
	commitment := msg.ParentContext.Commitment(msg.Block.Header.MinerAddress, msg.Block.Transactions)
	if commitment != msg.Block.Header.Commitment {
		return consensus.Checkpoint{}, errors.New("wrong checkpoint commitment")
	}

	return consensus.Checkpoint{
		Block:   msg.Block,
		Context: consensus.ApplyBlock(msg.ParentContext, msg.Block).Context,
	}, nil
}
