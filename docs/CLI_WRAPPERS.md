# Small DICOM command tools

These commands expose existing reusable library operations. Each `cmd` package contains a thin process entry point; parsing and lifecycle live in internal CLI families, and DICOM rules remain in `encapdoc`, `sr`, `gsps`, `printmgmt`, `mpps`, `ian`, `pathtemplate`, `net/dimse`, and `net/ul`. The four HL7 tools and their structural XML profile are documented in [HL7_CLI.md](HL7_CLI.md).

## Build and common behavior

```sh
go build ./cmd/pdf2dcm
./pdf2dcm -h
# Or run any tool from source:
go run ./cmd/mkkos -h
```

All 22 tools use the default Go build without an external codec requirement. They support `-h` and Go flag spellings such as `--enable-file-reject`. Place flags before positional filenames. Their command names indicate behavioral counterparts, not byte-compatible command-line interfaces to DCMTK or dcm4che.

The DICOM wrappers return 0 for successful work or help, 2 for invalid command arguments, and 1 for operation failures. File input uses the existing no-follow opener and is regular-file-only (FIFO/device and user-controlled symlink paths are refused). Parsing and payload tools limit files to 64 MiB each and batches to 128 inputs. The marker-only `dcmftest` reads four bytes at offset 128 regardless of the regular file's total size. DICOM parsing bounds elements/fragments to 100000 and sequence depth to 32. A smaller `-max-bytes` in encapsulation tools bounds the source payload; extraction still admits the DICOM container under the common 64 MiB bound. Rendering adds a limit of 16777216 pixels. These finite bounds do not promise constant memory or an instantaneous cancellation of parsing or filesystem sync.

New file outputs use a mode-0600 temporary file in the destination directory, an actual-byte limit of 64 MiB, file sync/close, and a create-only hard link. Existing paths are refused, including input paths and aliases. Temporary files are removed on success and failure. The parent directory must exist and be trusted; this does not promise descriptor-based protection against concurrent parent-directory replacement, directory-sync durability, or a replay/outbox protocol. Hard-link support is required. Stdout can fail after partial output; after publishing a file, failure of a final status write can report failure even though the file already exists.

## Encapsulated documents

| Command | Operation |
|---|---|
| `pdf2dcm` | Encapsulated PDF; requires a `%PDF-` signature |
| `dcm2pdf` | Extract PDF; requires the Encapsulated PDF SOP Class |
| `cda2dcm` | Encapsulated CDA, using existing CDA identifier validation |
| `stl2dcm` | Encapsulated STL with an explicit Frame of Reference UID |
| `dcmencap` | Select PDF, CDA, STL, OBJ or MTL with `-type` |
| `dcmdecap` | Extract an encapsulated payload; optional `-type` verifies MIME type |

```sh
pdf2dcm -patient-id SYNTHETIC -title 'Synthetic report' report.pdf report.dcm
dcm2pdf report.dcm extracted.pdf
cda2dcm -hl7-instance-identifier '1.2.3^sample' document.xml cda.dcm
cda2dcm -check-profile ccd continuity.xml continuity.dcm
stl2dcm -frame-of-reference 1.2.3 model.stl model.dcm
dcmencap -type obj -frame-of-reference 1.2.3 model.obj object.dcm
dcmencap -type mtl materials.mtl materials.dcm
dcmdecap -type obj object.dcm extracted.obj
```

The constructors generate absent Study, Series and SOP Instance UIDs with the library UID generator. `-study-from source.dcm` inherits the source patient/study modules and Study UID; explicit `-study-uid` overrides the inherited UID. Common flags include `-patient-name`, `-patient-id`, `-study-uid`, `-series-uid`, `-instance-uid`, `-title`, and `-max-bytes`. Signature checks and encoding success do not certify PDF content or a clinical document.

