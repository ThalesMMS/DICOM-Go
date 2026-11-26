package video

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/ThalesMMS/dicom-go/transfer"
	"os"
	"testing"
)

func importBoxBytes(kind string, payload []byte) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(len(payload)+8))
	copy(b[4:], kind)
	return append(b, payload...)
}
func importConcat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
func importAVCFixture(t testing.TB) []byte {
	t.Helper()
	source, e := os.ReadFile("testdata/vt-h264.h264")
	if e != nil {
		t.Fatal(e)
	}
	nals, e := splitAnnexB(source, DefaultMaxFragments)
	if e != nil {
		t.Fatal(e)
	}
	var sps, pps, slice []byte
	for _, n := range nals {
		switch n[0] & 31 {
		case 7:
			if sps == nil {
				sps = n
			}
		case 8:
			if pps == nil {
				pps = n
			}
		case 5:
			if slice == nil {
				slice = n
			}
		}
	}
	if sps == nil || pps == nil || slice == nil {
		t.Fatal("missing original fixture NALs")
	}
	avcc := []byte{1, sps[1], sps[2], sps[3], 255, 225, byte(len(sps) >> 8), byte(len(sps))}
	avcc = append(avcc, sps...)
	avcc = append(avcc, 1, byte(len(pps)>>8), byte(len(pps)))
	avcc = append(avcc, pps...)
	sample := make([]byte, 4)
	binary.BigEndian.PutUint32(sample, uint32(len(slice)))
	sample = append(sample, slice...)
	ftyp := importBoxBytes("ftyp", []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'a', 'v', 'c', '1'})
	mdat := importBoxBytes("mdat", sample)
	tk := make([]byte, 84)
	tk[3] = 7
	binary.BigEndian.PutUint32(tk[12:], 1)
	for i, v := range []uint32{65536, 0, 0, 0, 65536, 0, 0, 0, 1 << 30} {
		binary.BigEndian.PutUint32(tk[40+4*i:], v)
	}
	binary.BigEndian.PutUint32(tk[76:], 96<<16)
	binary.BigEndian.PutUint32(tk[80:], 64<<16)
	md := make([]byte, 24)
	binary.BigEndian.PutUint32(md[12:], 25)
	binary.BigEndian.PutUint32(md[16:], 1)
	hd := make([]byte, 24)
	copy(hd[8:], "vide")
	visual := make([]byte, 78)
	binary.BigEndian.PutUint16(visual[6:], 1)
	binary.BigEndian.PutUint16(visual[24:], 96)
	binary.BigEndian.PutUint16(visual[26:], 64)
	binary.BigEndian.PutUint16(visual[40:], 1)
	binary.BigEndian.PutUint16(visual[74:], 24)
	binary.BigEndian.PutUint16(visual[76:], 65535)
	stsd := make([]byte, 8)
	binary.BigEndian.PutUint32(stsd[4:], 1)
	stsd = append(stsd, importBoxBytes("avc1", append(visual, importBoxBytes("avcC", avcc)...))...)
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[4:], uint32(len(sample)))
	binary.BigEndian.PutUint32(stsz[8:], 1)
	stts := make([]byte, 16)
	binary.BigEndian.PutUint32(stts[4:], 1)
	binary.BigEndian.PutUint32(stts[8:], 1)
	binary.BigEndian.PutUint32(stts[12:], 1)
	stsc := make([]byte, 20)
	for _, at := range []int{4, 8, 12, 16} {
		binary.BigEndian.PutUint32(stsc[at:], 1)
	}
	stco := make([]byte, 12)
	binary.BigEndian.PutUint32(stco[4:], 1)
	binary.BigEndian.PutUint32(stco[8:], uint32(len(ftyp)+8))
	stbl := importBoxBytes("stbl", importConcat(importBoxBytes("stsd", stsd), importBoxBytes("stsz", stsz), importBoxBytes("stts", stts), importBoxBytes("stsc", stsc), importBoxBytes("stco", stco)))
	mdia := importBoxBytes("mdia", importConcat(importBoxBytes("mdhd", md), importBoxBytes("hdlr", hd), importBoxBytes("minf", stbl)))
	trak := importBoxBytes("trak", importConcat(importBoxBytes("tkhd", tk), mdia))
	moov := importBoxBytes("moov", trak)
	return importConcat(ftyp, mdat, moov)
}
func importMPEGFixture() []byte {
	es := []byte{0, 0, 1, 0xb3, 1, 0, 16, 0x13, 0, 0, 0, 0, 0, 0, 1, 0xb5, 0x14, 0x8a, 0, 0, 0, 0, 0, 0, 1, 0, 0, 8, 0, 0}
	pack := []byte{0, 0, 1, 0xba, 0x44, 0, 4, 0, 4, 1, 0, 0, 3, 0xf8}
	pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0}
	binary.BigEndian.PutUint16(pes[4:], uint16(len(es)+3))
	return importConcat(pack, pes, es, []byte{0, 0, 1, 0xb9})
}
func TestInspectSourceClassicAVCActualNALsAndContainerMetadata(t *testing.T) {
	data := importAVCFixture(t)
	before := bytes.Clone(data)
	r, e := InspectSource(context.Background(), data, ImportLimits{})
	if e != nil || r.Rows != 64 || r.Columns != 96 || r.Frames != 1 || r.FrameRate != 25 || r.TransferSyntax.UID != transfer.MPEG4HP41.UID || r.Container != ContainerMP4 || !bytes.Equal(before, data) {
		t.Fatalf("inspect: %+v %v", r, e)
	}
}
func TestInspectSourceMPEG2ProgramHeadersWithoutDecodeClaim(t *testing.T) {
	r, e := InspectSource(context.Background(), importMPEGFixture(), ImportLimits{})
	if e != nil || r.Rows != 16 || r.Columns != 16 || r.Frames != 1 || r.FrameRate != 25 || r.TransferSyntax.UID != transfer.MPEG2MPML.UID || r.Container != ContainerMPEGProgram {
		t.Fatalf("inspect: %+v %v", r, e)
	}
}
