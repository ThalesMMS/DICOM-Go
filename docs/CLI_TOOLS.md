# File tools

File commands expose library capabilities over Part 10 files, including seven after
the GDCM applications of the same purpose (gdcmconv, gdcmanon, gdcmgendir,
gdcmdiff, gdcmraw, gdcmscanner and gdcminfo) and `dcmsign` after DCMTK
dcmsign. `dcmodify` and `dcmdump` also expose attribute editing and inspection.
Input replacement requires an explicit `-in-place` on a command that supports it:
outputs normally go to `-o`, through a temporary that is renamed into
place. Errors print as
`<tool>: <class>: <message>` on standard error; usage errors exit with 2 and
failures with 1. `dcmdiff` exits with 1 when the files differ, as `diff` does,
and `dcmsign verify` when a check fails.

```sh
go run ./cmd/dcmconv -h
go run ./cmd/dcmanon -h
go run ./cmd/dcmgendir -h
go run ./cmd/dcmdiff -h
go run ./cmd/dcmraw -h
go run ./cmd/dcmsign -h
go run ./cmd/dcmscan -h
go run ./cmd/dcminfo -h
go run ./cmd/dcmdump -h
go run ./cmd/dcmodify -h
```

## Shared parser recovery

`dcmdump`, `dcmconv`, `dcmodify` and `storescp` share the following long
read-policy flags. Recovery is disabled by default and odd value lengths are
rejected. The existing reserved-byte policy remains permissive:
`-read-strict-reserved-bytes` opts into rejecting nonzero reserved bytes in
explicit VR headers. These options configure structural parsing; they do not
provide full IOD validation.

| Flag | Opt-in behavior |
| --- | --- |
| `-read-replace-wrong-delimiter` | Replace an item delimiter with a sequence delimiter, or the reverse, in its bounded context. |
| `-read-undefined-length-obow-as-sq` | Read non-Pixel-Data undefined OB/OW beginning with Items as SQ. |
| `-read-voi-lut-ow-as-sq` | Read VOI LUT Sequence incorrectly encoded as OW as a sequence. |
| `-read-private-sequence-as-bytes` | Keep a malformed defined private SQ as opaque UN bytes; lookahead has the existing hard 16 MiB cap. |
| `-read-prefer-dictionary-vr` | Prefer a public attribute's single standard dictionary VR. |
| `-read-prefer-dictionary-length-field-size` | Also use that VR's length-field width; requires the preceding flag. |
| `-read-correct-private-ul-length` | Read the supported private UL length-6 layout as length 4. |
| `-read-map-ox-to-ow` | Read nonstandard Pixel Data OX as OW. |
| `-read-short-length-un` | Read the supported malformed UN short-length header layout. |
| `-read-correct-ge-text-length` | Apply bounded GE implicit text length correction when its boundary is unambiguous. |
| `-read-recover-implicit-vr-elements` | Recover bounded implicit VR elements within explicit LE datasets. |
| `-read-recover-container-overruns` | Recover supported container-length anomalies; a primitive value crossing its container end remains refused. |
| `-read-recover-delimiter-anomalies` | Recover supported swapped, orphan or malformed delimiters. |
| `-read-odd-length=accept` | Accept and report odd value lengths; the default is `reject`. |
| `-read-allow-zlib-deflate` | Accept a checksum-checked zlib wrapper around dataset Deflate. |

File inputs also expose `-read-allow-implicit-file-meta` and
`-read-allow-missing-meta-group-length`. Those flags are unavailable in
`storescp`, which receives datasets and authors standard File Meta.

Each applied repair is reported on stderr with a fixed reason, tag/item path,
VRs, lengths and offset; attribute values and SOP instance UIDs are excluded.
Coordinates are physical for native file data, inflated-logical for Deflate,
and dataset-logical for received datasets. Offset `-1` means unavailable.
Reports cover the whole read even when the printed attributes are filtered.
`-read-max-boundary-recoveries` defaults to 128 and bounds the complete report
jointly across Meta and dataset. A truncated report is refused before editing,
exporting or publishing a store. `-read-max-implicit-vr-elements` defaults to
4096 and caps implicit-VR element switches. Delimiter anomaly recovery keeps
its existing default limit 4096, which these CLIs do not expose as a flag.
Both exposed caps require positive values.

File reads have positive finite defaults: `-read-max-total-bytes=1073741824`,
`-read-max-element-bytes=67108864`, `-read-max-pixel-data-bytes=805306368`,
`-read-max-elements=100000`, `-read-max-sequence-depth=128` (combined
sequence/item depth), and `-read-max-fragments=100000`. These limits apply per
parser pass and bound logical inflated bytes for Deflate. They are distinct
from preview/export and codec limits. `storescp` retains its existing
`-max-*` reception/admission limits and overlays only recovery policy.

To report explicit File Meta repairs, the file tools make an additional
bounded, cancellable token pass over the preamble and Meta from the same
retained regular-file descriptor. They use the original read options,
metadata dictionary and recovered explicit/implicit Meta syntax. This adds
prefix/Meta I/O, including bounded reader buffering; it does not reparse the
dataset. The descriptor stays open through replay and any deferred-value
serialization, then is closed by the adapter. The bit-preserving SCP uses a
second serial `StreamFile.ReadBack` of its same private uncommitted temporary.
The complete joint report precedes edits, export, commit and successful
C-STORE acknowledgement. Missing replay, changed/truncated sources,
inconsistent recovered Meta and incomplete reports are refused. Stat and
Meta comparisons detect ordinary inconsistencies; they do not prove the
remaining dataset's identity or exclude concurrent mutation.

`dcmconv` admits the configured read, emits its complete report, then writes
an owned bounded temporary and strictly reads it back before using the
ordinary transcode transaction. The temporary preserves the read sequence,
group-length and padding policies; final serialization flags apply to the
output. `-read-recover` remains the separate transfer-syntax/legacy recovery
switch; `-recover` configures pixel decoding. `dcmodify` reports parser
repairs before applying edits or recording Original Attributes.

## dcmdump: inspect and export values

```sh
dcmdump -search-tag PatientName -show-offsets in.dcm
dcmdump -search-path 'ReferencedImageSequence[*].ReferencedSOPInstanceUID' in.dcm
dcmdump -read-short-length-un -read-max-boundary-recoveries 4096 -search-tag PixelData in.dcm
dcmdump -search-tag PixelData -write-values existing-output-dir in.dcm
```

`-search-tag` is repeatable and accepts a keyword or numeric tag. It matches
Meta and dataset attributes at any depth. `-search-path` is repeatable,
dataset-rooted, uses dot separators and zero-based `[n]` or `[*]` sequence
ancestors, and ends in an attribute. Item-only leaves and private-creator
segments are refused; numeric private tags are accepted. Selectors form a
union and preserve source order. Selecting SQ prints its header without
implicitly selecting its children. No match is an explicit failure.

