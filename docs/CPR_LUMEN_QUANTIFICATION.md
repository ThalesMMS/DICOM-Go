# CPR lumen quantification

`roi.AnalyzeCPRLumen` samples transported transverse planes on an immutable CPR
path. The source-modality interval is explicit. A four-connected component seeded
at the centreline excludes disconnected structures. Threshold score contours
use marching squares with scalar subpixel interpolation. Signed contour areas
subtract holes; rotating calipers measure minimum supporting-line width and
maximum Feret diameter on the outer convex hull, in millimetres.

The reference is sampled at the exact requested arc length, even when it lies
between regularly spaced sections. Diameter reduction is
`100*(1-minimumFeret/referenceMinimumFeret)`; area reduction is
`100*(1-area/referenceArea)`. Negative values represent enlargement relative to
the reference and remain visible. Reference selection does not automatically
identify a normal segment or implement NASCET/WASID clinical selection rules.
The [original NASCET measurement study](https://pubmed.ncbi.nlm.nih.gov/8808967/)
uses a normal distal cervical ICA denominator; this generic analysis reports
its selected-reference geometry instead.

Each section has explicit validity. Missing centreline seeds, source/FOV gaps,
or components touching the analysis boundary produce gaps, not invented zero
areas or 100% stenosis. An invalid reference refuses relative measurement.
Limits: 512 sections, 513 samples per grid side, 16 million total scalar samples,
and one million retained contour points. Source-store memory is separate.
Cancellation is checked during source preparation, section sampling and flood
segmentation. This is numerical data/geometry analysis; it produces no 3D image.

`CPRLumenProfile.MeasurementReport` builds Comprehensive 3D SR TID 1500 groups.
Standard vessel-area, diameter/minimum/maximum and lumen-stenosis concepts use
UCUM mm2/mm/% units, following the concepts in [DICOM Vessel Measurements](https://dicom.nema.org/medical/dicom/current/output/chtml/part16/sect_TID_3907.html).
Private analysis parameters carry source thresholds and units, reference arc,
reference area/minimum diameter, grid spacing, radius and basis rotation.
Invalid sections carry validity markers without fabricated numeric measures.
SCOORD3D evidence records section centres and closed outer contours; excluded
hole area is listed separately. The application adds source-image evidence and
inherited Patient/Study context before publication.

Synthetic qualification includes a 0.5 mm partial-volume CT phantom with 6->3 mm
radius, 50% diameter/75% area narrowing, exact 1.3 mm reference position, oblique
elliptical planes, independent all-pairs Feret oracle, source-boundary rejection,
active cancellation, memory limits and SR round trips. These tests are not a
clinical segmentation or stenosis-grading validation.
