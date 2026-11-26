# Incomplete Pixel Data: explicit complete-frame inspection

`AllowIncompletePixelData` in `object.ReadFileOptions` and
`parser.ReaderOptions` inspects final root integer Pixel Data interrupted at
physical EOF. Ordinary reads remain strict. This is a malformed-input profile,
not a claim that the original DICOM instance is complete or conformant.

Reading and decoding require separate explicit choices. An inspected File is
returned **with an error**, recognizable as `object.ErrPartialRead`,
`object.ErrDataSet`, `object.ErrIncompletePixelData` and the original parser
cause. `File.PartialRead()` marks its Pixel Data root entry incomplete.
`Object.IncompletePixelData()` returns historical structural counts and offsets.
`File.IncompletePixelData()` binds that report to the File's own opt-in read;
replacing its Dataset cannot import a certificate into a generic partial File.
Raw dataset readers return the inspected Object and the same partial error.
`Reader.ReadDataSet` and `ReadAll` return retained elements plus
`*parser.IncompletePixelDataError`. Token users must check the Reader accessor;
EOF after the retained token never means the source instance was complete.

The read profile retains actual native bytes, including a partial last frame
only for raw inspection. For encapsulated input it retains a complete BOT Item
and complete fragment Items, dropping a terminal interrupted Item's present
payload. A retained Item is not proof of a complete image frame.

## Complete images require explicit normalization

`pixeldata.DecompressFileContext` / `DecompressDataSetContext` with
`DecodeRecovery.IncompleteData` separately verify a contiguous prefix of complete
frames. Native frames require the full byte-aligned integer frame extent.
JPEG/JPEG-LS frames require complete codestream endings and successful strict
codec decoding with exact native frame size. An EOI inside APP, COM or other
length-delimited payload is not an ending. No missing sample, row, frame, entropy
or EOI is supplied. `ErrNoCompleteFrames` refuses a source with no complete frame.

The result has a new native dataset structure, independently owned Pixel Data
bytes and Number of Frames equal to the retained complete count. Other materialized
metadata values retain the ordinary decompressor's borrowed backing semantics;
this is not a deep clone of every core value. The input metadata, bytes and
fragments stay unchanged. `DecompressReport.Incomplete` and
`DiagnosticIncompletePixelData` are published only on success. The report keeps
original declared/available byte counts, input declared/retained frames, parser reason,
value offset and physically discarded bytes/Items; it does not count missing
bytes as if they had been present. Native `DeclaredBytes` is the original VL;
encapsulated undefined VL is `-1`. Available encapsulated bytes count BOT payload
plus retained fragment payloads, excluding Item headers. Dropped partial-header
bytes and incomplete fragment payloads are recorded separately in the parser
report. The successful decode report combines those discarded bytes with any
complete Items belonging to a dropped terminal codestream.

Normal frame extraction, default decompression/decode, FrameReader, validation,
clone and transcode refuse diagnosed incomplete inputs. Writers always refuse
them, **even with `WriteFileOptions.AllowPartial`**. Padding an odd raw tail could
otherwise invent the missing half of a sample and erase its history on reread.
The File's historical incomplete-read guard persists even if its Dataset is
replaced by `FromElements`, which does not carry Object certification.
`WriteFileFrames` also refuses diagnosed File/Object input before consulting
FrameSource or writing bytes; normalize first. Generic partial rewriting with
new explicit frame payloads retains its ordinary contract.
Normalize the complete prefix before any rewrite. Generic partial-SQ rewriting
under `AllowPartial` retains its existing contract.

The private historical report is set by source-reading APIs; constructing a new
Object from short raw core values does not authorize incomplete-data decoding.
Actual payload size, headers and framing are rechecked, so stale count/length
changes cannot bypass admission. Objects and their borrowed values must remain
stable while used; the historical report does not authenticate user edits or
same-sized payload substitutions or metadata layout changes with the same extent.
`DeclaredFrames` describes input metadata when normalization is called; it does
not authenticate the original metadata of the parse. Raw `CopyValueTo` may export actual present
bytes for inspection; that is not a complete DICOM image.

