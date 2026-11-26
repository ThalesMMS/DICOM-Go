// Command dcminfo summarizes DICOM files without their patient data, in the
// manner of GDCM gdcminfo:
//
//	dcminfo image.dcm
//	dcminfo -md5 -json a.dcm b.dcm
//
// For each file it prints the SOP Class and transfer syntax; for an image
// its dimensions, frames and pixel format, how Pixel Data is stored and,
// when encapsulated, what the first codestream declares (JPEG process,
// JPEG-LS NEAR, JPEG 2000 wavelet, RLE segments); whether the pixels have
// been through lossy compression and on what evidence
// (pixeldata.DetectLossyState); and for an Encapsulated Document its MIME
// type, size, PDF version and MD5 (package encapdoc). -md5 adds the MD5 of
// the decoded pixel samples, as they would be stored natively in little
// endian, so that a lossless transcode keeps it.
//
// No patient, study or series attribute and no instance UID is printed.
// The exit status is 0 when every file was summarized, 1 otherwise, and 2 on
// usage errors.
package main

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

var tagEncapsulatedDocument = core.NewTag(0x0042, 0x0011)

type options struct {
	md5   bool
	json  bool
	files []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("dcminfo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&opts.md5, "md5", false, "decode Pixel Data and print the MD5 of its native little-endian samples")
	fs.BoolVar(&opts.json, "json", false, "write one JSON summary per file instead of text")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: dcminfo [-md5] [-json] file...\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return opts, errUsage
	}
	opts.files = fs.Args()
	return opts, nil
}

type imageInfo struct {
	Rows                int    `json:"rows"`
	Columns             int    `json:"columns"`
	Frames              int    `json:"frames"`
	SamplesPerPixel     int    `json:"samples_per_pixel"`
	BitsAllocated       int    `json:"bits_allocated"`
	BitsStored          int    `json:"bits_stored"`
	Signed              bool   `json:"signed"`
	Photometric         string `json:"photometric_interpretation"`
	PlanarConfiguration int    `json:"planar_configuration,omitempty"`
}

type pixelInfo struct {
	Encapsulated   bool        `json:"encapsulated"`
	Bytes          int64       `json:"bytes,omitempty"`
	Fragments      int         `json:"fragments,omitempty"`
	OffsetTable    bool        `json:"basic_offset_table,omitempty"`
	Codestream     *codestream `json:"codestream,omitempty"`
	MD5            string      `json:"md5,omitempty"`
	Lossy          bool        `json:"lossy"`
	LossyEvidence  string      `json:"lossy_evidence,omitempty"`
	LossyMethod    string      `json:"lossy_method,omitempty"`
	FramesDecoded  int         `json:"frames_decoded,omitempty"`
	DecodeFailures string      `json:"decode_failure,omitempty"`
}

type documentInfo struct {
	MIMEType   string `json:"mime_type"`
	Bytes      int    `json:"bytes"`
	PDFVersion string `json:"pdf_version,omitempty"`
	MD5        string `json:"md5"`
}

type summary struct {
	File               string        `json:"file"`
	SOPClassUID        string        `json:"sop_class_uid"`
	SOPClass           string        `json:"sop_class,omitempty"`
	TransferSyntaxUID  string        `json:"transfer_syntax_uid"`
	TransferSyntax     string        `json:"transfer_syntax,omitempty"`
	Image              *imageInfo    `json:"image,omitempty"`
	PixelData          *pixelInfo    `json:"pixel_data,omitempty"`
	Document           *documentInfo `json:"document,omitempty"`
	DocumentFailure    string        `json:"document_failure,omitempty"`
	ImageMetadataError string        `json:"image_metadata_error,omitempty"`
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	status := 0
	for _, path := range opts.files {
		info, err := summarize(context.Background(), path, opts)
		if err != nil {
			clidiag.Fprintln(stderr, "dcminfo", fmt.Errorf("%s: %w", path, err))
			status = 1
			continue
		}
		if opts.json {
			encoded, _ := json.Marshal(info)
			_, _ = fmt.Fprintln(stdout, string(encoded))
		} else {
			printText(stdout, info)
		}
	}
	return status
}

func summarize(ctx context.Context, path string, opts options) (summary, error) {
	file, err := clifile.ReadPart10(path)
	if err != nil {
		return summary{}, err
	}
	defer func() { _ = file.Close() }()
	out := summary{File: path, TransferSyntaxUID: file.TransferSyntax.UID, TransferSyntax: file.TransferSyntax.Name}
	out.SOPClassUID, _ = file.Dataset.GetString(core.NewTag(0x0008, 0x0016))
	out.SOPClassUID = strings.TrimRight(out.SOPClassUID, " \x00")
	if entry, ok := uid.Dictionary.ByUID(out.SOPClassUID); ok {
		out.SOPClass = entry.Name
	}
	if file.Dataset.Has(core.TagPixelData) {
		out.Image, out.PixelData = describePixels(ctx, file, opts.md5)
		if out.Image == nil {
			if _, err := pixeldata.ExtractMetadata(file.Dataset); err != nil {
				out.ImageMetadataError = string(clidiag.Classify(err))
			}
		}
	}
	if file.Dataset.Has(tagEncapsulatedDocument) {
		document, err := encapdoc.Extract(file.Dataset)
		if err != nil {
			out.DocumentFailure = err.Error()
		} else {
			sum := md5.Sum(document.Data)
			out.Document = &documentInfo{MIMEType: document.MIMEType, Bytes: len(document.Data), MD5: hex.EncodeToString(sum[:])}
			if match := pdfVersion.FindSubmatch(document.Data); match != nil {
				out.Document.PDFVersion = string(match[1])
			}
		}
	}
	return out, nil
}