For `cda2dcm`, `-check-profile base` checks selected CDA header rules and
local narrative references before publication. `-check-profile ccd` adds the
CCD template identity and LOINC document code checks. See the
[validation scope](CDA_NARRATIVE.md#opt-in-cda-validation).

Extraction checks Encapsulated Document Length before opening the destination.
If the declaration and stored size differ, the reader accepts only one trailing
NUL after an odd-length document. A contradictory declaration returns an
`encapdoc.DocumentLengthError` without returning a payload. Older objects that
omit the optional declaration use the format-specific end or padding rules.
For explicit analysis, `Object.CopyValueTo` replays the complete stored
Encapsulated Document (0042,0011) Value Field, including any padding.
[DICOM PS3.3 C.24.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.24.2.html) · [DICOM PS3.5 6.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.html)

## Structured reports and key objects

```sh
dsrdump -show-codes report.dcm
dsr2html -max-items 10000 -max-points 32 report.dcm report.html
mkkos -o selected.dcm image1.dcm image2.dcm
mkkos -title 113001 -quality-reason '111210|DCM|Motion blur' \
  -o rejected.dcm image1.dcm
mkkos -title 113039 -reference-type COMPOSITE -o expired.dcm object.dcm
```

`dsrdump` and `dsr2html` call `sr.ReadDocument` and the existing text/HTML renderers. An optional second filename selects create-only file output; otherwise output goes to stdout. `-show-codes`, `-max-items` (1..100000), and `-max-points` (1..10000) bound the presented content.

`mkkos` requires `-o`, valid Study/SOP UIDs, and same-study input references. Default title 113000 uses `sr.NewKeyObjectSelection`; `-title 113030` selects the CID 7010 title (113030, DCM, "Manifest"), DCMTK's `CODE_DCM_Manifest`, for the same IMAGE selection. Manifest with Description (131560), whose CP-2595 descriptive content `mkkos` does not produce, is not offered. Titles 113001 (quality), 113037 (patient safety), 113038 (incorrect worklist), and 113039 (retention expired) call the reusable IOCM KOS rejection constructor. Quality rejection requires one valid CID 7011 reason supplied as `code|scheme|meaning`. `-reference-type IMAGE|WAVEFORM|COMPOSITE` declares the role of rejection references; the caller remains responsible for the correct role for the input IOD. Of Interest uses IMAGE references. These files express the selected/rejected objects; this command neither deletes archive objects nor sends an IOCM notification.

## Grayscale presentation

```sh
dcmpsmk image.dcm state.dcm
dcmp2pgm -presentation-state state.dcm -frame 1 image.dcm frame.pgm
```


## Bounded DICOM services

```sh
mkdir -p worklist/WLMSCP queries notifications
# The lock file must already exist and external writers must lock the same file:
touch worklist/WLMSCP/lockfile
wlmscp -data-file-path worklist --request-file-path queries \
  --enable-file-reject -duration 5m
mppsscp -port 11113 -duration 5m -max-instances 128
ianscp -port 11114 -output-dir notifications -duration 5m
```

All three listeners default to `127.0.0.1:11112`, a five-minute lifetime, and a called AE equal to the uppercase command name. Use separate ports when running them concurrently. `-host`, `-port`, `-called-ae` and `-calling-ae` use the canonical DIMSE connection parser and its shared aliases/TLS/negotiation options; `-calling-ae` optionally restricts admissions. Lifetime must be positive and at most 24 hours. Negotiation defaults to 30 seconds; association idle/read/write/release timeouts are finite. Concurrent negotiating/active associations default to 16, maximum 64. `-single` handles one association and exits after it finishes; an association can contain multiple operations. Ctrl-C or lifetime expiry closes the listener and active peers and joins handlers.

`mppsscp` uses `mpps.NewService` with a process-local bounded memory repository: default 128 instances, maximum 1024, 1 MiB per dataset, 256 performed series and 10000 references. The repository is lost at process exit. Existing MPPS state transitions and refusal statuses remain the library's responsibility.

`ianscp` requires an existing `-output-dir`. It uses `ian.NewService`, admitting at most 4 MiB per dataset, 1000 series and 100000 instance references. Existing output entries plus new publications count toward `-max-instances` (default 128, maximum 1024) and a 64 MiB cumulative byte quota. Nonregular preexisting entries are refused. This directory must have one publisher during the listener lifetime: the quota counts the initial entry snapshot plus this instance's publications, and does not track external additions/deletions or enforce a process-shared quota. A successful operation follows private create-only publication. The Part10 capture contains the validated normalized service AttributeList plus the actual service SOP Class and notification SOP Instance UID. This capture is not certification of a new Storage IOD or a durable archive transaction.

### Worklist filesystem policy

`wlmscp` reads `root/CalledAE/*.wl` through the existing directory provider and C-FIND router. Its new reusable policy wrapper preserves the existing provider API and adds opt-in query staging, shared locking, and complete-core filtering.

The default lock name is `lockfile` in the AE directory. It must already exist and be a regular file (FIFO and directory handles are refused before locking); `-lockfile ''` explicitly disables locking. Unix uses real shared `flock`; Windows uses nonexclusive `LockFileEx`. Two readers coexist and an exclusive cooperating writer waits. Acquisition polls with context cancellation; releasing/closing the handle and process death release the kernel lock. Writers must cooperate with the same lockfile inode rather than replacing it. Other OS profiles report unsupported locking. Native macOS tests qualify concurrent readers, exclusive exclusion, cancellation, close and killed-child cleanup; Windows has cross-build qualification only.

`--enable-file-reject=true` defaults to skipping records missing the declared core profile: nonempty Patient Name/ID and Requested Procedure ID, valid Study UID, exactly one Scheduled Procedure Step with required station AE/start date/start time/modality/step ID, and valid requested-procedure and step description/code alternatives. VR/VM/character-set and code checks reuse the existing MWL validators. Optional complex specimen, sex/name/pronoun/language, pertinent-document and protocol/context profiles are explicitly refused by this core completeness gate. `--enable-file-reject=false` keeps the existing directory-provider behavior. This gate does not claim complete support for every optional profile in [PS3.4 Table K.6-1](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_K.6.html).

`--request-file-path` requires an existing directory and publishes the received raw Identifier's attributes re-encoded as an Explicit VR Little Endian dataset with a generated `.query` filename. It adds no File Meta or invented SOP identities and does not preserve the original wire bytes or DCMTK's textual dump format. Publication occurs before acquiring the worklist lock/reading records so a cooperating writer can react to a query. A query file may therefore remain when matching later fails. Defaults are 1 MiB per identifier, 10000 files, and 64 MiB cumulative bytes including the initial snapshot of preexisting regular entries. The request directory has one publisher for this provider lifetime; external readers may consume queries, but external additions/deletions do not update these counters, and deletion does not reclaim this instance's quota. Concurrent publisher instances or external mutations require a separate coordinator; no process-shared directory quota or RSS bound is claimed. Reusable options can lower these bounds or select ceilings up to 64 MiB per identifier and 1 GiB stored bytes. Authored objects also receive finite element/depth/value admission before serialization; recursive, bulk, and opaque values are refused.

## Print client

```sh
printscu -host 127.0.0.1 -port 11115 -called-ae PRINTER image.png
printscu -host 127.0.0.1 -port 11115 -called-ae PRINTER \
  -color -columns 2 -rows 1 first.png second.png
```

`printscu` accepts PNG files and calls `printmgmt.Client.PrintFilm` for Basic Grayscale or Basic Color Print Management. Film rows/columns are 1..16; the number of images cannot exceed slots or 128 inputs. Each PNG is admitted by `DecodeConfig` before decoding, with at most 16777216 pixels. Total submitted image samples are bounded to 64 MiB. The existing client negotiates the applicable contexts and creates/sets/prints/deletes the film objects. Warnings and cleanup failures are reported. There is no automatic retry or reprint after an uncertain outcome. Tests cover real local UL/DIMSE grayscale and color film transactions; no physical-printer acceptance is implied.

## Templates, marker checks and private negotiation

```sh
dcm2str -p '{PatientID}/{StudyInstanceUID}/{SeriesInstanceUID}' image.dcm
dcmftest image.dcm another-file
termscu -host 127.0.0.1 -port 11112 -called-ae SERVER
```

`dcm2str` calls `pathtemplate.Compile/Format`. Templates generate sanitized relative paths using the library's syntax and traversal rules; this is not dcm4che MessageFormat syntax. `dcmftest` reads only the four-byte `DICM` marker at offset 128 of each regular file and prints `yes:` or `no:`. Any missing marker gives exit 1, regardless of the count. A marker-only file can pass; no parser, IOD or conformance validation is implied.

`termscu` requests the DCMTK private shutdown presentation context `1.2.276.0.7230010.3.4.1915765545.18030.917282194.0` and releases an accepted association. It sends no standard DIMSE shutdown command. Both accepted negotiation and association rejection leave the shutdown outcome unconfirmed; a peer can reject while acting on its private policy. The tool reports this uncertainty rather than claiming server termination.

## Qualification

Focused tests cover actual payload round trips, KOS title/reason semantics, GSPS/PGM pixels, marker/template refusals, bounded filesystem publication, worklist locks and crash cleanup, MPPS/IAN/MWL loopback operations, and grayscale/color print transactions. Command smoke tests cover help and invalid CLI arguments. Integration evidence records native, race, Go 1.22 with the external Darwin linker, real binaries and Windows cross-builds. They are local synthetic qualification; target PACS/printer/HL7-system interoperability, native Windows execution and clinical acceptance remain outside these checks. CI workflows are not added.
