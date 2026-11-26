# Conventional RT Plan aperture inspection

`Plan.InspectAperture(ctx, beamNumber, controlPoint, limits...)` returns owned
geometry for one stored conventional control point. It reuses the admitted
RT Plan model and its finite resource limits, validates the model and refuses
invalid geometry without returning a partial drawing contract.

The coordinate contract is **IEC BEAM LIMITING DEVICE, machine isocentric
plane, millimetres**. It follows
[PS3.3 C.8.8.14](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.14.html).
No IEC-to-patient transform, anatomical projection or patient beam's-eye view is
implemented. The diagram uses +X to the right, +Y up, and equal axis scales.

| Stored content | Typed representation | Inspection behavior |
| --- | --- | --- |
| Block Data | `Block.Contour [][2]float64` | Ordered closed polygon; retained independently of jaws/MLC |
| Leaf/Jaw Positions | `ControlPoint.DevicePositions` | Effective stored state; no interpolation |
| Explicit device items at this CP | `ControlPoint.EncodedDeviceTypes` | Distinguishes declared MLC changes from inherited jaws |
| Jaws | `ApertureGeometry.Jaws` | Axis and both bank positions |
| MLC leaf-pair openings | `ApertureGeometry.Leaves` | Gaps clipped by available jaws; MLCY swaps travel/across axes |
| CP weight and fraction meterset | `State.CumulativeMetersetWeight`, `FractionMetersets` | Nil stays unavailable; stored fraction values retain their unit |
| Area | `DerivedArea`, `AreaBasis` | Derived jaw/MLC area, explicitly excluding blocks |

Nonempty Block Data must contain exactly twice Block Number of Points. The
reader validates DS syntax/length, finite coordinates, closed-polygon count,
unique block identifiers and APERTURE/SHIELDING type. Thickness/transmission
ranges and equal specified values across APERTURE blocks are checked according
to [C.8.8.14.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.14.4.html).
Structural problems produce `*ValidationError` with attribute paths, wrapping
`ErrInvalidObject`. Existing metadata/model budgets apply before numeric copies.

Empty or absent Type 2 outlines remain unavailable, with an explicit warning
and inspector limitation. No outline is invented. Block contours are drawn
individually with their type; composite aperture union, transmission and
block-corrected area are not calculated. The library performs no dose calculation,
delivery verification, plan editing or treatment simulation.

The original stored Block Sequence survives `object.WriteFile` and readback.
`WritePlan` admits the separate
[conventional photon profile](RT_FAMILY_MODELS.md#conventional-photon-writing)
for static and arc jaw/MLC beams. Authoring block contours remains outside
that profile and returns `ErrUnsupportedFeature`.


Both shells' existing RT Plan Summary window includes **Conventional aperture**.
Choose a plan and beam, then use **Previous control point** / **Next control
point**. The pane shows index, cumulative weight, fraction meterset, available
machine fields, jaws, leaf-pair count, block contours and the separately labelled
derived area. Missing fields stay unavailable; inherited device positions and
angles are labelled as inherited. The Summary and Ion/Brachy tabs remain present.

Source loading, model validation, geometry and the 2D diagram run on background
workers. Selection changes invalidate old deliveries and clear the old drawing;
closing cancels the worker. A structurally refused source is shown in Summary
and cannot supply the aperture pane. The 2D diagram does not invoke application
3D rendering. A non-finite or unrepresentable plot extent is refused rather than
showing collapsed geometry.

## Offline validation

Synthetic fixtures contain no patient data. Tests exercise asymmetric contour
serialization/readback, malformed DS and counts, absent outlines, normative
block relations, exact MLCX/MLCY coordinates and clipping, ownership, bounded
inspection and cancellation. Existing conventional/family/writer tests remain:

```sh
go test ./rtplan -count=1
```


```sh
go test ./internal/twinapp -run '^TestRTPlanReportsPreserveAperture' -count=1
go test ./internal/ui/shared -run '^(TestRTAperture|TestRTInspector)' -count=1
go test ./internal/ui/macstyle/database ./internal/ui/windowsstyle -run '^(TestMacRTPlan|TestWindowsRTPlan|TestMacRTFamily|TestWindowsRTFamily)' -count=1
```

The real shared controls navigate CP0/CP1/back, changing jaws, MLC image, weight
and derived area from 2400 to 800 mm²; a cleared plan cannot retain a stale
drawing. A separate pixel fixture checks axis signs, equal scale, jaw lines and
polygon closure. These are Windows/amd64 Fyne software-driver tests, serialized
to model the native UI thread contract. They do not qualify native macOS/Windows
GUI execution, patient projection or clinical use.
