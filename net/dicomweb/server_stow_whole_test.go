package dicomweb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func wholeSTOWObject(sop, modality string) *object.Object {
	o := object.New(nil)
	for tag, text := range map[core.Tag]string{core.NewTag(0x0008, 0x0016): sop, core.NewTag(0x0008, 0x0018): dicomtest.TestSOPInstanceUID, core.NewTag(0x0020, 0x000d): dicomtest.TestStudyInstanceUID, core.NewTag(0x0020, 0x000e): dicomtest.TestSeriesInstanceUID} {
		o.Put(core.NewRawElement(tag, core.VRUI, []byte(text)))
	}
	for tag, text := range map[core.Tag]string{core.NewTag(0x0008, 0x0060): modality, core.NewTag(0x0020, 0x0011): "1", core.NewTag(0x0020, 0x0013): "1", core.NewTag(0x0028, 0x0301): "NO"} {
		vr := core.VRCS
		if tag.Group == 0x0020 {
			vr = core.VRIS
		}
		o.Put(core.NewRawElement(tag, vr, []byte(text)))
	}
	return o
}
func wholeSTOWDoc(t *testing.T, mime string) (*object.Object, StoreBulkData) {
	t.Helper()
	var sop, modality string
	var data []byte
	switch mime {
	case "application/pdf":
		sop, modality, data = encapdoc.EncapsulatedPDFStorage, "DOC", []byte("%PDF-1.7\n1 0 obj <<>> endobj\n%%EOF\n")
	case "text/xml":
		sop, modality, data = encapdoc.EncapsulatedCDAStorage, "DOC", []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3.1535" extension="synthetic"/></ClinicalDocument>`)
	case "model/stl":
		sop, modality = encapdoc.EncapsulatedSTLStorage, "M3D"
		data = make([]byte, 134)
		copy(data, "Synthetic triangle")
		binary.LittleEndian.PutUint32(data[80:], 1)
	case "model/obj":
		sop, modality, data = encapdoc.EncapsulatedOBJStorage, "M3D", []byte("mtllib https://invalid.example/never-fetched.mtl\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n")
	case "model/mtl":
		sop, modality, data = encapdoc.EncapsulatedMTLStorage, "M3D", []byte("newmtl synthetic\nKd 1 0 0\nmap_Kd https://invalid.example/never-fetched.png\n")
	default:
		t.Fatal("unlisted family", mime)
	}
	o := wholeSTOWObject(sop, modality)
	for tag, vr := range map[core.Tag]core.VR{core.NewTag(0x0008, 0x0023): core.VRDA, core.NewTag(0x0008, 0x0033): core.VRTM, core.NewTag(0x0008, 0x002a): core.VRDT, core.NewTag(0x0042, 0x0010): core.VRST} {
		o.Put(core.NewRawElement(tag, vr, nil))
	}
	o.Put(object.SequenceElement(core.NewTag(0x0040, 0xa043)))
	declared := mime
	if mime == "text/xml" {
		declared = "text/XML"
	}
	o.Put(core.NewRawElement(storeDocumentMIMETag, core.VRLO, []byte(declared)))
	if modality == "M3D" {
		o.Put(core.NewRawElement(core.NewTag(0x0020, 0x0052), core.VRUI, []byte("1.2.3.1535.6")))
		o.Put(core.NewRawElement(core.NewTag(0x0020, 0x1040), core.VRLO, nil))
		units := object.FromElements([]core.Element{core.NewRawElement(core.NewTag(0x0008, 0x0100), core.VRSH, []byte("mm")), core.NewRawElement(core.NewTag(0x0008, 0x0102), core.VRSH, []byte("UCUM")), core.NewRawElement(core.NewTag(0x0008, 0x0104), core.VRLO, []byte("millimeter"))}, nil)
		o.Put(object.SequenceElement(core.NewTag(0x0040, 0x08ea), units))
	}
	uri := "https://invalid.example/transaction-local/" + mime
	o.Put(core.Element{Header: core.ElementHeader{Tag: storeDocumentTag, VR: core.VROB}, Value: core.BulkDataValue{URI: uri}})
	return o, StoreBulkData{URI: uri, ContentType: mime, Data: data}
}
func wholeSTOWMPEGFixture() []byte {
	// Synthetic Program Stream sequence/picture headers, not decoded pictures.
	es := []byte{0, 0, 1, 0xb3, 1, 0, 16, 0x13, 0, 0, 0, 0, 0, 0, 1, 0xb5, 0x14, 0x8a, 0, 0, 0, 0, 0, 0, 1, 0, 0, 8, 0, 0}
	pack := []byte{0, 0, 1, 0xba, 0x44, 0, 4, 0, 4, 1, 0, 0, 3, 0xf8}
	pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0}
	binary.BigEndian.PutUint16(pes[4:], uint16(len(es)+3))
	return bytes.Join([][]byte{pack, pes, es, {0, 0, 1, 0xb9}}, nil)
}
func wholeSTOWBox(kind string, data []byte) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(len(data)+8))
	copy(b[4:], kind)
	return append(b, data...)
}
func wholeSTOWMP4Fixture() []byte {
	// Independent ISO BMFF table authoring. SPS describes progressive 8-bit420
	// 96x64; two synthetic coded-sample headers exercise whole video versus two
	// still-image parts. This is a structural fixture, not decoder qualification.
	sps := []byte{0x27, 0x64, 0, 0x0b, 0xac, 0x56, 0x50, 0xc3, 0x78, 0x31, 0x84, 0x50}
	pps := []byte{0x28, 0xee, 0x3c, 0xb0}
	sample := []byte{0, 0, 0, 4, 0x65, 0x88, 0x84, 0}
	avcc := append([]byte{1, sps[1], sps[2], sps[3], 255, 225, 0, byte(len(sps))}, sps...)
	avcc = append(avcc, 1, 0, byte(len(pps)))
	avcc = append(avcc, pps...)
	ftyp := wholeSTOWBox("ftyp", []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'a', 'v', 'c', '1'})
	mdat := wholeSTOWBox("mdat", bytes.Repeat(sample, 2))
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
	binary.BigEndian.PutUint32(md[16:], 2)
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
	stsd = append(stsd, wholeSTOWBox("avc1", append(visual, wholeSTOWBox("avcC", avcc)...))...)
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[4:], uint32(len(sample)))
	binary.BigEndian.PutUint32(stsz[8:], 2)
	stts := make([]byte, 16)
	binary.BigEndian.PutUint32(stts[4:], 1)
	binary.BigEndian.PutUint32(stts[8:], 2)
	binary.BigEndian.PutUint32(stts[12:], 1)
	stsc := make([]byte, 20)
	for i, v := range []uint32{1, 1, 2, 1} {
		binary.BigEndian.PutUint32(stsc[4+4*i:], v)
	}
	stco := make([]byte, 12)
	binary.BigEndian.PutUint32(stco[4:], 1)
	binary.BigEndian.PutUint32(stco[8:], uint32(len(ftyp)+8))
	stbl := wholeSTOWBox("stbl", bytes.Join([][]byte{wholeSTOWBox("stsd", stsd), wholeSTOWBox("stsz", stsz), wholeSTOWBox("stts", stts), wholeSTOWBox("stsc", stsc), wholeSTOWBox("stco", stco)}, nil))
	mdia := wholeSTOWBox("mdia", bytes.Join([][]byte{wholeSTOWBox("mdhd", md), wholeSTOWBox("hdlr", hd), wholeSTOWBox("minf", stbl)}, nil))
	trak := wholeSTOWBox("trak", append(wholeSTOWBox("tkhd", tk), mdia...))
	return bytes.Join([][]byte{ftyp, mdat, wholeSTOWBox("moov", trak)}, nil)
}
func wholeSTOWVideo(media, sop string) (*object.Object, StoreBulkData, int) {
	modality := "XC"
	if strings.HasSuffix(sop, "77.1.1.1") {
		modality = "ES"
	}
	if strings.HasSuffix(sop, "77.1.2.1") {
		modality = "GM"
	}
	o := wholeSTOWObject(sop, modality)
	region := object.FromElements([]core.Element{core.NewRawElement(core.NewTag(0x0008, 0x0100), core.VRSH, []byte("SYNTHETIC")), core.NewRawElement(core.NewTag(0x0008, 0x0102), core.VRSH, []byte("99TEST")), core.NewRawElement(core.NewTag(0x0008, 0x0104), core.VRLO, []byte("Synthetic region"))}, nil)
	o.Put(object.SequenceElement(core.NewTag(0x0008, 0x2218), region))
	uri := "urn:synthetic:whole-video"
	o.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.BulkDataValue{URI: uri}})
	data := wholeSTOWMPEGFixture()
	frames := 1
	if media == "video/mp4" {
		data = wholeSTOWMP4Fixture()
		frames = 2
	}
	return o, StoreBulkData{URI: uri, ContentType: media, Data: data}, frames
}

