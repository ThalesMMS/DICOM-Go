# Dataset timezone conversion

`datasettime.Convert` changes a dataset's fixed UTC offset and its civil temporal
values without modifying the source:

```go
converted, err := datasettime.Convert(source.ToDataSet(), "+0530", datasettime.Options{})
if err != nil {
    return err
}
result := object.FromDataSet(converted, dictionary)
```

The API belongs in the reusable library. Its dependencies are `core` and
`dcmtime`; it does not depend on `object`, which already consumes `dcmtime`.

## Temporal behavior

Timezone Offset From UTC (0008,0201) provides context for DA/TM and DT without
their own suffix. An absent offset is undefined. The API never guesses UTC or
the machine's timezone. `Options.SourceOffset` explicitly supplies a fallback
when dataset/item context is absent. A nonempty stored offset takes precedence.
Offsets use a mandatory sign and four digits, from `-1200` through `+1400`;
UTC is `+0000`, and `-0000` is invalid. Only fixed offsets are supported; the
operation does not infer geographic zones or daylight saving transitions.
[DICOM PS3.3 C.12.1.1.8](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.12.html#sect_C.12.1.1.8),
[PS3.5 Table 6.2-1](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.html#table_6.2-1).

- Known DA/TM event pairs convert together, including calendar rollover and
  leap days. `20240228` / `234500.123456` at `+0000` becomes `20240229` /
  `051500.123456` at `+0530`. VM components pair by index, with equal VM and
  both components nonempty, or both empty, at each index.
- `StandardPairs()` returns a caller-owned list of supported event pairs.
  `Options.Pairs` adds explicit private or site-specific pairs. No relationship
  is inferred from tag adjacency. Both present elements must have DA/TM VRs.
  The standard list includes the existing PS3.4 combined matching pairs and
  other explicitly named event pairs in PS3.6. Independent SR Date/Time
  content items are not inferred to describe the same event.
- Implicit DT converts without adding an offset suffix. An explicitly qualified
  DT remains byte-for-byte unchanged at the value level, including partial
  dates, fractional precision, and its own offset. Its timezone is independent
  of (0008,0201).
- Standalone TM converts as a civil clock modulo 24 hours. This does not supply
  an unknown date or establish a unique instant. Nonempty standalone DA blocks
  a context change by default because choosing a time of day would invent
  information. `Options.UnpairedDates: datasettime.PreserveCalendarDate` is an
  explicit alternative: retain the original calendar date, including when its
  paired TM is empty, without claiming equivalence of an unknown dated instant.
  `ConvertWithReport` identifies every such retained DA component whose offset
  changed or was undefined. The default is `datasettime.RejectAmbiguous`.
- Trailing omission expresses unknown precision. HH can shift by whole hours;
  HH cannot represent a half-hour shift. Minute/second/fraction precision is
  preserved, including all 1–6 supplied fractional digits. Implicit DT below
  hour precision blocks a context change. Identity conversion preserves valid
  partial DT and leap seconds. Shifting SS=60 returns an error rather than
  normalizing it through Go's `time.Time`.
- Query ranges, malformed dates/times, legacy or truncated DA, mismatched pair
  VM, ambiguous duplicate temporal/context tags, and unavailable/deferred
  temporal values return explicit errors. No partially converted dataset is
  returned. Empty values carry no temporal information and remain empty. The
  calendar-date policy does not permit malformed DA or waive VM mismatches in
  pairs that contain nonempty times. An entirely empty TM can have zero VM:
  its nonempty paired DA is retained/reported only with the explicit policy.
  A nil temporal payload is unavailable even when its header length is zero;
  supply a materialized empty RawValue/StringValue for an actual empty value.

Trailing omission and explicit DT offsets are defined by
[PS3.5 Table 6.2-1](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.html#table_6.2-1).
Pair recognition, conservative ambiguity errors, and atomic publication are
API policies derived from those rules, not additional DICOM requirements.
[PS3.4 C.2.2.2.5](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_C.2.2.2.5.html)
defines negotiated combined matching rather than a general pairing heuristic.

## Nested items and version metadata

Every `core.SequenceValue` is traversed, including recovered UN sequences.
Items inherit enclosing source context; an item's own nonempty (0008,0201)
overrides that context. This explicit local-context policy matches the library's
dataset/item model. The output root receives the target offset, while an item
receives it only if that item already contained the attribute. Other items
inherit the output context. `MaxSequenceDepth` defaults to 64; `MaxElements`
and `MaxItems` each default to 100000 for the whole operation. Zero selects the
default; negative limits are invalid. Element visits/copies count repeated
nodes of an authored DAG and a generated root offset element. Item copies also
count repeated and empty items. Element budgets are checked before a node's
maps/element slice, and item budgets before its output item slice is allocated.
Depth bounds prevent cyclic traversal; total budgets bound structural expansion
even for a shallow DAG with many shared descendants.
The number of additional caller-supplied pairs is also bounded by MaxElements
before copying/indexing that configuration; standard pairs have a fixed size.

Context Group Version (0008,0106) has a narrow exception: an implicit day-only
version remains unchanged when the same dataset/item identifies the PS3.16
DCMR resource by Mapping Resource `DCMR` or the registered Mapping Resource UID
`1.2.840.10008.8.1.1`. Supplied identifiers must not conflict. Resource context
is not inferred from ancestors or Coding Scheme Designator. Context Group Local
Version follows ordinary DT conversion, and Patient Birth DA/TM follows its
explicit pair. A day-only version supplied with `+0000` remains unchanged under
the explicit-DT rule; it is not reinterpreted as a local version. This preserves
the input rather than asserting canonical encoding of the version metadata.
Mapping Resource uses CS comparison, where leading/trailing spaces are
insignificant. There is no universal tag exemption.
[PS3.3 8.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_8.4.html),
[8.5](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_8.5.html),
[8.7](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_8.7.html),
[PS3.6 Annex A](https://dicom.nema.org/medical/dicom/current/output/chtml/part06/chapter_A.html#table_A-1).

## Ownership and errors

The result owns new element lists, sequence/item structure, and changed temporal
text values. Unchanged payloads remain shared, including pixel data, fragments,
numeric arrays, bulk references, and unchanged text. Treat shared payloads as
read-only; this is not a deep clone. Source mutation during conversion is not
supported. Independent conversions may concurrently read an immutable source.

Element order and item offset metadata are preserved; a missing root offset is
appended. Replaced text values and rebuilt sequences clear stale encoded header
lengths so serializers calculate them. Use the serializer's normal group-length
policy rather than interpreting original retired Group Length values as current
encoded byte counts. Deferred non-temporal payloads remain deferred; this API
does not materialize them or acquire their backing value provider.

Errors wrap exported sentinels (`ErrInvalidOffset`, `ErrInvalidValue`,
`ErrMissingOffset`, `ErrAmbiguous`, `ErrUnsupported`, `ErrLimitExceeded`) and
identify the dataset/item
path, tag and VM component where appropriate. Every error returns a zero
`core.DataSet`; the source remains untouched even if earlier values converted
successfully in private result storage.

For a common dataset with birth date and no birth time, the policy/report API is:

```go
converted, report, err := datasettime.ConvertWithReport(source.ToDataSet(), "+0530",
    datasettime.Options{UnpairedDates: datasettime.PreserveCalendarDate})
if err != nil {
    return err
}
// report.PreservedDates identifies the retained birth calendar date;
// Study DA/TM still convert together with midnight rollover.
```

On error, the report is also empty. `Convert` discards the report when the caller
only needs the dataset; selecting the policy still explicitly accepts calendar
preservation instead of instant equivalence. Complete paired birth DA/TM convert
normally under either policy. The choice does not add a missing birth time.

## Reference and validation scope

dcm4che's `Attributes.setTimezoneOffsetFromUTC` was read as a clean-room
behavioral reference for recursive traversal, event pairing and explicit-offset
DT handling. The implementation uses the DICOM rules and synthetic fixtures,
without copying the reference implementation. It deliberately preserves
fractional precision and returns errors instead of retaining an ambiguous value
while replacing its context. dcm4che's universal birth/version exclusions and
default local timezone behavior are not inherited.

Tests exercise day/year rollover, half-hour and quarter-hour offsets, paired VM,
recursive override/inheritance, raw and string text, partial precision, explicit
offsets, source immutability, shallow payload ownership, concurrent readers,
atomic errors and Object/parser round trips. These are library fixture checks,
not live-PACS or clinical qualification.
