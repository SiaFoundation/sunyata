package p2p

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/consensus"
	"go.sia.tech/sunyata/internal/chainutil"
	"go.sia.tech/sunyata/internal/walletutil"
	"go.sia.tech/sunyata/miner"
	"go.sia.tech/sunyata/txpool"
	"go.sia.tech/sunyata/wallet"
)

type testNode struct {
	mining int32
	c      *chain.Manager
	cs     chain.ManagerStore
	tp     *txpool.Pool
	s      *Syncer
	w      *wallet.HotWallet
	m      *miner.Miner
}

func (tn *testNode) run() {
	go tn.mine()
	if err := tn.s.Run(); err != nil {
		panic(err)
	}
}

func (tn *testNode) Close() error {
	// signal miner to stop, and wait for it to exit
	if atomic.CompareAndSwapInt32(&tn.mining, 1, 2) {
		for atomic.LoadInt32(&tn.mining) != 3 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return tn.s.Close()
}

func (tn *testNode) send(amount sunyata.Currency, dest sunyata.Address) error {
	txn := sunyata.Transaction{
		Outputs: []sunyata.Beneficiary{{Value: amount, Address: dest}},
	}
	toSign, discard, err := tn.w.FundTransaction(&txn, amount, tn.tp.Transactions())
	if err != nil {
		return err
	}
	defer discard()
	if err := tn.w.SignTransaction(&txn, toSign); err != nil {
		return err
	}
	// give message to ourselves and to peers
	if err := tn.tp.AddTransaction(txn.DeepCopy()); err != nil {
		return err
	}
	tn.s.Broadcast(&MsgRelayTransactionSet{Transactions: []sunyata.Transaction{txn}})
	return nil
}

func (tn *testNode) startMining() bool { return atomic.CompareAndSwapInt32(&tn.mining, 0, 1) }
func (tn *testNode) stopMining() bool  { return atomic.CompareAndSwapInt32(&tn.mining, 1, 0) }

func (tn *testNode) mineBlock() error {
again:
	b := tn.m.MineBlock()

	err := tn.c.AddTipBlock(b)
	if errors.Is(err, chain.ErrUnknownIndex) {
		goto again
	} else if err != nil {
		return err
	}

	// broadcast it
	tn.s.Broadcast(&MsgRelayBlock{
		Header:       b.Header,
		Transactions: b.Transactions,
	})
	return nil
}

func (tn *testNode) mine() {
	for {
		// wait for permission
		for atomic.LoadInt32(&tn.mining) == 0 {
			time.Sleep(100 * time.Millisecond)
		}
		if atomic.CompareAndSwapInt32(&tn.mining, 2, 3) {
			return // shutdown
		}

		tn.mineBlock()
	}
}

func newTestNode(tb testing.TB, genesisID sunyata.BlockID, c consensus.Checkpoint) *testNode {
	cs := chainutil.NewEphemeralStore(c)
	cm := chain.NewManager(cs, c.Context)
	tp := txpool.New()
	cm.AddSubscriber(tp, cm.Tip())
	ws := walletutil.NewEphemeralStore()
	w := wallet.NewHotWallet(ws, wallet.NewSeed())
	cm.AddSubscriber(ws, cm.Tip())
	m := miner.New(c.Context, w.NextAddress(), tp, miner.CPU)
	cm.AddSubscriber(m, cm.Tip())
	s, err := NewSyncer(":0", genesisID, cm, tp)
	if err != nil {
		tb.Fatal(err)
	}
	return &testNode{
		c:  cm,
		cs: cs,
		tp: tp,
		s:  s,
		w:  w,
		m:  m,
	}
}

func TestNetwork(t *testing.T) {
	genesisBlock := sunyata.Block{
		Header: sunyata.BlockHeader{
			Timestamp: time.Unix(734600000, 0),
		},
	}
	sau := consensus.GenesisUpdate(genesisBlock, sunyata.Work{NumHashes: [32]byte{30: 1 << 2}})
	genesis := consensus.Checkpoint{
		Block:   genesisBlock,
		Context: sau.Context,
	}

	// create two nodes and connect them
	n1 := newTestNode(t, genesisBlock.ID(), genesis)
	defer n1.Close()
	go n1.run()
	n2 := newTestNode(t, genesisBlock.ID(), genesis)
	defer n2.Close()
	go n2.run()
	if err := n1.s.Connect(n2.s.Addr()); err != nil {
		t.Fatal(err)
	}

	// start mining on both nodes
	n1.startMining()
	n2.startMining()

	// simulate some chain activity by spamming simple txns, stopping when we reach height >= 100
	n1addr := n1.w.NextAddress()
	n2addr := n2.w.NextAddress()
	for n1.c.Tip().Height < 100 {
		time.Sleep(10 * time.Millisecond)
		go n1.send(sunyata.BaseUnitsPerCoin.Mul64(7), n2addr)
		go n2.send(sunyata.BaseUnitsPerCoin.Mul64(9), n1addr)
	}
	n1.stopMining()
	n2.stopMining()

	// disconnect and reconnect to trigger resync
	time.Sleep(10 * time.Millisecond)
	for _, p := range n1.s.Peers() {
		n1.s.Disconnect(p.String())
	}
	for _, p := range n2.s.Peers() {
		n2.s.Disconnect(p.String())
	}
	if err := n1.s.Connect(n2.s.Addr()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n1.c.Tip() != n2.c.Tip() {
		t.Fatal("nodes not synchronized", "\n", n1.s.Addr(), n1.c.Tip(), "\n", n2.s.Addr(), n2.c.Tip())
	}
}

func TestConnect(t *testing.T) {
	genesisBlock := sunyata.Block{
		Header: sunyata.BlockHeader{
			Timestamp: time.Unix(734600000, 0),
		},
	}
	sau := consensus.GenesisUpdate(genesisBlock, sunyata.Work{NumHashes: [32]byte{30: 1 << 2}})
	genesis := consensus.Checkpoint{
		Block:   genesisBlock,
		Context: sau.Context,
	}

	n := newTestNode(t, genesisBlock.ID(), genesis)
	defer n.Close()
	go n.run()

	if len(n.s.Peers()) != 0 {
		t.Fatal("expected 0 peers, got", len(n.s.Peers()))
	}

	// connect a peer
	handshake := n.s.handshake
	rand.Read(handshake.Key[:])
	conn, err := net.Dial("tcp", n.s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	writeHandshake(conn, handshake)
	time.Sleep(50 * time.Millisecond)
	if len(n.s.Peers()) != 1 {
		t.Fatal("expected 1 peer, got", len(n.s.Peers()))
	}

	// send garbage; router should disconnect
	conn.Write([]byte("foo bar baz"))
	time.Sleep(50 * time.Millisecond)
	if len(n.s.Peers()) != 0 {
		t.Fatal("expected 0 peers, got", len(n.s.Peers()))
	}

	// redial and send an incomplete message
	conn, err = net.Dial("tcp", n.s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	writeHandshake(conn, handshake)
	buf := make([]byte, 5)
	buf[0] = typGetHeaders
	binary.LittleEndian.PutUint32(buf[1:], 0)
	conn.Write(buf)
	conn.Close()
	time.Sleep(50 * time.Millisecond)
	if len(n.s.Peers()) != 0 {
		t.Fatal("expected 0 peers, got", len(n.s.Peers()))
	}
}

func TestCheckpoint(t *testing.T) {
	genesisBlock := sunyata.Block{
		Header: sunyata.BlockHeader{
			Timestamp: time.Unix(734600000, 0),
		},
	}
	sau := consensus.GenesisUpdate(genesisBlock, sunyata.Work{NumHashes: [32]byte{30: 1 << 2}})
	genesis := consensus.Checkpoint{
		Block:   genesisBlock,
		Context: sau.Context,
	}

	// create a node and mine some blocks
	n1 := newTestNode(t, genesisBlock.ID(), genesis)
	defer n1.Close()
	go n1.run()
	for i := 0; i < 10; i++ {
		if err := n1.mineBlock(); err != nil {
			t.Fatal(err)
		}
	}

	// download a checkpoint and use it to initialize a new node
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	checkpointIndex := n1.c.Tip()
	checkpoint, err := DownloadCheckpoint(ctx, n1.s.Addr(), genesisBlock.ID(), checkpointIndex)
	if err != nil {
		t.Fatal(err)
	}
	n2 := newTestNode(t, genesisBlock.ID(), checkpoint)
	defer n2.Close()
	go n2.run()
	if n2.c.Tip() != n1.c.Tip() {
		t.Fatal("tips should match after loading from checkpoint")
	}

	// connect the nodes and have n2 mine some blocks
	if err := n1.s.Connect(n2.s.Addr()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := n2.mineBlock(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	// tips should match
	if n1.c.Tip() != n2.c.Tip() {
		t.Fatal("tips should match after mining on checkpoint")
	}
}
