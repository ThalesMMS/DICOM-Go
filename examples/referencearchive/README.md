# Reference archive


Run from the `dicom-go` module:

```sh
go run ./examples/referencearchive -root ./reference-archive -move-destination MOVEDEST=127.0.0.1:11114
go run ./cmd/storescu -called REFERENCEARCHIVE 127.0.0.1:11113 /path/to/synthetic.dcm
go run ./cmd/findscu -host 127.0.0.1 -port 11113 -called REFERENCEARCHIVE -level study -k StudyInstanceUID= -k 'PatientName=SYNTH*'
```

Use a Study Instance UID returned by C-FIND in place of `STUDY_UID`:

```sh
go run ./cmd/dicom-go-retrieve -method get -remote 127.0.0.1:11113 -called-aet REFERENCEARCHIVE -output ./retrieved -level STUDY -study-uid STUDY_UID
```

For C-MOVE, start the explicitly allowed destination in another terminal:

```sh
go run ./cmd/storescp -address 127.0.0.1:11114 -aetitle MOVEDEST -output ./moved
go run ./cmd/dicom-go-retrieve -method move -remote 127.0.0.1:11113 -called-aet REFERENCEARCHIVE -level STUDY -study-uid STUDY_UID -move-destination MOVEDEST
```

Stop with Ctrl+C, then repeat the archive command with the same root to rebuild
the index. No separate database or migration command is required.

## Deliberate service subset

- Verification and Study Root C-FIND, C-GET and C-MOVE. C-GET negotiates the
  storage role on the requesting association; C-MOVE uses `StoreSession`.
- Secondary Capture, CT Image Storage and MR Image Storage with Implicit VR
  Little Endian, Explicit VR Little Endian or any encapsulated syntax in
  `transfer.PreservableCompressedReceiveSyntaxes` (JPEG, JPEG-LS, RLE, JPEG 2000,
  HTJ2K, JPEG XL, Encapsulated Uncompressed). Compressed instances are stored and
  retrieved opaquely in their own syntax: C-GET and C-MOVE offer only the stored
  syntax, so nothing is decoded or transcoded, and a requester that does not
  accept it gets a failed sub-operation. The acceptor honours the proposer's
  transfer syntax order so a C-GET context offering the stored syntax gets it.
  Pixel Data that contradicts the negotiated or stored syntax (native bytes
  under a compressed syntax, fragments under a native one) is refused with
  C000 and never published. No codec/transcode dependency.
- Canonical Study, Series and SOP UIDs, a nonempty Patient ID, and scalar
  standard-VR query metadata of at most 1024 UTF-8 bytes per selected value.
  This is an archive profile, not full SOP Class/IOD validation.
- Hierarchical queries require exact ancestor UIDs. Retrieval requires unique
  identity keys; UID lists are allowed at the requested level. Known optional
  C-FIND keys use `qrmatch`; unsupported keys fail explicitly. Matching remains
  limited to that package's supported rules, with no relational/fuzzy extension.
- Study metadata and selected patient metadata must agree exactly within a
  Study UID; a Series UID must belong to one study and have consistent series
  metadata. Conflicts fail instead of choosing one patient's metadata.
- Every duplicate SOP Instance UID, including identical content, is rejected
  with A900. Quota/write failures use A700; malformed or inconsistent content
  fails explicitly. Existing instances are never overwritten by this example.
- C-MOVE destinations are an explicit startup `AE=host:port` allowlist (at most
  16 entries). Unknown destinations return A801. Request data cannot provide a
  network destination. C-MOVE batch progress is reported after the bounded batch
  returns; it is not a per-instance real-time progress feed.

**Storage Commitment is disabled by default and its SOP Class is then not
negotiated.** A successful store acknowledges this example's complete file
publication and catalog admission. DICOMweb is outside this example.

## Storage Commitment (opt-in)

```sh
go run ./examples/referencearchive -root ./reference-archive \
  -commitment-dir ./reference-archive-commitments \
  -commitment-callback MODALITY=127.0.0.1:11114
```

`-commitment-dir` (outside and not containing `-root`) and at least one
`-commitment-callback AE=host:port` enable Storage Commitment Push Model
(PS3.4 J.3, 2026d) as the committing SCP. Only then is the SOP Class
negotiated, and the called AE must be the archive's AE Title. The library's
`StorageCommitmentWorkflow` does the protocol work; the example supplies the
policy, the durable transaction store and the callback routes.