func TestWholeSTOWClientServerDocumentJSONXMLMatrix(t *testing.T) {
	for _, media := range []string{"application/pdf", "text/xml", "model/stl", "model/obj", "model/mtl"} {
		for _, metadata := range []MetadataMediaType{MetadataMediaTypeDICOMJSON, MetadataMediaTypeDICOMXML} {
			t.Run(media+"/"+string(metadata), func(t *testing.T) {
				o, bulk := wholeSTOWDoc(t, media)
				spool := t.TempDir()
				backend := completeServerTestBackend()
				var calls atomic.Int32
				backend.store = func(_ context.Context, r StoreRequest) StoreOutcome {
					calls.Add(1)
					raw, e := io.ReadAll(r.Reader)
					if e != nil {
						return StoreOutcome{Err: e}
					}
					if sha256.Sum256(raw) != r.SHA256 {
						return StoreOutcome{Err: fmt.Errorf("Part10 hash")}
					}
					f, e := object.ReadFile(bytes.NewReader(raw))
					if e != nil {
						return StoreOutcome{Err: e}
					}
					defer f.Close()
					doc, e := encapdoc.Extract(f.Dataset)
					if e != nil || doc.SOPClassUID != r.SOPClassUID || !doc.DeclaredLength || !bytes.Equal(doc.Data, bulk.Data) || f.TransferSyntax.UID != explicitVRLittleEndianUID {
						return StoreOutcome{Err: fmt.Errorf("document identity/payload: %v", e)}
					}
					if media == "text/xml" && doc.HL7InstanceIdentifier != "1.2.3.1535^synthetic" {
						return StoreOutcome{Err: fmt.Errorf("CDA identity")}
					}
					return StoreOutcome{Status: StoreStatusStored}
				}
				s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool})
				if e != nil {
					t.Fatal(e)
				}
				h := httptest.NewServer(s)
				defer h.Close()
				endpoint := Endpoint{BaseURL: h.URL}
				r, e := (Client{Endpoint: endpoint}).StoreMetadata(wholeSTOWContext(t), StoreMetadataRequest{MediaType: metadata, Instances: []*object.Object{o}, BulkData: []StoreBulkData{bulk}})
				if e != nil || calls.Load() != 1 || len(r.Stored) != 1 {
					t.Fatalf("store=%+v err=%v calls=%d", r, e, calls.Load())
				}
				compressedSTOWEmpty(t, spool)
			})
		}
	}
}
func TestWholeSTOWClientServerVideoJSONXMLMatrix(t *testing.T) {
	for _, media := range []string{"video/mpeg", "video/mp4"} {
		for _, sop := range []string{"1.2.840.10008.5.1.4.1.1.77.1.1.1", "1.2.840.10008.5.1.4.1.1.77.1.2.1", "1.2.840.10008.5.1.4.1.1.77.1.4.1"} {
			for _, metadata := range []MetadataMediaType{MetadataMediaTypeDICOMJSON, MetadataMediaTypeDICOMXML} {
				t.Run(media+"/"+sop+"/"+string(metadata), func(t *testing.T) {
					o, bulk, frames := wholeSTOWVideo(media, sop)
					spool := t.TempDir()
					backend := completeServerTestBackend()
					var calls atomic.Int32
					backend.store = func(_ context.Context, r StoreRequest) StoreOutcome {
						calls.Add(1)
						f, e := object.ReadFile(r.Reader)
						if e != nil {
							return StoreOutcome{Err: e}
						}
						defer f.Close()
						syntax := transfer.MPEG2MPML.UID
						if media == "video/mp4" {
							syntax = transfer.MPEG4HP41.UID
						}
						if f.TransferSyntax.UID != syntax || r.TransferSyntaxUID != syntax || r.SOPClassUID != sop {
							return StoreOutcome{Err: fmt.Errorf("video coding identity")}
						}
						pixel, _ := f.Dataset.Get(core.TagPixelData)
						v, ok := pixel.Value.(core.FragmentSequence)
						want := bytes.Clone(bulk.Data)
						if len(want)%2 != 0 {
							want = append(want, 0)
						}
						if !ok || len(v.Fragments) != 1 || len(v.OffsetTable) != 0 || !bytes.Equal(v.Fragments[0], want) {
							return StoreOutcome{Err: fmt.Errorf("whole-video payload changed")}
						}
						count, e := f.Dataset.GetInt(core.NewTag(0x0028, 0x0008))
						if e != nil || count != int64(frames) {
							return StoreOutcome{Err: fmt.Errorf("frame count: %v", e)}
						}
						time, _ := f.Dataset.GetString(core.NewTag(0x0018, 0x1063))
						if time != "40" {
							return StoreOutcome{Err: fmt.Errorf("derived timing: %s", time)}
						}
						return StoreOutcome{Status: StoreStatusStored}
					}
					s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool})
					if e != nil {
						t.Fatal(e)
					}
					h := httptest.NewServer(s)
					defer h.Close()
					endpoint := Endpoint{BaseURL: h.URL}
					r, e := (Client{Endpoint: endpoint}).StoreMetadata(wholeSTOWContext(t), StoreMetadataRequest{MediaType: metadata, Instances: []*object.Object{o}, BulkData: []StoreBulkData{bulk}})
					if e != nil || calls.Load() != 1 || len(r.Stored) != 1 {
						t.Fatalf("store=%+v err=%v calls=%d", r, e, calls.Load())
					}
					compressedSTOWEmpty(t, spool)
				})
			}
		}
	}
}

func wholeSTOWContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func wholeSTOWRawRequest(t *testing.T, s *Server, objects []*object.Object, bulk []StoreBulkData, media MetadataMediaType, ctx context.Context) (int, string) {
	t.Helper()
	encoded, e := encodeStoreMetadata(ctx, media, objects)
	if e != nil {
		t.Fatal(e)
	}
	parts := []stowMetadataTestPart{}
	for _, data := range encoded {
		parts = append(parts, stowMetadataTestPart{contentType: string(media), body: data})
	}
	for _, b := range bulk {
		parts = append(parts, stowMetadataTestPart{contentType: b.ContentType, contentLocation: storeBulkContentLocation(b), body: b.Data})
	}
	body, contentType := stowMetadataMultipart(t, string(media), parts...)
	request := httptest.NewRequest(http.MethodPost, "/studies", body).WithContext(ctx)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/dicom+json")
	r := httptest.NewRecorder()
	s.ServeHTTP(r, request)
	return r.Code, r.Body.String()
}
func TestWholeSTOWSemanticFailuresArePerInstanceAndNeverPublished(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*object.Object, *StoreBulkData)
	}{
		{"MIME-attribute", func(o *object.Object, b *StoreBulkData) {
			o.Put(core.NewRawElement(storeDocumentMIMETag, core.VRLO, []byte("model/obj")))
		}},
		{"SOP", func(o *object.Object, b *StoreBulkData) {
			o.Put(core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte(encapdoc.EncapsulatedOBJStorage)))
		}},
		{"payload", func(o *object.Object, b *StoreBulkData) { b.Data = []byte("not PDF") }},
		{"missing-burned-in", func(o *object.Object, b *StoreBulkData) { o.Remove(core.NewTag(0x0028, 0x0301)) }},
		{"missing-type2", func(o *object.Object, b *StoreBulkData) { o.Remove(core.NewTag(0x0042, 0x0010)) }},
		{"length", func(o *object.Object, b *StoreBulkData) {
			o.Put(object.ULElement(core.NewTag(0x0042, 0x0015), binary.LittleEndian, uint32(len(b.Data)+1)))
		}},
	} {
		for _, media := range []MetadataMediaType{MetadataMediaTypeDICOMJSON, MetadataMediaTypeDICOMXML} {
			t.Run(c.name+"/"+string(media), func(t *testing.T) {
				bad, b := wholeSTOWDoc(t, "application/pdf")
				c.edit(bad, &b)
				bad.Put(core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte(dicomtest.TestSOPInstanceUID+".2")))
				good, g := wholeSTOWDoc(t, "model/mtl")
				spool := t.TempDir()
				backend := completeServerTestBackend()
				var calls atomic.Int32
				backend.store = func(_ context.Context, r StoreRequest) StoreOutcome {
					calls.Add(1)
					if r.SOPClassUID != encapdoc.EncapsulatedMTLStorage {
						return StoreOutcome{Err: fmt.Errorf("incomplete bad instance published")}
					}
					_, e := io.Copy(io.Discard, r.Reader)
					return StoreOutcome{Status: StoreStatusStored, Err: e}
				}
				s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool})
				if e != nil {
					t.Fatal(e)
				}
				status, response := wholeSTOWRawRequest(t, s, []*object.Object{bad, good}, []StoreBulkData{b, g}, media, wholeSTOWContext(t))
				if status != http.StatusAccepted || calls.Load() != 1 || !strings.Contains(response, "00081198") || !strings.Contains(response, strconv.Itoa(0xc000)) {
					t.Fatalf("mixed response status=%d calls=%d %s", status, calls.Load(), response)
				}
				compressedSTOWEmpty(t, spool)
			})
		}
	}
}
func TestWholeSTOWVideoContradictionsRejectPerInstance(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*object.Object, *StoreBulkData)
	}{
		{"unsupported-transfer", func(o *object.Object, b *StoreBulkData) {
			b.ContentType += "; transfer-syntax=" + transfer.JPEGBaseline.UID
		}},
		{"unsupported-container", func(o *object.Object, b *StoreBulkData) { b.ContentType = "video/quicktime" }},
		{"syntax", func(o *object.Object, b *StoreBulkData) {
			b.ContentType += "; transfer-syntax=" + transfer.MPEG2MPHL.UID
		}},
		{"mime", func(o *object.Object, b *StoreBulkData) { b.ContentType = "video/mp4" }},
		{"frames", func(o *object.Object, b *StoreBulkData) {
			o.Put(core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte("9")))
		}},
		{"geometry", func(o *object.Object, b *StoreBulkData) {
			o.Put(core.NewRawElement(core.NewTag(0x0028, 0x0010), core.VRUS, []byte{17, 0}))
		}},
		{"photometric", func(o *object.Object, b *StoreBulkData) {
			o.Put(core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte("RGB")))
		}},
		{"missing-region", func(o *object.Object, b *StoreBulkData) { o.Remove(core.NewTag(0x0008, 0x2218)) }},
		{"missing-instance-number", func(o *object.Object, b *StoreBulkData) { o.Remove(core.NewTag(0x0020, 0x0013)) }},
		{"timing", func(o *object.Object, b *StoreBulkData) {
			o.Put(core.NewRawElement(core.NewTag(0x0018, 0x1063), core.VRDS, []byte("20")))
		}},
		{"payload", func(o *object.Object, b *StoreBulkData) { b.Data = []byte("bad movie") }},
		{"still-SOP", func(o *object.Object, b *StoreBulkData) {
			o.Put(core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte(dicomtest.TestSOPClassUID)))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			o, b, _ := wholeSTOWVideo("video/mpeg", "1.2.840.10008.5.1.4.1.1.77.1.4.1")
			c.edit(o, &b)
			spool := t.TempDir()
			backend := completeServerTestBackend()
			var calls atomic.Int32
			backend.store = func(context.Context, StoreRequest) StoreOutcome {
				calls.Add(1)
				return StoreOutcome{Status: StoreStatusStored}
			}
			s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool})
			if e != nil {
				t.Fatal(e)
			}
			status, response := wholeSTOWRawRequest(t, s, []*object.Object{o}, []StoreBulkData{b}, MetadataMediaTypeDICOMJSON, wholeSTOWContext(t))
			if status != http.StatusConflict || calls.Load() != 0 || !strings.Contains(response, "00081198") {
				t.Fatalf("status=%d calls=%d %s", status, calls.Load(), response)
			}
			compressedSTOWEmpty(t, spool)
		})
	}
}
func TestWholeSTOWDocumentRequiredContextAndCDAIdentifier(t *testing.T) {
	for _, c := range []struct {
		name, media string
		edit        func(*object.Object, *StoreBulkData)
	}{
		{"CDA-id-conflict", "text/xml", func(o *object.Object, b *StoreBulkData) {
			o.Put(core.NewRawElement(core.NewTag(0x0040, 0xe001), core.VRST, []byte("1.2.3.999")))
		}},
		{"CDA-id-missing", "text/xml", func(o *object.Object, b *StoreBulkData) {
			b.Data = []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"/>`)
		}},
		{"STL-FOR-missing", "model/stl", func(o *object.Object, b *StoreBulkData) { o.Remove(core.NewTag(0x0020, 0x0052)) }},
		{"OBJ-units-missing", "model/obj", func(o *object.Object, b *StoreBulkData) { o.Remove(core.NewTag(0x0040, 0x08ea)) }},
		{"MTL-units-empty", "model/mtl", func(o *object.Object, b *StoreBulkData) { o.Put(object.SequenceElement(core.NewTag(0x0040, 0x08ea))) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			o, b := wholeSTOWDoc(t, c.media)
			c.edit(o, &b)
			backend := completeServerTestBackend()
			var calls atomic.Int32
			backend.store = func(context.Context, StoreRequest) StoreOutcome {
				calls.Add(1)
				return StoreOutcome{Status: StoreStatusStored}
			}
			s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: t.TempDir()})
			if e != nil {
				t.Fatal(e)
			}
			status, _ := wholeSTOWRawRequest(t, s, []*object.Object{o}, []StoreBulkData{b}, MetadataMediaTypeDICOMJSON, wholeSTOWContext(t))
			if status != http.StatusConflict || calls.Load() != 0 {
				t.Fatalf("status=%d calls=%d", status, calls.Load())
			}
		})
	}
}
func TestWholeSTOWRequestLocalGraphAndClientBindings(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*object.Object, *[]StoreBulkData)
	}{
		{"missing", func(o *object.Object, b *[]StoreBulkData) { *b = nil }},
		{"duplicate", func(o *object.Object, b *[]StoreBulkData) { *b = append(*b, (*b)[0]) }},
		{"numbered-video", func(o *object.Object, b *[]StoreBulkData) { (*b)[0].FrameNumber = 1 }},
		{"cross-attribute", func(o *object.Object, b *[]StoreBulkData) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x2000), VR: core.VROB}, Value: core.BulkDataValue{URI: (*b)[0].URI}})
		}},
		{"unreferenced", func(o *object.Object, b *[]StoreBulkData) {
			other := (*b)[0]
			other.URI = "urn:unused"
			*b = append(*b, other)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			o, b, _ := wholeSTOWVideo("video/mpeg", "1.2.840.10008.5.1.4.1.1.77.1.4.1")
			parts := []StoreBulkData{b}
			c.edit(o, &parts)
			if e := validateStoreBulkData(StoreMetadataRequest{Instances: []*object.Object{o}, BulkData: parts}); e == nil {
				t.Fatal("client invalid graph accepted")
			}
			spool := t.TempDir()
			backend := completeServerTestBackend()
			var calls atomic.Int32
			backend.store = func(context.Context, StoreRequest) StoreOutcome {
				calls.Add(1)
				return StoreOutcome{Status: StoreStatusStored}
			}
			s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool})
			if e != nil {
				t.Fatal(e)
			}
			status, _ := wholeSTOWRawRequest(t, s, []*object.Object{o}, parts, MetadataMediaTypeDICOMJSON, wholeSTOWContext(t))
			if status != http.StatusBadRequest || calls.Load() != 0 {
				t.Fatalf("status=%d calls=%d", status, calls.Load())
			}
			compressedSTOWEmpty(t, spool)
		})
	}
	o, b := wholeSTOWDoc(t, "application/pdf")
	copyObject := object.FromElements(o.Elements(), nil)
	if e := validateStoreBulkData(StoreMetadataRequest{Instances: []*object.Object{o, copyObject}, BulkData: []StoreBulkData{b}}); e == nil {
		t.Fatal("cross-instance document binding accepted")
	}
	for _, media := range []string{"video/quicktime", "video/h265", "application/pdf; charset=utf-8", "text/xml; charset=iso-8859-1", "video/mpeg; transfer-syntax=" + transfer.JPEGBaseline.UID} {
		if _, _, e := compressedStoreContentType(media); e == nil {
			t.Fatal("unsupported/contradictory header accepted", media)
		}
	}
}
func TestWholeSTOWLimitsCancellationAndNoIncompletePublication(t *testing.T) {
	o, b := wholeSTOWDoc(t, "application/pdf")
	for _, limits := range []ServerLimits{{MaxPartBytes: int64(len(b.Data) - 1)}, {MaxParts: 1}, {MaxDecodedPartBytes: 10}} {
		spool := t.TempDir()
		backend := completeServerTestBackend()
		var calls atomic.Int32
		backend.store = func(context.Context, StoreRequest) StoreOutcome {
			calls.Add(1)
			return StoreOutcome{Status: StoreStatusStored}
		}
		s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool, Limits: limits})
		if e != nil {
			t.Fatal(e)
		}
		status, _ := wholeSTOWRawRequest(t, s, []*object.Object{o}, []StoreBulkData{b}, MetadataMediaTypeDICOMJSON, wholeSTOWContext(t))
		if status != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
			t.Fatalf("limits=%+v status=%d calls=%d", limits, status, calls.Load())
		}
		compressedSTOWEmpty(t, spool)
	}
	// Complete semantic construction must not commit before malformed framing
	// is detected, and cancelling the request leaves neither spool nor backend.
	for _, cancelled := range []bool{false, true} {
		spool := t.TempDir()
		backend := completeServerTestBackend()
		var calls atomic.Int32
		backend.store = func(context.Context, StoreRequest) StoreOutcome {
			calls.Add(1)
			return StoreOutcome{Status: StoreStatusStored}
		}
		s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool})
		if e != nil {
			t.Fatal(e)
		}
		parts := compressedSTOWParts(t, o, []StoreBulkData{b})
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		status := compressedSTOWSend(t, s, ctx, parts, !cancelled)
		cancel()
		if status == http.StatusOK || calls.Load() != 0 {
			t.Fatalf("incomplete/cancelled status=%d calls=%d", status, calls.Load())
		}
		compressedSTOWEmpty(t, spool)
	}
}

