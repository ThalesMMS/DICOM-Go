# storescp layouts and bounded storage hooks


Without these flags the existing flat `<SOPInstanceUID>.dcm` and numbered collision
policy, store admission, read recovery and parsed/bit-preserving modes are retained.

## Naming

| Flag | Meaning |
| --- | --- |
| `-path-template PATTERN` | Complete relative slash path, including extension, using `pathtemplate`. Compile once; format the verified dataset. |
| `-sort-on-study-uid` | Put each file under its canonical VM1 StudyInstanceUID. |
| `-unique-filenames` | Use 128 random bits as a 32-character lowercase hex basename. Randomness failure refuses storage. |
| `-timenames` | Use UTC receipt time, `YYYYMMDDTHHMMSS.nnnnnnnnnZ`. Equal times use ordinary collision suffixes. |
| `-filename-extension SUFFIX` | Built-in naming suffix; default `.dcm`, explicit empty omits it. Otherwise leading dot, 2–32 portable ASCII bytes, no terminal dot. |

For example:

```text
storescp -output ./received -path-template '{StudyInstanceUID}/{SeriesInstanceUID}/{SOPInstanceUID}.dcm'
storescp -output ./received -sort-on-study-uid -unique-filenames -filename-extension .dicom
```

Templates exclude sorting, alternate basenames and an explicitly supplied extension.
Unique and time names exclude each other. Sorting may combine with either. Missing,
invalid or ambiguous VM selections fail without a filename fallback; use an explicit
index for multivalued attributes. The formatter handles DICOM character sets, numeric
byte order, date/hash modifiers and portable component sanitation. It never reads a
deferred value or bulk/sequence data for naming. Sorting and hooks requiring Study UID
refuse datasets without a valid StudyInstanceUID; the unchanged default still accepts
the SOP envelope allowed by its negotiation policy.

The formatter's limits are 4096 output bytes, 255 component bytes and 64 components.
Publication admits at most 4092 original relative bytes and 251 basename bytes,
reserving four bytes for `.999`; the actual collision result stays within those
formatter limits. Root path is at most 4096 bytes, event paths at most 8192. No
truncation is used. Suffix `.N` is inserted before the final extension (the last dot),
including a custom extension with several dots. At most 1000 destinations are tried.

## Publication and received bytes

`internal/netstore.SavePart10RelativeWithOptions` and `StreamFile.CommitRelative`
reuse the private temporary under the pinned output root. Parent directories are
created/opened relative to retained handles, without following links/reparse points.
New directories have private permissions (0700 on UNIX, owner ACL on Windows).
Root and parent identities are checked again before publication. UNIX publishes by
link-at between pinned directories; Windows renames by handle without replacement.
Cross-device publication fails; there is no copy or path-based rename fallback.

Content is synced and closed before the final file becomes visible. UNIX also syncs
new directory links and the destination publication; Windows keeps the existing
absence of a directory-fsync guarantee. Existing target bytes are never replaced.
Failure removes the original private temporary. Empty new directories may remain;
cleanup never recursively deletes another writer's directory. This protects against
ordinary link traversal/swaps, not arbitrary hostile filesystem mutation or relocation
by someone who can modify the trusted output tree.

Parsed mode writes the already verified dataset. Bit-preserving mode shares its
existing bounded verification result with naming/event preparation while the File
is alive; received dataset bytes are not rewritten or reparsed for layout. The local
File Meta header still uses the actual selected Called AE and the existing
Source/Sending/Receiving authorship rules. Recovery, metadata replay and dataset
absolute-byte budgets remain the existing read-policy behavior.

Pre-publication failure has no final file or activity/reception event. A typed
`netstore.Error{Published:true}` can leave a visible file after finalization, directory
sync or cleanup failure: it retains the existing failure status and produces no
successful-reception event. It is not rolled back. Successful C-STORE means local
storage under these platform semantics; it does not mean hook success or Storage
Commitment. See [PS3.4 B.2.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_B.2.2.html)
and [B.2.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_B.2.3.html).

## Events, inactivity and finite admission

`-exec-on-reception JSON` and `-exec-on-eostudy JSON` each accept one strict JSON
argv array with an operator-owned absolute executable. No implicit shell, PATH
search, redirection, recursive expansion or metadata-selected executable is used.
Event values occupy complete arguments; shell metacharacters in those values remain
data. Executables are trusted operator programs, not a sandbox against a program
that deliberately invokes an interpreter, escapes containment or forwards work to
an external privileged broker.

Reception tokens are `${path}`, `${directory}`, `${filename}`, `${called_ae}`,
`${calling_ae}`, `${sop_class_uid}`, `${sop_instance_uid}` and `${study_uid}`.
End tokens are only `${called_ae}`, `${study_uid}`, `${generation}`, `${count}` and
`${last_path}`. Embedded/unknown tokens and NUL are refused. An end event does not
invent a single sender, SOP or common directory for a study distributed across
custom paths. Example (trusted helper installed separately):

```text
storescp -output ./received -sort-on-study-uid \
  -exec-on-reception '["/opt/dicom/bin/receipt","${path}","${called_ae}"]' \
  -exec-on-eostudy '["/opt/dicom/bin/inactive","${called_ae}","${study_uid}","${generation}","${count}","${last_path}"]'
```

| Limit flag | Default and ceiling |
| --- | --- |
| `-eostudy-timeout` | 30s, positive, at most 24h |
| `-hook-timeout` | 30s per attempt, positive, at most 10m |
| `-hook-queue-depth` | 256 outstanding reservations/events; at most 4096 |
| `-hook-max-bytes` | 16MiB logical event budget; at most 256MiB |
| `-max-active-studies` | 128; at most 4096 and less than event capacity |
| `-max-study-state-bytes` | 1MiB logical tracker budget; at most 64MiB |

