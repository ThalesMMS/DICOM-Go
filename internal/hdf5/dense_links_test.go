package hdf5

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDenseLinkCreationOrderAndType(t *testing.T) {
	dir := filepath.Join("..", "..", "registration", "testdata", "itk1845")
	data, err := os.ReadFile(filepath.Join(dir, "ordered.h5"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		Ordered struct {
			Names  []string
			SHA256 string
		}
	}
	if err := json.Unmarshal(manifest, &expected); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != expected.Ordered.SHA256 || len(expected.Ordered.Names) != 73 {
		t.Fatal("unexpected public HDF5 fixture provenance or shape")
	}
	f := open(t, data)
	links, err := f.Links("/ordered")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(links))
	for _, link := range links {
		names = append(names, link.Name)
	}
	if !slices.Equal(names, expected.Ordered.Names) {
		t.Fatalf("creation order %v, want %v", names, expected.Ordered.Names)
	}
	address, err := f.lookup("/ordered")
	if err != nil {
		t.Fatal(err)
	}
	messages, err := f.messages(address)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.kind != msgLinkInfo {
			continue
		}
		info, err := f.linkInfo(m.data)
		if err != nil {
			t.Fatal(err)
		}
		// The HDF5 field holds the next assigned creation index.
		info.maxCreationOrder--
		if _, err := f.denseLinks(address, info); !errors.Is(err, ErrFormat) {
			t.Fatalf("link at the next creation index accepted: %v", err)
		}
	}
	if _, err := f.Links("/nonhard"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported dense link type: %v", err)
	}
}

func TestDenseLinkRejectsNameHashMismatch(t *testing.T) {
	data := fixture(t, "many_dense.h5")
	// This fixture has a single ten-record leaf of the name index.
	pos := bytes.Index(data, []byte("BTLF"))
	if pos < 0 {
		t.Fatal("missing dense name index fixture")
	}
	node := data[pos : pos+6+10*11+4]
	node[6] ^= 1
	// Repair the checksum so the hash-to-link check must catch the fault.
	binary.LittleEndian.PutUint32(node[len(node)-4:], metadataChecksum(node[:len(node)-4]))
	if _, err := open(t, data).Links("/TransformGroup"); !errors.Is(err, ErrFormat) || !strings.Contains(err.Error(), "name index") {
		t.Fatalf("name hash inconsistent with link: %v", err)
	}
}

func TestLinkNameLengthCheckedBeforeAllocation(t *testing.T) {
	f := &File{offsetSize: 8}
	// The encoded eight-byte link-name length is much larger than
	// the containing message. The decoder must reject it in place.
	if _, err := f.linkMessage([]byte{1, 3, 0, 0, 0, 0, 0, 1, 0, 0}); !errors.Is(err, ErrFormat) {
		t.Fatalf("oversized link name: %v", err)
	}
}

func TestDenseLinkRejectsSelfReference(t *testing.T) {
	data := fixture(t, "many_dense.h5")
	f := open(t, data)
	address, err := f.lookup("/TransformGroup")
	if err != nil {
		t.Fatal(err)
	}
	links, err := f.Links("/TransformGroup")
	if err != nil {
		t.Fatal(err)
	}
	blockAt := bytes.Index(data, []byte("FHDB"))
	block := data[blockAt : blockAt+512]
	// Replace one target with the owning group, retaining the name and index.
	// The fixture uses a single-byte name length and untracked creation order.
	name := links[0].Name
	messageAt := bytes.Index(block, append([]byte{1, 0, byte(len(name))}, []byte(name)...))
	if messageAt < 0 {
		t.Fatal("link message missing from fixed dense fixture")
	}
	binary.LittleEndian.PutUint64(block[messageAt+3+len(name):], address)
	// Direct block checksum follows signature, version, owner and heap offset.
	clear(block[17:21])
	binary.LittleEndian.PutUint32(block[17:21], metadataChecksum(block))
	if _, err := open(t, data).Links("/TransformGroup"); !errors.Is(err, ErrFormat) {
		t.Fatalf("self-referencing dense group: %v", err)
	}
}

func TestDenseLinkInfoRefusesInconsistentIndices(t *testing.T) {
	f := &File{offsetSize: 8}
	highCreationOrder := make([]byte, 26)
	highCreationOrder[1] = 1
	binary.LittleEndian.PutUint64(highCreationOrder[2:], uint64(math.MaxInt64)+1)
	for _, data := range [][]byte{
		{0, 2}, // indexed creation order without tracking
		{0, 0}, // truncated addresses
		append([]byte{0, 0}, make([]byte, 16)...),
		highCreationOrder,
	} {
		if len(data) == 18 {
			binary.LittleEndian.PutUint64(data[2:10], ^uint64(0))
		}
		if _, err := f.linkInfo(data); !errors.Is(err, ErrFormat) {
			t.Fatalf("invalid link info %v: %v", data, err)
		}
	}
}
