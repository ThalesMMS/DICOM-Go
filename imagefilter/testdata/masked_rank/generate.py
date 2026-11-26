#!/usr/bin/env python3
"""Own deterministic masked-rank inputs; no ITK or product invocation.

Inputs are sealed before comparison. --workunits-dir exports the same sealed
inputs to the public C++ harness's text protocol; Replicate is explicitly
adapted with global edge padding of both samples and mask. It never writes
expectations. Rerunning --inputs requires an explicit fresh-generation decision.
"""
import argparse
import hashlib
import json
import math
from pathlib import Path
import struct

PIN = "f51594ad88194a72cdc06a314dafff709fcdc2e9"


def bits(x):
    return struct.unpack("<I", struct.pack("<f", x))[0]


def canonical_inputs(cases):
    return [{k: v for k, v in c.items() if k != "expected"} for c in cases]


def input_hash(cases):
    return hashlib.sha256(json.dumps(canonical_inputs(cases), sort_keys=True,
                                    separators=(",", ":")).encode()).hexdigest()


def case(dimension, shape, pattern, q, family, boundary):
    dims = [7, 6, 1 if dimension == 2 else 5]
    spacing, radius = [.5, 1., 1.5], [.75, 1., 1.5]
    bounds = [math.floor(radius[a] / spacing[a]) if dims[a] > 1 else 0 for a in range(3)]
    bitmap = []
    for dz in range(-bounds[2], bounds[2] + 1):
        for dy in range(-bounds[1], bounds[1] + 1):
            for dx in range(-bounds[0], bounds[0] + 1):
                ds = [dx, dy, dz]
                if shape == "box":
                    included = all(abs(ds[a] * spacing[a]) <= radius[a]
                                   for a in range(3) if dims[a] > 1)
                else:
                    included = sum((ds[a] * spacing[a] / radius[a]) ** 2
                                   for a in range(3) if dims[a] > 1) <= 1.
                bitmap.append(included)
    data, valid = [], []
    for z in range(dims[2]):
        for y in range(dims[1]):
            for x in range(dims[0]):
                v = (7 * x + 13 * y + 19 * z) % 17 - 8
                if family == "fractional":
                    v = v / 8 + 1 / 16
                data.append(bits(v))
                valid.append(pattern == "all" or (pattern == "checker" and (x + 2 * y + 3 * z) % 4 != 0))
    name = f"d{dimension}-{shape}-{boundary}-{pattern}-q{int(q*100):03}-{family}"
    return dict(id=name, dimension=dimension, dims=dims, spacing_mm=spacing,
                radius_mm=radius, shape=shape, boundary=boundary,
                centers="require-valid", nonfinite="reject", rank=q,
                empty_value_bits=bits(-99.5), kernel_radius=bounds,
                kernel_bitmap=bitmap, data_bits=data, input_valid=valid,
                expected=None)


def generate():
    cases = []
    for d in (2, 3):
        for shape in ("box", "ellipsoid"):
            for mask in ("all", "checker", "empty"):
                for q in (0., .5, 1.):
                    for family in ("integral", "fractional"):
                        cases.append(case(d, shape, mask, q, family, "clip"))
    for d in (2, 3):
        for shape in ("box", "ellipsoid"):
            for mask in ("all", "checker"):
                for q in (0., .5, 1.):
                    cases.append(case(d, shape, mask, q, "integral", "replicate"))
    assert len(cases) == 96 and len({c["id"] for c in cases}) == 96
    return dict(schema=1, status="INPUTS_SEALED_PUBLIC_ORACLE_PENDING",
                source="Own formulas; ITK public MaskedRankImageFilter behavior only",
                itk_version="5.4.5", itk_source_commit=PIN,
                criterion="Exact finite float32 bits and output validity; canonical +0",
                expected_work_units=1, max_cases=96,
                inputs_sha256=input_hash(cases), cases=cases)


def export_workunit(c, path):
    d, original, b = c["dimension"], c["dims"], c["kernel_radius"]
    pad = b if c["boundary"] == "replicate" else [0, 0, 0]
    work = [original[a] + 2 * pad[a] for a in range(3)]
    data, mask = [], []
    for z in range(work[2]):
        for y in range(work[1]):
            for x in range(work[0]):
                coord = [max(0, min(original[a]-1, (x, y, z)[a]-pad[a])) for a in range(3)]
                i = (coord[2]*original[1] + coord[1])*original[0] + coord[0]
                data.append(c["data_bits"][i]); mask.append(int(c["input_valid"][i]))
    lines = ["MR1 " + str(d), " ".join(map(str, work[:d])),
             " ".join(map(repr, c["spacing_mm"][:d])), " ".join(map(str, b[:d])),
             " ".join(map(str, pad[:d])), " ".join(map(str, original[:d])),
             f'{c["rank"]!r} {c["empty_value_bits"]}', str(len(c["kernel_bitmap"])),
             " ".join(str(int(v)) for v in c["kernel_bitmap"]), str(len(data)),
             " ".join(map(str, data)), " ".join(map(str, mask))]
    path.write_text("\n".join(lines) + "\n")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--inputs", type=Path, help="author new input JSON; refuses replacement")
    parser.add_argument("--fixture", type=Path, help="existing sealed fixture to verify/export")
    parser.add_argument("--workunits-dir", type=Path)
    a = parser.parse_args()
    if bool(a.inputs) == bool(a.fixture):
        parser.error("choose exactly one of --inputs or --fixture")
    if a.inputs:
        doc = generate()
        with a.inputs.open("x") as f:
            json.dump(doc, f, indent=2, allow_nan=False); f.write("\n")
    else:
        doc = json.loads(a.fixture.read_text())
        assert len(doc["cases"]) == 96 and input_hash(doc["cases"]) == doc["inputs_sha256"]
    if a.workunits_dir:
        a.workunits_dir.mkdir(parents=True, exist_ok=False)
        entries = []
        for i, c in enumerate(doc["cases"]):
            p = a.workunits_dir / f"{i:03d}.workunit"
            export_workunit(c, p)
            entries.append(dict(index=i, id=c["id"], path=p.name, bytes=p.stat().st_size,
                                sha256=hashlib.sha256(p.read_bytes()).hexdigest()))
        (a.workunits_dir / "manifest.json").write_text(json.dumps(dict(inputs_sha256=doc["inputs_sha256"], files=entries), indent=2)+"\n")
    print(doc["inputs_sha256"])


if __name__ == "__main__":
    main()
