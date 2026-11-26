# Attribute path templates

`pathtemplate` is a reusable formatter over `*object.Object`. It returns a
validated relative slash path and performs no filesystem access, path resolution,
directory creation, deferred-payload loading, or file writes. The integrations
for storescp and dcm2str are separate work.

```go
template, err := pathtemplate.Compile(
    "{Modality}/{StudyDate,date,+1d}/{StudyInstanceUID,sha256}/{SOPInstanceUID}.dcm",
    pathtemplate.Options{},
)
if err != nil {
    return err
}
relativePath, err := template.Format(dataset)
```

## Syntax and attribute selection

| Syntax | Meaning |
| --- | --- |
| `{PatientID}` | Keyword resolved with the compile-time dictionary |
| `{00100020}` | Eight hexadecimal tag digits; hexadecimal case is ignored |
| `{ImageType[1]}` | Explicit zero-based VM component |
| `{PatientID,sha256}` | SHA-256 over the decoded selected value, lowercase hex |
| `{PatientID,hash}` | Alias of `sha256`, with the same 64 hex characters |
| `{SOPInstanceUID,md5}` | MD5 over the decoded selected value, lowercase 32 hex characters |
| `{StudyDate,date}` | Complete DA rendered as YYYYMMDD |
| `{StudyDate,date,+1d}` / `{StudyDate,date,-1d}` | Signed whole calendar-day offset |
| `{{` / `}}` | Literal opening/closing brace |

Keywords use the supplied `Options.Dictionary`, or the standard dictionary when
nil. Keyword lookup is case-insensitive by the library dictionary contract.
Compilation resolves tags and does not retain the dictionary; later dictionary
changes do not change a compiled template. An explicit selector never interprets
attribute content as another template. Only top-level attributes are selected;
nested paths, sequence traversal and recursive placeholder expansion are not
part of this grammar.

The parser requires exact syntax, including lowercase modifier names. It rejects
unknown keywords, malformed braces/indexes, negative indexes, unsupported
modifiers and unsupported date-offset syntax. Java MessageFormat patterns,
arbitrary date layouts, month/year periods, clock offsets, numeric offsets,
slicing, choice, randomness, URL encoding and `now` are not silently accepted.