`-show-paths` prints scope-prefixed numeric paths; displayed ancestor
separators are `/`, for example
`dataset:(0008,1140)[0]/(0008,1155)`. Existing `-max-value` bounds only the
preview. `-show-offsets` continues to show physical element-header offsets
and remains unavailable with Deflate or transfer-syntax inference. The
existing `-recover-transfer-syntax` remains an explicit bounded inference
switch for malformed/raw files. Selection, paths and export are text-only;
combining them with `-json` is a usage error.

`-decode-additional-private-headers` explicitly decodes the additional
Philips/Toshiba/Canon metadata profiles, with bounded output and explicit
unsupported-variant errors. It does not enable the legacy vendor parsers.
See [formats and decoding limits](./VENDOR_ADDITIONAL_HEADERS.md).

`-write-values DIR` requires a selector and an existing trusted directory.
It streams selected primitive source value spans in 64 KiB chunks without
assembling a complete bulk payload. The bytes retain their original
endianness, charset encoding and padding, without the attribute header.
Encapsulated Pixel Data includes BOT/Fragment Item framing and its Sequence
Delimiter; it is not an assembled frame or concatenated fragment payload.
For Deflate, an owned 0600 bounded spool supplies the inflated dataset value
bytes, and the manifest labels those coordinates. Zlib checksums must pass.
Raw dataset export requires explicit transfer-syntax recovery and an
unambiguous dataset start; recovered File Meta without an available original
span is refused. The manifest records the effective container syntax/order;
values inside a recovered mixed-encoding item retain that item's source bytes.

`values.json` records concrete paths, VR, value byte counts, SHA256 hashes,
coordinate space and numeric filenames. Default caps are
`-max-export-files=10000` and `-max-export-bytes=1073741824`; both require
positive values. SQ/item export, ambiguous duplicate source paths, unavailable
replay, truncation, changed sources and external BulkDataURI values are
refused. Selection and cumulative byte limits are checked before publication.
Files are staged with mode 0600, verified by byte count/hash readback, and
published create-only through descriptor-relative nofollow operations.
Existing entries and symlinks are refused. Publication is per value, followed
by the manifest; a later failure can leave already reported complete values.
Unpublished temporaries and inflated spools are removed.
Directory sync is performed on Unix platforms; Windows publication uses the
existing handle-based rename and does not claim directory sync.

## dcmodify: edit attributes

```sh
dcmodify -read-recover-implicit-vr-elements -i PatientID=TEST -o out.dcm in.dcm
dcmodify -m PatientName=ANON -in-place in.dcm
```

`-i`, `-m`, `-e` and `-e-if-present` take attribute paths in command-line
order; `-script` adds a file of edits. Output requires `-o` or explicit
`-in-place`. Parser recovery is a separate read policy and a refused or
incomplete read report prevents edits and output. Existing curated-private
dictionary and Original Attributes options remain available.

Root UID replacement, recursive edits, explicit private-tag removal, typed
value files, conditional processing and bounded multi-file execution are
described in [dcmodify parity](./DCMODIFY_PARITY.md). Private removal combined
with Original Attributes recording can retain prior private values in that
audit sequence; it does not establish complete de-identification.

## dcmconv: transcode

```sh
dcmconv -ts jpeg-ls-lossless -o out.dcm in.dcm
dcmconv -ts explicit-le -recover ignore-bot,rle-segment-overrun -o out.dcm in.dcm
dcmconv -ts jpeg-baseline -allow-lossy -jpeg-quality 85 -o out.dcm in.dcm
```

`-ts` takes a short name (`implicit-le`, `explicit-le`, `explicit-be`,
`deflated`, `jpeg-baseline`, `jpeg-extended`, `jpeg-lossless`,
`jpeg-lossless-sv1`, `jpeg-ls-lossless`, `jpeg-ls-near`, `rle`,
`deflated-frames`), a UID or a PS3.6 keyword such as `JPEGLSLossless`. The command wraps
`pixeldata.TranscodePath`: by default, lossless targets are decoded again and compared
pixel for pixel before the output is published, and lossy targets require
`-allow-lossy`. Encoders are those of the pure-Go profile: RLE, JPEG-LS,
JPEG Lossless, Deflated Image Frame Compression (`deflated-frames`, one raw
DEFLATE stream per frame; distinct from the data-set `deflated`) and, with `-allow-lossy`, JPEG Baseline, JPEG Extended and
JPEG-LS near-lossless (`-near`). JPEG 2000 Part 1 targets have an explicit optional OpenJPEG source-checkout
profile described in [dcmconv parity](DCMCONV_PARITY.md); the default build
refuses that profile. JPEG XL and other optional targets remain unavailable.

