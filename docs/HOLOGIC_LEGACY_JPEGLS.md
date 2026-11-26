# Legacy Hologic private JPEG-LS profile

`pixeldata/hologic.Open` is an explicit opt-in for a narrow private layout.
It checks the exact `HOLOGIC, Inc.` creator reservation at (7E01,0010), the
sequence at (7E01,1010), and one defined OB block at (7E01,1012) in each item.
It accepts Explicit VR Little Endian and gives all standard Pixel Data
elements precedence. Within this legacy layout the reader accepts block items
with an omitted creator reservation. An explicit item creator must match the
root.

The descriptor supplies rows, columns, and frame count. The prototype
admits unsigned MONOCHROME2 samples in 16-bit containers with precision
9 through 16 and NEAR=0. Any present standard pixel-module fields used by
the adapter must agree with the descriptor. Patient geometry continues
to come from the standard metadata.

The offset table is at the beginning of the final 1024 bytes of the last
block. The adapter checks ordered nonempty ranges against the descriptor,
the table, and the stored and per-frame byte limits. It joins only the
requested entropy range, even if it crosses blocks. It reconstructs the
monochrome SOF55/SOS header and EOI for the ordinary JPEG-LS decoder.

Callers may defer the OB values with `ReadFileOptions.DeferNestedValues`
and `DeferValueTags: []core.Tag{hologic.BlockValueTag()}` together with a
positive `InlineValueBytesThreshold` on a seekable source. Keep the file
and backing data open and immutable while the source is in use. Reopening
an archived object requires the application's source identity checks.

The wrapper is exercised with synthetic blocks and an independently
encoded JPEG-LS frame. A de-identified legacy object and an independent
reference decode are required to establish interoperability with the
original manufacturer format.
