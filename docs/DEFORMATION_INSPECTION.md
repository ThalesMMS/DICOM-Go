# Direct displacement-field inspection contracts

`spatialreg.Registration.InspectionField` exports owned original grid nodes for
GPU presentation. It performs no resampling, registration inversion or mesh
generation. The descriptor in `render.DeformationFieldDescriptor` specifies
the registered-to-source mapping `Post(Pre(p) + D(p))`. The grid is sampled at
the original registered point `p`, before Pre. Displacements use patient LPS
millimetres, including anisotropic and oriented grids.

The x-fastest payload holds four little-endian float32 values per node. XYZ
are original displacements; W is 1 for a defined node. Undefined DICOM triples
become XYZ=0/W=0, distinct from a defined zero displacement XYZ=0/W=1.
Only contributing interpolation corners participate in validity. GPU consumers
must omit an undefined sample and connected edges, rather than inventing a zero.
Partly non-finite, overflowing and subnormal vectors are refused explicitly.

`render.DeformationInspection` specifies a regional index box (start inclusive,
end exclusive), requested stride, instance budget, visual amplification and
displayed-displacement cap. The deterministic plan increases a common stride
until its lattice fits the budget. Arrows and the deformed grid must use the
same displayed displacement. Physical displacement/magnitude are evaluated
before visual amplification or clipping. The original-grid reference stays
undeformed. Zero at the admitted presentation precision remains distinct from
undefined data.

Limits are 2,097,152 original nodes, dimensions up to 2048 per axis, and 32,768
instances. Visual amplification is bounded to 0.000001–100, the displayed cap
to 0.0001–10,000 mm. The descriptor requires identified frames and finite,
invertible affine pre/post and lattice matrices. These contracts describe a
direct mapping; an inverse or registration chain needs its own qualified
contract and cannot be obtained by merely negating the vectors.

