# UN values and opt-in recovery

This page describes how dicom-go reads, writes and, on request, recovers
elements encoded with the Unknown VR (UN). Normative reference: PS3.5 2026d,
section 6.2.2.

## Default: preserve

Reading never changes a UN element.

- A defined-length UN value stays opaque bytes, exactly as received. Its bytes
  are Implicit VR Little Endian by definition, so they are never reversed,
  even in an Explicit VR Big Endian file or when a data set is transcoded.
- An undefined-length UN value is read as a sequence whose items, elements and
  delimiters are Implicit VR Little Endian. The element keeps VR UN. In a Big
  Endian data set the item values are converted to the data set's byte order,
  so they read like every other value of the object and transcode correctly.
- Writers reproduce UN elements. In an Explicit VR syntax an undefined-length
  UN sequence is written with undefined length and Implicit VR Little Endian
  items (values reversed back when the data set is Big Endian), so a read and
  write round trip of such a file is byte-exact in Little and Big Endian.

Before this, an Explicit VR writer refused undefined-length UN sequences, and
Big Endian item values were left little-endian inside a Big Endian object;
both are fixed.

## Opt-in: `Object.RecoverUN`

`(*Object).RecoverUN(ctx, UNRecoveryOptions)` returns a copy in which UN
elements with a known, unambiguous VR carry that VR. The receiver is not
modified, so writing it still reproduces the received bytes; writing the copy
is the explicit output transformation.

| UN element | Result |
|---|---|
| Standard attribute with one dictionary VR | That VR. Binary values converted from little-endian to the object's byte order; the length must be a whole number of values. |
| "US or SS", "OB or OW", LUT Data | Resolved only from the normative context (Pixel Representation of the data set or an ancestor, Bits Allocated, LUT Descriptor). Otherwise kept UN. |
| Defined-length value of an SQ attribute | Parsed as Implicit VR Little Endian items, isolated from the next element, with the element, element-byte and depth limits. Invalid or truncated payloads are kept UN. |
| Undefined-length value of an SQ attribute | Relabelled SQ; its items were already parsed. |
| Undefined-length value of a non-SQ attribute | Kept UN. |
| Private Creator (gggg,0010-00FF) | LO, with the diagnostic `private-creator-encoded-as-UN`: it is not a conforming encoding. |
| Private attribute | Only through an injected `PrivateDataDictionary` and a valid, unique creator of the same data set. Items never inherit reservations. |
| Unknown attribute, dictionary VR UN, group 0002 | Kept UN. File Meta Information is a separate object and is never recovered. |
| Deferred (unmaterialized) value | Kept UN; the copy still reads it through the receiver's source, which must stay open. |

A short-VR value longer than 65,535 bytes (encoded as UN because an Explicit
VR 16-bit length cannot hold it) is recovered with the diagnostic
`recovered-value-exceeds-16-bit-explicit-length`; Explicit VR writers then
refuse it explicitly instead of truncating, and Implicit VR writers accept it.

Diagnostics (`UNRecoveryReport`) carry the structural path, for example
`(0008,1140)[1].(0029,1001)`, the resolved VR (UN when kept) and a reason from
a fixed vocabulary; never values. `Strict` returns `*UNRecoveryError`
(`ErrUNRecovery`) instead of keeping UN when a VR is known but cannot be
applied safely: ambiguous or missing context, bad length, invalid sequence
payload, deferred value, unresolved creator or a limit. Unknown attributes are
never an error. `ctx` is checked for every element. Limits default to
`DefaultUNRecoveryOptions` (64 MiB per element, 100,000 parsed elements, depth
64, 4,096 creators per data set, 256 diagnostics).

## Transcoding

`pixeldata.TranscodeOptions.RecoverUN` (nil by default) applies `RecoverUN`
with the given options to the source before transcoding, so an Explicit VR
target carries the known VRs instead of UN, as `gdcmconv -X`/`-U` does; the
report is returned in `TranscodeReport.UNRecovery`, and `Strict` fails the
transcode with `ErrUNRecovery`. Without it the output keeps the received VRs.
Private Creators read in Implicit VR need no recovery: the reader already
reads them as LO.

## Evidence

- `object`: `TestReadKeepsUNOpaqueAndWritesItBack`,
  `TestUndefinedLengthUNSequenceValuesFollowObjectByteOrder`,
  `TestRecoverUNResolvesKnownAttributes`,
  `TestRecoveredObjectWritesAsResolvedVRs`,
  `TestRecoverUNUnsafeCasesStayUNOrFailUnderStrict`,
  `TestRecoverUNLimitsAndCancellation`, `TestRecoverUNKeepsDeferredValues`
  (Little and Big Endian fixtures with text, binary, contextual, private,
  nested, defined and undefined-length sequence and long values).
- `parser`: `TestWriterPrivateUNSequenceByTransferSyntax` (Implicit VR and
  Explicit VR Little/Big Endian round trips).
- `pixeldata`: `TestTranscodeKeepsUndefinedLengthUNSequenceValues`
  (Big Endian to Little Endian and back, byte-exact).
- Independent oracles (opt-in): `TestUNRecoveryIndependentOracles` compares
  the recovered values with DCMTK 3.7.0 `dcmdump +uc` for the Little and Big
  Endian fixtures and with pydicom 3.0.2 for Little Endian (pydicom cannot read
  an undefined-length UN sequence in a Big Endian file). Run it with
  `DICOMGO_DCMTK_INTEGRATION=1`, `dcmdump` on PATH and optionally
  `DICOMGO_PYTHON`; without the gate it skips and qualifies nothing.
