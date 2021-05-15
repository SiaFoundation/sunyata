package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.sia.tech/sunyata"
	"go.sia.tech/sunyata/chain"
	"go.sia.tech/sunyata/consensus"
	"go.sia.tech/sunyata/internal/chainutil"
	"go.sia.tech/sunyata/internal/walletutil"
	"go.sia.tech/sunyata/miner"
	"go.sia.tech/sunyata/p2p"
	"go.sia.tech/sunyata/txpool"
	"go.sia.tech/sunyata/wallet"
)

var (
	genesisTxns  = []sunyata.Transaction{}
	genesisBlock = sunyata.Block{
		Header: sunyata.BlockHeader{
			Timestamp: time.Unix(734600000, 0),
		},
		Transactions: genesisTxns,
	}
	genesisUpdate = consensus.GenesisUpdate(genesisBlock, sunyata.Work{NumHashes: [32]byte{29: 1 << 4}})
	genesis       = consensus.Checkpoint{Block: genesisBlock, Context: genesisUpdate.Context}
)

func die(context string, err error) {
	if err != nil {
		log.Fatalf("%v: %v", context, err)
	}
}

func main() {
	log.SetFlags(0)
	addr := flag.String("addr", ":0", "address to listen on")
	peer := flag.String("peer", "", "initial peer to connect to")
	dir := flag.String("dir", "", "directory to store node state in")
	checkpoint := flag.String("checkpoint", "", "checkpoint to sync from")
	seed := flag.String("seed", "", "wallet seed to use")
	flag.Parse()

	log.Println("空 sunyata v0.5.0")

	if *dir == "" {
		tmpdir, err := os.MkdirTemp(os.TempDir(), "sunyata")
		if err != nil {
			log.Fatal(err)
		}
		log.Println("Using tempdir", tmpdir)
		*dir = tmpdir
	}

	initCheckpoint := genesis
	if *checkpoint != "" {
		index, ok := parseIndex(*checkpoint)
		if !ok {
			log.Fatal("Invalid checkpoint")
		}
		if *peer == "" {
			log.Fatal("Must specify -peer to download checkpoint from")
		}
		fmt.Printf("Downloading checkpoint %v from %v...", index, *peer)
		c, err := p2p.DownloadCheckpoint(context.Background(), *peer, genesisBlock.ID(), index)
		if err != nil {
			fmt.Println()
			log.Fatal(err)
		}
		fmt.Println("Success!")
		initCheckpoint = c

		// overwrite existing chain store
		if err := os.RemoveAll(filepath.Join(*dir, "chain")); err != nil {
			log.Fatal(err)
		}
	}

	if *seed == "" {
		*seed = wallet.NewSeed().String()
		log.Println("Using wallet seed", *seed)
	}

	n, err := newNode(*addr, *dir, *seed, initCheckpoint)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Listening on", n.s.Addr())

	if *peer != "" {
		fmt.Printf("Connecting to %s...", *peer)
		if err := n.s.Connect(*peer); err != nil {
			fmt.Println("Failed:", err)
		} else {
			fmt.Println("Success!")
		}
	}

	prompt := bufio.NewReader(os.Stdin)
	go n.run()
	for {
		fmt.Print("空> ")
		line, err := prompt.ReadString('\n')
		if err == io.EOF {
			break
		} else if err != nil {
			die("Couldn't read input", err)
		} else if len(line) == 0 {
			continue
		}

		args := strings.Fields(line)
		if len(args) == 0 {
			continue
		}
		switch args[0] {
		case "help":
			fmt.Println(`
Available commands:

    connect [addr]           connect to a peer
    disconnect [addr]        disconnect from a peer
    peers                    list peers

    height                   print current height
    checkpoint [height]      print checkpoint for height

    miner start              start mining
    miner stop               stop mining
    miner status             print mining stats

    balance                  print wallet balance
    addr                     generate a wallet address
    send [amount] [dest]     send coins to an address
    txns                     list transactions relevant to wallet
`[1:])
		case "connect":
			if len(args) < 2 {
				fmt.Println("missing arg")
				continue
			}
			if err := n.s.Connect(args[1]); err != nil {
				fmt.Println(err)
			} else {
				fmt.Println("Connected to", args[1])
			}
		case "disconnect":
			if len(args) < 2 {
				fmt.Println("missing arg")
				continue
			}
			if !n.s.Disconnect(args[1]) {
				fmt.Println("Not connected to", args[1])
			} else {
				fmt.Println("Disconnected from", args[1])
			}
		case "peers":
			peers := n.s.Peers()
			if len(peers) == 0 {
				fmt.Println("No peers")
			} else {
				for _, p := range peers {
					fmt.Println(p)
				}
			}
		case "miner":
			if len(args) < 2 {
				fmt.Println("missing arg")
				continue
			}
			switch args[1] {
			case "start":
				if n.startMining() {
					fmt.Println("Started mining")
				} else {
					fmt.Println("Already mining")
				}
			case "stop":
				if n.stopMining() {
					fmt.Println("Stopped mining")
				} else {
					fmt.Println("Not mining")
				}
			case "status":
				n, rate := n.m.Stats()
				fmt.Printf("Mined %v blocks (%.2f/sec)\n", n, rate)
			default:
				fmt.Println("unknown command")
			}
		case "height":
			fmt.Println(n.tt.Height())
		case "checkpoint":
			if len(args) < 2 {
				fmt.Println("missing arg")
				continue
			}
			height, err := strconv.ParseUint(args[1], 10, 64)
			if err != nil {
				fmt.Println(err)
				continue
			}
			index, err := n.cs.BestIndex(height)
			if err != nil {
				fmt.Println(err)
				continue
			}
			fmt.Printf("%v::%x\n", index.Height, index.ID[:])
		case "balance":
			fmt.Println(n.w.Balance(), "C")
		case "addr":
			fmt.Println(n.w.NextAddress())
		case "send":
			if len(args) < 3 {
				fmt.Println("missing arg(s)")
				continue
			}
			if err := n.send(args[1], args[2]); err != nil {
				fmt.Println(err)
			} else {
				fmt.Printf("Sent %v to %v\n", args[1], args[2])
			}
		case "txns":
			if txns := n.w.Transactions(); len(txns) == 0 {
				fmt.Println("No transactions to show")
			} else {
				fmt.Println("Height        Delta    ID")
				for _, txn := range txns {
					var delta string
					switch txn.Inflow.Cmp(txn.Outflow) {
					case 1:
						delta = "+" + txn.Inflow.Sub(txn.Outflow).String()
					case 0:
						delta = "0"
					case -1:
						delta = "-" + txn.Outflow.Sub(txn.Inflow).String()
					}
					fmt.Printf("%6v    %7v C    %8v\n", txn.Index.Height, delta, txn.ID)
				}
			}
		default:
			fmt.Println("unknown command (try running 'help')")
		}
	}
	if err := n.Close(); err != nil {
		log.Println("Error shutting down:", err)
	}
}