func TestWholeSTOWExplicitVideoProfilesAndType2SpecimenContext(t *testing.T) {
	for _, high := range []bool{false, true} {
		for _, metadata := range []MetadataMediaType{MetadataMediaTypeDICOMJSON, MetadataMediaTypeDICOMXML} {
			o, b, _ := wholeSTOWVideo("video/mpeg", "1.2.840.10008.5.1.4.1.1.77.1.2.1")
			syntax := transfer.MPEG2MPML.UID
			rows, columns := 16, 16
			if high {
				// Main Profile/High Level: 1280x720,16:9,25Hz; unchanged PS bytes.
				index := bytes.Index(b.Data, []byte{0, 0, 1, 0xb3})
				copy(b.Data[index+4:index+8], []byte{0x50, 0x02, 0xd0, 0x33})
				index = bytes.Index(b.Data, []byte{0, 0, 1, 0xb5})
				b.Data[index+5] = 0x4a
				syntax, rows, columns = transfer.MPEG2MPHL.UID, 720, 1280
			}
			b.ContentType += "; transfer-syntax=" + syntax
			// Empty Type 2 values are legal; anatomy is conditional on specimen absence.
			o.Put(core.NewRawElement(core.NewTag(0x0020, 0x0011), core.VRIS, nil))
			o.Put(core.NewRawElement(core.NewTag(0x0020, 0x0013), core.VRIS, nil))
			o.Remove(core.NewTag(0x0008, 0x2218))
			specimen := object.FromElements([]core.Element{core.NewRawElement(core.NewTag(0x0040, 0x0551), core.VRLO, []byte("synthetic-specimen"))}, nil)
			o.Put(object.SequenceElement(core.NewTag(0x0040, 0x0560), specimen))
			o.Put(core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte("1")))
			o.Put(core.NewRawElement(core.NewTag(0x0018, 0x1063), core.VRDS, []byte("40")))
			for tag, value := range map[core.Tag]int{core.NewTag(0x0028, 0x0010): rows, core.NewTag(0x0028, 0x0011): columns} {
				v := make([]byte, 2)
				binary.LittleEndian.PutUint16(v, uint16(value))
				o.Put(core.NewRawElement(tag, core.VRUS, v))
			}
			spool := t.TempDir()
			backend := completeServerTestBackend()
			var calls atomic.Int32
			backend.store = func(_ context.Context, r StoreRequest) StoreOutcome {
				calls.Add(1)
				f, e := object.ReadFile(r.Reader)
				if e != nil {
					return StoreOutcome{Err: e}
				}
				defer f.Close()
				if f.TransferSyntax.UID != syntax {
					return StoreOutcome{Err: fmt.Errorf("explicit syntax changed")}
				}
				pixel, _ := f.Dataset.Get(core.TagPixelData)
				value, ok := pixel.Value.(core.FragmentSequence)
				want := bytes.Clone(b.Data)
				if len(want)%2 != 0 {
					want = append(want, 0)
				}
				if !ok || len(value.Fragments) != 1 || !bytes.Equal(value.Fragments[0], want) {
					return StoreOutcome{Err: fmt.Errorf("explicit profile bytes changed")}
				}
				return StoreOutcome{Status: StoreStatusStored}
			}
			s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool})
			if e != nil {
				t.Fatal(e)
			}
			h := httptest.NewServer(s)
			result, e := (Client{Endpoint: Endpoint{BaseURL: h.URL}}).StoreMetadata(wholeSTOWContext(t), StoreMetadataRequest{MediaType: metadata, Instances: []*object.Object{o}, BulkData: []StoreBulkData{b}})
			h.Close()
			if e != nil || len(result.Stored) != 1 || calls.Load() != 1 {
				t.Fatalf("high=%v metadata=%s result=%+v error=%v", high, metadata, result, e)
			}
			compressedSTOWEmpty(t, spool)
		}
	}
}

