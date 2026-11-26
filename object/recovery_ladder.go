package object

import (
	"github.com/ThalesMMS/dicom-go/parser"
)

// ReadRecovery is one named opt-in read tolerance of a recovery ladder
// (#1136). Apply enables it on the read options; a recovery that needs the
// transfer syntax recovery entry point sets TransferSyntax.
type ReadRecovery struct {
	// Name is a fixed, value-free diagnostic code.
	Name           string
	TransferSyntax bool
	Apply          func(*ReadFileOptions, *TransferSyntaxRecoveryOptions)
}

// NonConformantReadRecoveries returns every opt-in read recovery dicom-go
// offers, in ladder order: the cheapest structural repairs first, transfer
// syntax inference last. Each recovery is individually documented on
// ReadFileOptions or TransferSyntaxRecoveryOptions and reports what it did on
// the returned File.
func NonConformantReadRecoveries() []ReadRecovery {
	return []ReadRecovery{
		{Name: "odd-length", Apply: func(o *ReadFileOptions, _ *TransferSyntaxRecoveryOptions) {
			o.OddLengthPolicy = parser.AcceptOddLength
		}},
		{Name: "container-overrun", Apply: func(o *ReadFileOptions, _ *TransferSyntaxRecoveryOptions) {
			o.RecoverContainerOverruns = true
		}},
		{Name: "missing-meta-group-length", Apply: func(o *ReadFileOptions, _ *TransferSyntaxRecoveryOptions) {
			o.AllowMissingMetaElementGroupLength = true
		}},
		{Name: "implicit-vr-file-meta", Apply: func(o *ReadFileOptions, _ *TransferSyntaxRecoveryOptions) {
			o.AllowImplicitVRFileMeta = true
		}},
		{Name: "implicit-vr-element", Apply: func(o *ReadFileOptions, _ *TransferSyntaxRecoveryOptions) {
			o.RecoverImplicitVRElements = true
		}},
		{Name: "delimiter-anomalies", Apply: func(o *ReadFileOptions, _ *TransferSyntaxRecoveryOptions) {
			o.RecoverDelimiterAnomalies = true
		}},
		{Name: "zlib-deflate", Apply: func(o *ReadFileOptions, _ *TransferSyntaxRecoveryOptions) {
			o.AllowZlibDeflate = true
		}},
		{Name: "vr-repairs", Apply: func(o *ReadFileOptions, _ *TransferSyntaxRecoveryOptions) {
			o.VRRecovery = parser.VRRecoveryOptions{MapOXToOW: true, CorrectPrivateULLength: true, ShortLengthUN: true, CorrectGETextLength: true}
		}},
		{Name: "sequence-repairs", Apply: func(o *ReadFileOptions, _ *TransferSyntaxRecoveryOptions) {
			o.SequenceRecovery = parser.SequenceRecoveryOptions{UndefinedLengthOBOWAsSQ: true, VOILUTSequenceOWAsSQ: true, ReplaceWrongDelimiter: true, PrivateSequenceAsBytes: true}
		}},
		{Name: "transfer-syntax", TransferSyntax: true, Apply: func(_ *ReadFileOptions, t *TransferSyntaxRecoveryOptions) {
			t.AllowMissingPreamble = true
			t.AllowMissingFileMeta = true
			t.AllowMissingTransferSyntaxUID = true
			t.AllowUnknownTransferSyntaxUID = true
			t.AllowDeclaredMismatch = true
			t.AllowLegacySyntaxes = true
		}},
	}
}

// RecoveryReadReport records how OpenFileWithRecoveryLadder read a file. It
// holds fixed diagnostic codes only, never data set values.
type RecoveryReadReport struct {
	// Applied names the recoveries of the successful attempt, in ladder
	// order; it is empty when the strict read succeeded.
	Applied []string
	// Resolution is set when the transfer syntax recovery was applied.
	Resolution TransferSyntaxResolution
	// Attempts counts the reads performed, the strict one included.
	Attempts int
}

// Recovered reports whether any recovery was needed.
func (r RecoveryReadReport) Recovered() bool { return len(r.Applied) > 0 }

// OpenFileWithRecoveryLadder reads path strictly and, only if that fails,
// retries with recoveries in a bounded order: each recovery alone, in ladder
// order; then all of them together, followed by backward elimination, which
// drops each recovery in turn while the read still succeeds. The file is thus
// read with an irreducible set of tolerances in at most 2·len(recoveries)+2
// reads. base supplies the limits and every other option of each attempt.
// When every attempt fails the strict error is returned.
func OpenFileWithRecoveryLadder(path string, base ReadFileOptions, recoveries []ReadRecovery) (*File, RecoveryReadReport, error) {
	if err := rejectPrefixOptions(base); err != nil {
		return nil, RecoveryReadReport{}, err
	}
	report := RecoveryReadReport{Attempts: 1}
	file, strictErr := OpenFileWithOptions(path, base)
	if strictErr == nil {
		return file, report, nil
	}
	try := func(set []ReadRecovery) (*File, TransferSyntaxResolution, bool) {
		report.Attempts++
		opts := base
		var tsOpts TransferSyntaxRecoveryOptions
		useTS := false
		for _, recovery := range set {
			recovery.Apply(&opts, &tsOpts)
			useTS = useTS || recovery.TransferSyntax
		}
		if err := rejectPrefixOptions(opts); err != nil {
			return nil, TransferSyntaxResolution{}, false
		}
		var (
			file       *File
			resolution TransferSyntaxResolution
			err        error
		)
		if useTS {
			file, resolution, err = OpenFileWithTransferSyntaxRecovery(path, opts, tsOpts)
		} else {
			file, err = OpenFileWithOptions(path, opts)
		}
		if err != nil {
			if file != nil {
				_ = file.Close()
			}
			return nil, TransferSyntaxResolution{}, false
		}
		return file, resolution, true
	}
	succeed := func(file *File, resolution TransferSyntaxResolution, set []ReadRecovery) (*File, RecoveryReadReport, error) {
		report.Applied = make([]string, len(set))
		for i, recovery := range set {
			report.Applied[i] = recovery.Name
			if recovery.TransferSyntax {
				report.Resolution = resolution
			}
		}
		return file, report, nil
	}
	for i := range recoveries {
		if file, resolution, ok := try(recoveries[i : i+1]); ok {
			return succeed(file, resolution, recoveries[i:i+1])
		}
	}
	if len(recoveries) < 2 {
		return nil, report, strictErr
	}
	set := append([]ReadRecovery(nil), recoveries...)
	file, resolution, ok := try(set)
	if !ok {
		return nil, report, strictErr
	}
	for i := 0; i < len(set) && len(set) > 1; {
		smaller := append(append([]ReadRecovery(nil), set[:i]...), set[i+1:]...)
		if f, r, ok := try(smaller); ok {
			_ = file.Close()
			file, resolution, set = f, r, smaller
			continue
		}
		i++
	}
	return succeed(file, resolution, set)
}