func newNode(addr, dir, seedStr string, c consensus.Checkpoint) (*node, error) {
	chainDir := filepath.Join(dir, "chain")
	if err := os.MkdirAll(chainDir, 0700); err != nil {
		return nil, err
	}
	chainStore, tip, err := chainutil.NewFlatStore(chainDir, c)
	if err != nil {
		return nil, err
	}

	walletDir := filepath.Join(dir, "wallet")
	if err := os.MkdirAll(walletDir, 0700); err != nil {
		return nil, err
	}
	walletStore, walletTip, err := walletutil.NewJSONStore(walletDir, c.Context)
	if err != nil {
		return nil, err
	}
	seed, err := wallet.SeedFromString(seedStr)
	if err != nil {
		return nil, err
	}

	cm := chain.NewManager(chainStore, tip.Context)
	tp := txpool.New()
	cm.AddSubscriber(tp, cm.Tip())
	if err := cm.AddSubscriber(walletStore, walletTip); err != nil {
		return nil, err
	}
	w := wallet.NewHotWallet(walletStore, seed)
	m := miner.New(tip.Context, w.NextAddress(), tp, miner.CPU)
	cm.AddSubscriber(m, cm.Tip())

	tt := newTipTracker(cm.Tip())
	cm.AddSubscriber(tt, cm.Tip())

	s, err := p2p.NewSyncer(addr, genesisBlock.ID(), cm, tp)
	if err != nil {
		return nil, err
	}

	return &node{
		c:  cm,
		cs: chainStore,
		tp: tp,
		s:  s,
		w:  w,
		m:  m,
		tt: tt,
	}, nil
}

type node struct {
	mining int32
	c      *chain.Manager
	cs     *chainutil.FlatStore
	tp     *txpool.Pool
	s      *p2p.Syncer
	w      *wallet.HotWallet
	m      *miner.Miner
	tt     *tipTracker
}

