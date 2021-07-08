package p2p

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/internal/chainutil"
)

// helper for checking message responses
func testPeerResponse(t testing.TB, s *Syncer, m Message) Message {
	t.Helper()
	p := &Peer{cond: sync.Cond{L: new(sync.Mutex)}}
	resp := s.handleRequest(p, m)
	if s.err != nil {
		t.Fatal(s.err)
	}
	return resp
}

func TestSyncer(t *testing.T) {
	sim := chainutil.NewChainSim()

	n1 := newTestNode(t, sim.Genesis.Context.Index.ID, sim.Genesis)
	defer n1.Close()
	go n1.run()

	n2 := newTestNode(t, sim.Genesis.Context.Index.ID, sim.Genesis)
	defer n2.Close()
	go n2.run()

	if err := n2.s.Connect(n1.s.Addr()); err != nil {
		t.Fatal(err)
	}

	// give n1 a chain
	sim.MineBlocks(5)
	_ = testPeerResponse(t, n1.s, &MsgHeaders{
		Headers: chainutil.JustHeaders(sim.Chain),
	})
	_ = testPeerResponse(t, n1.s, &MsgBlocks{
		Blocks: sim.Chain,
	})

	// relay just the tip to n2; it should request the rest
	b := sim.Chain[len(sim.Chain)-1]
	n1.s.Broadcast(&MsgRelayBlock{b})

	time.Sleep(time.Second)
	if n1.c.Tip() != n2.c.Tip() {
		t.Fatal("peers did not sync:", n1.c.Tip(), n2.c.Tip())
	}
}

func TestMsgGetHeaders(t *testing.T) {
	sim := chainutil.NewChainSim()
	n := newTestNode(t, sim.Genesis.Context.Index.ID, sim.Genesis)

	// mine a chain
	sim.MineBlocks(100)
	for _, b := range sim.Chain {
		if err := n.c.AddTipBlock(b); err != nil {
			t.Fatal(err)
		}
	}

	headers := chainutil.JustHeaders(sim.Chain)
	tests := []struct {
		history Message
		exp     Message
	}{
		// empty; should reply with everything (except genesis)
		{&MsgGetHeaders{}, &MsgHeaders{Headers: headers}},
		// random headers; should reply with everything (except genesis)
		{
			&MsgGetHeaders{History: []sunyata.ChainIndex{
				{Height: 42, ID: sunyata.BlockID{1, 2, 3}},
				{Height: 14, ID: sunyata.BlockID{4, 5, 6}},
				{Height: 99, ID: sunyata.BlockID{9, 9, 9}},
			}},
			&MsgHeaders{Headers: headers},
		},
		// chainutil.Just tip; should reply with nothing
		{
			&MsgGetHeaders{History: []sunyata.ChainIndex{{
				Height: headers[len(headers)-1].Height,
				ID:     headers[len(headers)-1].ID(),
			}}},
			nil,
		},
		// halfway through; should reply with everything after
		{
			&MsgGetHeaders{History: []sunyata.ChainIndex{{
				Height: headers[len(headers)/2].Height,
				ID:     headers[len(headers)/2].ID(),
			}}},
			&MsgHeaders{Headers: headers[len(headers)/2+1:]},
		},
	}
	for _, test := range tests {
		resp := testPeerResponse(t, n.s, test.history)
		if !reflect.DeepEqual(resp, test.exp) {
			t.Errorf("\nexpected:\n\t%v\ngot:\n\t%v\n", test.exp, resp)
		}
	}
}

func TestMsgHeaders(t *testing.T) {
	sim := chainutil.NewChainSim()
	n := newTestNode(t, sim.Genesis.Context.Index.ID, sim.Genesis)

	// mine a chain
	sim.MineBlocks(5)
	headers := chainutil.JustHeaders(sim.Chain)

	// send a random header; should be ignored
	outbox := testPeerResponse(t, n.s, &MsgHeaders{
		Headers: []sunyata.BlockHeader{{Height: 4003, ParentID: sunyata.BlockID{1, 2, 3}}},
	})
	if outbox != nil {
		t.Fatal("expected empty outbox, got", outbox)
	}
	// send an orphan header; should be ignored
	outbox = testPeerResponse(t, n.s, &MsgHeaders{
		Headers: headers[len(headers)-1:],
	})
	if outbox != nil {
		t.Fatal("expected empty outbox, got", outbox)
	}
	// send a valid header; should request transactions
	outbox = testPeerResponse(t, n.s, &MsgHeaders{
		Headers: headers[0:1],
	})
	exp := &MsgGetBlocks{
		Blocks: []sunyata.ChainIndex{{Height: 1, ID: headers[0].ID()}},
	}
	if !reflect.DeepEqual(outbox, exp) {
		t.Errorf("\nexpected:\n\t%v\ngot:\n\t%v\n", exp, outbox)
	}
	// send next header; should request transactions for both blocks
	outbox = testPeerResponse(t, n.s, &MsgHeaders{
		Headers: headers[1:2],
	})
	exp = &MsgGetBlocks{
		Blocks: []sunyata.ChainIndex{
			{Height: 1, ID: headers[0].ID()},
			{Height: 2, ID: headers[1].ID()},
		},
	}
	if !reflect.DeepEqual(outbox, exp) {
		t.Errorf("\nexpected:\n\t%v\ngot:\n\t%v\n", exp, outbox)
	}
}

