# Specific Character Set support

`dicom-go` decodes text VRs through the dataset `SpecificCharacterSet`
(`(0008,0005)`) when a VR uses the DICOM specific character set rules. The
default repertoire is `ISO_IR 6`. Replacement character sets use direct
codecs; multi-valued `ISO 2022` declarations use the DICOM G0/G1 state machine
described in PS3.5 Section 6.1.2.5.

## Supported terms

| DICOM term | Codec path | Decode | Encode | Notes |
|---|---|---:|---:|---|
| empty / absent | built-in default | yes | yes | ASCII / ISO 646 default repertoire. |
| `ISO_IR 6` | built-in default | yes | yes | Also accepts `ISO 2022 IR 6`. |
| `ISO_IR 100` | built-in Latin-1 | yes | yes | Also accepts `ISO 2022 IR 100`. |
| `ISO_IR 101` | `x/text` `iso-8859-2` | yes | yes | Also accepts `ISO 2022 IR 101`. |
| `ISO_IR 109` | `x/text` `iso-8859-3` | yes | yes | Also accepts `ISO 2022 IR 109`. |
| `ISO_IR 110` | `x/text` `iso-8859-4` | yes | yes | Also accepts `ISO 2022 IR 110`. |
| `ISO_IR 126` | `x/text` `iso-ir-126` | yes | yes | Also accepts `ISO 2022 IR 126`. |
| `ISO_IR 127` | `x/text` `iso-ir-127` | yes | yes | Also accepts `ISO 2022 IR 127`. |
| `ISO_IR 138` | `x/text` `iso-ir-138` | yes | yes | Also accepts `ISO 2022 IR 138`. |
| `ISO_IR 144` | `x/text` `iso-ir-144` | yes | yes | Also accepts `ISO 2022 IR 144`. |
| `ISO_IR 148` | built-in ISO 8859-9 (from the `x/text` windows-1254 table) | yes | yes | Also accepts `ISO 2022 IR 148`. Bytes 80-9F are C1 controls; Windows-only characters such as Œ, Š, € and curly quotes are not representable. |
| `ISO_IR 166` | built-in TIS 620 (from the `x/text` windows-874 table) | yes | yes | Also accepts `ISO 2022 IR 166`. Bytes 80-9F are C1 controls; Windows-only characters are not representable. |
| `ISO_IR 13` | built-in JIS X 0201 + `x/text` tables | yes | yes | Restricted to JIS Roman in G0 and half-width Katakana in G1; `ISO 2022 IR 13` participates in G1 designation. |
| `ISO 2022 IR 87` | DICOM ISO 2022 + `x/text` EUC-JP tables | yes | yes | JIS X 0208 in G0. |
| `ISO 2022 IR 159` | DICOM ISO 2022 + `x/text` EUC-JP tables | yes | yes | JIS X 0212 supplementary characters in G0. |
| `ISO 2022 IR 149` | DICOM ISO 2022 + `x/text` EUC-KR tables | yes | yes | KS X 1001 in G1. |
| `ISO 2022 IR 58` | DICOM ISO 2022 + `x/text` GB 2312 tables | yes | yes | GB 2312 in G1. |
| `ISO_IR 192` | built-in UTF-8 | yes | yes | UTF-8. |
| `GB18030` | `x/text` `gb18030` | yes | yes | Chinese national standard. |
| `GBK` | `x/text` `gbk` | yes | yes | Chinese GBK extension. |

Term matching is case-insensitive and accepts either underscores or spaces, for
example `ISO_IR 100`, `ISO IR 100`, and `iso_ir_100`.

Declarations are validated before codec construction. Replacement repertoires
such as UTF-8, GBK, GB18030, and the `ISO_IR` terms must be single-valued. Code
Extension declarations must be multi-valued, use only `ISO 2022` terms, place
an empty value or an allowed single-byte repertoire in Value 1, and place
multi-byte repertoires only in Values 2 through n. Empty trailing values,
mixed replacement/extension terms, invalid initial repertoires, and duplicate
repertoires return `ErrInvalidCharsetDeclaration` with the offending value.

## Person Name handling

