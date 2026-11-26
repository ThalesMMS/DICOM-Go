# Value domains for window controls

`pixeldata/display.WindowDomain` describes the finite values entering VOI, their
unit and integer/rescale quantum. `RescaledWindowDomain` uses the resolved
BitsStored/sign/slope/intercept without reparsing DICOM metadata. Negative slopes
reverse the interval; missing/invalid metadata returns an unknown domain.
`MeasuredWindowDomain` summarizes finite calibrated values, leaving non-finite
samples out. It performs no RWVM selection or new calibration.

`parametricmap.Map.WindowDomain` applies the map's already resolved per-frame
mapping and padding semantics to at most 4096 deterministic positions, including
both ends. It allocates only that bounded sample list and does not populate the
frame cache. It returns a sampled useful interval, not complete-frame extrema or
a replacement for `FrameValues`/quantitative statistics. Samples whose
mapping overflows are omitted. Other invalid mapping errors propagate. Call it
during preparation, never as a volume scan on every gesture.

`ManualWindowMinimum` preserves DICOM VOI semantics: LINEAR width≥1;
LINEAR_EXACT/SIGMOID width>0, with a representable positive floor derived from
the known magnitude and interval. This API provides domain/minimum semantics;
application interaction sensitivity, editor limits and unit labels belong in
Twin's shared viewer core. No 3D sampling, CPU fallback or display calibration is
added. Tests cover negative fractional rescale, float domains, padding/non-finite
values, empty/constant intervals and bounded lazy mapping. They are authored but
not executed in the current private preparation.
