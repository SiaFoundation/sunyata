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

	time.Sleep(500 * time.Millisecond)
	if n1.c.Tip() != n2.c.Tip() {
		t.Fatal("peers did not sync:", n1.c.Tip(), n2.c.Tip())
	}

	// relay old block - shouldn't change anything
	oldTip := n1.c.Tip()
	b = sim.Chain[len(sim.Chain)-2]
	n1.s.Broadcast(&MsgRelayBlock{b})

	time.Sleep(500 * time.Millisecond)
	if n1.c.Tip() != oldTip || n2.c.Tip() != oldTip {
		t.Fatalf("tip went backwards (expected: %v): %v %v", oldTip, n1.c.Tip(), n2.c.Tip())
	}

	// relay invalid block - shouldn't change anything
	b = sim.Chain[len(sim.Chain)-1]
	b.Header.Height = 999

	n1.s.Broadcast(&MsgRelayBlock{b})

	time.Sleep(500 * time.Millisecond)
	if n1.c.Tip() != oldTip || n2.c.Tip() != oldTip {
		t.Fatalf("accepted invalid tip (expected: %v): %v %v", oldTip, n1.c.Tip(), n2.c.Tip())
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
		{
			&MsgGetHeaders{},
			&MsgHeaders{Headers: headers},
		},
		// random headers; should reply with everything (except genesis)
		{
			&MsgGetHeaders{History: []sunyata.ChainIndex{
				{Height: 42, ID: sunyata.BlockID{1, 2, 3}},
				{Height: 14, ID: sunyata.BlockID{4, 5, 6}},
				{Height: 99, ID: sunyata.BlockID{9, 9, 9}},
			}},
			&MsgHeaders{Headers: headers},
		},
		// just tip; should reply with nothing
		{
			&MsgGetHeaders{History: []sunyata.ChainIndex{
				headers[len(headers)-1].Index(),
			}},
			&MsgHeaders{Headers: []sunyata.BlockHeader{}},
		},
		// halfway through; should reply with everything after
		{
			&MsgGetHeaders{History: []sunyata.ChainIndex{
				headers[len(headers)/2].Index(),
			}},
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
	exp := &MsgBlocks{Blocks: nil}
	if !reflect.DeepEqual(outbox, exp) {
		t.Errorf("\nexpected:\n\t%v\ngot:\n\t%v\n", exp, outbox)
	}
	// request an unknown block height; should return nothing
	outbox = testPeerResponse(t, n.s, &MsgGetBlocks{
		Blocks: []sunyata.ChainIndex{{
			Height: 4000,
			ID:     sim.Chain[1].ID(),
		}},
	})
	if !reflect.DeepEqual(outbox, exp) {
		t.Errorf("\nexpected:\n\t%v\ngot:\n\t%v\n", exp, outbox)
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
	exp = &MsgBlocks{Blocks: []sunyata.Block{b12.Block}}
	if !reflect.DeepEqual(outbox, exp) {
		t.Errorf("\nexpected:\n\t%v\ngot:\n\t%v\n", exp, outbox)
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
}
