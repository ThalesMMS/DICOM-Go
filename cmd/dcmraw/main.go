// Command dcmraw extracts and inserts raw attribute values and encapsulated
// Pixel Data fragments, in the manner of GDCM gdcmraw:
//
//	dcmraw -t PixelData -o pixels.bin in.dcm            value (or fragments) as stored
//	dcmraw -t 7FE0,0010 -split -o frame in.dcm          frame.0000, frame.0001, ... (+ frame.bot)
//	dcmraw -insert -t "(0029,1010)" -vr OB -i blob.bin -o out.dcm in.dcm
//	dcmraw -insert-fragments -offset-table -o out.dcm in.dcm f0.jpg f1.jpg
//
// Extraction writes the bytes as stored in the file: fragments without
// their Item headers, concatenated unless -split writes one file each.
// Insertion replaces the attribute, or Pixel Data with the given fragments
// (each a frame when -offset-table builds the Basic Offset Table), and
// writes the result through a temporary to -o; the input is never replaced.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/object"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

type options struct {
	tag             string
	output          string
	split           bool
	insert          bool
	insertFragments bool
	offsetTable     bool
	vr              string
	value           string
	input           string
	fragments       []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("dcmraw", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.tag, "t", "", "attribute, as a keyword, (gggg,eeee) or ggggeeee")
	fs.StringVar(&opts.output, "o", "", "output file (with -split, the prefix of the numbered files)")
	fs.BoolVar(&opts.split, "split", false, "write each encapsulated fragment to its own file")
	fs.BoolVar(&opts.insert, "insert", false, "insert or replace -t with the bytes of -i")
	fs.StringVar(&opts.vr, "vr", "", "VR of the inserted attribute (default: its dictionary VR)")
	fs.StringVar(&opts.value, "i", "", "file holding the value to insert")
	fs.BoolVar(&opts.insertFragments, "insert-fragments", false, "replace Pixel Data with the fragment files given after the input")
	fs.BoolVar(&opts.offsetTable, "offset-table", false, "with -insert-fragments, build a Basic Offset Table with one frame per fragment")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: dcmraw -t tag [-split] -o out in.dcm\n       dcmraw -insert -t tag [-vr VR] -i value.bin -o out.dcm in.dcm\n       dcmraw -insert-fragments [-offset-table] -o out.dcm in.dcm fragment...\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	valid := opts.output != "" && fs.NArg() >= 1
	switch {
	case opts.insert && opts.insertFragments:
		valid = false
	case opts.insertFragments:
		valid = valid && fs.NArg() >= 2 && opts.tag == "" && !opts.split
	case opts.insert:
		valid = valid && fs.NArg() == 1 && opts.tag != "" && opts.value != "" && !opts.split
	default:
		valid = valid && fs.NArg() == 1 && opts.tag != "" && opts.value == "" && opts.vr == "" && !opts.offsetTable
	}
	if !valid {
		fs.Usage()
		return opts, errUsage
	}
	opts.input = fs.Arg(0)
	opts.fragments = fs.Args()[1:]
	return opts, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var tag core.Tag
	if opts.tag != "" {
		if tag, err = parseTag(opts.tag); err != nil {
			clidiag.Fprintln(stderr, "dcmraw", err)
			return 2
		}
	}
	var summary string
	switch {
	case opts.insertFragments:
		summary, err = insertFragments(opts)
	case opts.insert:
		summary, err = insertValue(opts, tag)
	default:
		summary, err = extract(opts, tag)
	}
	if err != nil {
		clidiag.Fprintln(stderr, "dcmraw", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "dcmraw: "+summary)
	return 0
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

func readFile(path string) (*object.File, error) {
	source, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	return object.ReadFile(source)
}

func extract(opts options, tag core.Tag) (string, error) {
	file, err := readFile(opts.input)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	element, ok := file.Dataset.Get(tag)
	if !ok {
		return "", fmt.Errorf("attribute %s is absent", tag)
	}
	if fragments, ok := element.Value.(core.FragmentSequence); ok {
		if opts.split {
			if len(fragments.OffsetTable) > 0 {
				if err := os.WriteFile(opts.output+".bot", fragments.OffsetTable, 0o644); err != nil {
					return "", err
				}
			}
			for i, fragment := range fragments.Fragments {
				if err := os.WriteFile(fmt.Sprintf("%s.%04d", opts.output, i), fragment, 0o644); err != nil {
					return "", err
				}
			}
			return fmt.Sprintf("%d fragment(s) written", len(fragments.Fragments)), nil
		}
		return writeOutput(opts.output, func(w io.Writer) (int64, error) {
			var total int64
			for _, fragment := range fragments.Fragments {
				n, err := w.Write(fragment)
				total += int64(n)
				if err != nil {
					return total, err
				}
			}
			return total, nil
		}, fmt.Sprintf("%d fragment(s)", len(fragments.Fragments)))
	}
	if opts.split {
		return "", fmt.Errorf("attribute %s is not encapsulated; -split needs fragments", tag)
	}
	if element.VR() == core.VRSQ {
		return "", fmt.Errorf("attribute %s is a sequence and has no raw value", tag)
	}
	return writeOutput(opts.output, func(w io.Writer) (int64, error) { return file.Dataset.CopyValueTo(tag, w) }, "value")
}

func writeOutput(path string, write func(io.Writer) (int64, error), what string) (string, error) {
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	n, err := write(out)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return fmt.Sprintf("%s written, %d bytes", what, n), nil
}

func insertValue(opts options, tag core.Tag) (string, error) {
	var vr core.VR
	if opts.vr == "" {
		entry, ok := std.Dictionary.ByTag(tag)
		if !ok || entry.VR == "" || entry.VR == core.VRSQ {
			return "", fmt.Errorf("attribute %s has no single dictionary VR; give -vr", tag)
		}
		vr = entry.VR
	} else {
		parsed, err := core.ParseVR(opts.vr)
		if err != nil || parsed == core.VRSQ {
			return "", fmt.Errorf("VR %q cannot hold a raw value", opts.vr)
		}
		vr = parsed
	}
	value, err := os.ReadFile(opts.value)
	if err != nil {
		return "", err
	}
	value = vr.PadToEvenLength(value)
	file, err := readFile(opts.input)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	file.Dataset.Put(core.NewRawElement(tag, vr, value))
	if err := writeFileAtomically(opts.output, opts.input, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s inserted, %d bytes", tag, vr, len(value)), nil
}

func insertFragments(opts options) (string, error) {
	file, err := readFile(opts.input)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	if !file.TransferSyntax.Encapsulated {
		return "", fmt.Errorf("the transfer syntax %s is not encapsulated; transcode first", file.TransferSyntax.Name)
	}
	sequence := core.FragmentSequence{}
	var offset uint64
	for _, path := range opts.fragments {
		fragment, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if len(fragment) == 0 {
			return "", fmt.Errorf("fragment %s is empty", filepath.Base(path))
		}
		if len(fragment)%2 == 1 {
			fragment = append(fragment, 0)
		}
		if opts.offsetTable {
			if offset > 0xFFFFFFFF {
				return "", errors.New("fragments exceed the 4 GiB reach of the Basic Offset Table")
			}
			sequence.OffsetTable = binary.LittleEndian.AppendUint32(sequence.OffsetTable, uint32(offset))
		}
		offset += 8 + uint64(len(fragment))
		sequence.Fragments = append(sequence.Fragments, fragment)
	}
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength}, Value: sequence})
	if err := writeFileAtomically(opts.output, opts.input, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d fragment(s) inserted", len(sequence.Fragments)), nil
}

func writeFileAtomically(target, input string, file *object.File) error {
	if same, _ := sameFile(target, input); same {
		return errors.New("the output is the input; choose another -o")
	}
	if err := file.RebuildFileMeta(); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".dcmraw-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if err := object.WriteFile(temp, file); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), target)
}

func sameFile(a, b string) (bool, error) {
	infoA, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(infoA, infoB), nil
}
