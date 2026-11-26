# Public ITK transform interchange fixtures

These are own synthetic transforms generated through the public APIs of
SimpleITK 2.5.3, reporting ITK 5.4, on Python 3.14.7. No patient data, upstream
implementation, header or private implementation source is used. The fixture
JSON records parameters, fixed parameters, matrices where exposed, points,
component order and complete public-writer files with SHA256 and byte lengths.
It is bounded to 8 MiB. HDF5 generation can contain runtime metadata; semantic
results and the frozen generation hashes are the authority, not reproducibility
of unrelated timestamps or platform metadata.

The 25 cases yield 68 files: text and HDF5 for every case, and MATLAB for the
18 supported single linear cases. Cases include the three new 3D types, all
four native 2D types, Euler3D ZXY/ZYX and other legacy linears, singular and
reflected forward maps, versor clamp, native composites, nontrivial oblique
B-spline/displacement geometry and mixed composites. Points cover interior,
support boundaries and identity extension. The public composite queue is
reversed exactly once to Go application order; optimization flags are not
serialized. Composite2D refuses repeated component references: use Clone for
independent parameter blocks, preserving parameter/Jacobian/Clone consistency.
The 2D types do not supply a registration engine, 3D lifting or
DICOM mapping.

The Go tests read these frozen public files without Python. They independently
compare native metadata and mapped points against the public API outputs, then
write and reread their own outputs. An optional campaign output directory does
not replace or skip those assertions. The external script requires the pinned
ITK runtime, reads the actual Go files and compares them to these frozen public
expectations, writes them again through ITK and permits a second Go read.

For an isolated runtime containing the public dependencies:

```sh
PYTHONPATH=/path/to/public-python python3 generate_public_api.py
ITK1345_OUTPUT_DIR=/tmp/itk1345-go go test ./registration -run TestITK1345ExportInterchange -count=1
PYTHONPATH=/path/to/public-python python3 registration/testdata/itk1345/verify_go_outputs.py --go-output /tmp/itk1345-go --rewrite /tmp/itk1345-rewrites --report /tmp/itk1345-public.json
ITK1345_INPUT_DIR=/tmp/itk1345-rewrites go test ./registration -run TestITK1345ReadPublicRewrites -count=1
```

Run generation from this directory, and Go/campaign commands from the module
root. h5py is used only for independent public HDF5 inspection; it is not a Go
runtime dependency. Qualification used h5py 3.16.0/HDF5 2.0.0. The external
script rejects a missing/mismatched ITK runtime instead of reporting a skip.

The writer's admitted subset is classic superblock v0, eight-byte offsets and
lengths, object headers v1, local heaps and multi-level TREE/SNOD symbol tables,
rank-one contiguous IEEE little-endian float64 and bounded ASCII VLEN metadata.
Each string is at most 1024 bytes with one fixed 4096-byte GCOL; no arbitrary
string arrays, nested VLEN, general datasets, compression or append are exposed.
The complete encoded EOF is checked against MaxBytes (zero defaults to 256 MiB)
before writing or scanning dense values. All Write calls are at most 64 KiB.
IO/cancellation can leave partial output; the budget is not an RSS cap and
cannot interrupt a blocking caller Writer.

Use the New constructors for linear transforms. The HDF5 writer checks the
concrete family and reconstructs only small built-in linears from their native
parameters/fixed parameters; it refuses a state that changes matrix or offset
on reread, before the first Write. Its per-coefficient tolerance is exactly
32 float64 epsilons times max(1, |actual|, |reconstructed|), a roundoff-level
check. This is not a uniform absolute point-error bound at unbounded world
coordinates. Zero-value Euler3D (stored zero matrix, native angles zero) and
uninitialized ScaleSkewVersor3D are examples of refused noncanonical states.
A versor update exactly at pi can also need refusal because its native setter
clamp would change its geometry; an explicit affine representation retains
the finite forward mapping. Constructed finite zero/negative scale mappings
remain valid. Dense transforms are not cloned/reconstructed by this check;
their geometry/array admission remains separate. Existing 3D classes and
historical text/MATLAB behavior are unchanged.

The pinned public ITK writer emits ASCII VLEN strings. Fixed ASCII, fixed UTF8
and VLEN UTF8 experiments were rejected by its reader's requested ASCII VLEN
conversion. The accepted output uses charset zero, independently verified by
h5py and the real ITK reader. No generic UTF8 interoperability is claimed.
The public [HDF5 format specification](https://support.hdfgroup.org/documentation/hdf5/latest/_f_m_t3.html)
defines the file layout. Public behavioral references are the ITK
[Translation](https://docs.itk.org/projects/doxygen/en/v5.4.0/classitk_1_1TranslationTransform.html),
[ScaleVersor3D](https://docs.itk.org/projects/doxygen/en/v5.4.0/classitk_1_1ScaleVersor3DTransform.html)
and [ScaleSkewVersor3D](https://docs.itk.org/projects/doxygen/en/v5.4.0/classitk_1_1ScaleSkewVersor3DTransform.html)
APIs.

The reader accepts the nine-component public `many_dense3` HDF file through
the [dense hard-link profile](../itk1845/README.md). The text form and the Go
classic HDF output also remain supported.
The large-output campaign separately validates 1024 records including the
marker with the real ITK reader and checks empty strings/vectors and multi-level
tree boundaries through h5py. This does not claim ANTs, Slicer or elastix runtime
interoperability beyond the tested public ITK formats.