`date` requires VR DA and a valid complete eight-digit Gregorian date. It does
not turn partial DA/DT into an assumed date or use a local timezone. Offsets use
an explicit sign, decimal day count and lowercase `d`, bounded to ±3652425 days.
The shifted result must remain within the four-digit year range accepted by
`dcmtime` (0000–9999). Invalid dates, query ranges, legacy NEMA dates, partial
values and other VRs return an error. Gregorian DA and trailing precision rules
come from [DICOM PS3.5 Table 6.2-1](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.html#table_6.2-1);
this deliberately narrow modifier is an API policy.

## Values, absence and multiplicity

The formatter uses `Object.LookupStrings` so decoding honors the Object's own
or inherited Specific Character Set and authorized fallback policy. DICOM
padding is removed according to the existing decoder; no display-cleaning or
whole-value whitespace normalization is added before hashing. Materialized
StringValue text is used as stored. Raw binary US/SS/UL/SL/SV, FL/FD and AT
reuse Object's numeric/tag helpers after input bounds and representation checks.

UV is formatted without narrowing to int64, including valid values above
MaxInt64. This is the only separate raw numeric read because `Object.GetInts`
cannot represent that range. Authored Int16Value/Int64Value select directly
because the current integer helper does not accept those typed representations.
Typed numeric values preserve their signed/unsigned meaning. AT renders eight
hexadecimal digits. Textual IS/DS retain their selected lexical form unless a
supported modifier applies.

The defaults are `MissingError` and `VMError`. Absent attributes, unavailable
indexes and empty selected components fail rather than producing an empty path
component. An unindexed attribute with multiple values fails. `[index]` selects
one component explicitly; `VMFirst` explicitly selects the first component for
unindexed placeholders. `MissingSubstitute` uses `Options.Replacement` (default
`_`), sanitized as a component. A substituted value bypasses the modifier;
it is not a hash of an empty/missing value.

Malformed or unsupported values remain errors under every missing policy.
Nil/deferred values, BulkDataURI references, discarded values, sequences,
fragments, binary blobs and unknown payload implementations are not read or
converted to fallbacks. A nil source supports literal-only templates; attributes
on a nil source follow the missing policy. Every Format error returns an empty
string, including late expansion or final path validation errors.

## Path safety

Literal text and attribute values have different roles. Literals are validated
as written; unsafe literals are rejected rather than rewritten. Each attribute
value is sanitized independently into one component before interpolation.
Forward slash and backslash in attributes become underscores and cannot add
directories. Forbidden characters, controls and nongraphic Unicode characters
also become underscores. Terminal dots/whitespace are removed. Empty, `.` and
`..` sanitize to `_`; reserved device names receive an underscore prefix.

The complete result is validated again. This catches a device name or unsafe
component created by concatenating individually safe literals and attributes,
for example `C{PatientID}N` with PatientID=`O`. Validation rejects empty/dot/dot-dot
components, rooted paths, drive prefixes, UNC/device namespace syntax, colon
including alternate-data-stream syntax, forbidden characters, trailing
dots/whitespace and reserved Windows device names (also with extensions).
The character/device rules follow
[Microsoft's filename/path conventions](https://learn.microsoft.com/en-us/windows/win32/fileio/naming-a-file).
Slash is the output separator on every host; no host-dependent filepath cleaner
is used to hide or normalize traversal.

`SanitizeAttribute` and `ValidateRelativePath` expose the same default rules.
Sanitization is lossy and can produce collisions; it does not guarantee filename
uniqueness. Digests are deterministic naming strategies. `hash`/`sha256` use
[Go's SHA-256](https://pkg.go.dev/crypto/sha256) and `md5` uses
[Go's MD5](https://pkg.go.dev/crypto/md5), over UTF-8 bytes of the decoded selected
value, before path sanitization. These names/encodings are an explicit contract.

Safety is lexical. The formatter does not inspect a destination root, symbolic
links, junctions, mounted filesystems, filename case/normalization collisions,
or actual platform path-length settings. A caller doing filesystem operations
must own destination containment and storage semantics; a relative string is
not proof against symlinks outside a destination directory.

## Limits, reuse and compatibility

Zero selects finite default budgets; negative limits are invalid:

| Limit | Default |
| --- | --- |
| Template bytes | 4096 |
| Output UTF-8 bytes | 4096 |
| Inline attribute payload and decoded selected value bytes | 4096 |
| Placeholder expansions | 128 |
| Component UTF-8 bytes | 255 |
| Path components | 64 |

The actual inline payload is bounded before a text/VM decoder or numeric helper
can allocate, without trusting authored encoded header lengths. Charset
declarations read during raw text decoding are bounded too. Whole VM arrays are
bounded by the inline input budget; no Cartesian expansion is performed. The
decoded selection is bounded before modifiers. Digest output uses the output
budget rather than the smaller source-input budget. Literal/attribute appends
check the remaining output budget before growing the result builder. Components
and the final path are rechecked after interpolation. Public standalone helpers
use the default budgets.

Templates are immutable and may be shared between goroutines. Object's text APIs
may populate its internal charset caches, so callers must synchronize concurrent
use of the same Object when caches can change, as well as any mutation of values.
Different Objects may use the same Template concurrently. The formatter does
not mutate attribute buffers or retain source Objects.

dcm4che `AttributesFormat.java` was read only as a clean-room behavioral
reference. This is not a byte-compatible implementation of its templates:
its `hash` uses Java String.hashCode with 8 hex characters and its `md5` uses a
custom 26 character encoding; this API uses 64 and 32 hex characters respectively.
Its broader MessageFormat behavior is not inherited. No reference code was
copied or translated. Validation uses synthetic fixtures on the host, including
Windows lexical cases; it does not claim native Windows execution, filesystem
containment proof, live-PACS qualification or CLI integration.
