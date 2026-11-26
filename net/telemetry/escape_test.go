package telemetry

import "testing"

func TestEscapePeerString(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"ARCHIVE", "ARCHIVE"},
		{"1.2.840.10008.5.1.4.1.1.2", "1.2.840.10008.5.1.4.1.1.2"},
		{"MY AE/../x", "MY AE/../x"},
		{"Grüße", "Grüße"},
		{"AE\x00\x00", `AE\x00\x00`},
		{"EVIL\nFORGED LINE", `EVIL\nFORGED LINE`},
		{"a\rb\tc", `a\rb\tc`},
		{"\x1b[2J", `\x1b[2J`},
		{"DEL\x7f", `DEL\x7f`},
		{`back\slash`, `back\\slash`},
		{"bad\xe9utf8", `bad\xe9utf8`},
		{"c1\u0085", `c1\u0085`},
		{"bidi\u202eevil", `bidi\u202eevil`},
		{"tag\U000e0041", `tag\U000e0041`},
	} {
		if got := EscapePeerString(tc.in); got != tc.want {
			t.Errorf("EscapePeerString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEscapePeerStringDoesNotAllocateForSafeInput(t *testing.T) {
	in := "1.2.840.10008.1.2.1"
	allocs := testing.AllocsPerRun(100, func() {
		if EscapePeerString(in) != in {
			t.Fatal("safe input changed")
		}
	})
	if allocs != 0 {
		t.Fatalf("EscapePeerString allocated %.0f times for safe input", allocs)
	}
}

func TestEndpointPolicyPlaintextEscapesAETitle(t *testing.T) {
	policy := EndpointPolicy{AETitles: EndpointPlaintext}
	if got := policy.AETitle("EVIL\nFORGED"); got != `EVIL\nFORGED` {
		t.Fatalf("plaintext AETitle = %q, want escaped value", got)
	}
	hmacPolicy := EndpointPolicy{AETitles: EndpointHMACSHA256, HMACKey: []byte("key")}
	if hmacPolicy.AETitle("EVIL\nFORGED") == hmacPolicy.AETitle(`EVIL\nFORGED`) {
		t.Fatal("HMAC must hash the unescaped value")
	}
}
