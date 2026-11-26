package storetranscode

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// This same harness is compiled over the pinned prior source and candidate in
// separate processes. Inputs are pre-generated in a separate bounded phase;
// no fixture-sized buffer is created by the measured process. OS maximum RSS
// and observed temporary-file samples are captured by the external campaign.
func TestStoreIncrementalPreparationMeasurement(t *testing.T) {
	input := os.Getenv("DICOM_STORE_PREPARATION_INPUT")
	if input == "" {
		t.Skip("explicit fixed local preparation measurement")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	options := testOptions(t, transfer.RLELossless)
	source, err := NewSource(dimse.NewPathStoreSource(input), options)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	descriptor, err := source.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inspected := time.Since(start)
	start = time.Now()
	opened, err := source.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prepared := time.Since(start)
	if descriptor.SOPInstanceUID != opened.Descriptor.SOPInstanceUID {
		t.Fatal("measurement identity differs")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, options.SpoolDirectory)
	record := struct {
		InspectNanoseconds, OpenNanoseconds int64
		TargetSyntax                        string
	}{inspected.Nanoseconds(), prepared.Nanoseconds(), options.Target.UID}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(raw))
}