var pdfVersion = regexp.MustCompile(`\A%PDF-([0-9]\.[0-9])`)

func describePixels(ctx context.Context, file *object.File, withMD5 bool) (*imageInfo, *pixelInfo) {
	pixels := &pixelInfo{}
	state := pixeldata.DetectLossyState(file)
	pixels.Lossy, pixels.LossyEvidence, pixels.LossyMethod = state.Lossy, string(state.Evidence), state.Method
	element, _ := file.Dataset.Get(core.TagPixelData)
	if fragments, ok := element.Value.(core.FragmentSequence); ok {
		pixels.Encapsulated = true
		pixels.Fragments = len(fragments.Fragments)
		pixels.OffsetTable = len(fragments.OffsetTable) > 0
		if len(fragments.Fragments) > 0 {
			stream := inspectCodestream(fragments.Fragments[0], file.TransferSyntax.UID == transfer.RLELossless.UID)
			pixels.Codestream = &stream
		}
	} else if length, ok := element.Length().Value(); ok {
		pixels.Bytes = int64(length)
	}
	metadata, err := pixeldata.ExtractMetadata(file.Dataset)
	if err != nil {
		return nil, pixels
	}
	image := &imageInfo{
		Rows: int(metadata.Rows), Columns: int(metadata.Columns), Frames: max(metadata.NumberOfFrames, 1),
		SamplesPerPixel: int(metadata.SamplesPerPixel), BitsAllocated: int(metadata.BitsAllocated), BitsStored: int(metadata.BitsStored),
		Signed: metadata.PixelRepresentation == 1, Photometric: metadata.PhotometricInterpretation,
	}
	if metadata.SamplesPerPixel > 1 {
		image.PlanarConfiguration = int(metadata.PlanarConfiguration)
	}
	if withMD5 {
		sum, frames, err := pixelMD5(ctx, file)
		if err != nil {
			pixels.DecodeFailures = string(clidiag.Classify(err))
		} else {
			pixels.MD5, pixels.FramesDecoded = sum, frames
		}
	}
	return image, pixels
}

// pixelMD5 hashes the decoded samples of every frame in little-endian
// order.
func pixelMD5(ctx context.Context, file *object.File) (string, int, error) {
	native, err := clifile.Native(ctx, file)
	if err != nil {
		return "", 0, err
	}
	frames, err := pixeldata.ExtractNativeFramesView(native.Dataset)
	if err != nil {
		return "", 0, err
	}
	hash := md5.New()
	width := int(frames.Metadata.BitsAllocated) / 8
	swap := native.TransferSyntax.ByteOrder == binary.BigEndian && width > 1
	for _, frame := range frames.Data {
		if !swap {
			_, _ = hash.Write(frame)
			continue
		}
		swapped := make([]byte, len(frame))
		for i := 0; i+width <= len(frame); i += width {
			for j := 0; j < width; j++ {
				swapped[i+j] = frame[i+width-1-j]
			}
		}
		_, _ = hash.Write(swapped)
	}
	return hex.EncodeToString(hash.Sum(nil)), len(frames.Data), nil
}

func printText(w io.Writer, s summary) {
	line := func(key, format string, args ...any) {
		_, _ = fmt.Fprintf(w, "%-16s%s\n", key+":", fmt.Sprintf(format, args...))
	}
	named := func(uid, name string) string {
		if name == "" {
			return uid
		}
		return uid + " (" + name + ")"
	}
	line("file", "%s", s.File)
	line("sop-class", "%s", named(s.SOPClassUID, s.SOPClass))
	line("transfer-syntax", "%s", named(s.TransferSyntaxUID, s.TransferSyntax))
	if s.Image != nil {
		signed := "unsigned"
		if s.Image.Signed {
			signed = "signed"
		}
		line("image", "%dx%d, %d frame(s), %d sample(s) %s, %d/%d bits %s", s.Image.Columns, s.Image.Rows, s.Image.Frames,
			s.Image.SamplesPerPixel, s.Image.Photometric, s.Image.BitsStored, s.Image.BitsAllocated, signed)
	} else if s.ImageMetadataError != "" {
		line("image", "unreadable image attributes (%s)", s.ImageMetadataError)
	}
	if p := s.PixelData; p != nil {
		if p.Encapsulated {
			bot := "without"
			if p.OffsetTable {
				bot = "with"
			}
			line("pixel-data", "encapsulated, %d fragment(s), %s Basic Offset Table", p.Fragments, bot)
			if p.Codestream != nil {
				line("codestream", "%s", p.Codestream)
			}
		} else {
			line("pixel-data", "native, %d bytes", p.Bytes)
		}
		lossy := "no"
		if p.Lossy {
			lossy = "yes (" + p.LossyEvidence
			if p.LossyMethod != "" {
				lossy += ", " + p.LossyMethod
			}
			lossy += ")"
		}
		line("lossy", "%s", lossy)
		switch {
		case p.MD5 != "":
			line("pixel-md5", "%s (%d frame(s) decoded)", p.MD5, p.FramesDecoded)
		case p.DecodeFailures != "":
			line("pixel-md5", "not computed: %s error", p.DecodeFailures)
		}
	}
	if d := s.Document; d != nil {
		version := ""
		if d.PDFVersion != "" {
			version = ", PDF " + d.PDFVersion
		}
		line("document", "%s, %d bytes%s, md5 %s", d.MIMEType, d.Bytes, version, d.MD5)
	} else if s.DocumentFailure != "" {
		line("document", "unreadable: %s", s.DocumentFailure)
	}
	_, _ = fmt.Fprintln(w)
}
