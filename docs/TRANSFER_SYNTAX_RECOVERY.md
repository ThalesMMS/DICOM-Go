# Transfer Syntax recovery

`dicom-go` keeps Part 10 and raw dataset reading strict by default. The regular
`ReadFile`, `OpenFile`, `ReadDataSet`, and `OpenDataSet` APIs never infer a
Transfer Syntax. Recovery is available only through dedicated APIs that return
an auditable `TransferSyntaxResolution`.

```go
file, resolution, err := object.OpenFileWithTransferSyntaxRecovery(
	"legacy.dcm",
	object.ReadFileOptions{MaxTotalBytes: 512 << 20},
	object.TransferSyntaxRecoveryOptions{
		AllowMissingPreamble:          true,
		AllowMissingFileMeta:          true,
		AllowMissingTransferSyntaxUID: true,
		AllowUnknownTransferSyntaxUID: true,
		AllowDeclaredMismatch:         true,
		Probe: parser.TransferSyntaxProbeOptions{
			MinimumConfidence:    0.80,
			MinimumConfidenceGap: 0.15,
			MaxProbeBytes:        1 << 20,
			MaxCandidateBytes:    1 << 20,
			MaxElements:          256,
			MaxSequenceDepth:     16,
			MaxFragments:         64,
			MaxCandidates:        8,
			MinimumElements:      2,
			MaxTokens:            1024,
		},
	},
)
```

Each policy flag authorizes one non-conformant input condition. A declared
syntax that parses successfully always wins. A declared/dataset mismatch is
overridden only when `AllowDeclaredMismatch` is true and another candidate
passes both the absolute confidence threshold and the margin over the
runner-up. The original File Meta object is retained; recovery changes the
effective `File.TransferSyntax` but does not rewrite the source or silently
replace `(0002,0010)`.

## Probe contract

The probe reuses `parser.Reader` for every candidate. It does not implement a
second header parser. Evidence includes structurally valid tokens, bytes
consumed, tag ordering, valid explicit VR/reserved-byte layout, dictionary VR
agreement, plausible fixed-width value lengths, and valid sequence/item
nesting. Private and unknown tags are neutral evidence.

The default candidates are:

- Implicit VR Little Endian;
- Explicit VR Little Endian;
- Explicit VR Big Endian.

Compressed, deflated, encapsulated, unsupported, and media-payload candidates
are rejected by the probe. Their Pixel Data bytes are not reliable codec
evidence; callers must supply their Transfer Syntax explicitly through the
ordinary read APIs.

A dictionary VR mismatch lowers an explicit VR candidate by at most the share
of known elements that mismatch, so a few retired VRs in an otherwise
consistent ACR-NEMA prefix do not make the probe ambiguous.

`TransferSyntaxProbeReport` distinguishes `selected`, `ambiguous`, and
`impossible`. Candidate diagnostics contain only syntax metadata, fixed reason
codes, conformance/failure classes, offsets, and counts. They never contain
element values, unknown File Meta UID text, or arbitrary parser error strings.
`ErrTransferSyntaxProbeAmbiguous` and
`ErrTransferSyntaxProbeImpossible` can be tested with `errors.Is`.

## Legacy encodings

`AllowLegacySyntaxes` accepts two native encodings that have no standard
Transfer Syntax UID. The `transfer` package describes them but does not
register them, so the ordinary readers keep rejecting them:

| Encoding | How it is selected | Normalization |
|---|---|---|
| `transfer.ImplicitVRBigEndianACRNEMA` (ACR-NEMA 1.0/2.0) | Added to the default probe candidates | Every fixed-width value is reversed to Little Endian; OW, Pixel Data included, by 16-bit words; UN values are kept |
| `transfer.PrivateGEImplicitVRBigEndianPixelData` (`1.2.840.113619.5.2`) | A File Meta Transfer Syntax UID naming it is read as declared | The data set is Implicit VR Little Endian; 16-bit native Pixel Data is swapped from Big Endian (DCMTK `EXS_PrivateGE_LEI_WithBigEndianPixelData`, GDCM `ImplicitVRBigEndianPrivateGE`) |

After the read the File, or raw data set, holds Implicit VR Little Endian
values: `File.TransferSyntax` and `TransferSyntaxResolution.Syntax` are
Implicit VR Little Endian and `TransferSyntaxResolution.Legacy` records the
encoding that was read. Deferred values are swapped as they are copied.
Frame and encapsulated sinks are refused (`object.ErrLegacySyntaxFrameSink`)
because they see values before normalization. Writing produces a standard
Implicit VR Little Endian file.

