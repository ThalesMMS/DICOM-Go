package jpeglossless

import (
	"context"
	"errors"
	"testing"
)

func TestHuffmanLookaheadShortTailAndLongCodesAcrossRestart(t *testing.T) {
	var counts [17]int
	counts[1], counts[9], counts[16] = 1, 1, 1
	table := buildHuffTable(counts, []byte{0, 1, 2})
	var writer bitWriter
	writer.writeBits(0, 1)
	writer.writeBits(256, 9)
	writer.writeBits(32896, 16)
	writer.writeBits(0, 1)
	writer.pad()
	data := append(writer.out, 0xff, 0xd0, 0x7f, 0xff, 0xd9)
	reader := &bitReader{data: data}
	for index, want := range []byte{0, 1, 2, 0} {
		got, err := table.decodeSymbol(reader)
		if err != nil || got != want {
			t.Fatalf("symbol %d: got %d, %v; want %d", index, got, err, want)
		}
	}
	if err := reader.alignToRestart(context.Background(), 0xd0); err != nil {
		t.Fatal(err)
	}
	// Only seven fill bits follow this one-bit code; requesting a full
	// lookahead must not consume EOI or reject the valid short symbol.
	got, err := table.decodeSymbol(reader)
	if err != nil || got != 0 {
		t.Fatalf("short final symbol: %d, %v", got, err)
	}
	pos, err := reader.finishEntropy()
	if err != nil || pos != len(data)-2 {
		t.Fatalf("EOI position=%d, %v", pos, err)
	}
}

func TestEntropyCacheStuffingAndStrictBoundaries(t *testing.T) {
	reader := &bitReader{data: []byte{0xab, 0xff, 0, 0xcd, 0xff, 0xd9}}
	for _, item := range []struct{ bits, want int }{{4, 0xa}, {16, 0xbffc}, {4, 0xd}} {
		got, err := reader.readBits(item.bits)
		if err != nil || got != item.want {
			t.Fatalf("%d bits: %x, %v; want %x", item.bits, got, err, item.want)
		}
	}
	pos, err := reader.finishEntropy()
	if err != nil || pos != 4 {
		t.Fatalf("stuffed byte moved marker: %d, %v", pos, err)
	}
	if _, err := reader.readBit(); !errors.Is(err, ErrInvalidStream) {
		t.Fatalf("marker consumed as entropy: %v", err)
	}
	for _, lenient := range []bool{false, true} {
		reader := &bitReader{data: []byte{0x7f, 0xff, 0, 0xff, 0xd9}, lenientFill: lenient}
		if _, err := reader.readBit(); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.finishEntropy(); !errors.Is(err, ErrInvalidStream) {
			t.Fatalf("lenient=%v accepted a whole trailing entropy byte: %v", lenient, err)
		}
	}
	for _, data := range [][]byte{nil, {0xff}, {0xff, 0xd0}} {
		reader := &bitReader{data: data}
		if _, err := reader.readBits(1); !errors.Is(err, ErrInvalidStream) {
			t.Fatalf("truncated/marker input %x: %v", data, err)
		}
	}
}

func TestDifferenceCategoryAllSixteenBitDifferences(t *testing.T) {
	for value := int32(-32768); value <= 32767; value++ {
		magnitude := value
		if magnitude < 0 {
			magnitude = -magnitude
		}
		want := 0
		for magnitude > 0 {
			want++
			magnitude >>= 1
		}
		if got := differenceCategory(value); got != want {
			t.Fatalf("difference %d: got %d want %d", value, got, want)
		}
	}
}

func TestHuffmanDifferenceLookaheadAllMagnitudesAndLongCodes(t *testing.T) {
	// Put categories 15/16 behind nine-/sixteen-bit codes, and use short
	// codes for 0..14. This exercises combined lookup, magnitude extraction,
	// canonical fallback and every possible signed sixteen-bit difference.
	var spec huffmanSpec
	spec.counts[5], spec.counts[9], spec.counts[16] = 15, 1, 1
	var counts [17]int
	for length, count := range spec.counts {
		counts[length] = int(count)
	}
	for category := 0; category <= 16; category++ {
		spec.symbols = append(spec.symbols, byte(category))
	}
	codes := spec.codes()
	var writer bitWriter
	for difference := int32(-32768); difference <= 32767; difference++ {
		category := differenceCategory(difference)
		writer.writeBits(int(codes.code[category]), codes.length[category])
		if category > 0 && category < 16 {
			magnitude := difference
			if magnitude < 0 {
				magnitude--
			}
			writer.writeBits(int(uint32(magnitude)&(1<<uint(category)-1)), category)
		}
	}
	// Finish with a five-bit category-zero code, exercising a short tail.
	writer.writeBits(int(codes.code[0]), codes.length[0])
	writer.pad()
	data := append(writer.out, 0xff, 0xd9)
	table := buildHuffTable(counts, spec.symbols)
	fast, canonical := &bitReader{data: data}, &bitReader{data: data}
	for difference := int32(-32768); difference <= 32768; difference++ {
		want := difference
		if difference == -32768 {
			want = 32768 // T.81 category 16 is the modular difference.
		} else if difference == 32768 {
			want = 0 // Final short symbol.
		}
		got, err := table.decodeDifference(fast)
		if err != nil || got != want {
			t.Fatalf("difference %d: fast got %d, %v; want %d", difference, got, err, want)
		}
		symbol, err := table.decodeSymbol(canonical)
		if err != nil {
			t.Fatal(err)
		}
		got, err = receiveExtend(canonical, int(symbol))
		if err != nil || got != want {
			t.Fatalf("difference %d: canonical got %d, %v; want %d", difference, got, err, want)
		}
	}
	for _, reader := range []*bitReader{fast, canonical} {
		pos, err := reader.finishEntropy()
		if err != nil || pos != len(data)-2 {
			t.Fatalf("end marker position=%d, %v", pos, err)
		}
	}
}
