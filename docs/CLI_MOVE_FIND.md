# MOVE reception and free FIND/retrieve queries

`dicom-go-retrieve` supports its existing C-MOVE/C-GET modes. `movescu` uses the same runner and accepts MOVE only. `findscu` supports Study Root, Patient Root and Modality Worklist C-FIND. These tools use the reusable library's existing association, DIMSE, storage and parser/writer primitives; they do not add application or Twin networking workflows.

## Query inputs

With only the fixed `-level` and identifier flags, retrieve builds a baseline
hierarchical C-GET or C-MOVE Identifier for one entity. Supply the selected
level's unique key and all its ancestors for the chosen model. Malformed keys,
keys from lower levels, and patient keys in Study Root are usage errors.
Non-ASCII Patient IDs in Patient Root are sent with the UTF-8 character set.

Both tools accept repeated `-k key[=value]`, repeated `-query-file path`, and trailing query-file paths. Keywords, `(gggg,eeee)`, legacy `gggg,eeee` and item paths such as `ScheduledProcedureStepSequence[0].Modality=CT` are accepted. An omitted value requests a zero-length return key. Unknown creation VRs are refused rather than guessed. Matching wildcards and date/time ranges remain matching expressions.

Files contain Part 10, DICOM JSON (one object or a one-object array), or a raw dataset. Raw syntax defaults to Explicit VR Little Endian; choose `-query-file-syntax implicit-le` or `explicit-be` when appropriate. Part 10 uses its File Meta transfer syntax. A file starts the query, explicitly supplied legacy UID flags override it, and ordered `-k` edits apply last. Multiple files create sequential operations on one association. Query-level conflicts with explicit `-level` are refused.

With no file, free keys can supply required retrieve UIDs:

```sh
movescu -c QRSCP@127.0.0.1:11112 -calling MOVESCU \
  -move-destination DESTAE -listen-port 11113 -output ./received \
  -k QueryRetrieveLevel=STUDY -k StudyInstanceUID=1.2.3

findscu -c OFFIS@127.0.0.1:11112 -worklist \
  -k 'ScheduledProcedureStepSequence[0].Modality=CT' -k PatientName
```

Retrieve level/required unique keys are checked after composition and before dialing. QueryRetrieveLevel must be one CS value; a wrong-VR file level cannot be relabeled by a compatible explicit `-level`. Query-file decoder diagnostics omit input tokens while retaining the error cause for cancellation and resource-limit classification. Free additional keys are transmitted; the SCP decides which service-specific matching keys it accepts. MWL uses its own SOP Class, carries no QueryRetrieveLevel, and requires one item when the Scheduled Procedure Step Sequence is supplied. QR level/extended-negotiation options conflict with MWL. This is a raw Identifier interface, preserving return keys beyond the narrower typed MWL helper's model; it does not implement every SCP's matching behavior.

Input limits are finite: 16 files, 4 MiB per file/serialized Identifier, 16 MiB aggregate file input, 1024 keys/1 MiB aggregate key text, 4096 elements, 1024 items, depth 16 and item index 255. Edits conservatively reserve possible created items before insertion; a near-limit query can be refused even if an edit would reuse existing items. Command/File Meta attributes, Pixel Data and unresolved bulk/deferred/unknown payloads are refused. Reads, editing and size checks precede listener startup and network requests. These are input/traversal quotas, not a process RSS limit.

## Embedded MOVE receiver

An explicit `-listen-port` enables the Storage SCP. `-port` keeps its common connection meaning: the remote SCP port. This differs from DCMTK movescu's listener `--port`; use `-listen-port` here. `-listen-host` defaults to loopback. Explicit listener port zero chooses an ephemeral port and reports it, but a remote archive still needs destination configuration for that port.

The local receiver AE defaults to `-move-destination`; `-listen-ae` must match it. The archive must map this destination AE to the listener address: C-MOVE transmits the AE title, not the host/port. `-listen-calling` defaults to the remote called AE. Explicitly setting it empty permits any calling AE. AE filters are admission checks; mutual TLS provides cryptographic peer authentication.

Requestor connection/TLS/User Identity flags retain their existing meanings. The listener uses independent `-listen-tls-profile`, `-listen-cert`, `-listen-key`, repeatable `-listen-ca`, `-listen-require-peer-cert` and `-listen-timeout`. Prefixed values reuse the existing listener flag validation/conflict rules. Listener TLS does not enable TLS on the requestor association or reuse its identity.

The receiver binds before the outgoing MOVE and accepts the finite default Storage SOP Class catalog with native ExplicitLE, ImplicitLE and ExplicitBE. It preserves the accepted syntax; it does not transcode or promise compressed-codec support. Defaults are two associations (configurable 1–16), 10 s negotiation/progress timeouts, 64 MiB dataset/element/pixel payload (reduce with `-receiver-max-dataset-bytes`), 100000 elements, depth 32 and 4096 fragments.

