package dcmdump

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clireadopts"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
)

// Real upstream files stay outside the repository. Ordinary tests use synthetic bytes.
func TestDumpGDCMBoundedRecoveryCorpus(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCM_DATA")
	if root == "" {
		t.Skip("set DICOMGO_GDCM_DATA to a trusted GDCM Testing/Data directory")
	}
	cases := []struct {
		name  string
		flags []string
	}{
		{"IllegalGroup2ImplicitTS.dcm", nil},
		{"KODAK-12-MONO1-Odd_Terminated_Sequence.dcm", []string{"-recover-transfer-syntax"}},
		{"MR_ELSCINT1_00e1_1042_SQ_feff_00e0_Item.dcm", []string{"-recover-transfer-syntax"}},
		{"ExplicitVRforPublicElementsImplicitVRforShadowElements.dcm", []string{"-read-recover-implicit-vr-elements"}},
		{"MR_Philips_Intera_PrivateSequenceExplicitVR_in_SQ_2001_e05f_item_wrong_lgt_use_NOSHADOWSEQ.dcm", []string{"-read-private-sequence-as-bytes"}},
		{"SIEMENS_MAGNETOM-12-MONO2-GDCM12-VRUN.dcm", []string{"-read-short-length-un"}},
		{"UnexpectedSequenceDelimiterInFixedLengthSequence.dcm", nil},
		{"MEDILABValidCP246_EVRLESQasOB.dcm", nil},
		{"AMIInvalidPrivateDefinedLengthSQasUN.dcm", nil},
		{"SIEMENS_Sonata-12-MONO2-SQ.dcm", nil},
		{"DX_GE_FALCON_SNOWY-VOI.dcm", nil},
		{"fffc0000UN.dcm", nil},
	}
	var total int64
	for _, c := range cases {
		path := filepath.Join(root, c.name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal("corpus member unavailable")
		}
		if !info.Mode().IsRegular() || info.Size() > 16<<20 || info.Size() <= 0 || total > 64<<20-info.Size() {
			t.Fatal("corpus admission limit exceeded")
		}
		total += info.Size()
		t.Run(c.name, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("corpus read failed")
			}
			hash := sha256.Sum256(data)
			read := func(flags []string) (bool, string, string) {
				opts, _, err := parseArgs(append(append([]string{"-read-max-boundary-recoveries", "4096"}, flags...), path), &bytes.Buffer{})
				if err != nil {
					t.Fatal("corpus flag policy rejected")
				}
				file, replay, _, _, _, cleanup, err := openFileForDumpInput(context.Background(), path, opts)
				if err != nil {
					class := string(clidiag.Classify(err))
					if errors.Is(err, parser.ErrOddElementLength) {
						class += "/odd"
					}
					var pe *parser.ParseError
					if errors.As(err, &pe) {
						class += fmt.Sprintf("/%s/tag=%s/offset=%d/length=%d", pe.Op, pe.Tag, pe.Offset, pe.Length)
					}
					return false, class, ""
				}
				defer cleanup()
				report, err := clireadopts.CollectFileReplay(context.Background(), file, opts.readPolicy, opts.readPolicy.Apply(object.ReadFileOptions{}), replay)
				if err != nil {
					return false, "report-limit", ""
				}
				reasons := map[string]bool{}
				var list []string
				for _, e := range report.Events {
					if !reasons[e.Reason] {
						reasons[e.Reason] = true
						list = append(list, e.Reason)
					}
				}
				return true, "", strings.Join(list, ",")
			}
			strict, strictClass, _ := read(nil)
			success, class, reasons := read(c.flags)
			t.Logf("sha256=%s bytes=%d strict=%t strict-class=%s selected=%t class=%s flags=%v reasons=%s", hex.EncodeToString(hash[:]), info.Size(), strict, strictClass, success, class, c.flags, reasons)
			if !success {
				t.Fatal("qualified corpus route refused")
			}
			if strict != (len(c.flags) == 0) {
				t.Fatal("strict control outcome changed")
			}
			expected := map[string]string{
				"ExplicitVRforPublicElementsImplicitVRforShadowElements.dcm":                                     "implicit-vr-element",
				"MR_Philips_Intera_PrivateSequenceExplicitVR_in_SQ_2001_e05f_item_wrong_lgt_use_NOSHADOWSEQ.dcm": "private-sequence-kept-as-bytes",
				"SIEMENS_MAGNETOM-12-MONO2-GDCM12-VRUN.dcm":                                                      "short-length-un",
			}
			if reasons != expected[c.name] {
				t.Fatal("actual recovery reasons changed")
			}
			var stderr bytes.Buffer
			if code := Run(append(append([]string{"-search-tag", "SOPClassUID", "-read-max-boundary-recoveries", "4096"}, c.flags...), path), io.Discard, &stderr); code != 0 {
				t.Fatal("qualified CLI inspection failed")
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(after) != hash {
				t.Fatal("corpus inspection changed source")
			}
		})
	}
}