Pixel data decompression accepts a File whose effective syntax is the one
its `TransferSyntaxResolution` recorded, even when `(0002,0010)` still holds
the value the File was recovered from. A File whose syntax was changed after
the read keeps the File Meta consistency check. `File.ValidateFile` and
`File.ValidatePart10Identity` compare `(0002,0010)` with the legacy encoding
that was read, which File Meta declares truthfully. `AllowUnknownTransferSyntaxUID`
never infers a syntax for a legacy UID, because a standard syntax would read
GE Big Endian pixels as Little Endian.

## Native Pixel Data under a compressed syntax

A file can declare a compressed, encapsulated syntax while its Pixel Data is
a defined-length native value; the data set parses (all encapsulated syntaxes
are Explicit VR Little Endian), so the transfer syntax probe keeps the
declared syntax and the codec refuses the value. Pixel data decoding does not
guess: `DecodeRecovery.NativePixelDataUnderEncapsulated` (opt-in, in
`DecompressOptions` and `TranscodeOptions.Recovery`) reads it as native only
when its length is exactly Rows x Columns x Samples per Pixel x Bits
Allocated / 8 x Number of Frames (plus one pad byte for an odd size), as GDCM
does, and reports `native-pixel-data-under-encapsulated-syntax`. The
decompressed data set, or the transcoded one, declares its real syntax.

## Encapsulated Pixel Data with a defined length

PS3.5 A.4 requires encapsulated Pixel Data to have an undefined length.
Some writers give it a defined one, so the Items are read as the bytes of an
OB value and the codec refuses them (`ErrIncompatiblePixelData`).
`DecodeRecovery.DefinedLengthFragments` (opt-in, in `DecompressOptions` and
`TranscodeOptions.Recovery`) reads such a value as the fragment sequence only
when it is exactly a Basic Offset Table Item and at least one fragment Item,
each with a defined length inside the value, optionally ended by a Sequence
Delimitation Item of length 0; bytes left over, a truncated or
undefined-length Item or more than 100000 Items leave it unchanged. It
reports `encapsulated-defined-length`, and a transcode writes a conforming
fragment sequence. gdcmData `JPEGDefinedLengthSequenceOfFragments.dcm` decodes
with it (and ReconcileWithCodestream for its Bits Stored) to GDCM's MD5.

## Resource bounds and replay

`MaxProbeBytes` bounds the shared in-memory prefix, while
`MaxCandidateBytes`, `MaxElements`, `MaxSequenceDepth`, `MaxFragments`,
`MaxTokens`, `MaxCandidates`, and the candidate deadline bound each parser
pass. `MaxDuration` is a shared
cooperative deadline across all candidates. Time is checked between tokens;
an arbitrary blocking `io.Reader` can only be interrupted by the reader's own
context or deadline support.

Seekable sources are rewound for probing and the final parse, preserving
deferred-value offsets and source lifetime. Their probe prefix is capped by the
remaining logical `ReadFileOptions.MaxTotalBytes` budget as well as
`MaxProbeBytes`; the final parser enforces `MaxTotalBytes` again over the
logical dataset extent. Consequently a seekable prefix may be read twice, but
neither pass can exceed its configured bound. A non-seekable source is replayed
from one bounded buffer. `MaxNonSeekableBytes` defaults to 16 MiB and the call
fails with `ErrTransferSyntaxRecoveryBufferExceeded` if the complete source
does not fit. This avoids unbounded buffering and abandoned goroutines. For a
non-seekable source, `ReadFileOptions.MaxTotalBytes`, when set, is also a hard
upper bound on source bytes read while creating that replay buffer.

Recovery is intended for explicit local ingestion and inspection workflows. It
is not enabled automatically in C-STORE, STOW-RS, DIMSE datasets, or the Twin
Viewer.

## `dcmdump`

The CLI exposes the same opt-in policy:

```sh
go run ./cmd/dcmdump -recover-transfer-syntax legacy-or-raw.dcm
go run ./cmd/dcmdump -json -recover-transfer-syntax legacy-or-raw.dcm
```

It writes a warning with the inferred UID, source code, and confidence to
standard error. Text and JSON output also mark the syntax as inferred. Recovery
cannot currently be combined with `-show-offsets`, because the offset renderer
reparses a conformant Part 10 layout.