Hook-only limits without hooks, study limits without the end hook, invalid values,
empty executable, relative executable and `-single` with the end hook are usage
errors. Configurations total at most 64KiB, 64 arguments per hook, 8192 bytes per
argument and 32KiB expanded argv. Admission reserves 34816 logical bytes per
outstanding event before owned event strings are constructed. The tracker reserves
8192 last-path bytes plus 128 header bytes and owned key lengths per generation;
the byte limit can therefore admit fewer than the count limit, including defaults.

These are logical retained-byte accounts, not an RSS cap. Formatter/read/spool
limits remain separate. Poll arrays are bounded by admitted state count; one
active runner adds bounded argv/header, quoting/UTF16, pipe-copy and environment
buffers. Old/new path replacement and event expansion have bounded transient copies.
Memory and execution inside the trusted helper are outside these Go buffer accounts.

Before commit, the runtime reserves a reception slot and, for a new observed
study generation, one future end slot plus tracker state. Formatting and worst
collision-token expansion are checked before publication. Full capacity returns
A700 before commit; no accepted event is silently dropped. A repeated receipt for
an existing generation does not charge its future slot again. Stores retain the
existing separate store-slot admission exactly once.

The tracker key is `(actual CalledAETitle, canonical StudyInstanceUID)`; AE case is
preserved. Receipts from different associations/senders combine only for the same
key. It owns only key, last committed path/time, generation/count and in-flight
counters, never Objects, pixel buffers or a filename list. Duplicate SOP stores
produce separate files and counts. Count/generation overflow refuses admission.

A successful durable commit updates inactivity. Failure/abort does not reset it.
Known in-flight reservations pause expiry; an upload whose study is not known yet
cannot delay a previous timeout. Association release or arrival of another study
is not an end signal. Once due, Poll retires the generation and queues its already
reserved event; a later receipt obtains a distinct generation. There is no study
completeness, expected-instance accounting or cross-restart reconstruction claim.
The reusable mutex-protected tracker takes caller-supplied monotonic times and has
no timers, files or processes. The CLI uses one global ticker/poller independently
of its single FIFO hook executor, so a blocked hook or continuous receipts for
another study cannot prevent inactivity accounting. End attempts still wait for
earlier FIFO attempts. Close signals both goroutines to stop; a successful return
confirms that the poller and executor have finished.
Accounting order is commit/expiry order, not network arrival order. An end attempt
follows all reception attempts for that generation, including failed attempts.

## Process lifetime and shutdown

Child stdin is null, working directory is the configured root, and environment is
only `LANG=C`, `LC_ALL=C`, `TZ=UTC` (plus the system Windows directory as `SystemRoot`
on Windows). Credentials, HOME/PATH and metadata-derived environment are not
inherited. Combined stdout/stderr is capped at 8KiB and discarded, with no content
retained in logs. Overflow cancels the attempt. New diagnostics are fixed, without
attributes, UIDs, paths, executable argv or child output.

On Darwin/Linux each direct child gets its own process group. Timeout/cancel kills
the whole group; normal direct exit also cleans remaining ordinary group members.
The direct child is waited once and pipe waits use a finite 2s WaitDelay. Deliberate
new-session detachment and kernel-uninterruptible tasks are outside this trusted
hook contract; there is no hostile-descendant containment claim or UNIX descendant
count limit.

Windows10/Server2016+ uses a private kill-on-close Job Object, no breakaway flags,
and active-process ceiling16. JOB_LIST and the three-handle HANDLE_LIST are supplied
before CreateProcessW, so no running unassigned process can create free descendants
between Start and Assign. Setup/admission failure has no child-only fallback. Only
regular absolute `.exe` programs are allowed; batch/cmd files, cmd.exe and msiexec.exe
are refused. Quoting uses the public Windows ComposeCommandLine API. A conservative
`1 + sum(2*UTF16Units(arg)+3) <= 32767` bound is checked before command/UTF16 allocation
and during precommit expansion; it can refuse some otherwise encodable argument
arrays. Ordinary descendants are terminated on direct exit, output is drained before
success, cancellation/cleanup waits are finite, and process/thread/job/attribute/pipe
handles are released. Unsupported/restricted capability refuses hooks; other OS
profiles refuse execution too. Crossbuilding these paths does not prove a native
Windows execution. Public API references: [process attributes](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute),
[CreateProcessW](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-createprocessw)
and [Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects).

External server cancellation closes admission/timers, cancels the active attempt
and skips queued attempts; already published files remain. Normal `-single` exit
drains reception work within the existing shutdown timeout before cancelling
leftovers. Close is bounded, idempotent and safe across concurrent callers. After
its context expires it cancels work and waits at most another 3s; a context error
can precede remaining runner/poller cleanup, whereas success confirms their join.
It cancels future end reservations without manufacturing study-end events. Summary
counts report completed/failed/cancelled attempts and cancelled future reservations.
Nonzero exit, output overflow or hook timeout does not change successful storage
and does not prevent the next event on a live server.

Events are attempted at most once in this process. There is no durable outbox,
replay, retry, exactly-once external side effect, or C-CANCEL service for C-STORE.
A crash can lose an event after storing its file; restart only observes new receipts
and preserves collision-protected existing files. No live-PACS/clinical qualification
or native Windows execution is claimed. Synthetic filesystem, timer/barrier,
subprocess, parsed/raw and two-AE catalog loopbacks qualify the stated profile;
Windows runner tests are crosscompiled separately from host-executed controls.