**What a commitment means here.** A reference in the Referenced SOP Sequence of
the result means that, when the result was produced, the archive held that SOP
Instance of that SOP Class, and that the file admitted to the catalog was the
same file (identity, size, modification time) with the same SHA-256 as at
admission and could be read and indexed again in full. That file was written to
a private temporary file, synced, published without overwrite and (on Unix)
the directory synced before admission, and retrieval returns exactly that file.
Every other reference is in the Failed SOP Sequence with a PS3.3 C.14.1.1
reason: 0122H for a SOP Class the archive does not store, 0112H for an instance
it does not hold or whose file is gone, 0119H when the instance is held under
another SOP Class, 0110H when the file changed or cannot be read. The result
always partitions exactly the requested references.

**Order and durability.** N-ACTION-RSP success is sent only after the
transaction record is durable in `-commitment-dir`: each record is written to a
temporary file, synced, atomically renamed and, on Unix, the directory is
synced. A record that cannot be written refuses the request with 0110H; a full
store (`-commitment-max-transactions`) refuses with Resource limitation 0213H.
The result is persisted before the N-EVENT-REPORT is sent, and delivery is at
least once with bounded backoff. After a restart the archive reloads every
record and resumes accepted, processing and undelivered work. A reused
Transaction UID with different references is refused; an identical retry is
accepted once.

**Delivery.** Results go on a separate association from the archive's AE Title
to the requestor's allowlisted address, negotiating the Storage Commitment SCP
role (PS3.4 J.3.3.1.2). Same-association delivery is not offered by this
example because its association loop serves every service at once; the
library supports it for a dedicated loop. A requestor without a callback route
is accepted but its result stays undelivered until retries are exhausted.

**Limits.** This is not a certified or clinical archive. Commitment covers one
local file system: no replication, no protection against disk or controller
failure, no retention policy, and no re-verification after the result. Windows
has no directory sync through Go's `os` package; there the rename relies on
NTFS metadata journaling. Files changed after admission are reported as
failures within the process lifetime, but a restart re-admits whatever intact
file it finds, because digests are not stored outside the process. Completed
transactions are purged after 24 hours.

## Persistence, restart and ownership

The root must be a dedicated directory in a trusted local namespace. The
process holds an OS advisory lock on `.archive.lock` until shutdown; a second
cooperating archive process cannot open it. The lock file remains on disk and
the OS releases the lock after a crash. Do not remove it while an owner runs.
Windows, Linux, macOS and the BSD platforms with the supplied lock backend are
supported; other platforms fail closed.

Storage uses the shared [safe persistence contract](../../docs/INSTANCE_PERSISTENCE.md):
canonical UID filenames, private temporary files, no-follow directory/file
opens, complete-file publication and no overwrite. A serialized, context-aware
transaction checks Part 10 encoded size before publication. The network handler
owns a materialized, bounded input dataset; callers must not mutate it during
the operation. Published instances are admitted to the index only after a
successful re-read. A failure after publication preserves the file and blocks
the archive until restart/reconciliation.

Restart validates each file through the existing parser, reading and discarding
Pixel Data to detect truncated payloads, then extracts detached metadata with
`index.Read`. Pixels are not materialized during rebuild, but all file bytes
are read for integrity of the parse. Incomplete `.partial` entries, unexpected
files, invalid identities, conflicting metadata, excess quotas and truncated
instances prevent startup. Nothing is silently deleted or advertised. An
operator must reconcile such entries offline before retrying.

Queries verify indexed file identity, size and modification time. Missing or
changed files fail selection; retrieval reopens each selected file. C-GET loads
one bounded instance at a time, while C-MOVE uses the existing rooted store
sources. File handles are closed after validation/read; no deferred pixel value
outlives its file. Stop the association server before closing the backend.
These checks are not protection against an adversarial local principal changing
files while preserving their attributes. External modifications and multiple
noncooperating writers are unsupported.

## Finite resource defaults

| Resource | Default | Configurable ceiling |
| --- | --- | --- |
| Stored instances | 1000 | 10000 (`-max-instances`) |
| Instances selected per query/retrieve | 1000 | Instance limit (`-max-results`) |
| Part 10 bytes per instance | 16 MiB | 64 MiB (`-max-instance-bytes`) |
| Committed Part 10 bytes | 1 GiB | 16 GiB (`-max-archive-bytes`) |
| Concurrent incoming associations | 4 | Fixed; saturation rejected |
| PDU | 64 KiB | Fixed |
| Parsed elements / nesting / pixel fragments | 10000 / 32 / 10000 | Fixed |
| Non-pixel element bytes | 1 MiB | Fixed |
| Negotiation / progress / idle / operation | 5 / 10 / 30 / 30 seconds | Fixed |

