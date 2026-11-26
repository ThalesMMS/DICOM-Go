// Command dcmgendir writes the DICOMDIR of a PS3.10 File-set, in the manner
// of GDCM gdcmgendir and DCMTK dcmgpdir:
//
//	dcmgendir -fileset-id MEDIA_01 media/
//	dcmgendir -profile STD-GEN-USB-JPEG -icons media/
//	dcmgendir -skip-invalid -o preview.dir media/
//
// The files must already sit at File ID paths under the root (1 to 8
// components of 1 to 8 characters from A-Z, 0-9 and _); nothing is copied
// or renamed. Without -o the DICOMDIR is published at the root atomically;
// an existing valid DICOMDIR is only replaced by a complete new one.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/dicomdir"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

var profiles = map[string]dicomdir.Profile{}

func init() {
	for _, profile := range []dicomdir.Profile{
		dicomdir.ProfileGeneralPurpose, dicomdir.ProfileGeneralDVDJPEG, dicomdir.ProfileGeneralDVDJ2K,
		dicomdir.ProfileGeneralUSBJPEG, dicomdir.ProfileGeneralUSBJ2K, dicomdir.ProfileXA1KCD, dicomdir.ProfileXA1KDVD,
	} {
		profiles[string(profile)] = profile
	}
}

type options struct {
	fileSetID   string
	fileSetUID  string
	profile     string
	icons       bool
	iconSize    int
	skipInvalid bool
	invent      bool
	output      string
	root        string
	append      bool
	update      bool
	legacyMRDR  bool
	files       fileSelections
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	fs := flag.NewFlagSet("dcmgendir", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.fileSetID, "fileset-id", "", "File-set ID (up to 16 characters from A-Z, 0-9 and _)")
	fs.StringVar(&opts.fileSetUID, "fileset-uid", "", "File-set UID (a new one is generated when empty)")
	fs.StringVar(&opts.profile, "profile", "", "application profile to enforce: "+strings.Join(names, ", "))
	fs.BoolVar(&opts.icons, "icons", false, "add Icon Image Sequences to image records")
	fs.IntVar(&opts.iconSize, "icon-size", 64, "icon size in pixels (1..128) with -icons")
	fs.BoolVar(&opts.skipInvalid, "skip-invalid", false, "skip files that cannot be referenced (reported) instead of failing")
	fs.BoolVar(&opts.invent, "invent", false, "invent Type 1 directory keys a file lacks (Patient ID, Study ID and Date/Time, Series and Instance Number), as dcmgpdir +I")
	fs.BoolVar(&opts.append, "append", false, "append to an existing DICOMDIR, retaining existing keys")
	fs.BoolVar(&opts.update, "update", false, "refresh selected existing sources and append new files")
	fs.BoolVar(&opts.legacyMRDR, "legacy-mrdr", false, "explicitly preserve a retired MRDR compatibility graph")
	fs.Var(&opts.files, "file", "select a File ID (repeatable); default is bounded root scan")
	fs.StringVar(&opts.output, "o", "", "write the DICOMDIR to this file instead of publishing it at the root")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: dcmgendir [-fileset-id ID] [-profile P] [-icons] [-invent] [-skip-invalid] [-o file] root\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return opts, errUsage
	}
	if _, ok := profiles[strings.ToUpper(opts.profile)]; opts.profile != "" && !ok {
		_, _ = fmt.Fprintf(stderr, "dcmgendir: unknown profile %q\n", opts.profile)
		return opts, errUsage
	}
	if opts.append && opts.update || (opts.append || opts.update) && (opts.fileSetID != "" || opts.fileSetUID != "") || opts.update && opts.skipInvalid {
		return opts, errUsage
	}
	opts.root = fs.Arg(0)
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
	ctx := context.Background()
	var set *dicomdir.FileSet
	var scan dicomdir.ScanReport
	sourceOptions := dicomdir.Options{FileSetID: opts.fileSetID, FileSetUID: opts.fileSetUID, InventMissingKeys: opts.invent}
	selection := []dicomdir.FileID(opts.files)
	if opts.append || opts.update {
		if len(selection) == 0 {
			// Discovery is bounded by the library scanner. Common invented directory
			// values are reconciled against the imported graph during preparation.
			discover, openErr := dicomdir.NewFileSet(opts.root, dicomdir.Options{InventMissingKeys: true})
			if openErr != nil {
				clidiag.Fprintln(stderr, "dcmgendir", openErr)
				return 2
			}
			scan, err = discover.Scan(ctx, dicomdir.ScanOptions{Policy: dicomdir.EntrySkip})
			if err != nil {
				clidiag.Fprintln(stderr, "dcmgendir", err)
				return 1
			}
			for _, r := range discover.Records() {
				selection = append(selection, r.FileID)
			}
		}
		reportDiagnostics(stderr, scan.Diagnostics)
		if scan.Skipped > 0 && !opts.skipInvalid {
			clidiag.Fprintln(stderr, "dcmgendir", errors.New("scan refused entries; nothing written"))
			return 1
		}
		mode := dicomdir.FileSetAppend
		if opts.update {
			mode = dicomdir.FileSetRefresh
		}
		var preparation dicomdir.FileSetUpdateReport
		set, preparation, err = dicomdir.PrepareFileSetUpdate(ctx, filepath.Join(opts.root, "DICOMDIR"), selection, dicomdir.FileSetUpdateOptions{Mode: mode, SourceOptions: sourceOptions, LegacyMRDR: opts.legacyMRDR})
		if err != nil {
			clidiag.Fprintln(stderr, "dcmgendir", fmt.Errorf("prepare update: %w", err))
			return 1
		}
		scan.Added = len(set.Records())
		_, _ = fmt.Fprintf(stdout, "dcmgendir: prepared %d new, %d unchanged, %d refreshed physical file(s), %d logical reference(s), %d legacy MRDR(s)\n", preparation.NewFiles, preparation.UnchangedFiles, preparation.RefreshedFiles, preparation.LogicalReferences, preparation.MRDRRecords)
	} else {
		set, err = dicomdir.NewFileSet(opts.root, sourceOptions)
		if err != nil {
			clidiag.Fprintln(stderr, "dcmgendir", fmt.Errorf("open file-set: %w", err))
			return 2
		}
		if len(selection) > 0 {
			for _, id := range selection {
				if err := set.Add(ctx, id); err != nil {
					clidiag.Fprintln(stderr, "dcmgendir", err)
					return 1
				}
				scan.Added++
			}
		} else {
			scan, err = set.Scan(ctx, dicomdir.ScanOptions{Policy: dicomdir.EntrySkip})
		}
		if err != nil {
			clidiag.Fprintln(stderr, "dcmgendir", fmt.Errorf("scan: %w", err))
			return 1
		}
		reportDiagnostics(stderr, scan.Diagnostics)
		if scan.Skipped > 0 && !opts.skipInvalid {
			hint := ""
			if hasCode(scan.Diagnostics, dicomdir.DiagnosticMissingKey) && !opts.invent {
				hint = " (-invent fills missing keys)"
			}
			clidiag.Fprintln(stderr, "dcmgendir", fmt.Errorf("%d file(s) cannot be referenced; nothing written; -skip-invalid leaves them out%s", scan.Skipped, hint))
			return 1
		}
	}
	if scan.Added == 0 {
		clidiag.Fprintln(stderr, "dcmgendir", errors.New("no file can be referenced; nothing written"))
		return 1
	}
	invented := 0
	for _, record := range set.Records() {
		if len(record.InventedKeys) > 0 {
			invented++
		}
	}
	write := dicomdir.WriteOptions{LegacyMRDR: opts.legacyMRDR, Profile: profiles[strings.ToUpper(opts.profile)], Icons: opts.icons}
	if opts.icons {
		write.IconSize = opts.iconSize
	}
	var report dicomdir.WriteReport
	if opts.output == "" {
		report, err = dicomdir.CommitDICOMDIR(ctx, set, write)
	} else {
		report, err = writeTo(ctx, opts.output, set, write)
	}
	if err != nil {
		clidiag.Fprintln(stderr, "dcmgendir", fmt.Errorf("write DICOMDIR: %w", err))
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "dcmgendir: %d file(s), %d skipped, %d with invented keys, %d record(s), %d icon(s), %d bytes, sha256 %s\n",
		scan.Added, scan.Skipped, invented, report.Records, report.Icons, report.BytesWritten, hex.EncodeToString(report.SHA256[:]))
	return 0
}