For PN values, `^` and `=` are interpreted only at encoded-character
boundaries. They restore the initial repertoire; an extension used after either
delimiter must be designated again. This avoids treating a delimiter-valued
byte inside a two-byte Japanese character as syntax: while JIS X 0208 or
JIS X 0212 is invoked in G0, every byte from 21 to 7E belongs to a character
pair, so a kanji whose first or second byte is `=` (3D), `^` (5E) or `\` (5C),
such as 春 (3D55), is read as that kanji. The same reset model is used for
value separators, line/page boundaries, and permitted C0 controls.
Code Extension is rejected in the first (alphabetic) PN component group as
required by PS3.5; ideographic and phonetic groups may use the declared
extensions.

The decoder accepts only declared DICOM escape sequences, fixed G0/GL and
G1/GR invocation, and complete characters in their registered byte areas.
G2/G3, locking or single shifts, C1 controls, unknown escapes, undeclared
repertoires, truncated characters, and a missing G0 restoration fail with an
`ErrInvalidCodeExtension`. `CodeExtensionError` reports the byte offset and a
bounded hexadecimal sequence without copying the surrounding value.

The encoder chooses the first declared repertoire that represents a character,
reuses the current designation where possible, emits the required escape after
each reset, and restores the initial G0 repertoire before delimiters, controls,
and the end of the value.

Conformance tests use the byte-exact Japanese, Korean, and Chinese examples from
DICOM PS3.5 Annexes H, I, and K. JIS X 0212 and JIS X 0201 expectations are also
cross-checked against independent pydicom fixtures rather than generated from
the production codec.

## Legacy text recovery (opt-in)

Reading is strict by default. Text is decoded with the Specific Character Set
of its scope (the data set, or the nearest enclosing one for an item without
its own declaration); an absent or empty declaration means the default
repertoire. Bytes the repertoire cannot represent are an error, never
replacement characters, and nothing detects a character set heuristically.
`object.TextOptions` authorizes `FallbackCharacterSet` case by case:

| Case | Example | Authorization |
|---|---|---|
| Unsupported or invalid declaration | `ISO_IR 999` | `AllowUnsupportedCharsetFallback` (existing) |
| Absent or empty declaration, value outside the default repertoire | Latin-1 name with no (0008,0005) | `CharsetFallback & FallbackUndeclared` |
| Supported declaration that rejects the bytes | Latin-1 bytes declared `ISO_IR 192` | `CharsetFallback & FallbackUndecodable` |

Each case is independent: authorizing one does not authorize another, and the
fallback itself decodes strictly. Items inherit whether their scope is
declared. A preserving read and write keeps every byte and declaration as
received whatever the options.

A reader that must show every value it received, such as one presenting
query results, uses `Object.LookupStringReplacing` (or
`LookupStringsReplacing`). It returns what `LookupString` returns when strict
decoding, or an authorized fallback, succeeds. Otherwise it returns the
readable text with one `encoding.ReplacementCharacter` (U+FFFD) for each run
of undecodable bytes, and `replaced` true, instead of an error: a stateless
declared set keeps every character it can decode
(`SpecificCharacterSet.DecodeReplacing`, `DecodePersonNameReplacing` per
component group); an invalid or unsupported declaration, or an ISO 2022 value
that cannot be followed, keeps the default repertoire
(`encoding.ReadDefaultRepertoireReplacing`), dropping escape sequences and
treating the bytes after a G0 designation other than ASCII or JIS X 0201 Roman
as undecodable. A missing or non-textual element is still an error, and no
character set is guessed. Evidence: `TestLookupStringReplacingMarksUndecodableText`,
`TestDecodeReplacingKeepsDecodableText`,
`TestReadDefaultRepertoireReplacingFollowsG0Designations`.

`Object.ConvertTextToUTF8` returns a copy whose text is UTF-8 under `ISO_IR
192`. It decodes every raw value of a VR that uses Specific Character Set (LO,
LT, PN, SH, ST, UC, UT) with its scope's declaration (PN component groups and
ISO 2022 escapes included), uses the fallback only where the object's
`TextOptions` authorize it, and re-encodes the text. Other VRs, binary values
and Private Creators are untouched; the receiver is not modified. The root and
every item that declared its own character set are declared `ISO_IR 192`;
items that inherited it keep inheriting.

If a value cannot be decoded, `CharsetConversionAtomic` (default) fails the
call with `*CharsetConversionError` and returns nothing;
`CharsetConversionKeepScope` keeps that data set or item byte-for-byte with its
original declaration, writing that declaration explicitly (empty for the
default repertoire) when the parent now declares UTF-8. The result is verified
by decoding every UTF-8 scope strictly before it is returned, so `ISO_IR 192`
never covers unconverted bytes. Diagnostics give the structural path, a reason
from a fixed vocabulary and the declaration reduced to 64 characters of
`[A-Za-z0-9 _-\]` (others become `?`); no value text is reported.

Deliberate difference from amrshadid/go-dicom v1.6.0: its reader repairs
Latin-1 text declared as UTF-8 automatically. Here that repair happens only
when the caller authorizes `FallbackUndecodable`, with a diagnostic; the
default keeps reporting the contradiction.

Evidence: `TestTextFallbackOnlyInAuthorizedCases`,
`TestConvertTextToUTF8ConvertsEveryScope` (inheriting item, ISO 2022 IR 87
and UTF-8 overrides, PN groups, untouched CS, Private Creator and binary
values, strict reopening in three transfer syntaxes and DICOM JSON),
`TestConvertTextToUTF8UsesOnlyAuthorizedFallbacks`,
`TestConvertTextToUTF8FailsAtomicallyOrKeepsScope` and
`TestConvertTextToUTF8RefusesDeferredText`. The opt-in
`TestPydicomCharsetConversionOracle` (`DICOM_GO_PYDICOM_CHARSET=1`,
`DICOM_GO_PYTHON`) has pydicom 3.0.2 decode the original file and reopen the
conversion with the same characters.

## Converting to a restricted character set

`Object.ConvertTextToCharacterSet(ctx, target, opts)` returns a copy whose
text is encoded in `target` (for example `""` for the default repertoire,
`ISO_IR 100`, or ISO 2022 terms separated by backslashes). It decodes the
source as `ConvertTextToUTF8` does, then re-encodes every value of a VR that
uses Specific Character Set (LO, LT, PN, SH, ST, UC, UT, except Private
Creators) with the codec its VR needs and rewrites the declarations of the
data set and of items that declare their own. UIDs, other VRs and binary
values are never touched; trailing spaces of each value, which are padding,
are removed after the conversion.

`CharsetEncodeOptions.Unrepresentable` decides what happens to a character
the target cannot hold:

| Policy | Result |
| --- | --- |
| `UnrepresentableFail` (default) | The call fails with `ErrUnrepresentableText`; nothing is lost. |
| `UnrepresentableTransliterate` | An explicit table (letters without a decomposition, ligatures, quotes, dashes, symbols such as € → EUR), then the compatibility decomposition without diacritics (é → e, Ⅳ → IV, ½ → 1/2), then `?`. |
| `UnrepresentableDiscard` | The character is dropped. |

`CharsetEncodeReport` counts transliterated and discarded characters and names
the affected element paths, never their text. Against DCMTK 3.7.0 `dcmconv`
(opt-in `DICOMGO_DCMTK_INTEGRATION=1`, 172 characters): `--discard-illegal`
with its built-in ASCII and Latin-1 converters gives identical bytes, and
`--transliterate` through oficonv (ISO_IR 101, 109, 110, 126, 144, 148) gives
the same text except where oficonv writes an ASCII accent mark before the
letter (`"u`, `´e`, `~N`), which this conversion reduces to the base letter,
or `?` where a decomposition gives an approximation. DCMTK's own ASCII and
Latin-1 converters write `?` for every character when transliterating, and
its oficonv-backed `--discard-illegal` also transliterates; here discard only
drops.

## Limitations

- Character set detection is not automatic. If `(0008,0005)` is absent, the
  default repertoire is used unless the caller authorizes a fallback.
- ISO 2022 designation is intentionally limited to the DICOM-defined terms in
  the table. Generic ISO 2022 profiles and escape sequences outside PS3.5 are
  rejected.
- Unsupported terms return `ErrUnsupportedCharset`. Callers may opt into
  `object.TextOptions.AllowUnsupportedCharsetFallback` when a deployment needs a
  controlled fallback.