func TestMsgGetBlocks(t *testing.T) {
	sim := chainutil.NewChainSim()
	n := newTestNode(t, sim.Genesis.Context.Index.ID, sim.Genesis)

	// mine a chain
	sim.MineBlocks(100)
	for _, b := range sim.Chain {
		if err := n.c.AddTipBlock(b); err != nil {
			t.Fatal(err)
		}
	}

	// request a random ID; should return nothing
	outbox := testPeerResponse(t, n.s, &MsgGetBlocks{
		Blocks: []sunyata.ChainIndex{{
			Height: 0,
			ID:     sunyata.BlockID{1, 2, 3},
		}},
	})
	if outbox != nil {
		t.Fatal("expected nil, got", outbox)
	}
	// request an unknown block height; should return nothing
	outbox = testPeerResponse(t, n.s, &MsgGetBlocks{
		Blocks: []sunyata.ChainIndex{{
			Height: 4000,
			ID:     sim.Chain[1].ID(),
		}},
	})
	if outbox != nil {
		t.Fatal("expected nil, got", outbox)
	}
	// request a valid block; should return its transactions
	outbox = testPeerResponse(t, n.s, &MsgGetBlocks{
		Blocks: []sunyata.ChainIndex{{
			Height: sim.Chain[11].Header.Height,
			ID:     sim.Chain[11].ID(),
		}},
	})
	// NOTE: DeepEqual treats []T(nil) and []T{} differently, so load the
	// transactions from our Store instead of using the ones already in memory
	b12, _ := n.cs.Checkpoint(sim.Chain[11].Index())
	exp := &MsgBlocks{
		Blocks: []sunyata.Block{b12.Block},
	}
	if !reflect.DeepEqual(outbox, exp) {
		t.Errorf("\nexpected:\n\t%v\ngot:\n\t%v\n", exp, outbox)
	}
}

func TestMsgBlocks(t *testing.T) {
	sim := chainutil.NewChainSim()
	n := newTestNode(t, sim.Genesis.Context.Index.ID, sim.Genesis)

	// mine a chain
	sim.MineBlocks(100)

	// send a few headers; should request transactions
	outbox := testPeerResponse(t, n.s, &MsgHeaders{
		Headers: chainutil.JustHeaders(sim.Chain[:3]),
	})
	var exp Message = &MsgGetBlocks{
		Blocks: chainutil.JustChainIndexes(sim.Chain[:3]),
	}
	if !reflect.DeepEqual(outbox, exp) {
		t.Errorf("\nexpected:\n\t%#v\ngot:\n\t%#v\n", exp, outbox)
	}
	if n.c.Tip().Height != 0 {
		t.Fatal("should not have reorged yet")
	}

	// send transactions; should reorg
	outbox = testPeerResponse(t, n.s, &MsgBlocks{
		Blocks: sim.Chain[0:3],
	})
	exp = &MsgGetHeaders{
		History: chainutil.JustChainIndexes(sim.Chain[2:3]),
	}
	if !reflect.DeepEqual(outbox, exp) {
		t.Errorf("\nexpected:\n\t%#v\ngot:\n\t%#v\n", exp, outbox)
	}
	if n.c.Tip().Height != 3 {
		t.Fatal("should have reorged")
	}
}

