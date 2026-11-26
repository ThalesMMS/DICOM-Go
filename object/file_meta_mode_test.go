package object

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	tagPrivateInformationCreatorUID = core.NewTag(0x0002, 0x0100)
	tagPrivateInformation           = core.NewTag(0x0002, 0x0102)
)

// foreignMetaFile is a file written by another implementation: its own
// identity, a Source AE Title and Private Information in group 0002.
func foreignMetaFile() *File {
	return &File{
		Meta: FromElements([]core.Element{
			dicomtest.NewUIElement(tagImplementationClassUID, "1.2.3.4.5"),
			dicomtest.NewStringElement(tagImplementationVersionName, core.VRSH, "OTHER_1"),
			dicomtest.NewStringElement(tagSourceAETitle, core.VRAE, "MODALITY"),
			dicomtest.NewUIElement(tagPrivateInformationCreatorUID, "1.2.3.4.6"),
			core.NewRawElement(tagPrivateInformation, core.VROB, []byte{1, 2}),
			// A stale Media Storage SOP Instance UID and transfer syntax.
			dicomtest.NewUIElement(tagMediaStorageSOPInstanceUID, "1.2.3.4.7"),
			dicomtest.NewUIElement(tagTransferSyntaxUID, transfer.ImplicitVRLittleEndian.UID),
		}, std.Dictionary),
		Dataset:        FromDataSet(core.DataSet{Elements: dicomtest.MinimalDataset()}, std.Dictionary),
		TransferSyntax: transfer.ExplicitVRLittleEndian,
	}
}

func metaString(t *testing.T, file *File, tag core.Tag) string {
	t.Helper()
	value, _ := file.Meta.GetString(tag)
	return strings.TrimSpace(value)
}

// TestFileMetaModes checks the three File Meta policies of #1049 and the AE
// titles.
func TestFileMetaModes(t *testing.T) {
	instanceUID, _ := foreignMetaFile().Dataset.GetString(tagSOPInstanceUID)
	for _, tc := range []struct {
		name     string
		opts     WriteFileOptions
		identity [2]string // Implementation Class UID and Version Name
		source   string
		private  bool
		sending  string
	}{
		{"update", WriteFileOptions{}, [2]string{ImplementationClassUID, ImplementationVersionName}, "MODALITY", true, ""},
		{"create", WriteFileOptions{MetaMode: FileMetaCreate}, [2]string{ImplementationClassUID, ImplementationVersionName}, "", false, ""},
		{"preserve", WriteFileOptions{MetaMode: FileMetaPreserve}, [2]string{"1.2.3.4.5", "OTHER_1"}, "MODALITY", true, ""},
		{"create with AE titles", WriteFileOptions{MetaMode: FileMetaCreate, AETitles: FileMetaAETitles{Source: "ARCHIVE", Sending: "CT01 ", Receiving: "ARCHIVE"}},
			[2]string{ImplementationClassUID, ImplementationVersionName}, "ARCHIVE", false, "CT01"},
		{"update replaces Source AE Title", WriteFileOptions{AETitles: FileMetaAETitles{Source: "ARCHIVE"}},
			[2]string{ImplementationClassUID, ImplementationVersionName}, "ARCHIVE", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var encoded bytes.Buffer
			if err := WriteFileWithOptions(&encoded, foreignMetaFile(), tc.opts); err != nil {
				t.Fatal(err)
			}
			back, err := ReadFile(bytes.NewReader(encoded.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if got := [2]string{metaString(t, back, tagImplementationClassUID), metaString(t, back, tagImplementationVersionName)}; got != tc.identity {
				t.Fatalf("identity %q, want %q", got, tc.identity)
			}
			if got := metaString(t, back, tagSourceAETitle); got != tc.source {
				t.Fatalf("Source AE Title %q, want %q", got, tc.source)
			}
			if got := metaString(t, back, tagSendingAETitle); got != tc.sending {
				t.Fatalf("Sending AE Title %q, want %q", got, tc.sending)
			}
			if _, ok := back.Meta.Get(tagPrivateInformation); ok != tc.private {
				t.Fatalf("Private Information kept %v, want %v", ok, tc.private)
			}
			// Every mode keeps the file consistent with its data set and
			// encoding.
			if got := metaString(t, back, tagMediaStorageSOPInstanceUID); got != strings.TrimSpace(instanceUID) {
				t.Fatalf("Media Storage SOP Instance UID %q, want the data set's %q", got, instanceUID)
			}
			if got := metaString(t, back, tagTransferSyntaxUID); got != transfer.ExplicitVRLittleEndian.UID {
				t.Fatalf("Transfer Syntax UID %q", got)
			}
		})
	}
	// RebuildFileMetaWithOptions applies the same policy to File.Meta.
	file := foreignMetaFile()
	if err := file.RebuildFileMetaWithOptions(WriteFileOptions{MetaMode: FileMetaCreate, AETitles: FileMetaAETitles{Receiving: "ARCHIVE"}}); err != nil {
		t.Fatal(err)
	}
	if metaString(t, file, tagReceivingAETitle) != "ARCHIVE" || metaString(t, file, tagSourceAETitle) != "" || metaString(t, file, tagImplementationClassUID) != ImplementationClassUID {
		t.Fatalf("rebuilt meta %v", file.Meta.Elements())
	}
}

func TestFileMetaOptionsValidation(t *testing.T) {
	for _, opts := range []WriteFileOptions{
		{MetaMode: FileMetaMode(7)},
		{MetaMode: FileMetaMode(-1)},
		{AETitles: FileMetaAETitles{Source: "SEVENTEEN_CHARSXX"}},
		{AETitles: FileMetaAETitles{Sending: `A\B`}},
		{AETitles: FileMetaAETitles{Receiving: "   "}},
		{AETitles: FileMetaAETitles{Source: "TAB\tAE"}},
	} {
		if err := WriteFileWithOptions(&bytes.Buffer{}, foreignMetaFile(), opts); !errors.Is(err, ErrInvalidFileMetaOptions) {
			t.Fatalf("options %+v: %v, want ErrInvalidFileMetaOptions", opts, err)
		}
	}
	if err := WriteFileWithOptions(&bytes.Buffer{}, foreignMetaFile(), WriteFileOptions{AETitles: FileMetaAETitles{Source: "SIXTEEN_CHARS_AE"}}); err != nil {
		t.Fatalf("16-character AE title: %v", err)
	}
}
