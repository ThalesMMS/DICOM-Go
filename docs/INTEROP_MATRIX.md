# Interop Matrix

External interoperability checks are opt-in. Normal `make test` and CI runs
must stay self-contained and skip peer-dependent tests unless an explicit peer
gate such as `DICOMGO_INTEGRATION=1` or `DICOMGO_HOROS_INTEGRATION=1` is set.


The same pinned Python gate also runs `TestNonPatientProfilesAgainstPynetdicom`
for [Hanging Protocol and Color Palette Q/R](NON_PATIENT_QR.md): both models'
FIND/GET/MOVE contexts, reverse/destination Storage roles, full synthetic dataset
equality and rejection statuses. Complete IOD and optional-key qualification
remain outside that test's scope.

The Python profiles require pydicom 3.0.2, the patched version for
[CVE-2026-32711](https://github.com/advisories/GHSA-v856-2rf8-9f28).
Historical fixture provenance can still identify 3.0.1; it does not authorize
using that older interpreter in the current test profiles.

## Required Tools


- Go 1.22 or newer.
- Optional external DICOM peer for network checks. The documented local target
  is Orthanc on `ORTHANC_HOST:ORTHANC_PORT`.
- Optional Horos peer for manual lab checks. The documented target is Horos on
  `HOROS_HOST:HOROS_PORT`.
- Optional Storage SCP reachable by the external peer when running C-MOVE.
- Optional sample DICOM files or seeded external studies, depending on the row.
- Python 3.12 with the exact packages in the
  [pinned interop requirements](../scripts/requirements-pydicom-interop.txt)
  for the independent Part 10 gate.

## Environment

Common variables:

- `DICOMGO_INTEGRATION=1`: enables external-peer tests.
- `ORTHANC_HOST`: defaults to `127.0.0.1`.
- `ORTHANC_PORT`: defaults to `4242`.
- `CALLING_AET`: defaults to `DICOMGO`.
- `CALLED_AET`: defaults to `ORTHANC`.
- `MOVE_DEST_AET`: defaults to `DICOMSTORE`.
- `STUDY_UID`: required only for the Study Root C-MOVE row.
- `DICOMGO_DCMTK_INTEGRATION=1`: enables the DCMTK runtime oracles below;
  `DICOMGO_DCMTK_BIN` optionally names the directory of the DCMTK tools
  (otherwise PATH) and `DICOMGO_DCMTK_DATA` the directory of its `.lut` data.
- `DICOMGO_HOROS_INTEGRATION=1`: enables Horos-only external-peer tests.
- `DICOMGO_PYNETDICOM_INTEGRATION=1`: enables independent pynetdicom DIMSE
  tests; `DICOMGO_PYTHON` may select the virtual-environment interpreter.
- `DICOM_GO_PYDICOM_PART10=1`: enables the synthetic general Part 10 matrix;
  `DICOM_GO_PYTHON` selects its pinned virtual-environment interpreter.
- `DICOM_GO_HIGHDICOM=1`: enables the highdicom TID 1500 Measurement Report
  read; `DICOM_GO_PYTHON` selects an interpreter with highdicom.
- `HOROS_HOST`: defaults to `192.168.100.62`.
- `HOROS_PORT`: defaults to `4007`.
- `HOROS_AET`: defaults to `HOROS`.
- `HOROS_CALLING_AET`: defaults to `DICOMGO`.
- `HOROS_TIMEOUT`: defaults to `10s`.

## Matrix


| Area | Check | Command | Default behavior |
|---|---|---|---|
| File parsing | Local Part 10 and dataset parser tests | `go test ./parser ./object ./internal/dcmdump` | Always runs locally. |
| General Part 10 | Bidirectional independent checks for Implicit VR Little Endian, Explicit VR Little Endian and retired Explicit VR Big Endian, including sequences, undefined lengths, UTF-8 text and native Pixel Data | `make pydicom-interop` | Opt-in. Creates only synthetic temporary files and requires exactly pydicom 3.0.2 and NumPy 2.1.3. The same command also runs the existing ISO 2022, RLE, wide-sample, JSON text and DICOMDIR independent gates. |
| RLE Lossless output | Independent pydicom read and pixel-array comparison of a file produced by `TranscodePath` | `DICOM_GO_PYDICOM_RLE=1 DICOM_GO_PYTHON=/path/to/python go test ./pixeldata -run PydicomRLE -count=1 -v` | Skipped unless explicitly enabled; Python must import pydicom and NumPy. |
| Wide native samples | pydicom reads Go Big/Little Endian Part 10 and Big Endian JSON with 32/64-bit samples, signed/unsigned values and 8/16-bit icon items; pydicom authors Big Endian files that Go reads and converts | `DICOM_GO_PYDICOM_WIDE_ENDIAN=1 DICOM_GO_PYTHON=/path/to/python go test ./pixeldata -run PydicomWideSampleEndian -count=1 -v` | Skipped unless explicitly enabled; included in `make pydicom-interop`; requires exactly pydicom 3.0.2 and NumPy 2.1.3. |
| DICOM JSON text | pydicom compares dicom-go JSON with its own reading and serialization of the same Part 10 file: LT/ST/UT/UR backslashes and spaces, multi-valued VM, PN, nested items, absent/empty/padding-only | `DICOM_GO_PYDICOM_JSON_TEXT=1 DICOM_GO_PYTHON=/path/to/python go test ./dicomjson -run PydicomJSONText -count=1 -v` | Skipped unless explicitly enabled; included in `make pydicom-interop`. pydicom's `""` for empty components is compared as the normative `null`. |
| DICOM file-set | Independent pydicom read of an authored multi-patient DICOMDIR | `DICOM_GO_PYDICOM_DICOMDIR=1 DICOM_GO_PYTHON=/path/to/python go test ./dicomdir -run PydicomInterop -count=1 -v` | Skipped unless explicitly enabled; Python must import pydicom. |
| TID 1500 Measurement Report | highdicom 0.28.1 `srread` and `MeasurementReport` read a written report: observers, image library, TID 1501/1411 groups, tracking identifiers, measurements with derivation, source images, SCOORD3D, segment references and evidence | `DICOM_GO_HIGHDICOM=1 DICOM_GO_PYTHON=/path/to/python go test ./sr -run '^TestMeasurementReportReadsInHighdicom$' -count=1 -v` | Skipped unless explicitly enabled; Python must import highdicom. The dcmqi 1.5.7 `tid1500writer` report in `testdata/upstream/dcmqi/` is read offline by `TestReadMeasurementReportReadsDcmqi`. |
| DICOM ISO 2022 text | Bidirectional Part 10 roundtrip for Japanese, Korean, and mixed repertoires through pydicom 3.0.2; Chinese IR 58 is covered by the byte-exact current PS3.5 Annex K fixture because this gate does not qualify independent IR 58 ISO 2022 writing | `DICOM_GO_PYDICOM_CHARSET=1 DICOM_GO_PYTHON=/path/to/python go test ./object -run PydicomISO2022Interop -count=1 -v` | Skipped unless explicitly enabled; the Python environment must contain exactly pydicom 3.0.2. |
| Encapsulated archive round trip | DCMTK 3.7.0 storescu/getscu/movescu/storescp and strict dcmdump plus pydicom 3.0.2 frame hashes for RLE, JPEG-LS, JPEG Lossless and opaque JPEG 2000 through the reference archive | `DICOMGO_DCMTK_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./examples/referencearchive -run TestArchiveEncapsulatedIndependentPeers -count=1 -v` | Skipped unless explicitly enabled; DCMTK tools must be on PATH. The offline chain `TestArchiveEncapsulatedStoreGetMovePreservesCodestreams` always runs. |
| Deflated C-STORE | DCMTK 3.7.0 storescp preferring Deflated (`+xd`) selects it from a `StoreSession` proposal; dcmdump, dicom-go and pydicom 3.0.2 read every sample | `DICOMGO_DCMTK_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./net/dimse -run TestDeflatedStoreIndependentDCMTKPeer -count=1 -v` | Skipped unless explicitly enabled; DCMTK tools must be on PATH. |
| Deflated C-FIND and C-GET | DCMTK 3.7.0 dcmqrscp preferring Deflated (`+xd`) selects it for Study Root C-FIND and C-GET, matches the deflated identifiers sent by `SendCFindRequest`, `Find` and `SendCGet`, and returns the instance in a deflated C-STORE sub-operation | `DICOMGO_DCMTK_INTEGRATION=1 go test ./net/dimse -run TestDeflatedQueryRetrieveIndependentDCMTKPeer -count=1 -v` | Skipped unless explicitly enabled; DCMTK tools must be on PATH. |
| Heterogeneous C-STORE batch | DCMTK 3.7.0 storescp `+xa` persists CT, RLE MR, RT Dose/Plan/Structure Set, SR and ECG sent by `StoreSession` (with an unreadable file mid-batch); dcmdump, dicom-go and pydicom 3.0.2 compare every element with its source | `DICOMGO_DCMTK_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./net/dimse -run TestStoreSessionHeterogeneousBatchIndependentPeers -count=1 -v` | Skipped unless explicitly enabled; DCMTK tools must be on PATH. |
| Reversible de-identification | OpenSSL `cms -decrypt` recovers the Encrypted Attributes Data Set dicom-go encrypted (RSAES-OAEP and PKCS1 v1.5, AES-128/192/256, issuer-serial and SKI recipients), and `Reidentify` restores originals from OpenSSL `cms -encrypt` content (DER, streamed BER, OAEP SHA-1 with Triple-DES) | `DICOMGO_OPENSSL_INTEGRATION=1 go test ./deid -run OpenSSL -count=1 -v` | Skipped unless enabled; needs OpenSSL 1.1.1 or later (not LibreSSL) on PATH or in `DICOMGO_OPENSSL`. Tolerance: exact plaintext. DCMTK has no encrypted-attributes tool. |
| C-ECHO | Verification association against Orthanc | `DICOMGO_INTEGRATION=1 go test ./net/dimse -run '^TestInteropOrthancCEcho$' -count=1` | Skipped unless `DICOMGO_INTEGRATION=1`. |
| C-ECHO | Verification association against Horos | `DICOMGO_HOROS_INTEGRATION=1 go test ./net/dimse -run '^TestInteropHorosCEcho$' -count=1 -v` | Skipped unless `DICOMGO_HOROS_INTEGRATION=1`. |
| C-STORE | Local SCU/SCP integration and CLI smoke coverage | `go test ./net/dimse ./cmd/storescu ./cmd/storescp` | Always runs locally; no external peer required. |
| Incremental lossless STORE/GET/MOVE preparation (`net/storetranscode`) | Synthetic local peers compare complete selected pixels, SOP identity and negotiated syntax; original routes and later-frame failure before command are covered. | `go test ./net/storetranscode -run '^TestStoreIncremental(LoopbackSelectedAndOriginal\|LateFrameFailureBeforeCommand\|CGetSelectedAndOriginal\|CMoveNativePreparedPixels)$' -count=1 -v` | Local lossless little-endian, nondeflated integer-frame profile; no external peer required. The [36-run measurement](TRANSCODE_DATASET_PATH.md) observed lower lifetime RSS but higher preparation time in all six pairs; sampled temporary peaks are lower bounds. No live-PACS, Windows runtime or universal speedup claim. |
| Opt-in session-owned prepared STORE representations (`net/storetranscode`) | Synthetic local peers check explicitly authorized unknown-outcome retry, association rejection cleanup, source/Origin/binding invalidation before C-STORE and aggregate quota/lifetime behavior. | `go test ./net/storetranscode -run '^TestPreparedStore(UnknownRetryOnlyWhenAuthorized\|RejectedAssociationReleasesSession\|InvalidationBeforeCStoreCommand\|OriginChangeBeforeCStoreCommand)$' -count=1 -v` | Immutable built-in pure-Go bindings only; ordinary `NewSource` defaults remain unchanged. Complete retained bytes and source are revalidated on every attempt; no global cache or automatic Twin reuse. [Ownership and limits](CSTORE_TRANSCODE_SOURCE.md#explicit-session-owned-prepared-representations). A separate 12-worker two-frame 2×2 uint8 RLE/JPEG95 campaign checked actual encoder66 versus2, complete pixels/UID/size and cleanup; per-run costs and logical-storage/RSS limits are in the ownership guide. The incremental 36-run comparison remains historical. No live-PACS, native Windows runtime or universal speedup claim. |
| Study Root C-FIND | Local DIMSE C-FIND SCU/SCP and `findscu` tests | `go test ./net/dimse ./cmd/findscu -run '(C)?Find'` | Always runs locally; external peer row can be added when a stable fixture is available. |
| Study Root C-FIND | Horos Study Root C-FIND using the configured lab node | `DICOMGO_HOROS_INTEGRATION=1 go test ./net/dimse -run '^TestInteropHorosCFindStudyRoot$' -count=1 -v` | Skipped unless `DICOMGO_HOROS_INTEGRATION=1`; empty results are acceptable. |
| Query/Retrieve SCP (`cmd/qrscp`) | Library C-FIND, C-GET and C-MOVE clients against `qrscp` over synthetic folders; pynetdicom requestor (Study and Patient Root C-FIND, C-GET, C-MOVE to its own Storage SCP) | `go test ./cmd/qrscp`; `DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./cmd/qrscp -run Pynetdicom -count=1 -v` | Local tests always run; the pynetdicom test is skipped unless enabled. |
| Modality Worklist C-FIND | Typed Go SCU against pynetdicom SCP and pynetdicom SCU against streaming Go SCP | `DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./net/dimse -run '^TestModalityWorklistSC[UP]AgainstPynetdicom$' -count=1 -v` | Skipped unless explicitly enabled; validates multiple matches, nested SPS Sequence and final status in both directions. |
| HL7 v2 over MLLP (`hl7`, `net/mllp`) | Independent Python standard-library peer written from the framing rules: two ADT^A08 frames split at odd offsets to the Go server (AA and AE acknowledgments), and a Go client message acknowledged by a Python server | `DICOMGO_HL7_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./net/mllp -run TestMLLPInteropPythonPeer -count=1 -v` | Skipped unless `DICOMGO_HL7_INTEGRATION=1`; this independent standard-library peer qualifies its MLLP1 framing subset, while local tests cover message semantics in `docs/HL7_MLLP.md`. |
| HL7 MLLP1/MLLP2 (`net/mllp`) | Own synthetic Java harness using actual dcm4che MLLPConnection/InputStream/OutputStream classes at commit `acbbb85eee1a2fcc441eb2d8e65023fece2b40ec`: both directions, default MLLP1 and explicit MLLP2, transport confirmation separated from AA/AE/AR, NAK/malformed/raw/truncated confirmation, timeout and closure; negative injections use the real stream classes. | `DICOMGO_DCM4CHE_HL7_CP=/qualified/classes:/qualified/slf4j-api.jar DICOMGO_JAVA=/qualified/java DICOMGO_JAVAC=/qualified/javac go test ./net/mllp -run TestMLLPInteropDCM4chePeer -count=1 -v` | Unset classpath skips; explicitly unavailable or failing runtime fails. Java and reference classes are external, never a Go runtime dependency. Synthetic loopback is not proof for a production HL7 endpoint, universal MLLP2 requirements or durable HIS commit. |
| Study Root C-MOVE | Orthanc C-MOVE using seeded `STUDY_UID` and configured move destination | `DICOMGO_INTEGRATION=1 STUDY_UID=... go test ./net/dimse -run '^TestInteropOrthancCMoveStudy$' -count=1` | Skipped unless `DICOMGO_INTEGRATION=1`; fails fast if `STUDY_UID` is missing. |
| Negative controls (`internal/interopcheck`) | pynetdicom/pydicom tampering SCP answers success for C-STORE and C-GET while deleting an attribute, relabelling the syntax, altering the last sample, removing a frame or corrupting a sequence; the content check must reject each | `DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./internal/interopcheck -run Tampering -count=1 -v` | Skipped unless `DICOMGO_PYNETDICOM_INTEGRATION=1`; the local controls always run. Run the required peer gates individually using the commands in this table. |
| Print Management (`printmgmt`) | pynetdicom SCU (`meta_uid` commands) against the SCP with job pixel hashes, and the client (including a 2x2 `PrintFilm` sheet) against a recording pynetdicom SCP (grayscale) | `DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./printmgmt -run 'Pynetdicom' -count=1 -v` | Skipped unless `DICOMGO_PYNETDICOM_INTEGRATION=1`; subset and statuses in `docs/PRINT_MANAGEMENT.md`. |
| MPPS workflow (`mpps`) | pynetdicom SCU against the `mpps` SCP (lifecycle, 0110H/A710H, 0106H, 0112H) and the `mpps` client against a validating pynetdicom SCP | `DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./mpps -run 'Pynetdicom' -count=1 -v` | Skipped unless `DICOMGO_PYNETDICOM_INTEGRATION=1`; subset and statuses in `docs/MPPS.md`. |
| Instance Availability Notification (`ian`) | pynetdicom SCU against the `ian` SCP (success, 0111H duplicate, 0106H invalid availability, 0120H missing Study Instance UID) and the `ian` client against a recording pynetdicom SCP | `DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./ian -run 'Pynetdicom' -count=1 -v` | Skipped unless `DICOMGO_PYNETDICOM_INTEGRATION=1`; DCMTK has no IAN tool. Subset and statuses in `docs/INSTANCE_AVAILABILITY_NOTIFICATION.md`. |
| Generic normalized DIMSE | MPPS N-CREATE/N-SET with pynetdicom in both SCU and SCP directions | `DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./net/dimse -run 'PynetdicomMPPS' -count=1 -v` | Skipped unless `DICOMGO_PYNETDICOM_INTEGRATION=1`; `DICOMGO_PYTHON` is optional when `python3` can import pydicom and pynetdicom. |
| Asynchronous Operations Window | pynetdicom SCU offers `4/3`; Go SCP negotiates asymmetric `3/2` in requestor perspective and serves C-ECHO through `AsyncSession` | `DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./net/dimse -run '^TestAsyncSessionAgainstPynetdicomNegotiation$' -count=1 -v` | pynetdicom negotiates 0x53 but executes DIMSE synchronously; always-on Go stress/race tests qualify runtime windows greater than one. |
| Unified Procedure Step | Normative message harness over in-memory UL associations for Push/Pull/Watch and Query, including filtered-global subscribe, state/outbox/restart and concurrency tests | `go test ./ups -run 'UPS\|Query\|Watch\|FilteredGlobal\|ServiceClaims' -count=1` | Always runs locally; exercises real N-DIMSE/C-FIND command and dataset messages, bounded existing/future filter materialization, restart persistence and atomic limit rollback without a production worklist or PHI. |
| Storage Commitment Push Model | N-ACTION acceptance followed by a separate-association, partial-failure N-EVENT-REPORT from pynetdicom | `DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./net/dimse -run '^TestStorageCommitmentWorkflowAgainstPynetdicom$' -count=1 -v` | Skipped unless `DICOMGO_PYNETDICOM_INTEGRATION=1`; validates Event Type 2, role negotiation, callback correlation, and release of the original association. |
| Storage Commitment, reference archive as committer | pynetdicom requestor: C-STORE, N-ACTION, separate-association N-EVENT-REPORT (Event Type 2 then 1), C-GET content check | `DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./examples/referencearchive -run '^TestReferenceArchiveIndependentStorageCommitment$' -count=1 -v` | Skipped unless `DICOMGO_PYNETDICOM_INTEGRATION=1`; archive policy, durability and limits in `examples/referencearchive/README.md`. |
| Storage Commitment | Local primitive/service tests plus manual external workflow | `go test ./net/dimse -run 'StorageCommitment|NAction|NEventReport'` | Local tests always run; external peer check is documented manually in `docs/INTEROP_ORTHANC.md`. |

## DCMTK runtime oracles

DCMTK ships no image corpus for its codecs and display pipeline, so its
command-line tools are the practical oracle. Every oracle goes through
`internal/dcmtkoracle`: it runs only with `DICOMGO_DCMTK_INTEGRATION=1`, finds
each tool in `DICOMGO_DCMTK_BIN` or on PATH, skips explicitly when a tool is
missing, and logs the tool version (the oracles were written against DCMTK
3.7.0, the local `../dcmtk` checkout at `8d68392d`), every command line and the
tolerance of the comparison made with its output. Lossy results are never
compared byte for byte. Run them all with the DCMTK tools on PATH:

```sh
DICOMGO_DCMTK_INTEGRATION=1 go test ./... -run 'DCMTK|DCM|DSRDump|DCIODVFY' -count=1 -v
```

| Area | Tools | Test | Tolerance |
|---|---|---|---|
| RLE Lossless | `dcmdrle`, `dcmcrle` | `pixeldata/rle` `TestRLEEncoderAgainstDCMTK`, `TestRLEDecoderAgainstDCMTK` (8/16/32 bits, signed, RGB, multi-frame) | exact samples |
| JPEG Lossless encoder | `dcmdjpeg` | `pixeldata/jpeglossless` `TestJPEGLosslessEncoderAgainstDCMTK` (every predictor, 2-16 bits, gray and RGB) | exact samples |
| JPEG Lossless workarounds | `dcmdjpeg +w6`, `+wc` | `pixeldata/jpeglossless` `TestWorkaroundsMatchDCMTK` | exact samples |
| JPEG decoders | `dcmcjpeg`, `dcmdjpeg` | `pixeldata/jpeg` `TestDecodersAgainstDCMCJPEGLossless` (SV1 and selection values 1-7); `TestDecodersAgainstDCMCJPEGLossy` (baseline, extended 12-bit, retired 8-bit spectral selection `+es` and full progression `+ep`); `TestDecodersAgainstDCMCJPEG12BitProgressive`; `TestProgressive12DCMTKQualification` (`DICOMGO_JPEG_PROGRESSIVE12_INTEROP=1`, also needs cjpeg) | lossless exact (DCMTK's 16-bit SOF3 for 12-bit data decodes with `DecodeRecovery.ReconcileWithCodestream`); lossy within 1 level (gray) or 2 (RGB) of `dcmdjpeg`, PSNR >= 35 dB; qualified unsigned mono/RGB12 4:4:4 progressive within 2 levels of IJG12; [corpus and limits](JPEG_PROGRESSIVE12.md) |
| JPEG lossy color policy | `dcmdjpeg` (streams from libjpeg `cjpeg`) | `pixeldata/jpeg` `TestLossyJPEGAgainstDCMTK` | within 1-2 per sample |
| JPEG-LS | `dcmcjpls`, `dcmdjpls` | `pixeldata/jpegls` `TestDecoderAgainstDCMCJPLS` (raw/cooked, line/sample interleave, LSE thresholds, zero padding, NEAR 2 and 5), `TestIndependentDecoderAcceptsGeneratedCodestreams` (encoder output including RGB line and sample interleaved, `DICOM_GO_JPEGLS_INDEPENDENT=1`), `TestJAIStreamWithCharLS` | lossless exact; near-lossless exact against `dcmdjpls` and within NEAR of the original |
| Encapsulated JPEG | `img2dcm`, `dcmdjpeg` | `encapimage` `TestFromJPEGMatchesDCMTK` | Image Pixel attributes and bitstream identical |
| Windows and VOI | `dcm2pnm` | `pixeldata/display` `TestWindowEdgesAgainstDCMTK` | within 1 display level |
| LUT encodings | `dcm2pnm` | `pixeldata/display` `TestLUTEncodingsAgainstDCMTK`, `TestLUTFirstValueSignAgainstDCMTK` | within 1 output level |
| Color | `dcm2img` | `pixeldata/display` `TestColorAgainstDCMTK` | within the per-channel bound of each case |
| Automatic windows | `dcm2img +Wm/+Wn/+Wh/+Wr` | `render` `TestAutoWindowAgainstDCMTK` | exact |
| GSDF, CIELAB and LIN OD | `dcm2img +Dm/+Dp` (and `+Dc` for CIELAB) with `monitor.lut`, `printer.lut` | `pixeldata/display` `TestGSDFAgainstDCMTK` | exact; LIN OD within 1 DDL |
| Overlays | `dcm2pnm` | `pixeldata/display` `TestOverlaysAgainstDCM2PNM` (clipped and negative-origin planes) | overlays exact on dcm2pnm's windowed image |
| GSPS | `dcmp2pgm`, `dcmpschk`, `dcmpsmk` | `gsps` `TestRenderMatchesDCMP2PGM`, `TestFromImageMatchesDCMPSMK` | within 1 level; generated states accepted |
| Structured Reporting | `dsrdump` and DCMTK-built oracles | `sr` `TestRelationshipConstraintsMatchDCMTK`, `TestDocumentLifecyclePassesDSRDump`, `TestMeasurementReportPassesDCMTKChecks`, `TestWriteMeasurementReportMatchesDCMTK` | accepted without warnings; relationship triples equal |
| Digital signatures | `dcmsign` | `dsig` `TestDCMSignSignaturesVerify`, `TestSignaturesVerifyWithDCMSign`, `TestDCMSignTrustSamples` (committed samples: `TestDCMSignTrustFixtures`) | verifies with the requested MAC and profile; tampering rejected; trusted, untrusted, expired and revoked signer certificates classified as `dcmsign --verify +cf +uf +cr +cl` does |
| Certified timestamps (RFC 3161) | `openssl ts`, `dcmsign +ts`/`+t` | `dsig` `TestOpenSSLTimestampSamples` (`DICOMGO_OPENSSL_INTEGRATION=1`; committed sample: `TestOpenSSLTimestampFixture`), `TestDCMSignTimestamps` (also `DICOMGO_DCMTK_INTEGRATION=1`) | OpenSSL tokens for requests built here verify and are trusted at the certified time; `openssl ts -verify` accepts the in-process test TSA; dcmsign-inserted tokens verify here and files timestamped here pass `dcmsign --verify +tr`; a changed token is refused |
| Data set encoding | `dcmconv`, `dcmodify`, `dcmdump`, `dcmmkdir` | `object` `TestWriteLengthPoliciesMatchDCMTK`, `TestConvertTextToCharacterSetMatchesDCMTK`, `TestUNRecoveryIndependentOracles`; `cmd/dcmodify` `TestDcmodifyMatchesDCMTK`; `dicomdir` `TestDICOMDIRProfilesMatchDCMTK`; `internal/dicomconv` `TestConversionsMatchDCMTK` | exact, or as each test states |
| Worklist fixtures | `dump2dcm`, `wlmscpfs`, `findscu` | `net/dimse` `TestDCMTKWorklistOracle` (in-memory handler, and the directory handler over wlmscpfs's own `.wl` files) | exact matches |
| Network | `storescu`, `storescp`, `echoscu`, `getscu`, `movescu`, `dcmqrscp` | `net/dimse` `TestTLSProfilesWithDCMTK` (BCP 195 profiles, refused NULL/AES-CBC), `TestAdmissionRejectionReachesDCMTK`, `TestProviderAbortReachesDCMTK`, `TestPresentationContextRulesWithDCMTK`, `TestLargeStoreSendPDUWithDCMTK` (PDU sizes), `TestDeflatedStoreIndependentDCMTKPeer`, `TestDeflatedQueryRetrieveIndependentDCMTKPeer`, `TestStoreSessionHeterogeneousBatchIndependentPeers`, `TestRetrieveProxyDCMTKPeers` (retrieve proxy between `dcmqrscp` and `movescu`/`getscu`); `cmd/storescp` `TestStorescpReceivesDeflatedFromDCMTKStorescu` (Deflated instances from `storescu -xd` on one association) | protocol outcome |
| Query/Retrieve SCP | `findscu`, `getscu`, `movescu` | `cmd/qrscp` `TestDCMTKRequestorsAgainstQRSCP` | protocol outcome (response and stored file counts) |

`TestConcatenationsMatchDCMTK`, `TestSegmentationsMatchDCMTK` and
`TestMKReportDocumentsMatchDSRDump` also need a small program built against
the DCMTK libraries (`DICOMGO_DCMTK_CONCAT`, `DICOMGO_DCMTK_SEGORACLE`,
`DICOMGO_DCMTK_MKREPORT`); see each test.

## JPEG 2000 and HTJ2K encoder oracles

`DICOMGO_J2K_ORACLE=1` decodes the output of the pure-Go
[JPEG 2000 and HTJ2K encoder](JPEG2000_ENCODER.md) with OpenJPEG and OpenJPH.
`DICOM_GO_OPENJPEG_DECOMPRESS`, `DICOM_GO_OPENJPH_EXPAND` and
`DICOM_GO_OPENJPH_COMPRESS` name the executables (otherwise PATH). When the
gate is enabled, a missing tool fails the test.

```sh
DICOMGO_J2K_ORACLE=1 go test ./pixeldata/jpeg2000 -run Independent -count=1 -v
(cd examples/codec-adapters/jpeg2000 && DICOMGO_J2K_ORACLE=1 go test -run OpenJPHEncoded -count=1 -v)
```

| Area | Tools | Test | Tolerance |
|---|---|---|---|
| Lossless encoder (`.90`, `.201`, `.202`) | `opj_decompress` 2.5.4, `ojph_expand` 0.32.0 | `pixeldata/jpeg2000` `TestIndependentDecodersReproduceLosslessCodestreams` (1-16 stored bits, signed and unsigned, RGB with and without RCT, 1x1 to 257x131) | exact samples |
| Lossy encoder (`.91`, `.203`) | `opj_decompress` 2.5.4, `ojph_expand` 0.32.0 | `pixeldata/jpeg2000` `TestIndependentDecodersAgreeOnLossyCodestreams` | within 1 level of the float64 quantization model (2 at 16 bits) |
| Core HTJ2K decoder | `ojph_compress` 0.32.0 | `examples/codec-adapters/jpeg2000` `TestCoreDecodesOpenJPHEncodedHTJ2K` (LRCP and RPCL) | exact samples |

## Part 10 capability and skip profile

The independent Part 10 gate qualifies only semantics that both sides exercise;
opening File Meta Information does not imply that a pixel codec was decoded.
The profile is therefore explicit:

| Representation | Gate status | Capability reference |
|---|---|---|
| Implicit VR Little Endian, Explicit VR Little Endian and retired Explicit VR Big Endian | Covered bidirectionally, including endian-sensitive values. | [`CONFORMANCE.md`](CONFORMANCE.md#supported-transfer-syntaxes) |
| Undefined-length sequences, UTF-8 (`ISO_IR 192`) and native 16-bit Pixel Data | Covered bidirectionally with synthetic, non-PHI values. | [`CONFORMANCE.md`](CONFORMANCE.md#supported-transfer-syntaxes) |
| ISO 2022 Japanese, Korean and mixed repertoires | Covered by the existing pydicom 3.0.2 gate; Chinese IR 58 remains covered by the byte-exact PS3.5 fixture because this gate does not qualify independent IR 58 ISO 2022 writing. | [`CHARACTER_SETS.md`](CHARACTER_SETS.md) |
| RLE Lossless | Covered by the existing pydicom pixel-array comparison and included by `make pydicom-interop`. | [`PIXEL_TRANSCODING.md`](PIXEL_TRANSCODING.md#verification) and [`CODEC_CAPABILITY_MATRIX.md`](CODEC_CAPABILITY_MATRIX.md) |
| JPEG Baseline/Extended/Lossless and JPEG-LS | Explicitly skipped by this Part 10 gate. pydicom pixel decoding depends on separately versioned handlers, while dicom-go qualification is codec- and metadata-subset-specific. | [`CODEC_CAPABILITY_MATRIX.md`](CODEC_CAPABILITY_MATRIX.md) |
| JPEG 2000, HTJ2K and JPEG XL families | Explicitly skipped by this Part 10 gate because release qualification requires the named optional or `codecfull` runtime and its independent corpus. | [`CODEC_CAPABILITY_MATRIX.md`](CODEC_CAPABILITY_MATRIX.md) and [`CODECFULL_PROFILE.md`](CODECFULL_PROFILE.md) |
| Deflated Image Frame Compression (`1.2.840.10008.1.2.8.1`) | Explicitly skipped by this Part 10 gate (the pinned pydicom runner has no handler for it). Qualified instead by the builtin codec's exact evidence: raw-deflate vectors written by C zlib (Python 3 `zlib`, `wbits=-15`) decode bit-exact, every encoded frame inflates through Go `compress/flate` to the native frame, and native/RLE/JPEG-LS transcodes into and out of the syntax are bit-exact. Dataset-level Deflated Explicit VR Little Endian is a distinct feature. | [`CONFORMANCE.md`](CONFORMANCE.md#supported-transfer-syntaxes) and [`CODEC_CAPABILITY_MATRIX.md`](CODEC_CAPABILITY_MATRIX.md) |
| Video and JPIP data-bin streams | Explicitly skipped as unsupported Part 10 pixel roundtrip representations. JPIP server/client data-bin interoperability is checked separately against OpenJPEG ([`JPIP_SERVER.md`](JPIP_SERVER.md#validation)). | [`CONFORMANCE.md`](CONFORMANCE.md#video-jpip-and-streaming-transfer-syntax-policy) |

The skip rows are capability statements, not successful tests. A compressed
syntax moves into this gate only with a pinned independent decoder, a synthetic
redistribution-safe fixture, and the matching codec-profile evidence.

## Pinned pydicom runner

Create an isolated environment and run the complete independent matrix from
`dicom-go`:

```sh
python3 -m venv .interop-pydicom
.interop-pydicom/bin/python -m pip install --requirement scripts/requirements-pydicom-interop.txt
make pydicom-interop PYDICOM_INTEROP_PYTHON=.interop-pydicom/bin/python
```

On Windows, use `.interop-pydicom/Scripts/python.exe`. The Make target verifies
the exact pydicom and NumPy versions before enabling any opt-in Go test. The
same disposable environment can be prepared locally on any supported platform
without adding Python to the ordinary offline baseline. These are local opt-in
commands; no GitHub Actions workflow is required.

## Matrix Runner

`scripts/interop_orthanc_matrix.sh` is the opt-in network runner:

```bash
DICOMGO_INTEGRATION=1 ./scripts/interop_orthanc_matrix.sh
```

It runs C-ECHO first. If `STUDY_UID` is set, it also runs Study Root C-MOVE.
Without `DICOMGO_INTEGRATION=1`, the script exits successfully after printing a
skip message.

Horos uses a separate opt-in gate so it can be run without requiring Orthanc:

```bash
DICOMGO_HOROS_INTEGRATION=1 go test ./net/dimse -run '^TestInteropHoros' -count=1 -v
```

See `docs/INTEROP_HOROS.md` for Horos environment variables, peer setup, and
troubleshooting notes.

## Skip Policy

- Go tests that require the Orthanc peer must call `testutil.SkipIfIntegration`.
- Go tests that require the Horos peer must skip unless
  `DICOMGO_HOROS_INTEGRATION` is set.
- Go tests that use DCMTK tools must find them with `dcmtkoracle.Tool` (or
  `Lookup` for a fallback oracle) and run them with `dcmtkoracle.Run` or
  `Command`, stating the tolerance.
- Missing external fixtures should skip or fail with a clear required variable,
  not silently pass a partial external check.
- New external rows must document peer setup, environment variables and expected
  failure modes before being enabled by script.


## Additional bounded profiles

- Native print LUT/annotation fixtures use qualified pydicom/pynetdicom peers
  and DCMTK print tools in both directions. DCMTK's SCP does not support Basic
  Annotation Box; that subset is covered by its SCU and the independent Python
  peer. See [print profile](PRINT_MANAGEMENT.md); no hardcopy acceptance is claimed.
- Vector confidence uses an explicitly qualified ITK 5.4.5 oracle for eight regular
  1/2-channel, 2D/3D, zero/two-refit population fixtures. Sample covariance remains
  the API default. `DICOM_GO_VECTOR_CONFIDENCE_ITK=1` and a qualified
  `DICOM_GO_VECTOR_CONFIDENCE_ITK_BIN` enable `TestVectorConfidenceITKOracle`.
  See [scope and oracle limits](VECTOR_CONFIDENCE_GROWING.md).
- Detailed SR measurement context uses bounded `dsr2xml +Wt` / `xml2dsr` /
  `dcmdump` preservation with independent XML parent/relation checks. Template
  identification preservation requires `+Wt`; the explicit per-NUM RWVM extension
  remains declared. See [measurement context](SR_MEASUREMENT_CONTEXT.md); this
  synthetic finite corpus is not complete SR IOD or PACS qualification.
- Incomplete Pixel Data recovery has two pinned GDCM compressed-payload MD5
  comparisons. Three historical native fixture acceptance items remain absent;
  see [incomplete input profile](INCOMPLETE_PIXEL_DATA.md).

- Masked physical rank filtering uses a pinned ITK 5.4.5 public API harness with
  96 presealed cases: 72 native clipped profiles and 24 explicit global-padding
  replication adapter profiles. All 12096 binary32 value-bit/validity entries
  match exactly; no cases or tolerance are omitted. The adapter does not claim
  native ITK replication. See [masked rank qualification](MASKED_RANK_FILTERS.md)
  and its [fixture contract](../imagefilter/testdata/masked_rank/README.md).
  The separate fixed Go 1.27.1 darwin/arm64 cost campaign retained all 144 rows;
  eight integer fixture medians were faster (time-ratio geometric mean 0.441896),
  while all fractional profiles reported 0.34–14.95% overhead. Shared archive
  I/O overlapped; these descriptive fixtures are not a general speedup claim.
