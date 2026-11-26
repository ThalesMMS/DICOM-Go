package dimse

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// This harness also runs unchanged against the preserved pre-#1299 module.
// It measures encoded P-DATA envelopes, not syscalls or network latency.
func BenchmarkMessagePacking(b *testing.B) {
	for _, size := range []int{512, 1024, 32 << 20} {
		for _, bodyCap := range []uint32{16378, 128 << 10} {
			for _, service := range []string{"C-FIND", "N-GET-RSP", "C-STORE-raw"} {
				b.Run(fmt.Sprintf("%s/%dB/%dBody", service, size, bodyCap), func(b *testing.B) {
					conn := &messagePackingConn{}
					assoc := &ul.Association{Conn: conn, PeerMaxPDU: bodyCap, MaxPDU: ul.DefaultMaxPDU, Context: context.Background(), AcceptedContexts: []ul.AcceptedContext{{ID: 1, AbstractSyntaxUID: secondaryCaptureSOPClassUID, TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID}}}
					data := messagePackingData(size)
					command := (NormalizedGetResponse{AffectedSOPClassUID: secondaryCaptureSOPClassUID, MessageIDBeingRespondedTo: 1, CommandDataSetType: DataSetPresent, Status: StatusSuccess, AffectedSOPInstanceUID: "2.25.1299"}).CommandSet()
					client := &StoreClient{Assoc: assoc}
					chunk := bytes.Repeat([]byte{0x5a}, 32<<10)
					rsp, err := EncodeCommandSet((CStoreResponse{AffectedSOPClassUID: secondaryCaptureSOPClassUID, AffectedSOPInstanceUID: "2.25.1299", MessageIDBeingRespondedTo: 1, Status: StatusSuccess}).CommandSet())
					if err != nil {
						b.Fatal(err)
					}
					var reply bytes.Buffer
					if err := ul.WritePDU(&reply, &ul.PDataTF{Values: []ul.PDataValue{{PresentationContextID: 1, IsCommand: true, IsLast: true, Data: rsp}}}); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.SetBytes(int64(size))
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						var err error
						switch service {
						case "C-FIND":
							err = SendCFindResponse(assoc, 1, CFindResponse{AffectedSOPClassUID: StudyRootFindSOPClassUID, MessageIDBeingRespondedTo: 1, Status: 0xff00}, data, transfer.ExplicitVRLittleEndian)
						case "N-GET-RSP":
							err = sendNormalizedSCPResponse(context.Background(), assoc, 1, transfer.ExplicitVRLittleEndian, command, data, nil)
						case "C-STORE-raw":
							conn.reply = bytes.NewReader(reply.Bytes())
							_, err = client.StoreEncodedWithOptions(context.Background(), func(_ context.Context, w io.Writer, _ transfer.Syntax) error {
								for remaining := size; remaining > 0; {
									n := min(remaining, len(chunk))
									if _, err := w.Write(chunk[:n]); err != nil {
										return err
									}
									remaining -= n
								}
								return nil
							}, CStoreOptions{MessageID: 1, AffectedSOPClassUID: secondaryCaptureSOPClassUID, AffectedSOPInstanceUID: "2.25.1299"})
						}
						if err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(conn.pdus)/float64(b.N), "P-DATA/message")
					b.ReportMetric(float64(conn.pdvs)/float64(b.N), "PDV/message")
					b.ReportMetric(float64(conn.wire)/float64(b.N), "wire-B/message")
				})
			}
		}
	}
}

func messagePackingData(encodedBytes int) *object.Object {
	// UT provides a legal long text VR, including for the generated large case.
	return object.FromElements([]core.Element{core.NewRawElement(core.NewTag(0x0040, 0xA160), core.VRUT, bytes.Repeat([]byte{'x'}, encodedBytes-12))}, nil)
}

type messagePackingConn struct {
	pDataDiscardConn
	reply            *bytes.Reader
	pdus, pdvs, wire int64
	capture          *bytes.Buffer
	writeHook        func([]byte) error
}

func (c *messagePackingConn) Write(p []byte) (int, error) {
	if c.writeHook != nil {
		if err := c.writeHook(p); err != nil {
			return 0, err
		}
	}
	if len(p) < 6 || p[0] != byte(ul.PDUDataTF) || int(binary.BigEndian.Uint32(p[2:6])) != len(p)-6 {
		return 0, fmt.Errorf("packing test: unexpected physical PDU encoding")
	}
	c.pdus++
	c.wire += int64(len(p))
	for offset := 6; offset < len(p); {
		if offset+4 > len(p) {
			return 0, io.ErrUnexpectedEOF
		}
		length := int(binary.BigEndian.Uint32(p[offset : offset+4]))
		if length < 2 || offset+4+length > len(p) {
			return 0, io.ErrUnexpectedEOF
		}
		c.pdvs++
		offset += 4 + length
	}
	if c.capture != nil {
		_, _ = c.capture.Write(p)
	}
	return len(p), nil
}
func (c *messagePackingConn) Read(p []byte) (int, error) {
	if c.reply == nil {
		return 0, io.EOF
	}
	return c.reply.Read(p)
}

// Keep net.Conn assertions explicit: the benchmark never treats one Write as
// a syscall measurement, although this current encoder writes one full PDU.
var _ net.Conn = (*messagePackingConn)(nil)
