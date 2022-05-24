package sunyata

import (
	"testing"
)

func TestWork(t *testing.T) {
	tests := []struct {
		id  BlockID
		exp string
	}{
		{BlockID{0b11111111}, "1"},
		{BlockID{0b10000000}, "2"},
		{BlockID{0b01000000}, "4"},
		{BlockID{0b00100000}, "8"},
		{BlockID{0b00010000}, "16"},
		{BlockID{0b00001000}, "32"},
		{BlockID{0b00000100}, "64"},
		{BlockID{0b00000010}, "128"},
		{BlockID{0b00000001}, "256"},
		{BlockID{0, 0x28, 0x7E}, "1618"},                // approx 7.154 * 10^73
		{BlockID{10: 1}, "309485009821345068724781056"}, // 2^88
	}
	for _, test := range tests {
		got := WorkRequiredForHash(test.id)
		if got.String() != test.exp {
			t.Errorf("expected %v, got %v", test.exp, got)
		}
	}
}

func BenchmarkWork(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		WorkRequiredForHash(BlockID{1})
	}
}

func BenchmarkTransactionID(b *testing.B) {
	txn := Transaction{
		Inputs:  make([]Input, 10),
		Outputs: make([]Output, 10),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = txn.ID()
	}
}

func BenchmarkBlockID(b *testing.B) {
	block := Block{
		Transactions: make([]Transaction, 10),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = block.ID()
	}
}

func BenchmarkBlockHeaderID(b *testing.B) {
	var bh BlockHeader
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = bh.ID()
	}
}
