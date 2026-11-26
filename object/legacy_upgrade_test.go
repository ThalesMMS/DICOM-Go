package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
)

func TestUpgradeLegacyLIBIDODimensions(t *testing.T) {
	for _, order := range []binary.AppendByteOrder{binary.LittleEndian, binary.BigEndian} {
		for _, code := range []string{"ACRNEMA_LIBIDO_1.0", "ACRNEMA_LIBIDO_1.1"} {
			t.Run(order.String()+"/"+code, func(t *testing.T) {
				pixel := []byte{1, 2, 3, 4}
				file, _, err := ReadFileWithTransferSyntaxRecovery(bytes.NewReader(acrNemaImage(order, "", "1", "1", "", pixel)), ReadFileOptions{}, legacyRecovery)
				if err != nil {
					t.Fatal(err)
				}
				file.Dataset.Put(newStringElement(tagRecognitionCode, core.VRLO, code))
				originalPixels, _ := file.Dataset.GetRaw(core.TagPixelData)
				upgraded, report, err := UpgradeLegacy(file)
				if err != nil {
					t.Fatal(err)
				}
				rows, _ := upgraded.Dataset.GetInt(tagLegacyRows)
				columns, _ := upgraded.Dataset.GetInt(tagLegacyColumns)
				if rows != 2 || columns != 1 || !slices.Equal(report.Corrected, []core.Tag{tagLegacyRows, tagLegacyColumns}) || !slices.Equal(report.Diagnostics, []string{LegacyDiagnosticLIBIDODimensions}) {
					t.Fatalf("dimensions %dx%d report %+v", columns, rows, report)
				}
				if originalRows, _ := file.Dataset.GetInt(tagLegacyRows); originalRows != 1 || !file.Dataset.Has(tagRecognitionCode) {
					t.Fatal("upgrade mutated the source")
				}
				if raw, _ := upgraded.Dataset.GetRaw(core.TagPixelData); !bytes.Equal(raw, originalPixels) {
					t.Fatal("upgrade reordered pixels")
				}
				var encoded bytes.Buffer
				if err := WriteFile(&encoded, upgraded); err != nil {
					t.Fatal(err)
				}
				strict, err := ReadFile(bytes.NewReader(encoded.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				again, againReport, err := UpgradeLegacy(strict)
				if err != nil || againReport.Changed() || len(againReport.Diagnostics) != 0 {
					t.Fatalf("repeat upgrade: %v %+v", err, againReport)
				}
				if rows, _ := again.Dataset.GetInt(tagLegacyRows); rows != 2 {
					t.Fatal("repeat upgrade swapped the dimensions again")
				}
			})
		}
	}
}

// acrNemaImage is an ACR-NEMA 2.0 style data set: no preamble, no File Meta,
// no SOP UIDs, with group lengths, a Recognition Code and no Samples per
// Pixel or Photometric Interpretation.
func acrNemaImage(order binary.AppendByteOrder, modality, seriesNumber, instanceNumber string, frames string, pixels []byte) []byte {
	pad := func(s string) []byte {
		if len(s)%2 == 1 {
			s += " "
		}
		return []byte(s)
	}
	var b []byte
	b = append(b, legacyElement(order, 0x0008, 0x0000, order.AppendUint32(nil, 0))...)
	b = append(b, legacyElement(order, 0x0008, 0x0010, pad("ACR-NEMA 2.0"))...)
	if modality != "" {
		b = append(b, legacyElement(order, 0x0008, 0x0060, pad(modality))...)
	}
	b = append(b, legacyElement(order, 0x0010, 0x0020, pad("SYNTH-1"))...)
	b = append(b, legacyElement(order, 0x0020, 0x0011, pad(seriesNumber))...)
	b = append(b, legacyElement(order, 0x0020, 0x0013, pad(instanceNumber))...)
	b = append(b, legacyElement(order, 0x0028, 0x0000, order.AppendUint32(nil, 0))...)
	if frames != "" {
		b = append(b, legacyElement(order, 0x0028, 0x0008, pad(frames))...)
	}
	b = append(b, legacyElement(order, 0x0028, 0x0010, us(order, 1))...)
	b = append(b, legacyElement(order, 0x0028, 0x0011, us(order, 2))...)
	b = append(b, legacyElement(order, 0x0028, 0x0100, us(order, 16))...)
	b = append(b, legacyElement(order, 0x0028, 0x0101, us(order, 12))...)
	b = append(b, legacyElement(order, 0x0028, 0x0102, us(order, 11))...)
	b = append(b, legacyElement(order, 0x0028, 0x0103, us(order, 0))...)
	if pixels != nil {
		b = append(b, legacyElement(order, 0x7fe0, 0x0010, pixels)...)
	}
	return b
}

func upgradeLegacyBytes(t *testing.T, data []byte) ([]byte, LegacyUpgradeReport) {
	t.Helper()
	file, _, err := ReadFileWithTransferSyntaxRecovery(bytes.NewReader(data), ReadFileOptions{}, legacyRecovery)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, report, err := UpgradeLegacy(file)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WriteFile(&out, upgraded); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), report
}

func TestUpgradeLegacyWritesConformingPart10(t *testing.T) {
	for _, order := range []binary.AppendByteOrder{binary.LittleEndian, binary.BigEndian} {
		data := acrNemaImage(order, "MR", "3", "1", "", []byte{1, 2, 3, 4})
		out, report := upgradeLegacyBytes(t, data)
		if report.SOPClassUID != "1.2.840.10008.5.1.4.1.1.4" || report.SOPClassSource != LegacySOPClassModality || report.Modality != "MR" {
			t.Fatalf("report SOP Class = %+v", report)
		}
		if want := []core.Tag{tagSOPInstanceUID, tagStudyInstanceUID, tagSeriesInstanceUID}; !equalTags(report.Generated, want) {
			t.Fatalf("Generated = %v, want %v", report.Generated, want)
		}
		if want := []core.Tag{core.NewTag(0x0008, 0x0000), tagRecognitionCode, core.NewTag(0x0028, 0x0000)}; !equalTags(report.Removed, want) {
			t.Fatalf("Removed = %v, want %v", report.Removed, want)
		}
		if want := []core.Tag{tagLegacySamples, tagLegacyPhotometric}; !equalTags(report.Defaulted, want) {
			t.Fatalf("Defaulted = %v, want %v", report.Defaulted, want)
		}

		strict, err := ReadFile(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("strict read of the upgraded file: %v", err)
		}
		if class, _ := strict.Meta.GetString(tagMediaStorageSOPClassUID); class != report.SOPClassUID {
			t.Fatalf("Media Storage SOP Class = %q", class)
		}
		instance, _ := strict.Dataset.GetUID(tagSOPInstanceUID)
		if !strings.HasPrefix(instance, "2.25.") || !core.IsValidUID(instance) {
			t.Fatalf("SOP Instance UID = %q, want a 2.25 UID", instance)
		}
		for _, tag := range []core.Tag{tagRecognitionCode, core.NewTag(0x0008, 0x0000), core.NewTag(0x0028, 0x0000)} {
			if strict.Dataset.Has(tag) {
				t.Fatalf("upgraded data set still has %s", tag)
			}
		}

		again, _ := upgradeLegacyBytes(t, data)
		if !bytes.Equal(again, out) {
			t.Fatal("upgrading the same legacy file twice gave different files")
		}
		reupgraded, report2, err := UpgradeLegacy(strict)
		if err != nil {
			t.Fatal(err)
		}
		if report2.Changed() {
			t.Fatalf("upgrading an upgraded file changed it: %+v", report2)
		}
		var rewritten bytes.Buffer
		if err := WriteFile(&rewritten, reupgraded); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(rewritten.Bytes(), out) {
			t.Fatal("upgrading an upgraded file is not idempotent")
		}
	}
}

func TestUpgradeLegacyKeepsSeriesTogether(t *testing.T) {
	first, _ := upgradeLegacyBytes(t, acrNemaImage(binary.LittleEndian, "CT", "2", "1", "", []byte{1, 0, 2, 0}))
	second, _ := upgradeLegacyBytes(t, acrNemaImage(binary.LittleEndian, "CT", "2", "2", "", []byte{3, 0, 4, 0}))
	other, _ := upgradeLegacyBytes(t, acrNemaImage(binary.LittleEndian, "CT", "5", "1", "", []byte{1, 0, 2, 0}))
	read := func(data []byte) (study, series, instance string) {
		file, err := ReadFile(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		study, _ = file.Dataset.GetUID(tagStudyInstanceUID)
		series, _ = file.Dataset.GetUID(tagSeriesInstanceUID)
		instance, _ = file.Dataset.GetUID(tagSOPInstanceUID)
		return
	}
	s1, se1, i1 := read(first)
	s2, se2, i2 := read(second)
	s3, se3, _ := read(other)
	if s1 != s2 || se1 != se2 || i1 == i2 {
		t.Fatalf("one legacy series: study %v series %v instances differ %v", s1 == s2, se1 == se2, i1 != i2)
	}
	if s3 != s1 || se3 == se1 {
		t.Fatalf("another series of the study: same study %v, distinct series %v", s3 == s1, se3 != se1)
	}
}

func TestUpgradeLegacyInfersSOPClass(t *testing.T) {
	tests := []struct {
		name, modality, frames string
		pixels                 []byte
		want                   string
		source                 LegacySOPClassSource
		err                    error
	}{
		{name: "no modality", pixels: []byte{1, 2, 3, 4}, want: "SecondaryCaptureImageStorage", source: LegacySOPClassSecondaryCapture},
		{name: "unknown modality", modality: "XC", pixels: []byte{1, 2, 3, 4}, want: "SecondaryCaptureImageStorage", source: LegacySOPClassSecondaryCapture},
		{name: "CT", modality: "CT", pixels: []byte{1, 2, 3, 4}, want: "CTImageStorage", source: LegacySOPClassModality},
		{name: "multi-frame US", modality: "US", frames: "2", pixels: []byte{1, 2, 3, 4, 5, 6, 7, 8}, want: "UltrasoundMultiFrameImageStorage", source: LegacySOPClassModality},
		{name: "no pixels", modality: "CT", err: ErrLegacySOPClass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, _, err := ReadFileWithTransferSyntaxRecovery(bytes.NewReader(acrNemaImage(binary.LittleEndian, tt.modality, "1", "1", tt.frames, tt.pixels)), ReadFileOptions{}, legacyRecovery)
			if err != nil {
				t.Fatal(err)
			}
			_, report, err := UpgradeLegacy(file)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("UpgradeLegacy error = %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			entry, ok := uid.Dictionary.ByUID(report.SOPClassUID)
			if !ok || entry.Keyword != tt.want || report.SOPClassSource != tt.source {
				t.Fatalf("SOP Class = %s (%s, %s), want %s (%s)", report.SOPClassUID, entry.Keyword, report.SOPClassSource, tt.want, tt.source)
			}
		})
	}
}

func TestLegacyModalityClassesAreStorageClasses(t *testing.T) {
	for modality, classes := range legacyModalityClasses {
		for _, class := range []string{classes.single, classes.multiFrame} {
			if class == "" {
				continue
			}
			if entry, ok := uid.Dictionary.ByUID(class); !ok || entry.Type != uid.SOPClass || !strings.Contains(entry.Keyword, "Storage") {
				t.Fatalf("%s maps to %s, not a storage SOP Class", modality, class)
			}
		}
	}
}

func equalTags(a, b []core.Tag) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
