package encaplayout

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestLayoutOffsetsCountHeadersAndTailPadding(t *testing.T) {
	got, err := Plan([]uint64{7, 5}, 4, Basic, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fragments != 4 || len(got.Basic) != 8 || binary.LittleEndian.Uint32(got.Basic[4:]) != 24 || got.Extended != nil {
		t.Fatalf("layout=%+v", got)
	}
	got, err = Plan([]uint64{7, 5}, 4, Empty, 4, false)
	if err != nil || got.Basic != nil || got.Extended != nil || got.Fragments != 4 {
		t.Fatalf("empty=%+v err=%v", got, err)
	}
}

func TestLayoutSingleFragmentEOTAndBounds(t *testing.T) {
	got, err := Plan([]uint64{MaxItem, 3}, 0, Auto, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Basic != nil || binary.LittleEndian.Uint64(got.Extended[8:]) != MaxItem+8 || binary.LittleEndian.Uint64(got.Lengths[8:]) != 3 {
		t.Fatalf("layout=%+v", got)
	}
	for _, test := range []struct {
		lengths []uint64
		cap     uint64
		policy  Policy
		limit   int
		single  bool
	}{
		{[]uint64{0}, 0, Auto, 0, false}, {[]uint64{3}, 3, Auto, 0, false}, {[]uint64{5}, 4, Auto, 0, true}, {[]uint64{5}, 4, Extended, 0, false}, {[]uint64{7, 5}, 4, Auto, 3, false}, {[]uint64{MaxItem, 3}, 0, Basic, 0, false}, {[]uint64{math.MaxUint64}, 2, Empty, 0, false}, {[]uint64{3}, 0, Policy(9), 0, false},
	} {
		if _, err := Plan(test.lengths, test.cap, test.policy, test.limit, test.single); err == nil {
			t.Fatalf("accepted %+v", test)
		}
	}
}

func TestSingleFragmentSyntaxesRefuseSplitFrames(t *testing.T) {
	for _, uid := range []string{
		"1.2.840.10008.1.2.5", "1.2.840.10008.1.2.1.98", "1.2.840.10008.1.2.8.1",
		"1.2.840.10008.1.2.4.201", "1.2.840.10008.1.2.4.202", "1.2.840.10008.1.2.4.203",
		"1.2.840.10008.1.2.4.110", "1.2.840.10008.1.2.4.111", "1.2.840.10008.1.2.4.112",
	} {
		if !SingleFragment(uid) {
			t.Fatalf("%s not single-fragment", uid)
		}
		if _, err := Plan([]uint64{5}, 4, Auto, 0, SingleFragment(uid)); err == nil {
			t.Fatalf("%s split a frame", uid)
		}
		if got, err := Plan([]uint64{4, 3}, 4, Auto, 0, SingleFragment(uid)); err != nil || got.Fragments != 2 {
			t.Fatalf("%s layout=%+v err=%v", uid, got, err)
		}
	}
	for _, uid := range []string{"1.2.840.10008.1.2.4.50", "1.2.840.10008.1.2.4.80", "1.2.840.10008.1.2.4.90", "1.2.840.10008.1.2.4.91", "1.2.840.10008.1.2.4.92", "1.2.840.10008.1.2.4.93"} {
		if SingleFragment(uid) {
			t.Fatalf("%s reported single-fragment", uid)
		}
	}
}

func TestFragmentBoundaryAdjustmentPreservesCountsOffsetsAndCap(t *testing.T) {
	for _, cap := range []uint64{2, 4, 6, 16, 32} {
		for length := uint64(0); length <= 2*cap+3; length++ {
			if length == 0 {
				if _, err := Plan([]uint64{length}, cap, Basic, 100, false); err == nil {
					t.Fatal("empty frame accepted")
				}
				continue
			}
			var count int
			var bytes uint64
			for remaining := length; remaining > 0; {
				n := FragmentLength(remaining, cap)
				if n == 0 || n > remaining || n > cap {
					t.Fatalf("cap%d length%d payload%d", cap, length, n)
				}
				remaining -= n
				count++
				if remaining > 0 && n%2 != 0 {
					t.Fatal("odd intermediate payload")
				}
				padded := n
				if remaining == 0 {
					padded += n % 2
					if padded > cap {
						t.Fatal("tail padding exceeds cap")
					}
					if cap >= 4 && length > cap && n < 3 {
						t.Fatal("tiny terminal payload")
					}
				}
				bytes += 8 + padded
			}
			layout, err := Plan([]uint64{length, cap + 3}, cap, Basic, 100, false)
			if err != nil {
				t.Fatal(err)
			}
			oldCount := int((length-1)/cap + 1)
			secondCount := int((cap+2)/cap + 1)
			if count != oldCount || bytes != length+length%2+8*uint64(oldCount) || binary.LittleEndian.Uint32(layout.Basic[4:]) != uint32(bytes) || layout.Fragments != oldCount+secondCount {
				t.Fatalf("cap%d length%d count%d bytes%d layout%+v", cap, length, count, bytes, layout)
			}
		}
	}
}
