package dicomjson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func bulkCampaignFixture(tb testing.TB, size int64) (*object.Object, int64) {
	tb.Helper()
	if size < 2 || size%2 != 0 || size > 128<<20 {
		tb.Fatal("invalid campaign fixture size")
	}
	file, err := os.CreateTemp(tb.TempDir(), "synthetic-bulk-*.dcm")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { file.Close() })
	seq := core.NewTag(0x0008, 0x1115)
	leaf := core.NewTag(0x0042, 0x0011)
	header := dicomtest.SequenceHeaderBytes(transfer.ExplicitVRLittleEndian, seq, uint32(core.UndefinedLength))
	header = append(header, dicomtest.SequenceControlBytes(transfer.ExplicitVRLittleEndian.ByteOrder, core.TagItem, uint32(12+size))...)
	header = append(header, dicomtest.ExplicitLongHeaderBytes(transfer.ExplicitVRLittleEndian.ByteOrder, leaf, core.VROB, uint32(size))...)
	if _, err := file.Write(header); err != nil {
		tb.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{0x5a}, 64<<10)
	for written := int64(0); written < size; {
		n := min(int64(len(chunk)), size-written)
		if _, err := file.Write(chunk[:n]); err != nil {
			tb.Fatal(err)
		}
		written += n
	}
	if _, err := file.Write(dicomtest.SequenceControlBytes(transfer.ExplicitVRLittleEndian.ByteOrder, core.TagSequenceDelimitationItem, 0)); err != nil {
		tb.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		tb.Fatal(err)
	}
	obj, err := object.ReadDataSetWithOptions(file, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{
		MaxElementBytes: size, MaxTotalBytes: size + int64(len(header)) + 9,
		BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{VR: core.VROB, Threshold: 1024}}},
	})
	if err != nil {
		tb.Fatal(err)
	}
	return obj, int64(len(header))
}

func bulkCampaignOptions(tb testing.TB, mode string, size, offset int64) Options {
	tb.Helper()
	const uri = "https://example.invalid/synthetic-bulk"
	opts := Options{MaxMaterializedValueBytes: size, Limits: Limits{MaxJSONBytes: 4096, MaxSequenceDepth: 4, MaxElements: 4, MaxSequenceItems: 4}}
	switch mode {
	case "reference":
		opts.BulkDataReferenceURIFunc = func(_ context.Context, ref object.BulkDataReference) (string, error) {
			if ref.Length != size || ref.Offset != offset || len(ref.Path) != 2 {
				return "", fmt.Errorf("campaign reference mismatch")
			}
			return uri, nil
		}
	case "bytes":
		opts.BulkDataURIFunc = func(_ core.Tag, _ core.VR, raw []byte) string {
			if int64(len(raw)) != size || raw[0] != 0x5a || raw[len(raw)-1] != 0x5a {
				tb.Fatal("campaign payload mismatch")
			}
			return uri
		}
	default:
		tb.Fatal("invalid campaign mode")
	}
	return opts
}

// This test requires explicit opt-in because it is a memory campaign, not a
// routine unit test. Fixture creation/parsing happens before measurement. Both
// paths export identical JSON; only the callback's input contract differs.
func TestBulkReferenceMemoryCampaign(t *testing.T) {
	mode := os.Getenv("DICOM_BULK_CAMPAIGN_MODE")
	if mode == "" {
		t.Skip("set DICOM_BULK_CAMPAIGN_MODE=reference|bytes for a reserved campaign")
	}
	size, err := strconv.ParseInt(os.Getenv("DICOM_BULK_CAMPAIGN_BYTES"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	obj, offset := bulkCampaignFixture(t, size)
	opts := bulkCampaignOptions(t, mode, size, offset)
	// Require contract equivalence before collecting the measured exports.
	reference, err := Marshal(obj, bulkCampaignOptions(t, "reference", size, offset))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	const exports = 5
	for i := 0; i < exports; i++ {
		encoded, err := MarshalContext(context.Background(), obj, opts)
		if err != nil || !bytes.Equal(encoded, reference) {
			t.Fatalf("export mismatch err=%v", err)
		}
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	result := struct {
		Mode            string `json:"mode"`
		PayloadBytes    int64  `json:"payload_bytes"`
		Exports         int    `json:"exports"`
		AllocatedBytes  uint64 `json:"allocated_bytes"`
		AllocationCount uint64 `json:"allocation_count"`
		ElapsedNS       int64  `json:"elapsed_ns"`
		JSONBytes       int    `json:"json_bytes"`
	}{mode, size, exports, after.TotalAlloc - before.TotalAlloc, after.Mallocs - before.Mallocs, elapsed.Nanoseconds(), len(reference)}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("BULK_CAMPAIGN %s", encoded)
}

func TestBulkReferenceCampaignFixtureContract(t *testing.T) {
	obj, offset := bulkCampaignFixture(t, 4096)
	reference, err := Marshal(obj, bulkCampaignOptions(t, "reference", 4096, offset))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Marshal(obj, bulkCampaignOptions(t, "bytes", 4096, offset))
	if err != nil || !bytes.Equal(encoded, reference) {
		t.Fatalf("campaign contract mismatch %v", err)
	}
}
