package main

import (
	"bytes"
	"encoding/binary"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
	"os"
	"path/filepath"
	"testing"
)

func cliMPEGHeaders() []byte {
	es := []byte{0, 0, 1, 0xb3, 1, 0, 16, 0x13, 0, 0, 0, 0, 0, 0, 1, 0xb5, 0x14, 0x8a, 0, 0, 0, 0, 0, 0, 1, 0, 0, 8, 0, 0}
	pack := []byte{0, 0, 1, 0xba, 0x44, 0, 4, 0, 4, 1, 0, 0, 3, 0xf8}
	pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0}
	binary.BigEndian.PutUint16(pes[4:], uint16(len(es)+3))
	return append(append(append(pack, pes...), es...), 0, 0, 1, 0xb9)
}
func TestImg2DCMVideoRequiresAnatomyAndReportsOnlyEncapsulation(t *testing.T) {
	data := cliMPEGHeaders()
	input := writeInput(t, "own.mpg", data)
	output := filepath.Join(t.TempDir(), "video.dcm")
	var out, diag bytes.Buffer
	if code := run([]string{input, output}, &out, &diag); code != 1 {
		t.Fatal("video without anatomy accepted")
	}
	if _, e := os.Stat(output); !os.IsNotExist(e) {
		t.Fatal("failure published")
	}
	if code := run([]string{"-region-code", "SYNTHETIC", "-region-scheme", "99TEST", "-region-meaning", "Synthetic test region", input, output}, &out, &diag); code != 0 {
		t.Fatalf("exit %d: %s", code, diag.String())
	}
	f := cliRead(t, output)
	if f.TransferSyntax.UID != transfer.MPEG2MPML.UID || !bytes.Contains(out.Bytes(), []byte("decoded=false")) {
		t.Fatal("wrong syntax/decode claim")
	}
	pixel, _ := f.Dataset.Get(core.NewTag(0x7fe0, 0x0010))
	seq, ok := pixel.Value.(core.FragmentSequence)
	if !ok || len(seq.Fragments) != 1 || !bytes.Equal(seq.Fragments[0][:len(data)], data) {
		t.Fatal("container changed")
	}
}
