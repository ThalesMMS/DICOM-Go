# Siemens CSA embedded datasets

The `siemens` package provides explicit, on-demand decoding of base64 datasets
in these CSA fields: `MRDiffusion`, `MR_ASL`,
`FmriAcquisitionDescriptionSequence`, `FmriConditionsDataSequence`, and
`FmriResultSequence`. Other strings are never guessed to be datasets.
`ParseCSA`, `ImageHeader`, `SeriesHeader`, and `mrdiffusion.Read` retain their
existing behavior; they do not activate this decoding automatically.

```go
header, err := siemens.ImageHeader(dataset, siemens.LookupOptions{})
if err != nil { return err }
embedded, err := header.EmbeddedDatasetContext(ctx, "MRDiffusion",
    siemens.EmbeddedDatasetLimits{MaxDecodedBytes: 1 << 20})
if err != nil { return err }
defer embedded.Close()
// embedded is a detached *object.Object containing the raw EVRLE dataset.
```

`EmbeddedDatasetsContext` returns all known fields in CSA element/value order.
Each entry carries `Name`, `ElementIndex`, `ValueIndex`, and `Dataset`.
`ValueIndex` indexes `CSAElement.Values`; the existing CSA parser removes empty
declared items, so it is not an original item-number mapping. Empty values are
omitted. The singular API rejects duplicate named fields and multiple nonempty
values with `ErrAmbiguousEmbeddedDataset`. The plural API exposes them with
explicit indices. Missing/empty selections return `ErrNoEmbeddedDataset`.

Only standard padded base64 is accepted, with a declared CR/LF line-break
exception. Spaces, tabs, URL-safe alphabet, misplaced padding, nonzero trailing
padding bits, and content after padding are rejected. See
[RFC 4648 Sections 3–4](https://www.rfc-editor.org/rfc/rfc4648.html#section-3).
Decoded bytes are parsed as a raw
[Explicit VR Little Endian dataset (PS3.5 A.2)](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.2.html),
without Part 10 detection, decompression, codecs, recovery, or recursive base64
decoding. A normal Part 10 preamble fails the raw parser; `DICM` inside a valid
dataset value is ordinary content, not a file-detection heuristic. Incomplete
headers/values, nonzero reserved bytes, and empty datasets fail explicitly.
Decoding does not assert IOD conformity,
source authenticity, clinical validity, or semantic interpretation of ASL/fMRI.

## Bounds and ownership

`EmbeddedDatasetLimits` applies these finite defaults:

| Limit | Default |
| --- | ---: |
| Encoded bytes per value, including CR/LF | 8 MiB |
| Decoded bytes per dataset | 6 MiB |
| Encoded bytes over the selection | 24 MiB |
| Decoded bytes over the selection | 16 MiB |
| Datasets; also `Values` entries per selected field | 64 |
| CSA header elements examined | 4096 |
| DICOM element/item nodes per dataset | 65536 |
| Combined sequence/item nesting depth | 32 |

Zero selects the corresponding default; negative limits are rejected. The
complete selection is admitted before allocating decoded buffers. Encoded text
is read without an integral byte-slice copy; each decoded buffer has the exact
admitted length. The parser materializes its values into the returned object,
so parser objects/value allocations coexist temporarily with that buffer.
These are logical work/allocation bounds, not a measured RSS ceiling.

Lifecycle node admission includes empty sequence items, which the ordinary
primitive-element counter alone would not count. The raw input reader cannot
supply bytes past the decoded extent; its parser byte ceiling permits one
additional byte solely to observe EOF at that exact extent. Cancellation is
checked during selection, base64 scanning/decoding, and dataset parsing. A
failed/canceled batch returns no partial list and closes prior objects. Inputs
must remain immutable during an operation; results do not alias CSA input or
another call's mutable output. Callers own successful results and should close
each dataset when finished.

`EmbeddedDatasetError` exposes bounded element/value indices and a fixed stage.
Its `Error()` excludes names, encoded content, and DICOM values. `errors.Is`
distinguishes invalid data, resource limits, ambiguity, absence and cancellation;
parser causes remain inspectable via `errors.Is`/`errors.As`.

## Diffusion integration

```go
frame, err := mrdiffusion.ReadSiemensEmbeddedContext(ctx, dataset, 0,
    mrdiffusion.SiemensEmbeddedOptions{
        Lookup: siemens.LookupOptions{},
        Limits: siemens.EmbeddedDatasetLimits{MaxDecodedBytes: 1 << 20},
    })
if err != nil { return err }
// frame.BValue, frame.Direction, frame.HasDirection, frame.DirectionFrame
```

This helper reads the CSA **image** header's `MRDiffusion` field. It accepts
standard attributes at the embedded root, a single root MR Diffusion Sequence
item, or a macro in Shared/Per-Frame Functional Groups. Conflicting groups,
multi-item macros, and multi-item gradient sequences are rejected. Numeric
finite/nonnegative checks reuse the existing diffusion reader; semantic error
messages omit the invalid text while preserving inspectable causes.
It reports `SourceSiemensCSAEmbedded`; `Tag` locates
the outer CSA header and `FrameIndex` is the requested index. The standard macro
defines b-value and patient-relative gradient orientation in
[PS3.3 C.8.13.5.9](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.13.5.9.html).
Both linked NEMA pages identified their edition as 2026d when consulted for
this implementation; the URLs are the publisher's current-edition locations.


```sh
go test ./siemens ./mrdiffusion
go test -race ./siemens ./mrdiffusion
```
