package core

import (
	"bytes"
	"errors"
	"math/big"
	"strings"
	"testing"
)

// uuidOf parses a 2.25 UID back into its 16 UUID bytes.
func uuidOf(t *testing.T, uid string) [16]byte {
	t.Helper()
	digits, ok := strings.CutPrefix(uid, UUIDRoot+".")
	if !ok {
		t.Fatalf("%q is not under %s", uid, UUIDRoot)
	}
	value, ok := new(big.Int).SetString(digits, 10)
	if !ok || value.BitLen() > 128 {
		t.Fatalf("%q is not a 128-bit integer", uid)
	}
	var uuid [16]byte
	value.FillBytes(uuid[:])
	return uuid
}

func checkUUIDUID(t *testing.T, uid string, version byte) {
	t.Helper()
	if !IsValidUID(uid) || len(uid) > 44 {
		t.Fatalf("%q is not a valid UID of at most 44 characters", uid)
	}
	uuid := uuidOf(t, uid)
	if uuid[6]>>4 != version || uuid[8]>>6 != 2 {
		t.Fatalf("%q: version %d variant %b, want version %d variant 10", uid, uuid[6]>>4, uuid[8]>>6, version)
	}
}

func TestNewUIDIsUniqueVersion4(t *testing.T) {
	seen := make(map[string]bool, 20000)
	for i := 0; i < 20000; i++ {
		uid, err := NewUID()
		if err != nil {
			t.Fatal(err)
		}
		checkUUIDUID(t, uid, 4)
		if seen[uid] {
			t.Fatalf("duplicate UID %q", uid)
		}
		seen[uid] = true
	}
	// The random bits reach the UID: all-zero and all-one sources give the
	// smallest and largest version 4 UUIDs.
	zero, err := NewUIDFromReader(bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if want := UIDFromUUID([16]byte{6: 0x40, 8: 0x80}); zero != want {
		t.Fatalf("zero source %q, want %q", zero, want)
	}
	checkUUIDUID(t, zero, 4)
	ones, _ := NewUIDFromReader(bytes.NewReader(bytes.Repeat([]byte{0xff}, 16)))
	checkUUIDUID(t, ones, 4)
	if _, err := NewUIDFromReader(bytes.NewReader(make([]byte, 15))); err == nil {
		t.Fatal("a short random source was accepted")
	}
}

func TestNameUIDIsDeterministicVersion8(t *testing.T) {
	a := NameUID("1.2.3", "series")
	if a != NameUID("1.2.3", "series") {
		t.Fatal("NameUID is not deterministic")
	}
	checkUUIDUID(t, a, 8)
	for _, other := range [][]string{{"1.2.3", "serie"}, {"1.2.3series"}, {"1.2.3s", "eries"}, {"series", "1.2.3"}, {}} {
		if NameUID(other...) == a {
			t.Fatalf("parts %q collide with [1.2.3 series]", other)
		}
	}
	keyed := KeyedNameUID([]byte("secret"), "1.2.3", "series")
	checkUUIDUID(t, keyed, 8)
	if keyed == a || keyed != KeyedNameUID([]byte("secret"), "1.2.3", "series") || keyed == KeyedNameUID([]byte("other"), "1.2.3", "series") {
		t.Fatal("KeyedNameUID does not depend on exactly its key and parts")
	}
}

func TestNewUIDWithRoot(t *testing.T) {
	for _, root := range []string{"1.2.826.0.1.3680043.10.543", "2.25", "1.2", strings.Repeat("1.", 21) + "1"} {
		for i := 0; i < 200; i++ {
			uid, err := NewUIDWithRoot(root)
			if err != nil {
				t.Fatalf("root %q: %v", root, err)
			}
			suffix, ok := strings.CutPrefix(uid, root+".")
			if !ok || !IsValidUID(uid) || len(uid) > 64 {
				t.Fatalf("root %q gave %q", root, uid)
			}
			if want := min(64-len(root)-1, 39); len(suffix) != want {
				t.Fatalf("root %q: %d digits, want %d", root, len(suffix), want)
			}
		}
	}
	// 43 characters leave exactly 20 digits; 44 leave 19.
	fits := "1.2." + strings.Repeat("9", 40)
	if uid, err := NewUIDWithRoot(fits[:43]); err != nil || len(uid) != 64 {
		t.Fatalf("43-character root: %q %v", uid, err)
	}
	for _, root := range []string{fits[:44], "", "1", "1.02.3", "1.2.", "3.1", "1.2.a"} {
		if _, err := NewUIDWithRoot(root); !errors.Is(err, ErrUIDRoot) {
			t.Fatalf("root %q: %v, want ErrUIDRoot", root, err)
		}
	}
	if _, err := NewUIDWithRootFromReader("1.2.3", bytes.NewReader(nil)); err == nil || errors.Is(err, ErrUIDRoot) {
		t.Fatalf("empty random source: %v", err)
	}
}

func BenchmarkNewUID(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := NewUID(); err != nil {
			b.Fatal(err)
		}
	}
}
