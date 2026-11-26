# dcmodify editing options

The command edits Part 10 files through the library's attribute-path editor.
Actions run in command-line order, followed by the existing `-script` edits.
Use flags before operands. `--` protects filenames that start with a sign.
Choose `-o output.dcm` with one input or `-in-place` with one or more inputs.

```sh
dcmodify -gst -gse -gin -in-place image1.dcm image2.dcm
dcmodify -ma 'PatientName=ANON' -ea OtherPatientIDsSequence -in-place input.dcm
dcmodify -iff 'Modality=CT' -if 'PixelData=pixels-le.raw' -o out.dcm in.dcm
dcmodify +fc -i SOPClassUID=1.2.840.10008.5.1.4.1.1.7 -gin -in-place new.dcm
```

These operations do not validate a complete IOD, compute dose, transcode
pixels, or de-identify an object. They do not rewrite referenced UIDs.

| Option | Behavior |
|---|---|
| `-gst`, `--gen-stud-uid` | Generate only root Study Instance UID. |
| `-gse`, `--gen-ser-uid` | Generate only root Series Instance UID. |
| `-gin`, `--gen-inst-uid` | Generate only root SOP Instance UID; synchronize File Meta. |
| `-ma`, `--modify-all` | Modify every existing occurrence of a single tag/keyword, recursively. |
| `-ea`, `--erase-all` | Remove every occurrence of a single tag/keyword, including sequences. |
| `-ep`, `--erase-private` | Remove odd-group attributes, creators and private sequences recursively through `object.StripAttributes`. Public unknown attributes and even-group File Meta private information remain. |
| `-if`, `--insert-from-file` | Insert/replace `path=encoded-value-file`, creating missing path nodes. |
| `-mf`, `--modify-from-file` | Replace existing `path=encoded-value-file`. |
| `-iff`, `--process-if` | Apply edits only when all supplied conditions hold. |
| `-imt`, `--ignore-missing-tags` | Suppress missing tags for modify/erase; other failures remain errors. |
| `+fc`, `--create-file` | Create an absent input as Part 10 Explicit VR Little Endian. |

Each UID action generates a fresh UID for each input. Batch processing is
sequential and stops at its first error. Earlier successful files remain
published; later files are untouched. Each file is written to a temporary file
in its destination directory and renamed only after all edits succeed. This
is not a transaction across the batch and does not create implicit backups.
Existing in-place permissions are retained. Newly created files use 0600.
Duplicate input paths, symlink/hardlink aliases, directories and stdin are
refused. Output/input/script/value-source collisions are refused. This does
not provide locking against other processes changing a filesystem namespace.

Creation happens only for not-exist, never as recovery of a malformed or
unreadable input. The existing writer requires valid SOP Class UID and SOP
Instance UID. Supply both, or use `-gin` for the instance. An empty query file
without these identities is refused; no placeholder is published.

## Encoded values

Value files contain value bytes only: no element header, file format, fragment
stream or decimal-number conversion. Length must already be even; the command
adds no value padding. For fixed-width binary VRs, source bytes are explicitly
little endian and length must match the VR's unit size. Units are swapped to
the target dataset byte order when necessary. Text remains encoded bytes in
the target item's declared/inherited Specific Character Set; absence means
the DICOM default repertoire. No UTF-8 guess, BOM stripping or newline trim.

The target VR must be known from its existing attribute or dictionary, with
contextual VR resolved from the target dataset. Unknown/UN or ambiguous VR,
SQ/item, fragments, bulk references and nonmaterialized values are refused.
Native Pixel Data can receive known OB/OW bytes; an encapsulated target syntax
is refused. Pixel geometry and sample count remain the author's responsibility.

Value sources are snapshotted through bounded reads before any batch input is
edited. Their descriptors are closed after the snapshot. Every target gets
independent value storage, charged to the per-file applied-value budget. The
snapshots live for the command; one input model and optional Original
Attributes clone live at a time. Limits describe logical work and values,
not an exact RSS ceiling. An externally changed source after snapshot cannot
change the owned bytes; this is not proof of an atomic snapshot during a
concurrent same-size write.