`DetectableDecodeRecovery` does not enable this option: choosing a frame subset
is an explicit content decision. This primitive preserves the existing SOP/Series
identity; it creates no UID, clinical approval, archive policy or UI adoption.
Applications must decide publication and any required derived identity/provenance
before presenting or storing the result as a new instance. Synthetic headless 2D
checks exercise the existing display/render pipeline after normalization; short
frame checks in display/frame/render remain unchanged. Twin's GPU-only 3D policy
is unrelated and unchanged.

## Qualified profile and explicit refusals

| Input | Contract |
|---|---|
| Explicit/Implicit VR Little Endian and Explicit VR Big Endian integer native Pixel Data | Retain actual EOF bytes; decode whole byte-aligned frames only. A missing final even-VL pad can leave all declared frames complete. |
| JPEG Baseline, Extended, Lossless and JPEG-LS still-image transfer syntaxes | Retain complete Items; independently verify codestream endings across Items before ordinary codec decoding. Codec availability and its existing subset still apply. |
| Encapsulated EOF without delimiter, 1–7 bytes of a cut Item/delimiter header, or a full Item header with interrupted payload | Recover only at physical EOF, after a complete BOT and at least one complete fragment. |
| Exactly eight garbage terminal bytes | Require a sized stable ReaderAt, an available dictionary identifying an unknown public tag, invalid VR and no following byte. Unsized streams cannot authorize this case. |
| Populated BOT | Validate original frame count, ordered Item-aligned starts and independently scanned prefix starts. Conflicts or references outside the retained source refuse. |
| EOT | Incomplete framing recovery refuses this initial profile, including empty-but-present EOT attributes. |
| RLE, JPEG 2000/HTJ2K, Encapsulated Uncompressed and video | Incomplete framing recovery refuses; ordinary strict support is unchanged. |
| Packed 1-bit/12-bit native samples; Float/Double Float Pixel Data; nested interruption; deflated or legacy syntax | No recovery in this profile. |
| Changed frame count with per-frame functional groups, concatenation, dimensions, frame increment/time vectors or frame overlay references | Refuse unsupported frame-dependent metadata rather than silently shortening it. |

Middle corruption, malformed delimiters, undefined Item lengths, known/private
or otherwise valid later elements, a cut BOT, an extra complete frame, invalid
codestream structure and table conflicts remain errors. An interrupted final
codestream may be dropped; this does not authorize malformed earlier codestreams.
No search for later DICOM tags is performed.

The read option refuses selective/prefix reading, validation lifecycle, sinks,
Skip/DeferPixelData, inline bulk deferral, generic `AllowPartialRead`, unlimited
negative depth, and other grammar-repair switches. The decode option refuses
combination with metadata/BOT/JP2/defined-length/native-under-encapsulated repairs.
Separately selected codec quirks remain explicit and diagnosed. Independent File
Meta group reading can still be requested through the existing
`AllowMissingMetaElementGroupLength` option; incomplete-data mode does not turn
it on.

## Finite work and ownership

Zero read limits in this mode use Pixel Data 128 MiB, other elements 64 MiB,
logical total extent 512 MiB, 1,000,000 elements, 100,000 fragments and ordinary
combined depth 64. Pixel Data and fragment counts have hard recovery ceilings of
128 MiB/100,000 even if a higher numeric limit was supplied. Lower limits remain
effective. BOT and fragments are cumulatively admitted before payload reads;
large declared VLs are consumed in at most 32 KiB read requests, growing payload
storage only from actual bytes. A cut Item's buffer is discarded. No complete source reference
is fabricated for recovered Pixel Data.

Original declared metadata is admitted against DecompressLimits before reducing
Number of Frames. MaxFrames, MaxPixels, MaxNativeBytes, MaxInputBytes and compressed
expansion ratio therefore cannot be bypassed by selecting a smaller prefix.
Marker planning also checks frames/fragments/input and per-frame byte quotas.

Arbitrary I/O, context cancellation/deadline, byte/work limits and container-fence
failures never become recovered output. Original non-EOF reader failures remain
errors even when a read supplies the full requested byte count or later reaches
EOF; an early EOF before a known source extent also refuses recovery. Public
error sentinels alone never authorize a private incomplete-data certificate. A supplied context-aware Reader can
propagate cancellation; this API does not interrupt a blocking OS Read. Decode
and framing use cooperative checks, including before output/report publication.
No new goroutines, shared scratch or registry are introduced. Ordinary deferred
metadata still obeys source ownership and Close during inspection; recovered
Pixel Data is always materialized. Complete-prefix normalization refuses any
deferred metadata value, including nested items, because independent output
cannot inherit source replay. Read with materialized metadata before normalizing.
Normalized output owns its native bytes.