func (d *node) run() {
	go d.mine()
	if err := d.s.Run(); err != nil {
		die("Fatal error", err)
	}
}

func (d *node) Close() error {
	// signal miner to stop, but don't bother waiting for it to exit
	atomic.StoreInt32(&d.mining, 2)
	errs := []error{
		d.s.Close(),
		d.cs.Close(),
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *node) send(amountStr string, destStr string) error {
	amountInt, err := strconv.ParseUint(amountStr, 10, 64)
	if err != nil {
		return err
	}
	amount := sunyata.BaseUnitsPerCoin.Mul64(uint64(amountInt))
	destBytes, err := hex.DecodeString(strings.TrimPrefix(destStr, "addr:"))
	if err != nil {
		return err
	} else if len(destBytes) != 32 {
		return errors.New("invalid address length")
	}
	var dest sunyata.Address
	copy(dest[:], destBytes)

	txn := sunyata.Transaction{
		Outputs: []sunyata.Beneficiary{{Value: amount, Address: dest}},
	}
	toSign, discard, err := d.w.FundTransaction(&txn, amount, d.tp.Transactions())
	if err != nil {
		return err
	}
	defer discard()
	if err := d.w.SignTransaction(&txn, toSign); err != nil {
		return err
	}
	// give message to ourselves and to peers
	if err := d.tp.AddTransaction(txn.DeepCopy()); err != nil {
		return fmt.Errorf("txpool rejected transaction: %w", err)
	}
	d.s.Broadcast(&p2p.MsgRelayTransactionSet{Transactions: []sunyata.Transaction{txn}})
	return nil
}

func (d *node) startMining() bool { return atomic.CompareAndSwapInt32(&d.mining, 0, 1) }
func (d *node) stopMining() bool  { return atomic.CompareAndSwapInt32(&d.mining, 1, 0) }

func (d *node) mine() {
	for {
		// wait for permission
		for atomic.LoadInt32(&d.mining) == 0 {
			time.Sleep(100 * time.Millisecond)
		}
		if atomic.CompareAndSwapInt32(&d.mining, 2, 3) {
			return // shutdown
		}

		b := d.m.MineBlock()

		// give it to ourselves
		if err := d.c.AddTipBlock(b); err != nil {
			if !errors.Is(err, chain.ErrUnknownIndex) {
				log.Println("Couldn't add block:", err)
			}
			continue
		}

		// broadcast it
		d.s.Broadcast(&p2p.MsgRelayBlock{
			Header:       b.Header,
			Transactions: b.Transactions,
		})
	}
}

type tipTracker struct {
	tip sunyata.ChainIndex
	mu  sync.Mutex
}

func (tt *tipTracker) Height() uint64 {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.tip.Height
}

func (tt *tipTracker) ProcessChainApplyUpdate(cau *chain.ApplyUpdate, _ bool) error {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.tip = cau.Context.Index
	return nil
}

func (tt *tipTracker) ProcessChainRevertUpdate(cru *chain.RevertUpdate) error {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.tip = cru.Context.Index
	return nil
}

func newTipTracker(tip sunyata.ChainIndex) *tipTracker {
	return &tipTracker{tip: tip}
}

func formatTxn(txn sunyata.Transaction) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "{\n  Inputs: {\n")
	for _, in := range txn.Inputs {
		fmt.Fprintln(&b, "   ", in.Parent.LeafIndex, in.Parent.Address, in.Parent.Value)
	}
	fmt.Fprintf(&b, "  }\n  Outputs: {\n")
	for _, out := range txn.Outputs {
		fmt.Fprintln(&b, "   ", out.Address, out.Value)
	}
	fmt.Fprintf(&b, "  }\n  MinerFee: %v\n}\n", txn.MinerFee)
	return b.String()
}

func parseIndex(s string) (sunyata.ChainIndex, bool) {
	parts := strings.Split(s, "::")
	if len(parts) != 2 {
		return sunyata.ChainIndex{}, false
	}
	height, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return sunyata.ChainIndex{}, false
	}
	var id sunyata.BlockID
	if n, err := hex.Decode(id[:], []byte(parts[1])); n != len(id) || err != nil {
		return sunyata.ChainIndex{}, false
	}
	return sunyata.ChainIndex{
		Height: height,
		ID:     id,
	}, true
}
