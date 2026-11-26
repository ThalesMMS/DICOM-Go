# dcmqi TID 1500 fixture

`tid1500.dcm` is an Enhanced SR TID 1500 Measurement Report written by
dcmqi's `tid1500writer` (dcmqi 1.5.7, tag commit
`506306a0980876058a0f2060c645b74d23cdc555`, the official `win64` release
binary) from synthetic inputs only: the metadata
[`sr/testdata/dcmqi_tid1500.json`](../../../sr/testdata/dcmqi_tid1500.json),
which follows dcmqi's `doc/examples/sr-tid1500-ct-liver-example.json` and
`doc/schemas/sr-tid1500-schema.json`, and a three-slice 4x4 CT series with a
binary Segmentation of it made by
[`sr/testdata/dcmqi_tid1500_inputs.py`](../../../sr/testdata/dcmqi_tid1500_inputs.py)
(pydicom 3.0.2, highdicom 0.28.1). The patient is `Synthetic^Phantom`; there
is no patient data. No dcmqi code or data is copied. dcmqi is BSD-3-Clause
([LICENSE.dcmqi](LICENSE.dcmqi)); provenance is in
`THIRD_PARTY_PROVENANCE.json`.

| File | dcmqi | Consumed by | Comparison |
|---|---|---|---|
| `tid1500.dcm` | `tid1500writer` | `sr` `TestReadMeasurementReportReadsDcmqi` | `ReadMeasurementReport` reads the language, person observer, procedure reported, the three-image library with its study and series, and two TID 1411 groups (tracking identifier and UID, finding, finding site, referenced segment with study and series, source series, and NUM values, units and derivations) equal to the metadata |

Regenerate, with dcmqi's `bin` directory and a Python with pydicom and
highdicom:

```sh
python sr/testdata/dcmqi_tid1500_inputs.py "$TMP/in"
tid1500writer --inputMetadata sr/testdata/dcmqi_tid1500.json \
    --inputImageLibraryDirectory "$TMP/in" \
    --inputCompositeContextDirectory "$TMP/in" \
    --outputDICOM testdata/upstream/dcmqi/tid1500.dcm
```

The inputs are deterministic, but dcmqi generates the SR's SOP Instance and
Series Instance UIDs, the Tracking Unique Identifier of the second group and
the content date and time, so a regenerated file differs in those values (and
in its SHA-256 in the manifest); the test does not depend on them.