// maxListedDiagnostics bounds the entries listed one by one; the rest are
// counted by code.
const maxListedDiagnostics = 10

func reportDiagnostics(w io.Writer, diagnostics []dicomdir.Diagnostic) {
	counts := map[dicomdir.DiagnosticCode]int{}
	for i, diagnostic := range diagnostics {
		counts[diagnostic.Code]++
		if i < maxListedDiagnostics {
			_, _ = fmt.Fprintf(w, "dcmgendir: entry %d: %s: %s\n", diagnostic.SourceIndex, diagnostic.Code, diagnostic.Message)
		}
	}
	if len(diagnostics) <= maxListedDiagnostics {
		return
	}
	codes := make([]string, 0, len(counts))
	for code := range counts {
		codes = append(codes, string(code))
	}
	sort.Strings(codes)
	for _, code := range codes {
		_, _ = fmt.Fprintf(w, "dcmgendir: %d entries in all with %s\n", counts[dicomdir.DiagnosticCode(code)], code)
	}
}

func hasCode(diagnostics []dicomdir.Diagnostic, code dicomdir.DiagnosticCode) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

// writeTo writes the DICOMDIR to path through a temporary in its directory.
func writeTo(ctx context.Context, path string, set *dicomdir.FileSet, write dicomdir.WriteOptions) (dicomdir.WriteReport, error) {
	temp, err := os.CreateTemp(filepath.Dir(path), ".dcmgendir-*")
	if err != nil {
		return dicomdir.WriteReport{}, err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	report, err := dicomdir.WriteDICOMDIR(ctx, temp, set, write)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return report, err
	}
	return report, os.Rename(temp.Name(), path)
}

type fileSelections []dicomdir.FileID

func (f *fileSelections) String() string { return "" }
func (f *fileSelections) Set(value string) error {
	id, err := dicomdir.ParseFileID(strings.ReplaceAll(value, "/", "\\"))
	if err != nil {
		return errUsage
	}
	for _, previous := range *f {
		if previous.String() == id.String() {
			return errUsage
		}
	}
	if len(*f) >= 100000 {
		return errUsage
	}
	*f = append(*f, id)
	return nil
}