func TestMsgRelayBlock(t *testing.T) {
	sim := chainutil.NewChainSim()
	n := newTestNode(t, sim.Genesis.Context.Index.ID, sim.Genesis)

	// add a dummy peer, for testing relay
	relay := &Peer{cond: sync.Cond{L: new(sync.Mutex)}}
	n.s.peers = append(n.s.peers, relay)

	// mine a block and relay it; should be accepted and relayed
	sim.MineBlocks(1)
	b := sim.Chain[0]
	resp := n.s.handleRequest(new(Peer), &MsgRelayBlock{b})
	if resp == nil {
		t.Fatal("expected relay")
	}

	// send the block again; should no-op
	outbox := testPeerResponse(t, n.s, &MsgRelayBlock{b})
	if outbox != nil {
		t.Fatal("expected empty outbox, got", outbox)
	}

	// mine to height 99
	sim.MineBlocks(98)
	for _, b := range sim.Chain[1:] {
		if err := n.c.AddTipBlock(b); err != nil {
			t.Fatal(err)
		}
	}

	// send an orphan header; should send history
	outbox = testPeerResponse(t, n.s, &MsgRelayBlock{
		Block: sunyata.Block{
			Header: sunyata.BlockHeader{Height: 5000},
		},
	})
	headers := chainutil.JustHeaders(sim.Chain)
	exp := &MsgGetHeaders{
		// last 10, then exponentially backwards
		History: []sunyata.ChainIndex{
			{Height: 99, ID: headers[99-1].ID()},
			{Height: 98, ID: headers[98-1].ID()},
			{Height: 97, ID: headers[97-1].ID()},
			{Height: 96, ID: headers[96-1].ID()},
			{Height: 95, ID: headers[95-1].ID()},
			{Height: 94, ID: headers[94-1].ID()},
			{Height: 93, ID: headers[93-1].ID()},
			{Height: 92, ID: headers[92-1].ID()},
			{Height: 91, ID: headers[91-1].ID()},
			{Height: 90, ID: headers[90-1].ID()},
			{Height: 88, ID: headers[88-1].ID()},
			{Height: 84, ID: headers[84-1].ID()},
			{Height: 76, ID: headers[76-1].ID()},
			{Height: 60, ID: headers[60-1].ID()},
			{Height: 28, ID: headers[28-1].ID()},
			{Height: 0, ID: sim.Genesis.Context.Index.ID},
		},
	}
	if !reflect.DeepEqual(outbox, exp) {
		t.Errorf("\nexpected:\n\t%v\ngot:\n\t%v\n", exp, outbox)
	}
}

func TestMultiplePeers(t *testing.T) {
	sim := chainutil.NewChainSim()
	n := newTestNode(t, sim.Genesis.Context.Index.ID, sim.Genesis)

	// mine a chain
	sim.MineBlocks(9)

	// send blocks to the syncer
	outbox := testPeerResponse(t, n.s, &MsgHeaders{
		Headers: chainutil.JustHeaders(sim.Chain),
	})
	if _, ok := outbox.(*MsgGetBlocks); !ok {
		t.Error("expected MsgGetBlocks")
	}
	outbox = testPeerResponse(t, n.s, &MsgBlocks{
		Blocks: sim.Chain,
	})
	if _, ok := outbox.(*MsgGetHeaders); !ok {
		t.Error("expected MsgGetHeaders")
	}

	// mine two diverging chains
	chain1 := sim.Fork()
	chain2 := sim.Fork()
	blocks1 := chain1.MineBlocks(5)
	blocks2 := chain2.MineBlocks(5)

	// ensure that chain2 has more work
	totalWork := func(blocks []sunyata.Block) (w sunyata.Work) {
		for _, b := range blocks {
			w = w.Add(sunyata.WorkRequiredForHash(b.ID()))
		}
		return
	}
	for totalWork(blocks1).Cmp(totalWork(blocks2)) >= 0 {
		blocks2 = append(blocks2, chain2.MineBlock())
	}

	// send headers2, but not blocks2
	outbox = testPeerResponse(t, n.s, &MsgHeaders{
		Headers: chainutil.JustHeaders(blocks2),
	})
	if _, ok := outbox.(*MsgGetBlocks); !ok {
		t.Error("expected MsgGetBlocks")
	}

	// send headers1, then blocks1
	outbox = testPeerResponse(t, n.s, &MsgHeaders{
		Headers: chainutil.JustHeaders(blocks1),
	})
	if _, ok := outbox.(*MsgGetBlocks); !ok {
		t.Error("expected MsgGetBlocks")
	}
	outbox = testPeerResponse(t, n.s, &MsgBlocks{
		Blocks: blocks1,
	})
	if _, ok := outbox.(*MsgGetHeaders); !ok {
		t.Error("expected MsgGetHeaders")
	}

	// should have reorged to chain1
	if n.c.Tip() != blocks1[len(blocks1)-1].Index() {
		t.Fatal("didn't reorg to chain1")
	}
	for _, b := range blocks1 {
		index, err := n.cs.BestIndex(b.Header.Height)
		if err != nil {
			t.Fatal(err)
		} else if index != b.Index() {
			t.Error("store does not contain chain1:", index, b.Index())
		}
	}

	// send blocks2
	outbox = testPeerResponse(t, n.s, &MsgBlocks{
		Blocks: blocks2,
	})
	if _, ok := outbox.(*MsgGetHeaders); !ok {
		t.Error("expected MsgGetHeaders")
	}

	// should have reorged to chain2
	if n.c.Tip() != blocks2[len(blocks2)-1].Index() {
		t.Fatal("didn't reorg to chain2")
	}
	for _, b := range blocks2 {
		index, err := n.cs.BestIndex(b.Header.Height)
		if err != nil {
			t.Fatal(err)
		} else if index != b.Index() {
			t.Error("store does not contain chain2:", index, b.Index())
		}
	}
}
