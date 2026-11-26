package jpegxl

import "testing"

func TestNaturalOrderDCT8IsZigZag(t *testing.T) {
	nat := naturalOrderDCT8()
	for k, p := range nat {
		transposed := p
		if transposed != jpegNaturalOrder[k] {
			t.Fatalf("k=%d: natural %d transposes to %d, JPEG zig-zag %d", k, p, transposed, jpegNaturalOrder[k])
		}
	}
}

func TestDecodeLehmer(t *testing.T) {
	got := decodeLehmer([]int{2, 0, 1, 0, 0})
	want := []int{2, 0, 3, 1, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