`-max-results` counts selected instances before grouping C-FIND results, so a
study with many instances can exceed it even when the final response count is
small. Oversized requests fail instead of silently truncating answers. The disk
quota covers instance bytes; the small persistent lock file and filesystem
metadata are additional. Per-association parsing and retrieval can consume up
to the configured instance size; these are not global RSS guarantees.

## Validation

Default tests are synthetic and offline:

```sh
go test -race ./examples/referencearchive ./net/dimse
```

The independent profile uses the pinned requirements shared with the DIMSE
interop suite (pydicom 3.0.2, NumPy 2.1.3 and pynetdicom 3.0.4):

```sh
python -m venv /path/to/interop-venv
/path/to/interop-venv/bin/python -m pip install -r scripts/requirements-pynetdicom-interop.txt
DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/interop-venv/bin/python go test -race ./examples/referencearchive -run TestReferenceArchiveIndependent -count=1 -v
```

On PowerShell, set `$env:DICOMGO_PYNETDICOM_INTEGRATION='1'` and
`$env:DICOMGO_PYTHON` to the venv's `Scripts/python.exe` before the Go command.
This test launches only loopback peers: six synthetic multi-frame instances,
two studies, four series, C-STORE, C-FIND, C-GET, C-MOVE, duplicate rejection,
unknown destination rejection, C-GET cancellation counters and a disk restart.
The independent reader/receiver compares every metadata field in the fixture
and all 3876 pixel samples per store/get/move pass, before and after restart.
It also verifies that Storage Commitment is not negotiated by default.
`TestReferenceArchiveIndependentStorageCommitment` (same gate) has pynetdicom
act as the Storage Commitment requestor: C-STORE, N-ACTION, release, the
N-EVENT-REPORT on a separate association (Event Type 2 with a 0112H failure,
then Event Type 1) and a C-GET whose metadata and pixel SHA-256 match what was
committed. Offline tests cover every Failure Reason above, a same-size
same-time tampered file, restart between acceptance and processing, duplicate
Transaction UIDs, failed record sync, a full store and cancellation. Unit tests cover
failed writes, concurrent duplicates, byte/result/instance quotas, truncated
pixels, missing files, inconsistent identities and post-publication failure.

The peer qualification is specific to these native syntax, synthetic fixtures
and service flows; it is not a claim of parity with another archive or PACS.

Encapsulated preservation runs offline by default:
`TestArchiveEncapsulatedStoreGetMovePreservesCodestreams` stores RLE (populated
BOT, two-frame RGB), JPEG-LS (empty BOT, and two Items per frame with a
populated BOT), JPEG Lossless SV1 and ten-frame JPEG 2000 files through
`StoreSession`, then retrieves them with C-GET and C-MOVE. Every archived and
retrieved Part 10 file must keep its syntax, identity, BOT, every fragment
(SHA-256), a private attribute after Pixel Data and, for built-in codecs, every
decoded sample; RLE/JPEG-LS frames also equal the independent native
`MR_small.dcm`. `TestArchiveRejectsFalseSyntaxAndTruncatedEncapsulatedStores`
and `TestEncapsulatedComparatorDetectsCorruption` cover false syntax, fragments
on a native context, an aborted transfer, a duplicated delimiter, a lost trailing
attribute and a corrupted late fragment.

The independent peer gate uses DCMTK 3.7.0 (`storescu -R`, `getscu`,
`movescu` to `storescp +xa`, and `dcmdump` with its default strict error
handling) plus the pinned pydicom 3.0.2 for per-frame codestream hashes:

```sh
DICOMGO_DCMTK_INTEGRATION=1 DICOMGO_PYTHON=/path/to/interop-venv/bin/python go test ./examples/referencearchive -run TestArchiveEncapsulatedIndependentPeers -count=1 -v
```

JPEG 2000 is exercised as opaque transport only; no base-profile decoder is
claimed.

## Deployment boundary

The default listener is `127.0.0.1:11113`. This is a development reference, not
a clinical PACS: no authentication, TLS policy, authorization, backup, deletion,
retention, audit retention or migration system is supplied. AE allowlisting
controls outbound C-MOVE routing and does not authenticate incoming clients.
Use synthetic/non-PHI fixtures. The example explicitly enables patient and
description index profiles to serve queries; it does not anonymize stored data.
Production logs omit dataset values, UIDs and storage paths. Host/network access
controls and any decision to expose a non-loopback listener remain deployment
responsibilities.