func TestWholeSTOWServerCrossInstanceAndPriorTransactionReferences(t *testing.T) {
	o, b := wholeSTOWDoc(t, "application/pdf")
	other := object.FromElements(o.Elements(), nil)
	other.Put(core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.2.3.1535.99")))
	spool := t.TempDir()
	backend := completeServerTestBackend()
	var calls atomic.Int32
	backend.store = func(context.Context, StoreRequest) StoreOutcome {
		calls.Add(1)
		return StoreOutcome{Status: StoreStatusStored}
	}
	s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool})
	if e != nil {
		t.Fatal(e)
	}
	status, _ := wholeSTOWRawRequest(t, s, []*object.Object{o, other}, []StoreBulkData{b}, MetadataMediaTypeDICOMJSON, wholeSTOWContext(t))
	if status != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("cross reference status=%d calls=%d", status, calls.Load())
	}
	status, _ = wholeSTOWRawRequest(t, s, []*object.Object{o}, []StoreBulkData{b}, MetadataMediaTypeDICOMXML, wholeSTOWContext(t))
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("prior valid status=%d calls=%d", status, calls.Load())
	}
	status, _ = wholeSTOWRawRequest(t, s, []*object.Object{o}, nil, MetadataMediaTypeDICOMJSON, wholeSTOWContext(t))
	if status != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("prior transaction reference status=%d calls=%d", status, calls.Load())
	}
	compressedSTOWEmpty(t, spool)
}

func TestWholeSTOWVideoFrameQuotaBeforePublication(t *testing.T) {
	o, b, _ := wholeSTOWVideo("video/mp4", "1.2.840.10008.5.1.4.1.1.77.1.4.1")
	spool := t.TempDir()
	backend := completeServerTestBackend()
	var calls atomic.Int32
	backend.store = func(context.Context, StoreRequest) StoreOutcome {
		calls.Add(1)
		return StoreOutcome{Status: StoreStatusStored}
	}
	s, e := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool, Limits: ServerLimits{MaxFrames: 1}})
	if e != nil {
		t.Fatal(e)
	}
	status, _ := wholeSTOWRawRequest(t, s, []*object.Object{o}, []StoreBulkData{b}, MetadataMediaTypeDICOMJSON, wholeSTOWContext(t))
	if status != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
		t.Fatalf("frame quota status=%d calls=%d", status, calls.Load())
	}
	compressedSTOWEmpty(t, spool)
}