Each C-STORE must have complete single SOP identities agreeing with its command and presentation context, and must match the admitted retrieve Patient/Study/Series/SOP unique keys. Successful instances are published atomically through the existing netstore transaction and have receiving/sending AE File Meta. Incomplete or rejected stores do not become complete output files. Incoming datasets are bounded resident objects; memory is finite per association but includes parsed representation and writer overhead.

Remote progress/final sub-operation counts and local committed/rejected/association-error counts are reported separately. A local commit is not proof that the remote peer received its response. Local storage or shutdown failure causes a nonzero exit even if the remote reported success. Output/error diagnostics for the receiver omit patient values, UIDs, full destination paths and raw remote Error Comments.

After the last terminal MOVE or an operation failure, the receiver stops accepting and drains accepted associations for `-receiver-drain-timeout` (default 5 s, maximum 60 s), then closes transports on expiration and joins its server before the outgoing association release handshake. A peer withholding ReleaseRP cannot keep the local listener accepting. Interrupts preserve requestor C-CANCEL/drain and receiver cleanup. Filesystem reads/writes/sync/close are operating-system operations without a hard context deadline; cancellation is checked between bounded chunks, and a blocked filesystem operation can delay the final join. Existing external-receiver MOVE and C-GET behavior remain available.

## FIND cancellation and extraction

`-cancel-after N` (alias `-cancel N`) sends one C-CANCEL after N pending matches. Later pending responses are drained without delivery/extraction until the terminal response. Locally requested count cancellation exits zero after canceled or raced successful terminal; a failure/drain timeout exits nonzero. Interrupt exit remains 130; operation timeout remains failure. `-max-results` retains its historical print-only behavior and does not cancel the operation.

`-extract -extract-dir directory` writes delivered Identifiers as `rsp000001.dcm`, `rsp000002.dcm`, etc. They are **raw Explicit VR Little Endian datasets**, without a Part 10 preamble/File Meta or fabricated SOP instance identity. Read them as raw datasets using that syntax; they can also become query files. This intentionally differs from assuming each C-FIND response is a storage instance. Ordinal names do not embed patient or UID values.

Extraction uses private temporary files anchored in an existing nonsymlink output directory, bounded chunked writes, file sync/close, and no-replacement atomic publication. Collisions, symlinks, cancellation and write/resource failures cannot overwrite an existing response. Previous complete responses remain; the incomplete temporary is removed and the C-FIND is canceled/drained on an extraction failure. Publication is atomic, not a claim of durable directory synchronization or all-responses transactions.

Limits are 16 MiB per extracted response, 10000 files and 256 MiB aggregate extracted bytes. Text/JSONL output can accompany extraction. Its global ordinal/budget spans sequential query files. MWL/extraction receive mode uses the configured response byte cap (16 MiB default, maximum 256 MiB), four queued messages, 100000 elements and depth 32. Plain text retains the existing asynchronous session limits; JSONL retains its existing byte/queue policy. Parsed object/copy overhead lies outside payload counters. Results are streamed rather than retained as a complete result list.

## Qualification and scope

Tests cover raw LE/BE/JSON queries, ordered overrides, limits/no-network refusal, sequential files, MWL identifiers, count cancellation with a pending response after CANCEL, extraction bytes/readback/collisions/cancellation/short writes, receiver identity/query binding, parser byte cap, AE refusal, occupied listener port, independent mutual TLS and bounded active-association drain. Existing retrieve test bodies move unchanged apart from their package declaration.

Opt-in real socket tests use DCMTK `dcmqrscp`/`dcmqridx` and `wlmscpfs` with owned synthetic image/worklist fixtures. Enable them with `DICOMGO_DCMTK_INTEGRATION=1`, `DICOMGO_DCMTK_BIN` pointing to qualified tools and `DCMDICTPATH` when a development tool installation requires its external dictionary. They prove the stated local protocol cases, not interoperability with an external PACS, native Windows execution or all DCMTK options. No performance claim is made.

Protocol authority: DICOM PS3.4 [C.4.2](https://dicom.nema.org/Medical/Dicom/current/output/chtml/part04/sect_C.4.2.html), [K.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_K.4.html), [K.6](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_K.6.html); PS3.7 [9.3.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part07/sect_9.3.4.html), [9.3.2](https://dicom.nema.org/Medical/dicom/current/output/chtml/part07/sect_9.3.2.html), [9.3.2.4](https://dicom.nema.org/Medical/dicom/current/output/chtml/part07/sect_9.3.2.4.html). Official DCMTK documentation/public commands are behavioral references; no upstream implementation is copied. Twin remains GPU-only and has no new CPU/application callsite.
