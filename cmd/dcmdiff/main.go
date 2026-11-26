// Command dcmdiff compares the attributes of two DICOM files, in the manner
// of GDCM gdcmdiff:
//
//	dcmdiff a.dcm b.dcm
//	dcmdiff -meta -ignore SOPInstanceUID -ignore "(0008,0012)" a.dcm b.dcm
//
// Attributes are compared by value, not by encoding: text by its values,
// numbers by their decoded values, binary values and Pixel Data fragments by
// length and SHA-256, and sequences item by item. Each difference is one
// line:
//
//	< (0010,0020) PatientID LO "A"          only in the first file
//	> (0008,103E) SeriesDescription LO "B"  only in the second file
//	! (0020,0013) InstanceNumber IS "1" | "2"
//
// The exit status is 0 when the files match, 1 when they differ and 2 on
// errors, as for diff.
//
// -pixels compares decoded pixel values instead of attributes (package
// pixeldata/compare), after DCMTK dcmicmp, to judge lossy exports and
// transcodes by metrics rather than by hash:
//
//	dcmdiff -pixels original.dcm lossy.dcm
//	dcmdiff -pixels -min-psnr 45 -max-error 8 original.dcm lossy.dcm
//
// It prints the maximum and mean absolute error, RMSE, PSNR and SNR of the
// second file against the first, on modality values (or with -stored, stored
// values) for grayscale and 8-bit RGB for color. Without limits the exit
// status is 1 when any sample differs; with -max-error, -max-mae, -max-rmse,
// -min-psnr or -min-snr it is 1 only when a limit is exceeded.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/object"
	pixelcompare "github.com/ThalesMMS/dicom-go/pixeldata/compare"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

type tagList []string

func (l *tagList) String() string { return strings.Join(*l, ",") }

func (l *tagList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

type options struct {
	meta     bool
	quiet    bool
	maxValue int
	ignore   map[core.Tag]bool
	pixels   bool
	stored   bool
	limits   []limit
	first    string
	second   string
}

// limit is one -max-* or -min-* bound of -pixels.
type limit struct {
	name    string
	minimum bool
	bound   float64
	value   func(pixelcompare.Metrics) float64
}

var limitFlags = []limit{
	{name: "max-error", value: func(m pixelcompare.Metrics) float64 { return m.MaxAbsoluteError }},
	{name: "max-mae", value: func(m pixelcompare.Metrics) float64 { return m.MeanAbsoluteError }},
	{name: "max-rmse", value: func(m pixelcompare.Metrics) float64 { return m.RootMeanSquareError }},
	{name: "min-psnr", minimum: true, value: func(m pixelcompare.Metrics) float64 { return m.PeakSignalToNoiseRatio }},
	{name: "min-snr", minimum: true, value: func(m pixelcompare.Metrics) float64 { return m.SignalToNoiseRatio }},
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{ignore: map[core.Tag]bool{}}
	var ignore tagList
	fs := flag.NewFlagSet("dcmdiff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&opts.meta, "meta", false, "also compare the File Meta Information (group 0002)")
	fs.BoolVar(&opts.quiet, "q", false, "print nothing; report only through the exit status")
	fs.IntVar(&opts.maxValue, "max-value", 64, "truncate displayed values to this many characters (0 keeps them whole)")
	fs.Var(&ignore, "ignore", "attribute to skip at any depth, as a keyword or (gggg,eeee) (repeatable)")
	fs.BoolVar(&opts.pixels, "pixels", false, "compare decoded pixel values (errors, RMSE, PSNR, SNR) instead of attributes")
	fs.BoolVar(&opts.stored, "stored", false, "with -pixels: compare stored values, without the Modality LUT or color conversion")
	bounds := make([]float64, len(limitFlags))
	for i, l := range limitFlags {
		what := map[string]string{
			"max-error": "largest absolute error", "max-mae": "mean absolute error", "max-rmse": "root mean square error",
			"min-psnr": "smallest PSNR in dB", "min-snr": "smallest SNR in dB",
		}[l.name]
		fs.Float64Var(&bounds[i], l.name, 0, "with -pixels: "+what+" accepted")
	}
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: dcmdiff [-meta] [-ignore tag] [-max-value n] [-q] first.dcm second.dcm\n       dcmdiff -pixels [-stored] [-max-error e] [-max-mae e] [-max-rmse e] [-min-psnr dB] [-min-snr dB] [-q] reference.dcm test.dcm\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	valid := fs.NArg() == 2 && opts.maxValue >= 0
	fs.Visit(func(f *flag.Flag) {
		for i, l := range limitFlags {
			if f.Name == l.name {
				l.bound = bounds[i]
				opts.limits = append(opts.limits, l)
				valid = valid && opts.pixels && !math.IsNaN(l.bound) && l.bound >= 0
			}
		}
	})
	if opts.pixels {
		valid = valid && !opts.meta && len(ignore) == 0
	} else {
		valid = valid && !opts.stored
	}
	if !valid {
		fs.Usage()
		return opts, errUsage
	}
	for _, name := range ignore {
		tag, err := parseTag(name)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "dcmdiff: %v\n", err)
			return opts, errUsage
		}
		opts.ignore[tag] = true
	}
	opts.first, opts.second = fs.Arg(0), fs.Arg(1)
	return opts, nil
}

