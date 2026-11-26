package transfer

import "testing"

func TestLegacySyntaxesAreNotRegistered(t *testing.T) {
	for _, s := range []Syntax{ImplicitVRBigEndianACRNEMA, PrivateGEImplicitVRBigEndianPixelData} {
		got, ok := LegacySyntax(s.UID + "\x00")
		if !ok || got.UID != s.UID {
			t.Fatalf("LegacySyntax(%q) = %v %v", s.UID, got.UID, ok)
		}
		if _, ok := DefaultRegistry.Get(s.UID); ok {
			t.Fatalf("%s is registered", s.UID)
		}
	}
	if _, ok := LegacySyntax(ImplicitVRLittleEndian.UID); ok {
		t.Fatal("a standard syntax is legacy")
	}
}
