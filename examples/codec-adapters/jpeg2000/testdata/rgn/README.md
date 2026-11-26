# Region-of-interest (RGN) fixtures

Synthetic JPEG 2000 Part 1 codestreams with RGN marker segments (Maxshift,
ISO/IEC 15444-1 Annex H) for the pure-Go core decoder. No patient
data: every frame is generated from a fixed formula or seed.

## OpenJPEG fixtures

`u8-*`, `u12-*`, `u16-*` (`.j2c`): written by OpenJPEG 2.5.4 `opj_compress`
from the `codecfixture.JPEG2000LosslessEncoderCases` frames, with the
arguments listed in `rgnFixtures` (`core_rgn_test.go`), each with `-ROI
c=<component>,U=<shift>`. `.raw` is the frame `opj_decompress` reconstructs
(native, little-endian samples); `.lN.raw` is `opj_decompress -l N`.
Regenerate with

    DICOMGO_REGEN_RGN_FIXTURES=1 go test -run OpenJPEGRegionOfInterest .

`opj_compress -ROI` scales a whole component and writes the same
code-blocks it writes without the option, so these never mix region and
background coefficients. Reversible fixtures decode exactly; irreversible
ones within one step of OpenJPEG, the core's 9/7 policy, and exactly as the
same codestream without its RGN.

## Synthetic Maxshift fixtures

`maxshift-*` (`.j2c`): built by `TestMaxshiftFixturesAreReproducible` in
`pixeldata/jpeg2000/maxshift_fixture_test.go` from that package's reversible
encoder stages: the 5/3 coefficients whose position falls in a rectangle of
the image are scaled by 2^s, the bands' zero bit planes count from Mb + s, and
an RGN segment follows the QCD. Region and background coefficients share
code-blocks. `.raw` is the source frame; the test rebuilds every codestream
and requires it to equal the committed one, and with
`DICOMGO_REGEN_RGN_FIXTURES=1` rewrites them.

s is the bit length of the largest background coefficient plus one, which
`opj_decompress` 2.5.4 reconstructs exactly. `maxshift-u8-gray-minimal-shift`
uses the bit length alone, the smallest s Annex H allows; OpenJPEG 2.5.4
treats coefficients from 2^(s-1) up as region and decodes it wrongly, while
the core, which takes 2^s as the boundary, reconstructs the source frame.