func parseTag(name string) (core.Tag, error) {
	if tag, err := core.ParseTag(name); err == nil {
		return tag, nil
	}
	if entry, ok := std.Dictionary.ByKeyword(name); ok {
		return entry.Tag, nil
	}
	return core.Tag{}, fmt.Errorf("unknown attribute %q", name)
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if opts.pixels {
		return comparePixels(opts, stdout, stderr)
	}
	first, err := flattenFile(opts.first, opts)
	if err != nil {
		clidiag.Fprintln(stderr, "dcmdiff", fmt.Errorf("%s: %w", opts.first, err))
		return 2
	}
	second, err := flattenFile(opts.second, opts)
	if err != nil {
		clidiag.Fprintln(stderr, "dcmdiff", fmt.Errorf("%s: %w", opts.second, err))
		return 2
	}
	differences := compare(first, second, opts.maxValue)
	if !opts.quiet {
		for _, line := range differences {
			_, _ = fmt.Fprintln(stdout, line)
		}
	}
	if len(differences) > 0 {
		return 1
	}
	return 0
}

// attribute is one flattened attribute: its display path, its sort key and
// its comparable value.
type attribute struct {
	path, key, name, vr, value string
}

func flattenFile(path string, opts options) (map[string]attribute, error) {
	source, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	file, err := object.ReadFile(source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	out := map[string]attribute{}
	if opts.meta && file.Meta != nil {
		if err := flatten(file.Meta, "", "", opts.ignore, out); err != nil {
			return nil, err
		}
	}
	return out, flatten(file.Dataset, "", "", opts.ignore, out)
}

// flatten adds every attribute of dataset below prefix; sequences add an
// item count and then their items' attributes.
func flatten(dataset *object.Object, prefix, keyPrefix string, ignore map[core.Tag]bool, out map[string]attribute) error {
	for _, element := range dataset.Elements() {
		tag := element.Tag()
		if ignore[tag] {
			continue
		}
		label := fmt.Sprintf("(%04X,%04X)", tag.Group, tag.Element)
		attr := attribute{path: prefix + label, key: keyPrefix + label, name: keyword(tag), vr: element.VR().String()}
		if element.VR() == core.VRSQ {
			items, _ := dataset.GetSequence(tag)
			attr.value = fmt.Sprintf("%d item(s)", len(items))
			out[attr.key] = attr
			for i, item := range items {
				if err := flatten(item, fmt.Sprintf("%s[%d].", attr.path, i), fmt.Sprintf("%s[%08d].", attr.key, i), ignore, out); err != nil {
					return err
				}
			}
			continue
		}
		value, err := render(dataset, element)
		if err != nil {
			return fmt.Errorf("%s: %w", attr.path, err)
		}
		attr.value = value
		out[attr.key] = attr
	}
	return nil
}

func keyword(tag core.Tag) string {
	var dict dictionary.DataDictionary = std.Dictionary
	if entry, ok := dict.ByTag(tag); ok && entry.Keyword != "" {
		return entry.Keyword
	}
	if tag.Group%2 == 1 {
		return "Private"
	}
	return "Unknown"
}

// render is the comparable form of a value.
func render(dataset *object.Object, element core.Element) (string, error) {
	if fragments, ok := element.Value.(core.FragmentSequence); ok {
		parts := []string{"offset table " + digest(fragments.OffsetTable)}
		for i, fragment := range fragments.Fragments {
			parts = append(parts, fmt.Sprintf("fragment %d %s", i, digest(fragment)))
		}
		return strings.Join(parts, "; "), nil
	}
	tag := element.Tag()
	switch vr := element.VR(); vr {
	case core.VRUS, core.VRSS, core.VRUL, core.VRSL, core.VRUV, core.VRSV:
		values, err := dataset.GetInts(tag)
		if err != nil {
			return "", err
		}
		return joinNumbers(values, func(v int64) string { return strconv.FormatInt(v, 10) }), nil
	case core.VRFL, core.VRFD:
		values, err := dataset.GetFloats(tag)
		if err != nil {
			return "", err
		}
		return joinNumbers(values, func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }), nil
	case core.VROB, core.VROW, core.VROF, core.VROD, core.VROL, core.VROV, core.VRUN, core.VRAT:
		hash := sha256.New()
		var sink io.Writer = hash
		// Hash multi-byte words little endian, so a Big Endian file's value
		// matches the same value in a Little Endian one.
		if width := wordWidth[vr]; width > 1 && dataset.ValueByteOrder() == binary.BigEndian {
			sink = &swapWriter{w: hash, width: width}
		}
		n, err := dataset.CopyValueTo(tag, sink)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d bytes sha256 %s", n, hex.EncodeToString(hash.Sum(nil))[:16]), nil
	default:
		// Trailing SPACE or NUL padding is encoding, not value.
		values, _ := dataset.GetStrings(tag)
		for i := range values {
			values[i] = strings.TrimRight(values[i], " \x00")
		}
		return strconv.Quote(strings.Join(values, `\`)), nil
	}
}

var wordWidth = map[core.VR]int{core.VROW: 2, core.VRAT: 2, core.VROF: 4, core.VROL: 4, core.VROD: 8, core.VROV: 8}

// swapWriter reverses the bytes of each width-byte word; a trailing partial
// word is written as is.
type swapWriter struct {
	w       io.Writer
	width   int
	pending []byte
}

func (s *swapWriter) Write(p []byte) (int, error) {
	data := append(s.pending, p...)
	whole := len(data) / s.width * s.width
	swapped := make([]byte, whole)
	for i := 0; i < whole; i += s.width {
		for j := 0; j < s.width; j++ {
			swapped[i+j] = data[i+s.width-1-j]
		}
	}
	s.pending = append(s.pending[:0:0], data[whole:]...)
	if _, err := s.w.Write(swapped); err != nil {
		return 0, err
	}
	return len(p), nil
}

func joinNumbers[T any](values []T, format func(T) string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = format(v)
	}
	return strings.Join(parts, `\`)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%d bytes sha256 %s", len(data), hex.EncodeToString(sum[:])[:16])
}

// compare lists the differences in attribute order.
func compare(first, second map[string]attribute, maxValue int) []string {
	keys := make([]string, 0, len(first)+len(second))
	for key := range first {
		keys = append(keys, key)
	}
	for key := range second {
		if _, ok := first[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var lines []string
	for _, key := range keys {
		a, inFirst := first[key]
		b, inSecond := second[key]
		switch {
		case !inSecond:
			lines = append(lines, fmt.Sprintf("< %s %s %s %s", a.path, a.name, a.vr, truncate(a.value, maxValue)))
		case !inFirst:
			lines = append(lines, fmt.Sprintf("> %s %s %s %s", b.path, b.name, b.vr, truncate(b.value, maxValue)))
		case a.vr != b.vr && !sameBytesVR(a.vr, b.vr):
			lines = append(lines, fmt.Sprintf("! %s %s VR %s | %s", a.path, a.name, a.vr, b.vr))
		case a.value != b.value:
			lines = append(lines, fmt.Sprintf("! %s %s %s %s | %s", a.path, a.name, a.vr, truncate(a.value, maxValue), truncate(b.value, maxValue)))
		}
	}
	return lines
}

// sameBytesVR: OB and OW hold the same bytes; Implicit VR files read
// native Pixel Data as OW whatever the writer's Explicit VR choice was.
func sameBytesVR(a, b string) bool {
	return (a == "OB" || a == "OW") && (b == "OB" || b == "OW")
}

func truncate(value string, max int) string {
	if max == 0 || len([]rune(value)) <= max {
		return value
	}
	return string([]rune(value)[:max]) + "..."
}

// comparePixels reports the pixel metrics of the second file against the
// first and checks the limits.
func comparePixels(opts options, stdout, stderr io.Writer) int {
	files := make([]*object.File, 2)
	for i, path := range []string{opts.first, opts.second} {
		file, err := clifile.ReadPart10(path)
		if err != nil {
			clidiag.Fprintln(stderr, "dcmdiff", fmt.Errorf("%s: %w", path, err))
			return 2
		}
		defer func() { _ = file.Close() }()
		files[i] = file
	}
	domain := pixelcompare.DomainModality
	if opts.stored {
		domain = pixelcompare.DomainStored
	}
	metrics, err := pixelcompare.Files(context.Background(), files[0], files[1], pixelcompare.Options{Domain: domain})
	if errors.Is(err, pixelcompare.ErrMismatch) {
		if !opts.quiet {
			_, _ = fmt.Fprintf(stdout, "! pixels: %v\n", err)
		}
		return 1
	}
	if err != nil {
		clidiag.Fprintln(stderr, "dcmdiff", err)
		return 2
	}
	if !opts.quiet {
		number := func(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }
		_, _ = fmt.Fprintf(stdout, "frames %d\nsamples %d\nmax-error %s\nmae %s\nrmse %s\npsnr-db %s\nsnr-db %s\n", metrics.Frames, metrics.Samples,
			number(metrics.MaxAbsoluteError), number(metrics.MeanAbsoluteError), number(metrics.RootMeanSquareError),
			number(metrics.PeakSignalToNoiseRatio), number(metrics.SignalToNoiseRatio))
	}
	if len(opts.limits) == 0 {
		if metrics.Identical() {
			return 0
		}
		return 1
	}
	status := 0
	for _, l := range opts.limits {
		value := l.value(metrics)
		if (l.minimum && value < l.bound) || (!l.minimum && value > l.bound) {
			status = 1
			if !opts.quiet {
				_, _ = fmt.Fprintf(stdout, "! -%s %g exceeded: %s\n", l.name, l.bound, strconv.FormatFloat(value, 'g', 6, 64))
			}
		}
	}
	return status
}
