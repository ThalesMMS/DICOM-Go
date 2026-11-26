package vendors_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/vendors"
	"github.com/ThalesMMS/dicom-go/vendors/vendorfixture"
)

func fixtureDataset(t *testing.T) *object.Object {
	t.Helper()
	dataset := object.New(std.Dictionary)
	if err := vendorfixture.Put(dataset); err != nil {
		t.Fatal(err)
	}
	return dataset
}

func TestDetectPrivateHeadersFlagsIdentifyingHeaders(t *testing.T) {
	found := vendors.DetectPrivateHeaders(fixtureDataset(t))
	var kinds []string
	identifying := map[vendors.PrivateHeaderKind]bool{}
	for _, location := range found {
		kinds = append(kinds, string(location.Kind))
		identifying[location.Kind] = location.MayIdentify
	}
	want := "siemens-csa-image siemens-csa-series siemens-mr-protocol siemens-medcom-history ge-protocol-data-block elscint-protocol vepro-data"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("kinds %v", kinds)
	}
	if !identifying[vendors.SiemensMedComHistory] || !identifying[vendors.VEPROHeader] || identifying[vendors.SiemensCSAImage] || identifying[vendors.GEProtocolDataBlockHeader] {
		t.Fatalf("identifying %v", identifying)
	}
	if found[0].Tag != core.NewTag(0x0029, 0x1010) || found[3].Tag != core.NewTag(0x0029, 0x1120) {
		t.Fatalf("tags %v %v", found[0].Tag, found[3].Tag)
	}
	if got := vendors.DetectPrivateHeaders(object.New(std.Dictionary)); len(got) != 0 {
		t.Fatalf("empty data set: %v", got)
	}
}

func TestDecodePrivateHeadersRendersEveryVendor(t *testing.T) {
	headers := vendors.DecodePrivateHeaders(fixtureDataset(t), vendors.PrivateHeaderOptions{})
	text := map[vendors.PrivateHeaderKind]string{}
	for _, header := range headers {
		if header.Err != nil {
			t.Fatalf("%s: %v", header.Kind, header.Err)
		}
		text[header.Kind] = strings.Join(header.Lines, "\n")
	}
	for kind, want := range map[vendors.PrivateHeaderKind]string{
		vendors.SiemensCSAImage:           vendorfixture.CSAImageElementName + " US 1 [" + vendorfixture.CSAImageElementValue + "]",
		vendors.SiemensCSASeries:          "MrPhoenixProtocol UN 1",
		vendors.SiemensMRProtocol:         vendorfixture.MRProtocolKey + " = " + vendorfixture.MRProtocolValue,
		vendors.SiemensMedComHistory:      "CsaPatient\n  0 | " + vendorfixture.MedComPatientName,
		vendors.GEProtocolDataBlockHeader: vendorfixture.GEProtocolKey + " = " + vendorfixture.GEProtocolValue,
		vendors.ElscintProtocolHeader:     ": protocol-name = " + vendorfixture.ElscintProtocolName,
		vendors.VEPROHeader:               "Data Field 2: " + vendorfixture.VEPROPatientName,
	} {
		if !strings.Contains(text[kind], want) {
			t.Errorf("%s does not show %q:\n%s", kind, want, text[kind])
		}
	}
	if !strings.Contains(text[vendors.ElscintProtocolHeader], "scans[0]: mas = 200") {
		t.Errorf("ELSCINT multiple group:\n%s", text[vendors.ElscintProtocolHeader])
	}

	only := vendors.DecodePrivateHeaders(fixtureDataset(t), vendors.PrivateHeaderOptions{Kinds: []vendors.PrivateHeaderKind{vendors.GEProtocolDataBlockHeader}, MaxLines: 1})
	if len(only) != 1 || only[0].Kind != vendors.GEProtocolDataBlockHeader || len(only[0].Lines) != 1 || !only[0].Truncated {
		t.Fatalf("selected header %+v", only)
	}
}

func TestDecodePrivateHeadersReportsBrokenBlobs(t *testing.T) {
	dataset := object.New(std.Dictionary)
	dataset.Put(core.NewRawElement(core.NewTag(0x0055, 0x0010), core.VRLO, []byte("VEPRO VIF 3.0 DATA")))
	dataset.Put(core.NewRawElement(core.NewTag(0x0055, 0x1020), core.VROB, []byte("short")))
	headers := vendors.DecodePrivateHeaders(dataset, vendors.PrivateHeaderOptions{})
	if len(headers) != 1 || !errors.Is(headers[0].Err, vendors.ErrInvalid) || len(headers[0].Lines) != 0 {
		t.Fatalf("headers %+v", headers)
	}
}

// TestDecodePrivateHeadersOnGDCMData decodes every gdcmData file and requires
// each vendor header kind to decode somewhere in the corpus, and every
// detected header to decode or fail with a reported error (never a panic).
// Enable with DICOMGO_GDCMDATA_ROOT.
func TestDecodePrivateHeadersOnGDCMData(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("set DICOMGO_GDCMDATA_ROOT to a separately obtained GDCMData checkout")
	}
	paths, _ := filepath.Glob(filepath.Join(root, "*.dcm"))
	decoded := map[vendors.PrivateHeaderKind]int{}
	for _, path := range paths {
		file, err := object.OpenFileWithOptions(path, object.ReadFileOptions{RecoverDelimiterAnomalies: true})
		if err != nil {
			continue
		}
		for _, header := range vendors.DecodePrivateHeaders(file.Dataset, vendors.PrivateHeaderOptions{}) {
			if header.Err == nil && len(header.Lines) > 0 {
				decoded[header.Kind]++
			} else if header.Err == nil {
				t.Errorf("%s: %s decoded to nothing without an error", filepath.Base(path), header.Kind)
			}
		}
	}
	for _, info := range vendors.PrivateHeaderKinds {
		if decoded[info.Kind] == 0 {
			t.Errorf("no gdcmData file decoded %s", info.Kind)
		}
	}
	t.Logf("decoded headers per kind: %v", decoded)
}