## Evidence and reference limits

Synthetic tests cover three native syntaxes, exact unsigned/signed samples,
partial sample/row/frame boundaries, zero complete frames, missing final pad,
seekable/short streams, partial Item/delimiter headers, opaque embedded markers,
JPEG/JPEG-LS split-Item framing, terminal incomplete codestreams, metadata/table
conflicts, stale lengths, limits, cancellation, write refusal and complete-prefix
round trips. Existing parser/partial-sequence and consumer suites remain separate
gates. These are functional/structural controls, not clinical, PACS or performance
qualification; no benchmark was needed for this correctness change.

The optional `TestIncompletePixelDataGDCMCompleteFrames` uses
`DICOMGO_GDCMDATA_ROOT` without vendoring third-party files. Two available pinned
GDCM fixtures reproduce complete native payload MD5s:

| Fixture | Native payload MD5 | Additional explicit prerequisite |
|---|---|---|
| JPEGInvalidSecondFrag.dcm | `f111ed4eea73f535261039d3f7b112e9` | Existing group-based File Meta reading; discard terminal Item with no payload. |
| gdcm-JPEG-LossLess3a.dcm | `ad67e448e8923c34f1e019c3395e616c` | Existing group-based File Meta reading; discard exactly eight unknown terminal garbage bytes. |

Both files have incorrect File Meta Group Length, so incomplete-data mode alone
refuses them before Pixel Data. Source SHA checks and post-read hashes keep
external functional comparisons tied to unchanged inputs. The three historical
SIEMENS/PMS/GEMS native IncompletePixelData files were unavailable in the local
reference inventory. Their historical GDCM MD5 records do not prove any complete
frame exists; those original acceptance items remain unqualified. Files requiring
GDCM-style zero-fill of a partial frame would deliberately diverge from this
complete-frame profile and must not be reported as passed.


The external gate now attempts actual native recovery when each original is
present, checks complete-frame and discarded-byte accounting against the input,
requires the recovery diagnostic, and renders every retained frame through the
headless 2D pipeline with a full stored-range window. The same display/diagnostic
checks apply to the two compressed fixtures. No-frame inputs and display errors
fail; file presence alone cannot pass. Native complete-prefix bytes are compared
with these historical upstream pixel expectations:

| Original native fixture | Historical GDCM payload MD5 |
|---|---|
| SIEMENS-IncompletePixelData.dcm | `dc549f804f1f59bfee245ddf100ee26b` |
| PMS-IncompletePixelData.dcm | `92655f77a55eb9c316223b9e73d55800` |
| GEMS-IncompletePixelData.dcm | `098bba76d4e75a3148c9b62aee35f950` |

These records were under the upstream `No tested:` comment; a mismatch remains
a failing acceptance result, including if GDCM's expectation needs zero-fill.
The gate logs the observed native source SHA256 as **unpinned**, and checks that
the file is unchanged after processing. It does not establish the absent source
identity, provenance, permission to redistribute, or a current independent GDCM
execution. Those checks remain necessary before qualifying newly obtained
originals. Missing files remain explicit skips, not functional passes.

`TestIncompleteNativeAcceptanceSyntheticContract` exercises that same acceptance
path with clearly identified generated inputs in all three native syntaxes. It
checks independent expected little-endian samples and displayed gray values,
source preservation, and refusal of missing diagnostics or contradictory frame
counts. `TestIncompleteNativeAcceptanceSyntheticNoFrame` checks that a partial
frame cannot become an accepted image. These tests do not stand in for the three
historical files or their hashes.

Implementation authority is DICOM grammar and independent sample/framing checks:
[PS3.5 native Pixel Data and concatenation](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/chapter_8.html)
and [PS3.5 encapsulated Item framing](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.4.html).
GDCM is a behavioral and payload oracle reference; its source was not copied.
