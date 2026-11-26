# Received-instance persistence

`storescp` and `dicom-go-retrieve -method get` use
`internal/netstore.SavePart10WithContext`. Both validate the C-STORE command,
accepted presentation context and dataset identity before persistence. The
library's generic DIMSE services still leave persistence policy to their caller.

## Identity and names


Invalid identities are refused. There is no compatibility switch that silently
sanitizes or replaces an invalid identity. `SafeFileBase` remains a legacy name
transform, but it is not used to authorize received identities or form their
filenames. Valid instances use `<UID>.dcm`, then `<UID>.1.dcm` through
`<UID>.999.dcm` for collisions. Each attempt is exclusive. Existing regular files,
directories and symlink destinations are not replaced. Separate invalid UIDs
that previously sanitized to the same name now both fail before file creation.
Repeated valid instances retain their actual identities inside the files.

## Publication and cancellation

Persistence opens the trusted output directory without following links, creates
a random `.netstore-<token>.partial` entry exclusively with private permissions,
and writes the complete Part 10 file through the existing object writer.
Cancellation is checked before preparation, between output chunks of at most
32 KiB, after writing and immediately before publication. A blocked OS write,
sync or close cannot be interrupted by these checks; existing network/parser
limits continue to bound incoming datasets.

The temporary is synced and closed before it can become a `.dcm` entry. The
directory is reopened using no-follow traversal and compared by file identity
before publication. A replaced directory causes failure and cleanup stays
anchored to the original open directory. Publication rechecks temporary identity.

| Platform | Publication | File protection | Durability scope |
| --- | --- | --- | --- |
| Unix families supported by `x/sys/unix` | `linkat` into an absent destination, then remove the temporary alias | `0600`, applied to the opened descriptor | File sync before publication; directory sync after the destination link |
| Windows | Relative rename by handle with replacement disabled | Private DACL at creation; verified protected DACL for current user and SYSTEM before writing, applied by handle | File sync before publication; no equivalent directory-fsync or power-loss guarantee claimed |
| Other targets | Fail closed | Unsupported | No publication fallback |

Windows rename behavior follows
[`FILE_RENAME_INFORMATION`](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information).
No copy fallback exposes a partially written final filename. On Unix, a filesystem
that cannot provide hard links fails explicitly. Pre-publication write, sync,
close, cancellation and publication failures remove the owned temporary and
preserve preexisting files. A process crash can leave `.partial` entries; automatic
deletion of such files is outside this helper's scope.

Once publication succeeds, the complete destination is authoritative. A later
close, directory-sync or cleanup failure returns `*netstore.Error` with
`Published=true` and the destination path; it does not roll back a published
instance or overwrite an older file. The CLIs return a C-STORE failure status on
that error, so an explicit peer retry can produce a duplicate complete instance.
Cancellation observed after commit does not remove the committed object.
Consumers must use `.dcm` as the publication boundary and ignore `.partial`.

`CreateInstanceFile` remains a low-level exclusive reservation helper: it exposes
an open, unfinished file and does not offer the complete-instance publication
contract. Neither receiving CLI uses it for persistence.

## Bit-preserving reception

`storescp -bit-preserving` and `dimse.StorageSCPOptions.StreamSink`
store a data set without building it in memory or re-encoding it, after DCMTK's
`storescp --bit-preserving`. The File Meta Information is built from the
command's SOP UIDs and the accepted transfer syntax
(`object.EncodeFileMetaHeader`), and the P-DATA bytes follow unchanged, so the
stored file equals the sent data set byte for byte outside group 0002 (a
Deflated data set keeps its deflate stream and any PS3.5 A.5 pad byte). The
temporary, publication and naming guarantees above are the same
(`netstore.BeginStream`, then `Commit` or `Abort`).

Only the command is checked while streaming: valid SOP UIDs, the SOP Class equal
to the accepted abstract syntax, and `MaxDataSetBytes` (over it the C-STORE is
answered 0xA700, the rest of the data set is drained and nothing is published).
`storescp` then parses the stored file within its limits and checks its identity
before publication, as its re-encoding path does. `dimse.FileStreamSink` does the
same only with `Verify`; without it a data set whose content differs from the
command is stored as received. A sink that returns a nil stream leaves that
C-STORE to the in-memory `StoreHandler`. The heap stays about 7 MiB above its
starting point while 64 MiB are streamed in 4 KiB PDUs; the parsing path holds
the whole data set as an object.

## Spooled reception

`dimse.StorageSCPOptions.SpoolDir` and, for a C-GET requestor,
`dimse.WithCGetSpoolDir` keep the `StoreHandler` contract but write
each data set to a temporary file of the directory (`.dimse-spool-*`) as its
P-DATA arrives. The file is then parsed with `DeferPixelData`,
`InlineValueBytesThreshold` (`SpoolInlineBytes`, by default 64 KiB) and
`DeferNestedValues`: the handler receives an `*object.Object` whose Pixel Data
and other long values stay in the file and are read on demand, and
`object.WriteFile` streams them back. The identity, CT Pixel Data and
encoding checks and the size limits apply as for the in-memory reception; a
data set over `MaxDataSetBytes`, or one the spool cannot hold, is answered
0xA700 and drained. The data set is valid until the handler and
`OnCStoreResponse` return; the file is removed after the response. A removal
that fails, as when a scanner briefly holds the file on Windows, is retried a
few times with a growing delay; a file that stays is reported to
`StorageSCPOptions.OnSpoolRemoveError` (`dimse.WithCGetSpoolRemoveError` for
C-GET), without changing the C-STORE response, so the application can remove
it later. A Deflated
data set is read into memory, because values cannot be deferred on a
compressed stream. The live heap stays at its starting point while a 64 MiB
data set is received and written back; the in-memory reception holds all of
it.


## Containment and diagnostics


The output root and its ancestor namespace must be configured by a trusted
operator. Untrusted peers control command/dataset values, not local directory
ownership, mount configuration or permissions. The tests also exercise a
directory replacement before publication: replacement is detected and the new
directory remains untouched. Moving the actual open directory after the final
identity check, modifying temporary entries as the same OS principal, malicious
filesystems and privileged local attackers are outside the guarantee. This is
not a sandbox against local administrators. Directory ACLs must protect filenames
as well as file contents; existing directory permissions are not rewritten.

Default C-STORE and C-GET store logs contain outcomes and status codes, without
SOP identities, saved paths or remote values. Persistence errors expose stable
operation names and preserve causes through `Unwrap` for `errors.Is/As`; logging
an unwrapped OS error can reveal a path and is not the default. This change does
not provide authentication, transport security or general PHI auditing.

## Qualification

All fixtures are synthetic. Tests cover malformed/overlong UIDs, Windows names,
traversal separators, command/dataset mismatch, multiplicity, maximum-length
UIDs, padding, preexisting files, 24 concurrent publications, symlink targets and
ancestors, directory replacement, default log redaction and injected write,
sync, close, cancellation, publication and post-commit failures. Network tests
exercise the handlers actually used by both CLIs.


