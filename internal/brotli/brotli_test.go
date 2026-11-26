package brotli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDictionaryMatchesRFC7932(t *testing.T) {
	sum := sha256.Sum256(dictionary)
	if got := hex.EncodeToString(sum[:]); got != "20e42eb1b511c21806d4d227d07e5dd06877d8ce7b3a817f378f313653f35c70" {
		t.Fatalf("dictionary SHA-256 = %s", got)
	}
	if dOffset[24]+24<<ndBits[24] != len(dictionary) {
		t.Fatalf("dictionary offsets end at %d, size %d", dOffset[24]+24<<ndBits[24], len(dictionary))
	}
}

// The vectors were produced by the brotli 1.2.0 CLI from synthetic inputs at
// qualities 0-11 and window sizes 10-24; they exercise prefix codes, block
// switches, context maps, distance codes and dictionary transforms.
func TestDecodeReferenceVectors(t *testing.T) {
	streams, err := filepath.Glob("testdata/*.br")
	if err != nil || len(streams) == 0 {
		t.Fatalf("no vectors: %v", err)
	}
	for _, path := range streams {
		base := filepath.Base(path)
		want, err := os.ReadFile(filepath.Join("testdata", base[:strings.IndexByte(base, '.')]+".bin"))
		if err != nil {
			t.Fatal(err)
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(src, 1<<20)
		if err != nil {
			t.Errorf("%s: %v", base, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: decoded %d bytes differ from %d expected", base, len(got), len(want))
		}
	}
}

// TestVectorsExerciseDictionary guards the vector set: decoding with a
// scrambled dictionary must change the output of the dictionary-heavy text.
func TestVectorsExerciseDictionary(t *testing.T) {
	src, err := os.ReadFile("testdata/text.q11.br")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Decode(src, 1<<20)
	saved := dictionary
	dictionary = bytes.Repeat([]byte{'#'}, len(saved))
	defer func() { dictionary = saved }()
	got, err := Decode(src, 1<<20)
	if err == nil && bytes.Equal(got, want) {
		t.Fatal("text.q11.br does not reference the static dictionary")
	}
}

func TestEncodeStoredRoundTrips(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, size := range []int{0, 1, 2, 65535, 65536, 65537, 200000} {
		data := make([]byte, size)
		rng.Read(data)
		enc := EncodeStored(data)
		got, err := Decode(enc, size)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("size %d: round trip differs", size)
		}
	}
}

func TestDecodeRejectsHostileStreams(t *testing.T) {
	src, err := os.ReadFile("testdata/text.q11.br")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(src, 100); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("limit: %v", err)
	}
	for cut := 1; cut < len(src); cut += len(src)/37 + 1 {
		if _, err := Decode(src[:cut], 1<<20); err == nil {
			t.Fatalf("truncated at %d decoded without error", cut)
		}
	}
	if _, err := Decode(append(append([]byte{}, src...), 0), 1<<20); err == nil {
		t.Fatal("trailing byte accepted")
	}
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 2000; i++ {
		junk := make([]byte, rng.Intn(64))
		rng.Read(junk)
		_, _ = Decode(junk, 1<<16) // must not panic
	}
	for i := 0; i < 500; i++ {
		mutated := append([]byte{}, src...)
		mutated[rng.Intn(len(mutated))] ^= byte(1 << rng.Intn(8))
		_, _ = Decode(mutated, 1<<20)
	}
}

// TestBrotliCLIInterop cross-checks against the brotli command when
// DICOMGO_BROTLI_ORACLE=1: the CLI decodes EncodeStored output and Decode
// reads fresh CLI streams of random text.
func TestBrotliCLIInterop(t *testing.T) {
	if os.Getenv("DICOMGO_BROTLI_ORACLE") != "1" {
		t.Skip("set DICOMGO_BROTLI_ORACLE=1 to run the brotli CLI oracle")
	}
	cli, err := exec.LookPath("brotli")
	if err != nil {
		t.Fatal("brotli CLI not found")
	}
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(3))
	words := strings.Fields("the of and to in is that for with as on by from this which have time people year")
	for i := 0; i < 20; i++ {
		var b strings.Builder
		for b.Len() < 5000+i*700 {
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteString([]string{" ", ", ", ". ", "\n"}[rng.Intn(4)])
		}
		plain := []byte(b.String())
		in := filepath.Join(dir, "in.txt")
		out := filepath.Join(dir, "out.br")
		if err := os.WriteFile(in, plain, 0o600); err != nil {
			t.Fatal(err)
		}
		q := []string{"0", "2", "6", "9", "11"}[i%5]
		if msg, err := exec.Command(cli, "-q", q, "-f", "-o", out, in).CombinedOutput(); err != nil {
			t.Fatalf("brotli: %v %s", err, msg)
		}
		src, _ := os.ReadFile(out)
		got, err := Decode(src, 1<<20)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("q%s: decode mismatch: %v", q, err)
		}
		if err := os.WriteFile(out, EncodeStored(plain), 0o600); err != nil {
			t.Fatal(err)
		}
		back, err := exec.Command(cli, "-d", "-c", out).Output()
		if err != nil || !bytes.Equal(back, plain) {
			t.Fatalf("CLI could not decode EncodeStored output: %v", err)
		}
	}
}
