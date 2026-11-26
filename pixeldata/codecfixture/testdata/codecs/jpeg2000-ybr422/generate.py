"""Generate the JPEG 2000 YBR_FULL_422 (4:2:2 chroma) fixture (#2028).

The codestream is encoded by OpenJPEG from deterministic synthetic samples:
three unsigned 8-bit components, the first at XRsiz=YRsiz=1 and the two
chroma components at XRsiz=2, YRsiz=1, reversible 5/3, no multiple component
transform. The image is 17 columns wide, so each chroma component has
ceil(17/2) = 9 columns.

The expected output does not come from dicom-go. OpenJPEG decodes each
component on its own (opj_decompress -c N, which also keeps opj_decompress
from applying its own sYCC guess), and this script expands the chroma by
replication (output column x takes chroma sample floor(x/2), the sample whose
reference-grid area covers it) and converts YCbCr to RGB with the YBR_FULL
equations of PS3.3 C.7.6.3.1.2, clamped to 0..255 and rounded half away from
zero.

OpenJPEG 2.5.4 reads floor(W*H/(XRsiz*YRsiz)) samples per component from raw
input, which is 51 rather than 54 for a 9x6 chroma component; it leaves the
last three samples of each chroma component zero. The script writes the input
in that layout and records OpenJPEG's own reconstruction as the reference, so
the reference planes are what the codestream holds.

Run from this directory:

    DICOM_GO_OPENJPEG_COMPRESS=opj_compress \
    DICOM_GO_OPENJPEG_DECOMPRESS=opj_decompress python generate.py
"""

import hashlib
import json
import math
import os
import subprocess
import sys
import tempfile

ID = "ybr422-17x6"
COLUMNS, ROWS = 17, 6
SAMPLING = [(1, 1), (2, 1), (2, 1)]
ARGS = ["-mct", "0", "-n", "2"]


