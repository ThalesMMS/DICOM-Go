# Own quirk fixtures

These 29 DICOM carriers were authored by `internal/quirkcorpus` under the
repository license. They contain synthetic identifiers and tiny artificial
pixel/overlay/sequence payloads. No vendor patient data, gdcmData files,
gdcmConformanceTests files or third-party codec source was imported.

[manifest.json](manifest.json) records each phenomenon, explicit recovery policy,
strict/recover stage expectations, metadata/pixel oracle, size and source hash.
The [corpus guide](../../docs/GDCM_QUIRK_CORPUS.md) defines the checks and limits.
Large named workloads are small declaration-budget analogues only.

`gdcm_oracle.cpp` and `verify_gdcm.py` are own test harnesses around separately
installed public GDCM APIs. `gdcm-3.2.5-windows.json` records their Windows result,
including rejection, native process failure, semantic differences and skipped
declared-large workloads. It is not an all-files equivalence or support claim.
