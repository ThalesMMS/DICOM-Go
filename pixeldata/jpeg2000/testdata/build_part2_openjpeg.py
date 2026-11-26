"""Build a pinned test-only OpenJPEG float-coefficient extractor.

The hook only serializes values after inverse spatial coding and before final
rounding. It does not implement any Annex J operation or change decoded values.
"""
import argparse
import hashlib
from pathlib import Path
import shutil
import subprocess


parser = argparse.ArgumentParser()
parser.add_argument("--source", type=Path, required=True)
parser.add_argument("--build", type=Path, required=True)
args = parser.parse_args()
original = args.source / "src/lib/openjp2/tcd.c"
source_bytes = original.read_bytes()
expected = "3cacb9dbb6b4d00ae61a9dda2b63e6b89a64120b523356869045d0b788e37348"
if hashlib.sha256(source_bytes).hexdigest() != expected:
    raise SystemExit("expected unmodified OpenJPEG 2.5.4 tcd.c")
source = args.build / "source"
shutil.copytree(args.source, source)
target = source / "src/lib/openjp2/tcd.c"
code = source_bytes.decode()
start = code.index("static OPJ_BOOL opj_tcd_dc_level_shift_decode(opj_tcd_t *p_tcd)\n{")
position = code.index("        if (l_tccp->qmfbid == 1) {", start)
hook = r'''
        /* Test-only extraction of unrounded, decoded spatial coefficients. */
        if (l_tccp->qmfbid == 0) {
            const char *directory = getenv("DICOM_GO_PART2_FLOAT_DIR");
            if (directory != NULL) {
                char filename[4096];
                FILE *output;
                OPJ_UINT32 row, column;
                if (!p_tcd->whole_tile_decoding || sizeof(OPJ_FLOAT32) != 4 ||
                        snprintf(filename, sizeof(filename), "%s/component%u.f32",
                                 directory, compno) < 0 || strlen(filename) >= sizeof(filename) - 1) {
                    return OPJ_FALSE;
                }
                output = fopen(filename, "wb");
                if (output == NULL) {
                    return OPJ_FALSE;
                }
                for (row = 0; row < l_height; ++row) {
                    for (column = 0; column < l_width; ++column) {
                        OPJ_UINT32 bits;
                        OPJ_BYTE bytes[4];
                        memcpy(&bits, l_current_ptr + row * (l_width + l_stride) + column, 4);
                        bytes[0] = (OPJ_BYTE)bits;
                        bytes[1] = (OPJ_BYTE)(bits >> 8);
                        bytes[2] = (OPJ_BYTE)(bits >> 16);
                        bytes[3] = (OPJ_BYTE)(bits >> 24);
                        if (fwrite(bytes, 1, 4, output) != 4) {
                            fclose(output);
                            return OPJ_FALSE;
                        }
                    }
                }
                if (fclose(output) != 0) {
                    return OPJ_FALSE;
                }
            }
        }
'''
target.write_text(code[:position] + hook + code[position:], newline="\n")
build = args.build / "build"
subprocess.run(["cmake", "-S", str(source), "-B", str(build), "-G", "Ninja",
                "-DCMAKE_BUILD_TYPE=Release", "-DBUILD_SHARED_LIBS=OFF", "-DBUILD_TESTING=OFF",
                "-DBUILD_CODEC=ON", "-DBUILD_PKGCONFIG_FILES=OFF"], check=True)
subprocess.run(["cmake", "--build", str(build), "--target", "opj_decompress", "--parallel", "2"], check=True)
print("patched tcd.c SHA-256", hashlib.sha256(target.read_bytes()).hexdigest())
print("float decoder", build / "bin/opj_decompress.exe")