`-workers=0` (the default) or `1` keeps codec calls serial. A value greater than
one explicitly selects bounded parallel frame work. `-in-flight-frames`,
`-in-flight-bytes` and `-max-encoded-frame-bytes` configure the finite window
and encoded-frame cap; the byte limit is a managed-buffer charge, not an RSS
cap. `-verify=false` explicitly skips the additional top-level output pixel
check. `-durability=atomic` explicitly omits file/directory sync; the default
`durable` retains them where supported. Both modes preserve staged structural
readback and atomic publication. The report identifies the actual checks,
syncs and streaming route. See [frame-window policies](PIXEL_TRANSCODING.md#frame-windows-verification-and-durability).

`-compress-icons` compresses Icon Image Sequence items with the same lossless
target codec. By default, encapsulated output keeps icons native; native
output decompresses them. The command reports each icon's action and flags
preserved incomplete metadata. Explicit compression of an incomplete icon
fails without publishing the destination. See
[icon transcoding](PIXEL_TRANSCODING.md#icon-image-sequence).

The tolerances for damaged or non-conformant sources are opt-in and each one
applied is printed: `-recover` takes the `pixeldata.DecodeRecovery` names
(`ignore-bot`, `default-image-pixel-module`, `unpack-12-bit`,
`jpeg-lossless-quirks`, `jpeg-lossless-predictor6`, `jpeg-sequential-quirks`,
`jpeg-ls-jai-thresholds`, `rle-segment-overrun`, `reconcile-with-codestream`,
`unwrap-jp2`, `native-under-encapsulated`, `defined-length-fragments`,
`repair-bit-count-masks`). `-recover-un` gives UN elements their dictionary VR,
as gdcmconv `-U` does. `-rewrite` changes the stored pixels between decoding and
encoding (`palette-to-rgb`, `linearize-palette`, `ybr-full-to-rgb`,
`monochrome1-to-monochrome2`, `interleaved`, `planar`), and
`-extract-overlays` moves overlays held in unused pixel bits to Overlay Data.

`-group-length=preserve|recalculate|create|remove` controls dataset Group
Length attributes; its default is `preserve`. File Meta Group Length is
always rebuilt. `-sequence-length=undefined|preserve|defined` controls both
sequence and item lengths; its default is `undefined`. `-dataset-only`
publishes only the encoded dataset body, including the already produced raw
Deflate stream and final NULL when applicable. It uses a structurally
verified private Part 10 precursor and verifies the extracted body by exact
byte count/hash readback. This mode requires an absent destination and
rejects nonzero `-pad-file`; Part 10 output retains its existing atomic
replacement semantics.

Existing `-compression-level=-1|0..9` controls only raw dataset Deflate and
requires a Deflated target for levels 0..9. `-padding=preserve|remove|create`,
`-pad-file`, `-pad-item` and `-max-padding-bytes` retain their existing
contracts: file alignment includes the logical Part 10 prefix/meta before
Deflate, while item alignment excludes its Header/Delimiter. Item padding
also applies to dataset-only output; a nonzero Part 10 file alignment does
not. The default generated padding cap remains 16 MiB.

`-remove-gl` aliases `-group-length remove`. `-remove-private-tags` and
`-remove-retired-tags` use bounded, atomic recursive library filtering; private
creators are included, unknown public tags are retained, and this is not complete
de-identification. `-split bytes` aliases the Fragment cap and rounds odd values
down; it requires an encapsulated target. `-generate-icon` uses the public
64×64 representative-frame generator; `-icon-minmax MIN,MAX` or
`-icon-auto-minmax` selects monochrome VOI. `-check-meta` reports/rebuilds the
three Part 10 identity fields using codes/tags only. `-root-uid root` applies to
new SOP Instance UIDs in this call and preserves related identities/references.
See [contracts, optional J2K wiring and limitations](DCMCONV_PARITY.md).

## dcmanon: de-identify

```sh
dcmanon -o anon.dcm in.dcm
dcmanon -option retain-device-identity,clean-descriptors -o outdir indir
dcmanon -uid-key-file secret.key -date-shift-days -30 -report report.json -o outdir indir
```

The command applies the PS3.15 Basic Application Level Confidentiality Profile
(`deid.BasicProfilePath`). A directory is processed recursively into the same
layout under `-o`, with one UID remapping for the run, so references between
the files stay consistent. `-uid-key-file` makes the remapping deterministic
across runs; keep that key secret. Without it the UIDs are random.

`-option` accepts `clean-graphics`, `clean-structured-content`,
`clean-descriptors`, `retain-full-dates`, `retain-patient-characteristics`,
`retain-device-identity`, `retain-institution-identity` and `retain-uids`.
`-date-shift-days` selects Retain Longitudinal Temporal Information with
Modified Dates and shifts every date by the same number of days. The options
that need application code, such as pixel cleaning, visual-feature cleaning
and the safe private attribute registry, are refused.

Each file's report records its actions, residual risks and table provenance
without attribute values. It is written as JSON with `-report`, and the risk
codes are counted on standard output. Pixel Data is not cleaned, so every
image reports `pixel-cleaning-not-attested` and the burned-in and visual
feature risks. Review such output before release. `-strict` fails a file whose
report is incomplete or has residual risks and removes that file's output.

```sh
dcmanon -encrypt-for recipient.crt -o outdir indir
dcmanon -reidentify -recipient-cert recipient.crt -recipient-key recipient.key -o restored outdir
```

`-encrypt-for` (repeatable; each file holds PEM or DER certificates) makes the
de-identification reversible, as `gdcmanon -e` does. The original values are
encrypted for the recipients into the Encrypted Attributes Sequence
(0400,0500). `-key-transport` selects `rsaes-oaep-sha256` (default) or
`rsaes-pkcs1-v1_5`. `-reidentify` restores the originals with a recipient
certificate and its unencrypted private key, as `gdcmanon -d` does, and takes
no de-identification flags. Pixel Data is not restored. See
[De-identification](DEIDENTIFICATION.md).

## dcmgendir: DICOMDIR

```sh
dcmgendir -fileset-id MEDIA_01 media
dcmgendir -invent -icons -profile STD-GEN-USB-JPEG media
dcmgendir -skip-invalid -o preview.dir media
```

The files must already be at PS3.10 File ID paths below the root: 1 to 8
components of 1 to 8 characters from `A-Z`, `0-9` and `_`. Nothing is copied or
renamed. Without `-o` the DICOMDIR is published at the root through
`dicomdir.CommitDICOMDIR`, which reads the result back and checks every
referenced file before it replaces anything. Every entry that cannot be
referenced is reported. The run then fails unless `-skip-invalid` leaves those
entries out. The command writes only PATIENT, STUDY, SERIES and IMAGE records;
other SOP classes are refused.

Several keys are Type 1 in directory records but only Type 2 in image objects.
De-identified files usually lack Study ID, Study Date and Study Time. `-invent`
(`dicomdir.Options.InventMissingKeys`, as `dcmgpdir +I`) fills them from values
the file determines, so a rerun produces the same keys:

| Key | Invented value |
| --- | --- |
| Patient ID | From the Study Instance UID. |
| Study ID | From the Study Instance UID. |
| Series Number | From the Series Instance UID. |
| Instance Number | From the SOP Instance UID. |
| Study Date and Time | From the Series, Acquisition or Content Date and Time; else `19000101` and `000000`. |

Reading a DICOMDIR takes the values it records for keys its files lack,
whichever tool invented them.

## dcmdiff: compare

```sh
dcmdiff a.dcm b.dcm
dcmdiff -meta -ignore SOPInstanceUID -ignore "(0008,0012)" a.dcm b.dcm
```

The command compares values, not encodings, so an Implicit VR file and its
Explicit VR or Big Endian transcode match. How each value is compared:

- Text is compared without its trailing padding.
- Numbers are compared as decoded values.
- Binary values and Pixel Data fragments are compared by length and SHA-256,
  with words in little-endian order.
- Sequences are compared item by item.

OB and OW are treated as one VR because both hold the same bytes. `-ignore`
skips an attribute at any depth, and `-meta` adds the File Meta Information.
Each difference is one line:

```text
< (0010,0020) PatientID LO "A"          only in the first file
> (0008,103E) SeriesDescription LO "B"  only in the second file
! (0020,0013) InstanceNumber IS "1" | "2"
```

`-pixels` compares decoded pixel values instead of attributes, after DCMTK
dcmicmp, to judge a lossy export or transcode by metrics rather than by hash:

```sh
dcmdiff -pixels original.dcm lossy.dcm
dcmdiff -pixels -min-psnr 45 -max-error 8 original.dcm lossy.dcm
```

The second file is measured against the first with package
`pixeldata/compare`, frame by frame, after decoding compressed Pixel Data.
Grayscale images are compared on their modality values (after the Modality
LUT, without VOI) or, with `-stored`, on their stored values; color images on
8-bit RGB or their stored samples. The output gives the frame and sample
counts, the maximum and mean absolute error, the RMSE, and the PSNR and SNR
in dB as dcmicmp defines them: the peak is the largest squared reference
value, and identical images have an infinite PSNR and SNR. Without limits the
exit status is 1 when any sample differs. With `-max-error`, `-max-mae`,
`-max-rmse`, `-min-psnr` or `-min-snr` it is 1 only when a limit is exceeded,
and each exceeded limit is printed. Images of other dimensions, frame counts
or color models differ. The opt-in test `TestDCMTKMetrics`
(`pixeldata/compare`) matches dcmicmp's five metrics on a near-lossless CT
transcode, two different CT images and two RGB images.

## dcmraw: raw values and fragments

```sh
dcmraw -t PixelData -o pixels.bin in.dcm
dcmraw -t 7FE0,0010 -split -o frame in.dcm
dcmraw -insert -t "(0029,1010)" -vr OB -i blob.bin -o out.dcm in.dcm
dcmraw -insert-fragments -offset-table -o out.dcm in.dcm f0.jpg f1.jpg
```

Extraction writes a value as stored in the file. Encapsulated fragments are
written without their Item headers: concatenated, as gdcmraw does, or one
file each with `-split` (`frame.0000`, `frame.0001`, and `frame.bot` for a
non-empty Basic Offset Table). Insertion replaces an attribute with the bytes
of a file, padded to even length with the VR's padding. Pixel Data can be
replaced with fragment files when the transfer syntax is encapsulated;
`-offset-table` makes each fragment one frame of a Basic Offset Table. The
fragments are not checked against the transfer syntax or the image attributes.

## dcmsign: digital signatures

```sh
dcmsign list in.dcm
dcmsign sign -key signer.key -cert signer.pem -profile creator -purpose 1 -o signed.dcm in.dcm
dcmsign sign -key signer.key -cert signer.pem -item "ContentSequence[0]" -in-place signed.dcm
dcmsign verify -ca roots.pem -intermediate ca.pem -crl ca.crl -require-signature signed.dcm
dcmsign timestamp -uid 2.25.123 -tsa-url https://tsa.example/ -in-place signed.dcm
dcmsign timestamp -uid 2.25.123 -write-query q.tsq signed.dcm
dcmsign timestamp -uid 2.25.123 -query q.tsq -response r.tsr -in-place signed.dcm
dcmsign remove -all -o unsigned.dcm signed.dcm
```

The commands use package `dsig` (see the Digital Signatures section of the
[conformance statement](CONFORMANCE.md#digital-signatures-ps315-annex-c)).
`sign` signs the main data set or, with `-item`, one sequence item named by an
attribute path (like DCMTK `--sign-item`); `-tag` limits the Data Elements
Signed, `-mac` picks the MAC algorithm (SHA256 by default), `-profile` refuses
to sign a signature that would not meet the Base, Creator, Authorization or SR
RSA profile, `-purpose` adds a PS3.16 CID 7007 purpose code, and `-tsa-url`
also requests an RFC 3161 certified timestamp. Encapsulated files are signed
in their own transfer syntax. `verify` checks every signature, and with `-ca`
(and `-intermediate`, `-crl`, `-require-crl`, `-time`, `-at-signature-time`)
the signer certificates; with `-tsa-ca` the time stamping authorities, and
`-at-timestamp-time` checks certificates at the certified time.
`-require-signature`, `-require-timestamp` and `-require-profile` make missing
items fail. `timestamp` adds a certified timestamp from an HTTP TSA, or writes
a request (`-write-query`) and later inserts the response to it, checking that
it answers that request for that signature. `remove` deletes one signature
(`-uid`) or all of them, in sequence items too.

Profile output is labeled **attribute-profile coverage** and remains separate
from integrity, certificate trust, timestamps and IOD validity. Authorization
sign/verify needs repeatable `-human-verified-path PATH` or the mutually
exclusive `-human-verified-empty` caller declaration. Neither is implied by
`-profile auth` or `-require-profile auth`; unknown context fails closed.
Paths select existing attributes, with signed parent sequences covering nested
content. The declaration does not establish human authority or clinical truth.
See [coverage, bounds and migration](DIGITAL_SIGNATURE_PROFILES.md).

Keys and certificates come from PEM or DER files. No secret is accepted on the
command line or from the environment: the private key must be an unencrypted
PKCS #8, PKCS #1 or SEC 1 file (encrypted keys are refused), and a warning is
printed when other users can read it.

## dcmscan: attribute table of a tree

```sh
dcmscan -t Modality -t SeriesInstanceUID studies/
dcmscan -t "(0020,000D)" -t ReferencedSeriesSequence/SeriesInstanceUID -format json a.dcm dir/
```

Reads the `-t` attributes of every DICOM file below the given files and
directories and prints one row per file, ordered by path: TSV with a header
(several values joined by `\`) or, with `-format json`, one JSON object per
line. Attributes are keywords or `(gggg,eeee)`; nested ones are named by their
sequence path with `/`, and `-all` collects the value of every matching item
instead of the first. Binary numbers are decoded; OB, OW and other bulk values
print as their length. Only metadata is read (package `index`): the reader
stops at the top-level Pixel Data and never reads it, which
`TestScanDoesNotReadPixelData` and `BenchmarkScanIO` measure as a few hundred
bytes read from an 8 MiB image. Directories are walked without following
links by `-workers` parallel readers, bounded by `-max-files`. Files that are
not DICOM are counted on standard error, never named. The values printed are
exactly those requested and may contain patient data.

## dcminfo: file summary

```sh
dcminfo image.dcm
dcminfo -md5 -json a.dcm b.dcm
```

Summarizes each file without patient, study, series or instance
identifiers: the SOP Class and transfer syntax; for an image its dimensions,
frames and pixel format, whether Pixel Data is native or encapsulated (with
the fragment count and Basic Offset Table) and what the first codestream
declares (JPEG process, JPEG-LS NEAR, JPEG 2000 or HTJ2K wavelet, RLE
segments); whether the pixels were lossy compressed and on what evidence
(`pixeldata.DetectLossyState`: the attribute, the transfer syntax or the
codestream); and for an Encapsulated Document its MIME type, size, PDF
version and MD5 (`encapdoc`). `-md5` decodes Pixel Data and hashes the
samples in native little-endian order, so a lossless transcode keeps the
value. `-json` writes one JSON summary per line.

# Rendering, validation and conversion tools

These commands expose rendering, IOD validation, multi-frame conversion and
NIfTI and Segmentation conversion, after the dcm4che tools of the same names
where there is one (behavior reference only; the code follows the DICOM
standard). They share the conventions of the file tools:
inputs are never replaced, outputs are published through temporaries, errors
print as `<tool>: <class>: <message>`, usage errors exit with 2 and failures
with 1.

```sh
go run ./cmd/dcm2jpg -h
go run ./cmd/dcmvalidate -h
go run ./cmd/emf2sf -h
go run ./cmd/dcm2nii -h
go run ./cmd/nii2seg -h
go run ./cmd/seg2nii -h
```

## dcm2jpg: render frames

```sh
dcm2jpg in.dcm out.jpg
dcm2jpg -frame 3 -format png in.dcm frame.png
dcm2jpg -window 40,400 -q 95 in.dcm out.jpg
dcm2jpg -presentation -overlays -bits 16 in.dcm out.tiff
dcm2jpg -gsps state.dcm in.dcm out.png
dcm2jpg -auto-window histogram -histogram-ignore 0.05 in.dcm out.png
dcm2jpg -monitor-curve monitor.txt -display-function gsdf in.dcm out.png
dcm2jpg -rotate 90 -flip horizontal -scale 2 in.dcm out.png
```

Writes all frames (numbered outputs when several) or `-frame n` as JPEG,
PNG or TIFF. Defaults remain JPEG quality 90 and 8-bit output. Grayscale
uses modality and file/explicit VOI or min/max, with MONOCHROME1 inversion;
RGB, YBR and PALETTE COLOR become RGB. Opt-in image presentation and overlays,
referenced GSPS, calibrated GSDF/CIELAB display functions, additional rotation,
flip and nearest-neighbour scaling are described with their pipeline order and
limits in [dcm2jpg presentation and export](./DCM2JPG_PRESENTATION.md).
PNG/TIFF `-bits 16` preserves actual 16-bit grayscale presentation samples;
GSPS and color output remain 8-bit, and unsupported combinations are refused.
The existing opt-in `TestDCMTKMinMaxWindow` compares min/max with DCMTK
`dcm2pnm +Wm` within one gray level.

## dcmvalidate: IOD and encoding checks

```sh
dcmvalidate image.dcm
dcmvalidate -iod "Enhanced CT Image" -json a.dcm b.dcm
dcmvalidate -values -info -json image.dcm
```

Each file gets two checks. The IOD check (`validation/iod`) finds missing or
empty Type 1, 1C, 2 and 2C attributes of the Modules of the IOD that the SOP
Class names, or of `-iod`, at every sequence depth. The encoding check
(`validation`) finds VR, length, multiplicity, structure and File Meta
consistency problems; `-iod-only` skips it. Each finding names its severity,
code, attribute path and, for the IOD check, the Type and Module; values are
never printed, so the report carries no patient data. Conditions that could
not be evaluated are informational and listed only with `-info`. `-json`
writes one JSON report per line. The exit status is 1 when a file has an
error finding (or a warning, with `-strict`) or cannot be read; an unknown
SOP Class is a failure.

`-values` adds scoped PS3.3 Enumerated Values, extensible Defined Terms and
proven contextual multiplicity checks without changing default presence rules.
Closed-list violations are errors; additional Defined Terms are warnings.
Findings include a stable rule ID and pinned normative source, without examined
values. Unformalized restrictions remain informational/manual (`-info`). See
[scoped value constraints](VALIDATION.md#scoped-value-constraints) for coverage,
limits and the distinction between dictionary VM and SQ Item count.

## emf2sf: Enhanced to single-frame and back

```sh
emf2sf -o outdir enhanced.dcm
emf2sf -frames 1,5-8 -o outdir enhanced.dcm
emf2sf -reverse -o legacy.dcm slice*.dcm
```

Converts an Enhanced or Legacy Converted Enhanced CT, MR or PET image into
classic single-frame images (`multiframe.ConvertToClassic`), written to the
`-o` directory as `<input name>-<frame>.dcm`. `-frames` takes 1-based frame
numbers and ranges. `-reverse` converts classic CT, MR or PET images of one
series into one Legacy Converted Enhanced image
(`multiframe.ConvertToLegacyEnhanced`), with frames ordered along the slice
normal (or by Instance Number). Either way the result is a new series
(`-series-uid`, or a new UID) with new SOP Instance UIDs, its Pixel Data is
native Explicit VR Little Endian, and the Contributing Equipment Sequence
records the conversion.

## dcm2nii, nii2seg and seg2nii: NIfTI volumes and Segmentations

```sh
dcm2nii -o ct.nii.gz study/series/
nii2seg -labels labels.ctbl -o seg.dcm labels.nii.gz study/series/
seg2nii -labels segments.ctbl -o labels.nii.gz seg.dcm study/series/
```

`dcm2nii` writes one image series as a NIfTI-1 volume through
`nifti.PlanVolume` and `Write` ([NIFTI_EXPORT.md](NIFTI_EXPORT.md)): slices
ordered along their normal on one regular grid (`-resample` interpolates a
grid render can regularize), an affine to RAS+ millimetres, stored values with
one uniform rescale in `scl_slope`/`scl_inter` or rescaled with
`-scaling float32|float64`, and no presentation transform. A one-frame series
needs `-single-slice patient|image-grid`. Inputs are files and directories;
when they hold several series, `-series` selects one. `-sidecar` writes the
closed, PHI-free JSON sidecar, and a `.gz` name (or `-gz`) compresses.

`nii2seg` converts an integer labelmap (`.nii`, `.nii.gz` or `.hdr`/`.img`)
drawn on a reference series into a binary DICOM Segmentation
(`nifti.Volume.SEGDocument`, [NIFTI_IMPORT.md](NIFTI_IMPORT.md)). The
labelmap must lie on the series grid up to an axis permutation, flips and an
integer offset; nothing is interpolated. Each label becomes the segment of
the same number, named and coloured by the `-labels` table (3D Slicer,
FreeSurfer or ITK-SNAP layout) or `Label N`. The Segmentation is a new series
of the reference study and frame of reference and copies the patient and
study modules from the first reference image, as its IOD requires.

`seg2nii` writes the segment number covering each voxel (0 for none):
BINARY and LABELMAP segmentations as stored (a label map's background, its
Pixel Padding Value, as 0), FRACTIONAL ones where the value
reaches `-threshold` of the maximum (0.5). The grid is the reference series'
when one is given, else the Segmentation's planes, which must lie on regularly
spaced slices. Slices run along the row direction cross the column direction,
as `dcm2nii` writes them, so the labelmap of a series lies on `dcm2nii`'s
volume of it. Overlapping segments fail unless `-split` writes one binary
volume per segment (`labels-1.nii.gz`, ...). `-labels` writes a 3D Slicer
colour table of the segment labels and recommended display colours.

`TestRoundTrip` (in `cmd/seg2nii`) runs an oblique series through `dcm2nii`,
a labelmap on that grid through `nii2seg` and the Segmentation through
`seg2nii`, with and without the reference series, and finds the same affine
and labels.

# DIMSE service tools

Eleven DIMSE commands share `-host`, `-port`, `-called`, `-calling` and
`-timeout`, with `-c AET@host:port` as the combined peer alias. Existing
flags and per-tool defaults remain available; conflicting explicit aliases
are rejected independently of argument order. Listener identities and
`-net-config` retain their documented roles. See [connection flags and
compatibility](./DIMSE_CONNECTION_FLAGS.md) for the command list, IPv6,
timeout semantics and configuration restrictions.

Four requestor commands expose the library's normalized services, after the
dcm4che tools of the same names (behavior reference only; the code follows
the DICOM standard). Each opens one association to `-host`/`-port` with
`-called`/`-calling` AE titles, bounds the exchange with `-timeout`, and takes
the TLS (`-tls-profile`, `-cert`, `-key`, `-ca`, `-require-peer-cert`,
`-tls-server-name`) and User Identity flags of `storescu` and `findscu`. A
password or token is read only from `-user-identity-secret-file` or the
variable named by `-user-identity-secret-env`, never from an argument, and
never appears in output or diagnostics.

```sh
go run ./cmd/mppsscu -h
go run ./cmd/stgcmtscu -h
go run ./cmd/upsscu -h
go run ./cmd/ianscu -h
```

Every response prints one line, `<operation> status=0xHHHH (<class>)`, with
the Error ID, Error Comment and attribute tags the peer returned. A warning
status exits with 0, a failure status or a transport error with 1, usage
errors with 2 and an interrupt with 130. Instance inputs are files or
directories; files found in a directory that are not DICOM instances are
counted and skipped without naming them, and only metadata is read (no Pixel
Data). Data set inputs (`-attributes`) are DICOM JSON (`.json`, or a
one-object array) or Part 10 files of at most 16 MiB.

## storescp: receive storage instances

```sh
storescp -address 127.0.0.1:11112 -output received
storescp -prefer-jls -output received
storescp -accept-all -prefer-transfer-syntaxes 1.2.840.10008.1.2.5,1.2.840.10008.1.2.4.80 -output received
storescp -transfer-syntaxes 1.2.840.10008.1.2.4.80,1.2.840.10008.1.2.5 -bit-preserving -output received
```

The default keeps the existing storage preference order (Implicit VR Little
Endian, Explicit VR Little Endian, Deflated Explicit VR Little Endian), then
accepts JPEG Baseline, JPEG Extended, JPEG Lossless Process 14, JPEG Lossless
SV1, JPEG-LS lossless/near-lossless, JPEG 2000 lossless/general, and RLE.
A peer proposing only one of these compressed syntaxes can store it. The
receiver preserves the negotiated Transfer Syntax and compressed Pixel Data
without decoding or transcoding frames; acceptance does not require a pixel
codec or promise that the receiver can render that image.

`-accept-all` expands storage acceptance to all known transfer syntaxes whose
DICOM dataset envelope the library can read/write (`transfer.Syntax.Supported`).
This includes optional-codec/retired JPEG and video envelopes; known MIME,
XML and ST2110 encodings whose envelopes are unsupported stay refused, as do
unknown transfer syntax UIDs. The default native preference remains first.

`-prefer-lossless` prefers JPEG Lossless SV1 then Process 14;
`-prefer-jpeg8` prefers Baseline; `-prefer-jpeg12` prefers Extended;
`-prefer-jls` prefers JPEG-LS lossless then near-lossless;
`-prefer-j2k` prefers JPEG 2000 lossless then general;
`-prefer-rle` prefers RLE; `-prefer-hevc` prefers HEVC Main then Main 10.
The selected family is accepted even when outside the default list (HEVC).
Choose one preset, or `-prefer-transfer-syntaxes` for an ordered comma-separated
list of readable UIDs; the preferred syntaxes come before the remaining list.

`-transfer-syntaxes` restricts accepted storage syntaxes and sets their exact
preference order. It cannot be combined with `-accept-all`. A preset preference
selects its permitted members from the restricted list, and fails if none is
permitted. Explicit UID preferences must all be in the restricted list. Empty,
duplicate, unknown, unreadable or contradictory options fail before listening.
Order is the receiver's order, regardless of the order proposed by the peer.
Verification has its own rule using the three native syntaxes and does not
inherit compressed storage preferences or restrictions.

Known Storage SOP Classes are accepted by default. `-promiscuous` explicitly
allows unknown SOP Classes too, while retaining C-STORE command/context/dataset
SOP identity agreement and valid UID checks. It does not relax resource,
association, TLS or persistence controls. The command still handles only
C-STORE and C-ECHO. `-bit-preserving` retains the received dataset bytes after
an authored File Meta header; both storage routes validate identity and limits
before publication.

Shared `-read-*` recovery flags apply to both parsed and bit-preserving
reception and coexist with the exact association-profile rules from
`-config-file`/`-profile`. They do not broaden negotiation or bypass identity,
PDV framing, admission or persistence limits. Parsed storage writes the
admitted repaired encoding. `-bit-preserving` validates with the configured
recovery policy and stores the original received bytes; such a file can
still require that read policy later. Both routes refuse incomplete reports.
Existing failure response/drain/peer-close behavior is retained.

Opt-in study directories, relative filename templates and bounded storage or
end-of-study event hooks are described in
[storage layout and hooks](./STORESCP_LAYOUT_AND_HOOKS.md). Events follow
durable instance publication, and inactivity is tracked per Called AE and
Study Instance UID. Hook attempts are process-local FIFO operations; a timeout
or command failure is reported without undoing an accepted C-STORE.

## mppsscu: Modality Performed Procedure Step

```sh
mppsscu create -called RIS -retrieve-ae ARCHIVE -complete -protocol "CT HEAD" ./study
mppsscu create -called RIS -attributes create.json -uid 1.2.3.4
mppsscu complete -called RIS -uid 1.2.3.4 ./study
mppsscu discontinue -called RIS -uid 1.2.3.4 -reason "110514^DCM^Incorrect worklist entry selected" ./study
mppsscu set -called RIS -uid 1.2.3.4 -attributes modifications.json
```

`create` sends N-CREATE (IN PROGRESS) with `-attributes`, or with an
unscheduled-step Attribute List built from the instances: patient attributes
of the first instance (all instances must share its Patient ID), one
Scheduled Step Attributes item per study with its Accession Number and empty
request identifiers, Modality from the first instance (or `-modality`),
Performed Station AE Title `-station-ae` (default `-calling`), start
`-start` (default now) and Performed Procedure Step ID `-pps-id` (default
`YYMMDDHHMMSS` of the start). Without `-uid` the SCP assigns the SOP Instance
UID, which is printed. `-complete` also sends the COMPLETED N-SET on the same
association. `complete` and `discontinue` send N-SET with End Date and Time
(`-end`, default now) and one Performed Series item per series: Referenced
Image Sequence for instances with Rows, Referenced Non-Image Composite SOP
Instance Sequence otherwise, Series Description from the instances,
`-retrieve-ae`, and Protocol Name (Type 1) from the instances or
`-protocol`. The Attribute List is checked with `mpps.ValidateCreate` before
connecting.

## stgcmtscu: Storage Commitment

```sh
stgcmtscu -called ARCHIVE -wait 2m ./study
stgcmtscu -called ARCHIVE -calling MODALITY -listen 0.0.0.0:11115 ./study
stgcmtscu -called ARCHIVE -wait 0 -transaction-uid 2.25.1234 ./study
```

Sends one Storage Commitment Push Model N-ACTION referencing every instance
(at most 10,000) with `-transaction-uid` or a new 2.25 UID, then keeps the
association open for at most `-wait` (default one minute) to receive the
N-EVENT-REPORT on it. With `-listen` the tool also accepts the report on a
separate association that the committer opens to that address with
`-calling` as the called AE title and the SCP role negotiated; the rest of
`-wait` is spent waiting there once the requesting association is released.
The callback listener is plain TCP and cannot be combined with
`-tls-profile`. `-wait 0` sends the N-ACTION only. The result prints the
committed and failed counts and one `FAILED` line per instance with its
Failure Reason; the exit status is 0 only when every instance was committed,
and no result within `-wait` is a failure.

## upsscu: Unified Procedure Step

```sh
upsscu create -called UPS -label "CT RECON" -worklist RECON -priority HIGH
upsscu find -called UPS -k 'ProcedureStepLabel=CT*' -k SOPInstanceUID= -k ProcedureStepState=
upsscu state -called UPS -uid 1.2.3.4 -state "IN PROGRESS"
upsscu set -called UPS -uid 1.2.3.4 -transaction-uid 2.25.99 -attributes progress.json
upsscu get -called UPS -uid 1.2.3.4 -tag ProcedureStepState
upsscu subscribe -called UPS -uid global -receiver WATCHER -deletion-lock
upsscu subscribe -called UPS -uid filtered -receiver WATCHER -k WorklistLabel=RECON
```

| Action | Request | SOP Class proposed |
| --- | --- | --- |
| `create` | N-CREATE, `-attributes` or a SCHEDULED workitem from `-label`, `-priority`, `-worklist`, `-start`, `-readiness`; `-uid` defaults to a new UID | Push |
| `get` | N-GET of `-uid`, `-tag` keywords or `(gggg,eeee)` (default all) | Push, Pull, Watch |
| `set` | N-SET of `-attributes`; `-transaction-uid` adds the Transaction UID | Pull |
| `state` | N-ACTION Change State to `-state`; IN PROGRESS generates and prints a Transaction UID when none is given | Pull |
| `cancel` | N-ACTION Request Cancel, optional `-attributes` | Push, Watch |
| `subscribe` | N-ACTION Subscribe to `-uid`, `global` or `filtered` (with `-k` keys), optional `-deletion-lock` | Watch |
| `unsubscribe` | N-ACTION Unsubscribe from `-uid` or `global` | Watch |
| `suspend` | N-ACTION Suspend Global Subscription | Watch |
| `find` | C-FIND with `-k Keyword=value` keys (an empty value returns the attribute; `\` separates values) | Query, Watch, Pull |

The first accepted SOP Class of the list is used. `-receiver` defaults to
`-calling`. N-GET results and C-FIND matches are printed in `dcmdump` form.
The tool sends requests only; it does not receive the N-EVENT-REPORTs a
subscription causes.

## ianscu: Instance Availability Notification

```sh
ianscu -called RIS -retrieve-ae ARCHIVE -pps-uid 1.2.3.4 ./study
```

Sends one N-CREATE of the Instance Availability Notification SOP Class per
study (PS3.4 R, PS3.3 C.4.23): Study Instance UID, a Referenced Performed
Procedure Step Sequence (Type 2, holding `-pps-uid` of class `-pps-class`,
MPPS by default, or empty), and one Referenced Series Sequence item per
series whose Referenced SOP Sequence items carry Instance Availability
(`-availability`, default ONLINE) and Retrieve AE Title (`-retrieve-ae`,
default `-calling`). Each notification has a new SOP Instance UID, or `-uid`
when the instances belong to one study. The data sets are built, validated
(PS3.4 Table R.3-1) and sent by package `ian`
([`INSTANCE_AVAILABILITY_NOTIFICATION.md`](INSTANCE_AVAILABILITY_NOTIFICATION.md)),
whose `ian.Service` is the matching SCP.

# Query/Retrieve SCP


```sh
go run ./cmd/qrscp -h
```

```sh
qrscp -ae QRSCP -listen 127.0.0.1:11112 -move-destination VIEWER=127.0.0.1:11113 ./fixtures
findscu -port 11112 -called QRSCP -level study -k 'PatientID=SYNTH*' -k StudyInstanceUID=
dicom-go-retrieve -method get -remote 127.0.0.1:11112 -called-aet QRSCP -output ./out -level STUDY -study-uid 1.2.3
```

C-GET proposes the default Storage subset with Implicit and Explicit VR Little
Endian in one context per class, and a peer accepts one syntax per context. A
peer that does not transcode (for example the dcm4che `dcmqrscp`) then fails
with `0xA702` for objects stored in the other syntax. Repeat
`-storage-sop-class <UID>` (at most 63 classes) to propose only those classes
with one context per native syntax.

At startup the folders (or files) are indexed once with `index.Scan`: every
file whose metadata yields SOP Class, SOP Instance, Study and Series Instance
UIDs is served; other files are counted as skipped and the first file of a
repeated SOP Instance UID wins. Pixel Data is not read until a retrieve. The
startup line reports instances, studies, skipped files and duplicates, never
paths or patient values. `-max-instances` (100,000) bounds the index.

Services, all on one AE title (`-ae`, `QRSCP`; the called AE title must match
unless `-require-called-ae=false`):

- Verification.
- Patient Root and Study Root C-FIND at every level of the model. Keys are
  matched with `qrmatch` (single value, list of UIDs, wild card, range, with
  `YYYY` and `YYYYMM` dates accepted) against the attributes the catalog
  holds: patient (Patient's Name, Patient ID, Issuer of Patient ID, birth
  date, sex, related study/series/instance counts), study (date, time,
  Accession Number, Study ID, description, Referring Physician's Name,
  Modalities and SOP Classes in Study, related series/instance counts),
  series (Modality, number, date, time, description, Institution Name,
  related instance count) and instance (SOP Class, Instance Number, Number
  of Frames, Rows, Columns). Keys of higher levels are matched like any other
  key, so hierarchical and relational identifiers both work; a key the
  catalog does not hold does not filter and is returned empty. Retrieve AE
  Title is returned as `-ae` and Instance Availability as ONLINE.
- Patient Root and Study Root C-GET: one C-STORE sub-operation per matched
  instance on the requesting association, which must negotiate the SCP role
  for the instances' Storage SOP Classes. Every standard Storage SOP Class
  and every class in the folders is accepted with both roles. Each file is
  read again and checked against its indexed SOP Class and Instance UID; it
  is offered in its stored transfer syntax (native little endian files also
  in the other native syntax), never transcoded.
- Patient Root and Study Root C-MOVE to the destinations of
  `-move-destination AE=host:port` only (repeatable); any other Move
  Destination fails with A801. One `StoreSession` per request sends the
  instances with `-ae` as calling AE title and the requestor as Move
  Originator, over plain TCP.

A retrieve needs a value for the Unique Key of its level (else A900). C-FIND
returns at most `-max-results` matches (10,000; beyond that A700). TLS for
incoming associations uses the `storescp` flags (`-tls-profile`, `-cert`,
`-key`, `-ca`, `-require-peer-cert`). `-max-associations` bounds concurrent
associations; the server stops on interrupt.

Opt-in interoperability tests drive it with pynetdicom
(`DICOMGO_PYNETDICOM_INTEGRATION=1`, `TestPynetdicomRequestorAgainstQRSCP`)
and with DCMTK `findscu`, `getscu` and `movescu`
(`DICOMGO_DCMTK_INTEGRATION=1`, `TestDCMTKRequestorsAgainstQRSCP`).

## Thin DICOM and HL7 tools

The default build includes the following commands. Each supports `-h`; commands reuse the library operations and expose finite input, output and network lifecycle limits. See [Small DICOM command tools](CLI_WRAPPERS.md) and [HL7 command tools](HL7_CLI.md) for examples, defaults, refusal profiles and qualification limits.

| Family | Commands |
|---|---|
| Worklist, MPPS and IAN servers | `wlmscp`, `mppsscp`, `ianscp` |
| Encapsulated documents | `pdf2dcm`, `dcm2pdf`, `cda2dcm`, `stl2dcm`, `dcmencap`, `dcmdecap` |
| Structured reports and key objects | `dsr2html`, `dsrdump`, `mkkos` |
| Grayscale presentation | `dcmpsmk`, `dcmp2pgm` |
| Basic Print Management client | `printscu` |
| Templates, Part10 marker and private shutdown negotiation | `dcm2str`, `dcmftest`, `termscu` |
| HL7 v2 ER7 / MLLP and structural XML | `hl7snd`, `hl7rcv`, `hl72xml`, `xml2hl7` |

`dcmftest` checks only the `DICM` marker. `termscu` reports private-context negotiation without claiming that the peer shut down. HL7 XML is the library's structural version-1 format, not XML ITS. The MWL complete-core profile and OS lock qualifications are explicitly scoped in the guide.

## img2dcm

`img2dcm [flags] input [input ...] output.dcm` imports opaque PNG/BMP/PNM/PGX or explicitly described raw samples into SC/Multi-frame SC, or one 8-bit VL Photographic image with `-vlp`. It encapsulates supported JPEG/JLS/J2K without recoding, and a bounded AVC MP4/MPEG-2 Program Stream profile as Video Photographic. Video needs an explicit anatomic code and reports encapsulation without codec decode.

`-series-from` inherits series identity with a new SOP instance; `-study-from` inherits patient/study. Input/dimension/frame/native-span limits are finite. Atomic publication reuses the shared Part 10 helper and refuses aliases of image/source inputs. Full profiles, J2K lossless declaration, video exclusions and limits: [IMG2DCM.md](IMG2DCM.md).

### MOVE and FIND query input

`movescu` accepts MOVE-only requests through the same runner as `dicom-go-retrieve`. Explicit `-listen-port` enables its embedded native-syntax Storage SCP; `-port` remains the remote SCP port. The remote archive must map `-move-destination` to that listener. Prefixed listener TLS/AE flags are independent of requestor options.

Both retrieve and `findscu` accept repeated `-k key[=value]`, `-query-file`, raw/Part10/JSON query files and trailing files. `findscu -worklist` selects MWL, `-cancel-after N` cancels after delivered matches and drains the terminal response, and `-extract -extract-dir` publishes raw ExplicitLE Identifiers with ordinal names. Existing C-GET/external-receiver MOVE and print-only `-max-results` remain available.

See [MOVE reception and free FIND/retrieve queries](CLI_MOVE_FIND.md) for admission limits, matching/UID binding, cancellation, extraction format, atomic publication and the qualified offline DCMTK cases.

### Writable qrscp archive


Writable mode accepts C-STORE only after durable Part10 publication and
persistent metadata admission, preserving File Meta AE provenance. Duplicate
UIDs return A900 without overwrite. Instance/study count and complete Part10
byte quotas refuse with A700 before image publication; index byte/record quotas
can refuse after publication and then preserve the image and make the archive
unavailable. Startup verifies metadata and SHA-256; canonical orphan files
require explicit `-repair-index`. `-storage-commitment` uses allowlisted callback
peers and the existing durable workflow; `-stgcmt-same-assoc` requires a dedicated
association with exactly one accepted STGCMT context. `-cfind-error 0xC123` and
`-delay-cstore DURATION` supply bounded synthetic failure/delay profiles.


### HL7 transport release selection

Both `hl7snd` and `hl7rcv` accept `-mllp-release 1` (default) or `2`, plus
`-transport-ack-timeout` (default 30s, positive and at most five minutes).
Release 2 must match the peer's endpoint configuration. A transport ACK followed
by application AE/AR still fails the sender; NAK, malformed confirmation and
unknown request outcome have distinct value-free diagnostics and never cause
automatic replay. Receiver transport ACK precedes file publication; its existing
application AA/AE and cumulative output quota semantics remain separate.
The commands expose plain TCP; TLS remains available through library options.
See [HL7 command profiles](HL7_CLI.md).