## Conditions

Conditions support `path`, `!path`, and `path` followed by `= != < <= > >=`
and a value. Multiple conditions are AND. They are evaluated after complete
read diagnostics and before UID generation or any edit. False skips the input
without output and is successful; malformed or unsupported conditions fail.

Existence accepts attributes, sequences and items. A final `[n]` on a primitive
leaf addresses a VM component only in a condition; the global edit-path
grammar is unchanged. Existence across wildcards means any selected component
exists. Comparisons require every selected branch to have a matching leaf and
satisfy the check. Missing/zero targets or out-of-range VM are false, also for
`!=`. SQ/item comparisons and terminal VM `[*]` are refused.

String-like VRs, including DS/IS, compare lexically by component, then VM
length. `10` sorts before `2` for DS/IS. Binary scalar integer VRs retain full
signed/unsigned 64-bit precision; FL/FD must be finite. AT uses canonical tag
values. Known UI keywords can resolve through the UID dictionary. Text uses
the target item's charset and allowed VR padding. Without a VM index, the
whole component vector is compared. Bulk VRs OB/OW/OF/OD/OL/OV, UN, Pixel Data,
fragments and deferred values support existence only.

## Limits and compatibility

Default caps: 256 inputs; 1 MiB aggregate flag/script text and 4096 jobs; 128
conditions of at most 4096 bytes; 64 MiB per value source and 128 MiB cumulative
snapshots; sequence depth64, 100000 elements/items, 2000000 cumulative visits;
8192 matches per selection and 8 MiB retained paths per action; 100000 touches,
1024 created items per edit and 8192 per file; 128 MiB cumulative applied values
and comparison/conversion work. `-max-edit-value-bytes` shares that ledger.
Comparisons admit conservative charset expansion, operand conversion, VM/PN
split headers and charset declarations before decoding or allocating slices.
Supported codecs have a four-byte UTF-8 expansion envelope per source byte
and a twelve-byte encoding envelope per UTF-8 byte, including ISO 2022 resets;
these work reservations can reject a value before the reader's primitive cap.
Borrowed numeric vectors keep their existing storage, and scalar comparisons
and finite-value checks charge visits. Temporary condition paths also share
the per-action path budget. Private removal reserves its whole traversal and
a conservative touch envelope before the helper mutates the working object.
The `-max-batch-files`, `-max-value-file-bytes`, `-max-value-source-bytes` and
`-max-edit-*` flags accept positive limits only, without unlimited sentinels.
Session APIs use finite defaults for zero fields and reject negatives.

Read policy is unchanged: recovery off, odd lengths rejected, strict reserved
bytes opt-in. Existing positive `-read-max-*` caps still apply. Complete Meta
and dataset repair diagnostics precede skip/edit/publication. Original
Attributes, curated private creators and script grammar remain available.

`-original-attributes` explicitly records values from before the edits. Private
removal runs before that audit record is appended. Combining it with `-ep`
therefore preserves previous private attributes inside Modified Attributes
Sequence, while removing their current occurrences. The command preserves
this existing audit behavior and does not refilter or reorder the record.

Behavioral reference: DCMTK commit
`8d68392db66cd69e13dc878dc9889681062ad479`, without copying its implementation.
[Pinned manual](https://raw.githubusercontent.com/DCMTK/dcmtk/8d68392db66cd69e13dc878dc9889681062ad479/dcmdata/docs/dcmodify.man).
Process-if is absent from the documented 3.6.8/3.6.9 releases; the command does
not claim all-release or all-option parity. It deliberately requires explicit
output mode, validates primitive values, refuses opaque binary comparisons
and incomplete new-file identities, and retains strict read defaults.

Native tests reopen written files, including real LE/BE binary values and
charset. A compiled-CLI correctness campaign uses
`DICOMGO_DCMODIFY_E2E_BINARY`; package tests do not build recursively. External
oracle tests require a separate coordinated gate
`DICOMGO_DCMODIFY_PARITY_ORACLE=1` and explicit reference executable. That
finite synthetic matrix is not a PACS, full-IOD or clinical qualification.
