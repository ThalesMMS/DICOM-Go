package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const wideEndianPydicomEnableEnv = "DICOM_GO_PYDICOM_WIDE_ENDIAN"

type wideEndianManifestCase struct {
	Name                string   `json:"name"`
	BitsAllocated       uint16   `json:"bits_allocated"`
	BitsStored          uint16   `json:"bits_stored"`
	PixelRepresentation uint16   `json:"pixel_representation"`
	Values              []string `json:"values"`
	IconBits            uint16   `json:"icon_bits"`
	IconValues          []int    `json:"icon_values"`
}

// TestPydicomWideSampleEndianInterop is the opt-in independent oracle for #937.
// pydicom 3.0.2/NumPy 2.1.3 read every Go output (BE and LE Part 10, BE DICOM
// JSON) and compare all samples; they also author Big Endian files that Go must
// read and convert. Enable with DICOM_GO_PYDICOM_WIDE_ENDIAN=1 and
// DICOM_GO_PYTHON; without the gate the test skips and qualifies nothing.
func TestPydicomWideSampleEndianInterop(t *testing.T) {
	if os.Getenv(wideEndianPydicomEnableEnv) != "1" {
		t.Skip("set " + wideEndianPydicomEnableEnv + "=1 and DICOM_GO_PYTHON to run the pydicom wide-sample oracle")
	}
	python := strings.TrimSpace(os.Getenv("DICOM_GO_PYTHON"))
	if python == "" {
		t.Fatal("DICOM_GO_PYTHON is required when the pydicom wide-sample oracle is enabled")
	}
	ctx := context.Background()
	dir := t.TempDir()
	var manifest []wideEndianManifestCase
	for index, c := range wideEndianCases() {
		entry := wideEndianManifestCase{Name: c.name, BitsAllocated: c.bitsAllocated, BitsStored: c.bitsStored, PixelRepresentation: c.pixelRepresentation, IconBits: c.iconBits}
		for _, v := range c.values {
			entry.Values = append(entry.Values, strconv.FormatUint(v, 10))
		}
		switch c.iconBits {
		case 8:
			for _, b := range c.iconBytes(binary.LittleEndian) {
				entry.IconValues = append(entry.IconValues, int(b))
			}
		case 16:
			for _, v := range wideIconValues16 {
				entry.IconValues = append(entry.IconValues, int(v))
			}
		}
		manifest = append(manifest, entry)

		bigEndian, _, err := pixeldata.TranscodeDataSet(ctx, c.object(200+index), transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		writeWidePart10(t, filepath.Join(dir, "go-be-"+c.name+".dcm"), bigEndian, transfer.ExplicitVRBigEndian)
		little, _, err := pixeldata.TranscodeDataSet(ctx, bigEndian, transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		writeWidePart10(t, filepath.Join(dir, "go-le-"+c.name+".dcm"), little, transfer.ExplicitVRLittleEndian)
		document, err := dicomjson.Marshal(bigEndian, dicomjson.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go-be-"+c.name+".json"), document, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "wide_endian_oracle.py")
	if err := os.WriteFile(script, []byte(wideEndianPydicomScript), 0o600); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(runCtx, python, script, dir).CombinedOutput(); err != nil {
		t.Fatalf("pydicom wide-sample oracle failed: %v\n%s", err, output)
	}

	// Go reads the pydicom-authored Big Endian files.
	for _, c := range wideEndianCases() {
		file, err := object.OpenFile(filepath.Join(dir, "py-be-"+c.name+".dcm"))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		t.Cleanup(func() { _ = file.Close() })
		if file.TransferSyntax.UID != transfer.ExplicitVRBigEndian.UID {
			t.Fatalf("%s: pydicom syntax %s", c.name, file.TransferSyntax.UID)
		}
		assertWideLayout(t, "pydicom BE "+c.name, c, file.Dataset, binary.BigEndian)
		little, _, err := pixeldata.TranscodeDataSet(ctx, file.Dataset, file.TransferSyntax, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertWideLayout(t, "pydicom BE -> LE "+c.name, c, little, binary.LittleEndian)
		if raw := wideRawPixel(t, little); bytes.Equal(raw, wordSwapped(c.pixelBytes(binary.BigEndian))) {
			t.Fatalf("%s: half-swapped conversion", c.name)
		}
	}
}

const wideEndianPydicomScript = `
import json
import sys
from pathlib import Path

import numpy as np
import pydicom
from pydicom.dataset import Dataset, FileMetaDataset
from pydicom.sequence import Sequence
from pydicom.uid import ExplicitVRBigEndian, ExplicitVRLittleEndian

assert (pydicom.__version__, np.__version__) == ("3.0.2", "2.1.3"), (pydicom.__version__, np.__version__)
root = Path(sys.argv[1])
manifest = json.loads((root / "manifest.json").read_text())


def dtype(bits, signed, big):
    return np.dtype(("i" if signed else "u") + str(bits // 8)).newbyteorder(">" if big else "<")


def expected(case):
    raw = np.array([int(v) for v in case["values"]], dtype=np.uint64)
    bits = case["bits_allocated"]
    stored = raw.astype(np.dtype("u" + str(bits // 8)))
    if case["pixel_representation"]:
        stored = stored.view(np.dtype("i" + str(bits // 8)))
    return stored.reshape(2, 2, 3)


def check_icon(case, item, big, label):
    if not case["icon_bits"]:
        assert "IconImageSequence" not in item, label
        return
    icon = item.IconImageSequence[0]
    assert icon.BitsAllocated == case["icon_bits"], label
    got = np.frombuffer(icon.PixelData, dtype=dtype(case["icon_bits"], False, big))
    assert got.tolist() == case["icon_values"], (label, got.tolist())


for case in manifest:
    name = case["name"]
    want = expected(case)
    for prefix, big in (("go-be-", True), ("go-le-", False)):
        label = prefix + name
        ds = pydicom.dcmread(root / (label + ".dcm"))
        assert ds.file_meta.TransferSyntaxUID == (ExplicitVRBigEndian if big else ExplicitVRLittleEndian), label
        assert ds.BitsAllocated == case["bits_allocated"], label
        arr = ds.pixel_array
        assert arr.shape == want.shape, (label, arr.shape)
        assert np.array_equal(arr.astype(want.dtype), want), (label, arr.ravel().tolist(), want.ravel().tolist())
        raw = np.frombuffer(ds.PixelData, dtype=dtype(case["bits_allocated"], bool(case["pixel_representation"]), big))
        assert np.array_equal(raw.reshape(want.shape), want), label
        check_icon(case, ds, big, label)

    # DICOM JSON: InlineBinary is the Big Endian Value Field of the source.
    label = "go-be-" + name + ".json"
    js = Dataset.from_json((root / label).read_text())
    raw = np.frombuffer(js.PixelData, dtype=dtype(case["bits_allocated"], bool(case["pixel_representation"]), True))
    assert np.array_equal(raw.reshape(want.shape), want), label
    assert js.BitsAllocated == case["bits_allocated"], label
    check_icon(case, js, True, label)

    # Independently authored Big Endian file for the Go reader.
    ds = Dataset()
    ds.SOPClassUID = "1.2.840.10008.5.1.4.1.1.481.2"
    ds.SOPInstanceUID = "1.2.826.0.1.3680043.10.543.937.3" + str(manifest.index(case))
    ds.Modality = "RTDOSE"
    ds.PatientName = "SYNTHETIC^PYDICOM"
    ds.NumberOfFrames = 2
    ds.SamplesPerPixel = 1
    ds.PhotometricInterpretation = "MONOCHROME2"
    ds.Rows = 2
    ds.Columns = 3
    ds.BitsAllocated = case["bits_allocated"]
    ds.BitsStored = case["bits_stored"]
    ds.HighBit = case["bits_stored"] - 1
    ds.PixelRepresentation = case["pixel_representation"]
    if case["icon_bits"]:
        icon = Dataset()
        icon.SamplesPerPixel = 1
        icon.PhotometricInterpretation = "MONOCHROME2"
        icon.Rows = 2
        icon.Columns = 2
        icon.BitsAllocated = case["icon_bits"]
        icon.BitsStored = case["icon_bits"]
        icon.HighBit = case["icon_bits"] - 1
        icon.PixelRepresentation = 0
        icon.PixelData = np.array(case["icon_values"], dtype=dtype(case["icon_bits"], False, True)).tobytes()
        icon["PixelData"].VR = "OB" if case["icon_bits"] == 8 else "OW"
        ds.IconImageSequence = Sequence([icon])
    ds.PixelData = want.astype(dtype(case["bits_allocated"], bool(case["pixel_representation"]), True)).tobytes()
    ds["PixelData"].VR = "OW"
    ds.file_meta = FileMetaDataset()
    ds.file_meta.MediaStorageSOPClassUID = ds.SOPClassUID
    ds.file_meta.MediaStorageSOPInstanceUID = ds.SOPInstanceUID
    ds.file_meta.TransferSyntaxUID = ExplicitVRBigEndian
    out = root / ("py-be-" + name + ".dcm")
    ds.save_as(out, implicit_vr=False, little_endian=False, enforce_file_format=True)
    check = pydicom.dcmread(out)
    assert np.array_equal(check.pixel_array.astype(want.dtype), want), "pydicom self-check " + name

print("pydicom wide-sample oracle: %d cases" % len(manifest))
`
