package core

import (
	"fmt"
	"testing"
)

// Exhaust the two-byte input domain, not only the accepted VR spellings: the
// header parser must keep rejecting unknown, lower-case and binary VR codes.
func TestParseVRCompleteTwoByteDomain(t *testing.T) {
	known := make(map[string]VR, len(allKnownVRs))
	for _, vr := range allKnownVRs {
		known[string(vr)] = vr
	}
	for code := 0; code < 1<<16; code++ {
		raw := [2]byte{byte(code >> 8), byte(code)}
		input := string(raw[:])
		got, err := ParseVR(input)
		want, valid := known[input]
		if valid {
			if err != nil || got != want {
				t.Fatalf("ParseVR(%q) = %q, %v; want %q", input, got, err, want)
			}
		} else if err == nil || got != "" || err.Error() != fmt.Sprintf("dicom: invalid VR %q", input) {
			t.Fatalf("invalid VR %q = %q, %v", input, got, err)
		}
	}
}

func TestParseVRErrorTextForOtherLengths(t *testing.T) {
	for _, input := range []string{"", "A", "PN ", "\x00PN", "AE\x00", "\"\\\n\t", "\ufffd", "\u2028", "\xff\xfe\xfd"} {
		got, err := ParseVR(input)
		if err == nil || got != "" || err.Error() != fmt.Sprintf("dicom: invalid VR %q", input) {
			t.Fatalf("invalid VR %q = %q, %v", input, got, err)
		}
	}
}

var parseVR1301Sink VR
var parseVR1301ErrorSink error

var parseVR1301BaselineMap = func() map[VR]struct{} {
	known := make(map[VR]struct{}, len(allKnownVRs))
	for _, vr := range allKnownVRs {
		known[vr] = struct{}{}
	}
	return known
}()

// Preserve the former implementation to compare lookup cost and the distinct
// temporary-string ownership cost without changing either public API.
func parseVR1301Baseline(s string) (VR, error) {
	vr := VR(s)
	if _, ok := parseVR1301BaselineMap[vr]; !ok {
		return "", fmt.Errorf("dicom: invalid VR %q", s)
	}
	return vr, nil
}

func BenchmarkParseVR1301(b *testing.B) {
	strings := make([]string, len(allKnownVRs))
	bytes := make([][2]byte, len(allKnownVRs))
	for i, vr := range allKnownVRs {
		strings[i] = string(vr)
		copy(bytes[i][:], strings[i])
	}
	b.Run("stable_string/baseline", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			parseVR1301Sink, parseVR1301ErrorSink = parseVR1301Baseline(strings[i%len(strings)])
		}
	})
	b.Run("stable_string/candidate", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			parseVR1301Sink, parseVR1301ErrorSink = ParseVR(strings[i%len(strings)])
		}
	})
	// Keep these calls direct and the conversions inside each loop. Passing a
	// function value would make both input strings escape at the indirect call,
	// hiding the same optimization the real explicit-header parser can use.
	b.Run("header_bytes/baseline", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			raw := bytes[i%len(bytes)]
			parseVR1301Sink, parseVR1301ErrorSink = parseVR1301Baseline(string(raw[:]))
		}
	})
	b.Run("header_bytes/candidate", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			raw := bytes[i%len(bytes)]
			parseVR1301Sink, parseVR1301ErrorSink = ParseVR(string(raw[:]))
		}
	})
}