def component_size(dx, dy):
    # ISO/IEC 15444-1 B.2 with XOsiz = YOsiz = 0: ceil(Xsiz/XRsiz) x ceil(Ysiz/YRsiz).
    return -(-COLUMNS // dx), -(-ROWS // dy)


def source_planes():
    """Deterministic synthetic planes; no patient data."""
    planes = []
    patterns = [
        lambda x, y: (x * 13 + y * 29) % 256,
        lambda x, y: (0, 255, 128, 16, 240)[(x + y) % 5] if y % 2 else (x * 41 + y * 7 + 60) % 256,
        lambda x, y: (255, 0, 128, 64, 192)[(2 * x + y) % 5] if y % 3 == 1 else (x * 23 + y * 53 + 200) % 256,
    ]
    for (dx, dy), pattern in zip(SAMPLING, patterns):
        width, height = component_size(dx, dy)
        planes.append([pattern(x, y) for y in range(height) for x in range(width)])
    return planes


def openjpeg_raw_input(planes):
    out = bytearray()
    for (dx, dy), plane in zip(SAMPLING, planes):
        out += bytes(plane[: (COLUMNS * ROWS) // (dx * dy)])
    return bytes(out)


def read_pgx(path):
    data = open(path, "rb").read()
    header, body = data.split(b"\n", 1)
    fields = header.split()
    # "PG ML + 8 W H": little-endian, unsigned, 8-bit.
    if len(fields) != 6 or fields[:4] != [b"PG", b"ML", b"+", b"8"]:
        raise SystemExit(f"{path}: unexpected PGX header {header!r}")
    width, height = int(fields[4]), int(fields[5])
    if len(body) != width * height:
        raise SystemExit(f"{path}: unexpected PGX {header!r} with {len(body)} bytes")
    return width, height, list(body)


def ybr_full_to_rgb(y, cb, cr):
    cb, cr = cb - 128, cr - 128
    values = (y + 1.402 * cr, y - 0.344136 * cb - 0.714136 * cr, y + 1.772 * cb)
    return [int(math.floor(max(0.0, min(255.0, v)) + 0.5)) for v in values]


def sha256(path):
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


def run(*args):
    subprocess.run(args, check=True, stdout=subprocess.DEVNULL)


def main():
    compress = os.environ.get("DICOM_GO_OPENJPEG_COMPRESS")
    decompress = os.environ.get("DICOM_GO_OPENJPEG_DECOMPRESS")
    if not compress or not decompress:
        raise SystemExit("set DICOM_GO_OPENJPEG_COMPRESS and DICOM_GO_OPENJPEG_DECOMPRESS")
    planes = source_planes()
    codestream = ID + ".j2k"
    sampling = ":".join(f"{dx}x{dy}" for dx, dy in SAMPLING)
    with tempfile.TemporaryDirectory(prefix="jpeg2000-ybr422-") as temp:
        raw = os.path.join(temp, "input.raw")
        open(raw, "wb").write(openjpeg_raw_input(planes))
        run(compress, "-i", raw, "-o", codestream, "-F", f"{COLUMNS},{ROWS},3,8,u@{sampling}", *ARGS)
        decoded = []
        for component, (dx, dy) in enumerate(SAMPLING):
            run(decompress, "-i", codestream, "-o", os.path.join(temp, f"c{component}.pgx"), "-c", str(component))
            width, height, plane = read_pgx(os.path.join(temp, f"c{component}_0.pgx"))
            if (width, height) != component_size(dx, dy):
                raise SystemExit(f"component {component} is {width}x{height}")
            decoded.append(plane)
    # The reversible codestream holds what OpenJPEG read from the raw input.
    for component, (dx, dy) in enumerate(SAMPLING):
        read = (COLUMNS * ROWS) // (dx * dy)
        expected = planes[component][:read] + [0] * (len(planes[component]) - read)
        if decoded[component] != expected:
            raise SystemExit(f"component {component}: OpenJPEG reconstruction differs from its input")
    reference = ID + ".openjpeg.raw"
    open(reference, "wb").write(bytes(sum(decoded, [])))
    chroma_width = component_size(*SAMPLING[1])[0]
    rgb = bytearray()
    for y in range(ROWS):
        for x in range(COLUMNS):
            at = y * chroma_width + x // 2
            rgb += bytes(ybr_full_to_rgb(decoded[0][y * COLUMNS + x], decoded[1][at], decoded[2][at]))
    expected_rgb = ID + ".rgb.raw"
    open(expected_rgb, "wb").write(bytes(rgb))

    generation = {
        "schemaVersion": 1,
        "issue": 2028,
        "profile": "JPEG 2000 Part 1 YBR_FULL_422: 3 unsigned 8-bit components, component 0 at 1x1, components 1 and 2 at XRsiz=2 YRsiz=1, odd width; decoded to full-resolution interleaved RGB",
        "provenance": {
            "source": "Project-authored deterministic synthetic planes (generate.py)",
            "generator": "generate.py",
            "synthetic": True,
            "noPHI": True,
            "license": "Apache-2.0",
            "permission": "Project-owned synthetic data and generator; redistributable under dicom-go LICENSE",
            "deIdentification": "Not applicable: all samples generated from arithmetic; no patient data imported",
            "vendorImplementationCopied": False,
        },
        "tools": [
            {
                "name": "opj_compress",
                "version": "OpenJPEG 2.5.4",
                "sha256": sha256(compress),
                "args": ["-F", f"{COLUMNS},{ROWS},3,8,u@{sampling}", *ARGS],
                "use": "Encode the codestream from raw planar input",
            },
            {
                "name": "opj_decompress",
                "version": "OpenJPEG 2.5.4",
                "sha256": sha256(decompress),
                "args": ["-c", "<component>"],
                "use": "Independent per-component reconstruction (no sYCC conversion)",
            },
            {
                "name": "Python",
                "version": sys.version.split()[0],
                "use": "Independent chroma replication and PS3.3 C.7.6.3.1.2 YBR_FULL to RGB conversion",
            },
        ],
        "fixtures": [
            {
                "id": ID,
                "codestream": codestream,
                "transferSyntax": "1.2.840.10008.1.2.4.90",
                "input": {
                    "rows": ROWS,
                    "columns": COLUMNS,
                    "samplesPerPixel": 3,
                    "bitsAllocated": 8,
                    "bitsStored": 8,
                    "highBit": 7,
                    "pixelRepresentation": 0,
                    "planarConfiguration": 0,
                    "photometric": "YBR_FULL_422",
                },
                "components": [
                    {"xrsiz": dx, "yrsiz": dy, "columns": component_size(dx, dy)[0], "rows": component_size(dx, dy)[1], "depth": 8, "signed": False}
                    for dx, dy in SAMPLING
                ],
                "reference": {
                    "path": reference,
                    "layout": "planar component planes at their own sizes, component 0 then 1 then 2, 8-bit",
                    "backend": "OpenJPEG opj_decompress 2.5.4, one component per run",
                },
                "expected": {
                    "path": expected_rgb,
                    "layout": "interleaved RGB, 8-bit, Rows x Columns x 3",
                    "photometric": "RGB",
                    "lossyImageCompression": "01",
                    "upsampling": "replication: output column x uses chroma column floor(x/2)",
                    "conversion": "PS3.3 C.7.6.3.1.2 YBR_FULL to RGB, clamped to 0..255, rounded half away from zero",
                },
            }
        ],
        "assets": [
            {"path": path, "bytes": os.path.getsize(path), "sha256": sha256(path)}
            for path in ("generate.py", codestream, reference, expected_rgb)
        ],
    }
    with open("generation.json", "w", newline="\n") as f:
        json.dump(generation, f, indent=2)
        f.write("\n")


if __name__ == "__main__":
    main()
