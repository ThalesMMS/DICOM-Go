package dicomconv

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ThalesMMS/dicom-go/dicomxml"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/object"
)

// Tool names one of the four converters.
type Tool string

const (
	DCM2JSON Tool = "dcm2json"
	JSON2DCM Tool = "json2dcm"
	DCM2XML  Tool = "dcm2xml"
	XML2DCM  Tool = "xml2dcm"
)

func (t Tool) format() Format {
	if t == DCM2XML || t == XML2DCM {
		return XML
	}
	return JSON
}

func (t Tool) toText() bool { return t == DCM2JSON || t == DCM2XML }

// Run executes a converter with command-line arguments and returns the exit
// status: 0 on success, 1 on a conversion error, 2 on a usage error.
func Run(tool Tool, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(string(tool), flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		compact, withMeta bool
		keepCharset       bool
		recoverPrivate    bool
		recoverText       bool
		bulkDir           string
		bulkFile          string
		bulkThreshold     int
		syntax, charset   string
		unrepresentable   string
	)
	if tool.toText() {
		fs.BoolVar(&compact, "compact", false, "write without indentation")
		fs.BoolVar(&withMeta, "meta", false, "include File Meta Information (group 0002) so the transfer syntax round-trips")
		fs.BoolVar(&keepCharset, "keep-charset", false, "keep the source Specific Character Set instead of declaring ISO_IR 192 (the text is UTF-8 either way)")
		fs.StringVar(&bulkDir, "bulk-dir", "", "write binary values of at least -bulk-threshold bytes to this directory (inside the output directory) as BulkDataURI")
		fs.StringVar(&bulkFile, "bulk-file", "", "append binary values of at least -bulk-threshold bytes to this file and reference them with absolute file: BulkDataURIs carrying offset and length (excludes -bulk-dir)")
		fs.IntVar(&bulkThreshold, "bulk-threshold", 1024, "minimum size in bytes of a value written to -bulk-dir or -bulk-file")
		if tool == DCM2XML {
			fs.BoolVar(&recoverPrivate, "recover-private", false, "trim NUL padding from private creators and omit or merge private elements the XML model cannot hold (missing or repeated creator), reporting each on stderr")
			fs.BoolVar(&recoverText, "recover-text", false, "trim trailing NUL padding, which XML 1.0 cannot hold, from text values, reporting each on stderr")
		}
	} else {
		fs.StringVar(&syntax, "transfer-syntax", "", "transfer syntax UID when the document has no File Meta Information (default Explicit VR Little Endian)")
		fs.StringVar(&charset, "charset", "", `re-encode text into this Specific Character Set, e.g. "ISO_IR 192" or "ISO_IR 100"`)
		fs.StringVar(&unrepresentable, "unrepresentable", "fail", "with -charset: fail, transliterate or discard characters the target cannot hold")
	}
	fs.Usage = func() {
		in, out := "in.dcm", "out.json"
		switch tool {
		case JSON2DCM:
			in, out = "in.json", "out.dcm"
		case DCM2XML:
			out = "out.xml"
		case XML2DCM:
			in, out = "in.xml", "out.dcm"
		}
		_, _ = fmt.Fprintf(stderr, "Usage: %s [flags] %s [%s]\n\nFlags:\n", tool, in, out)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 || !tool.toText() && fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	input := fs.Arg(0)
	output := fs.Arg(1)
	ctx := context.Background()
	var err error
	if tool.toText() {
		opts := EncodeOptions{Compact: compact, WithMeta: withMeta, BulkDir: bulkDir, BulkFile: bulkFile, BulkThreshold: bulkThreshold, KeepCharacterSet: keepCharset}
		if recoverPrivate {
			opts.PrivateRecovered = func(r dicomxml.PrivateRecovery) {
				_, _ = fmt.Fprintln(stderr, strings.TrimSpace(fmt.Sprintf("%s: recovery: %s: %s %s", tool, r.Kind, r.Path, r.Creator)))
			}
		}
		if recoverText {
			opts.TextRecovered = func(r dicomxml.TextRecovery) {
				_, _ = fmt.Fprintf(stderr, "%s: recovery: %s: %s\n", tool, r.Kind, r.Path)
			}
		}
		err = encodeFile(ctx, tool, input, output, stdout, opts)
	} else {
		var policy object.UnrepresentablePolicy
		switch unrepresentable {
		case "fail":
			policy = object.UnrepresentableFail
		case "transliterate":
			policy = object.UnrepresentableTransliterate
		case "discard":
			policy = object.UnrepresentableDiscard
		default:
			fs.Usage()
			return 2
		}
		err = decodeFile(ctx, tool, input, output, DecodeOptions{TransferSyntaxUID: syntax, CharacterSet: charset, Unrepresentable: policy})
	}
	if err != nil {
		clidiag.Fprintln(stderr, string(tool), err)
		return 1
	}
	return 0
}

func encodeFile(ctx context.Context, tool Tool, input, output string, stdout io.Writer, opts EncodeOptions) error {
	file, err := ReadPart10(input)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if opts.BulkDir != "" {
		if output == "" {
			return fmt.Errorf("%w: -bulk-dir needs an output file", ErrBulkDataURI)
		}
		opts.BaseDir = filepath.Dir(output)
	}
	data, err := Encode(ctx, file, tool.format(), opts)
	if err != nil {
		return err
	}
	if output == "" {
		_, err = stdout.Write(data)
		return err
	}
	return os.WriteFile(output, data, 0o644)
}

func decodeFile(ctx context.Context, tool Tool, input, output string, opts DecodeOptions) error {
	data, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	opts.BaseDir = filepath.Dir(input)
	file, err := Decode(ctx, data, tool.format(), opts)
	if err != nil {
		return err
	}
	return WritePart10(output, file)
}
