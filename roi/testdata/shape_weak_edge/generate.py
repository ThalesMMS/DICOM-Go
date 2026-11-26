"""Author exact float32 inputs. No ITK import/execution and no Go comparison.

Public-oracle execution is a separately authorized stage, using the sealed input
artifact and the authored public C++ harness. This script never invents expected
values. The six protected existing expected masks are copied byte-for-byte in
canonical packed-bit form, with their original scalar settings preserved.
"""
import argparse
import base64
import hashlib
import json
import math
import pathlib
import struct

MAX_CASES = 512
MAX_VOXEL_VISITS = 16 << 20
MAX_BYTES = 64 << 20
F32 = struct.Struct("<f")


def sha(data):
    return hashlib.sha256(data).hexdigest()


def packed_mask(bits):
    out = bytearray((len(bits) + 7) // 8)
    for i, bit in enumerate(bits):
        if bit not in (0, 1):
            raise ValueError("mask bit outside {0,1}")
        if bit:
            out[i // 8] |= 1 << (i % 8)
    return bytes(out)


def floats(values, count):
    out = bytearray(4 * count)
    seen = 0
    for i, value in enumerate(values):
        if i >= count or not math.isfinite(value):
            raise ValueError("non-finite or oversized input")
        F32.pack_into(out, 4 * i, value)
        seen += 1
    if seen != count:
        raise ValueError("truncated float input")
    return bytes(out)


def coords(dims):
    for z in range(dims[2]):
        for y in range(dims[1]):
            for x in range(dims[0]):
                yield x, y, z


def generated_input(case):
    dims, spacing, definition = case["dims"], case["spacing_mm"], case["input"]
    dimension = 2 if dims[2] == 1 else 3
    half = .5 * min(spacing[:dimension])
    kind = definition["kind"]
    if kind == "plane":
        axis = definition["axis"]
        center = .5 * (dims[axis] - 1) * spacing[axis]
        threshold = center + definition["center_offset_in_hmin"] * min(spacing[:dimension])
        initial = (-half if xyz[axis] * spacing[axis] < threshold else half for xyz in coords(dims))
        feature = (definition["g"] for _ in coords(dims))
    elif kind == "radial":
        def radius(xyz):
            return math.sqrt(sum(((xyz[a] - .5 * (dims[a] - 1)) * spacing[a]) ** 2 for a in range(dimension)))
        initial = (-half if radius(xyz) < definition["initial_radius_mm"] else half for xyz in coords(dims))
        if definition["feature"] == "weak":
            feature = (1. / (1. + 100. * math.exp(-((radius(xyz) - definition["edge_radius_mm"]) / definition["width_mm"]) ** 2)) for xyz in coords(dims))
        elif definition["feature"] == "zero-barrier":
            feature = (1. if radius(xyz) < definition["edge_radius_mm"] else 0. for xyz in coords(dims))
        else:
            raise ValueError("unsupported radial feature")
    elif kind in ("clipped-sphere", "padded-clipped-sphere"):
        center = definition["center_index"]
        pad = definition.get("padding_samples", 0)
        def inside(xyz):
            local = tuple(xyz[a] - (pad if a < dimension else 0) for a in range(3))
            if kind == "padded-clipped-sphere" and any(local[a] < 0 or local[a] >= 21 for a in range(dimension)):
                return False
            return math.sqrt(sum(((local[a] - center[a]) * spacing[a]) ** 2 for a in range(dimension))) < definition["initial_radius_mm"]
        initial = (-half if inside(xyz) else half for xyz in coords(dims))
        feature = (definition["g"] for _ in coords(dims))
    else:
        raise ValueError("unsupported authored input kind")
    count = math.prod(dims)
    return floats(feature, count), floats(initial, count)


def write_inputs(contract_path, protected_path, output_path):
    raw_contract = contract_path.read_bytes()
    if len(raw_contract) > 4 << 20 or protected_path.stat().st_size > 16 << 20:
        raise ValueError("source artifact exceeds authoring ceiling")
    contract = json.loads(raw_contract)
    cases = contract["cases"]
    if len(cases) != 504 or len({c["id"] for c in cases}) != 504 or len(cases) > MAX_CASES:
        raise ValueError("case enumeration is not the sealed 504-case contract")
    visits = sum(math.prod(c["dims"]) for c in cases)
    if visits > MAX_VOXEL_VISITS:
        raise ValueError("input visits exceed authoring ceiling")
    protected_raw = protected_path.read_bytes()
    protected = json.loads(protected_raw)
    protected_cases = {c["name"]: c for c in protected["cases"] + protected["positive_cases"]}
    expected_sha = contract["protected_corpus_sha256"]
    if sha(protected_raw) != expected_sha:
        raise ValueError("protected original corpus source hash changed")
    blobs, raw_blob_bytes, records = {}, 0, []
    input_cache = {}
    def blob(data, encoding, count):
        nonlocal raw_blob_bytes
        digest = sha(data)
        if digest not in blobs:
            if raw_blob_bytes + len(data) > MAX_BYTES // 2:
                raise ValueError("raw blob ceiling before base64 allocation")
            raw_blob_bytes += len(data)
            blobs[digest] = dict(sha256=digest, encoding=encoding, count=count, base64=base64.b64encode(data).decode("ascii"))
        return digest
    for case in cases:
        dims = case["dims"]
        if any(type(n) is not int or n <= 0 or n > MAX_VOXEL_VISITS for n in dims):
            raise ValueError("invalid dimensions before input allocation")
        count = math.prod(dims)
        if count > MAX_VOXEL_VISITS or any(not math.isfinite(x) or x <= 0 for x in case["spacing_mm"]):
            raise ValueError("invalid spacing or product before input allocation")
        record = {k: v for k, v in case.items() if k != "input"}
        if case["input"]["kind"] == "protected":
            original = protected_cases[case["input"]["source_case"]]
            if original["dims"] != dims or original["spacing"] != case["spacing_mm"]:
                raise ValueError("protected source geometry changed")
            feature = floats(original["feature"], count)
            half = .5 * min(case["spacing_mm"][:2 if dims[2] == 1 else 3])
            initial = floats((-half if b else half for b in original["initial"]), count)
            for key, source_key in [("propagation", "propagation"), ("curvature", "curvature"), ("maximum_iterations", "max_iterations"), ("maximum_rms", "maximum_rms")]:
                if case["settings"][key] != original[source_key]:
                    raise ValueError("protected scalar setting changed")
            record["protected_expected_mask_blob"] = blob(packed_mask(original["mask"]), "bits-lsb-first-x-fast", count)
            record["protected_expected_observations"] = {"elapsed_iterations": original["iterations"], "rms": original["rms"]}
        else:
            cache_key = json.dumps({"dims": dims, "spacing_mm": case["spacing_mm"], "input": case["input"]}, sort_keys=True, separators=(",", ":"))
            cached = input_cache.get(cache_key)
            if cached is not None:
                record.update(cached)
                records.append(record)
                continue
            feature, initial = generated_input(case)
        record["feature_blob"] = blob(feature, "ieee754-f32-little-endian-x-fast", count)
        record["initial_phi_blob"] = blob(initial, "ieee754-f32-little-endian-x-fast", count)
        if case["input"]["kind"] != "protected":
            input_cache[cache_key] = {"feature_blob": record["feature_blob"], "initial_phi_blob": record["initial_phi_blob"]}
        records.append(record)
    document = dict(schema=1, role="sealed authored inputs, not newly computed expected results", contract_sha256=sha(raw_contract), protected_corpus_sha256=sha(protected_raw), cases=records, blobs=blobs, voxel_visits=visits, unique_raw_blob_bytes=raw_blob_bytes)
    data = (json.dumps(document, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode()
    if len(data) > MAX_BYTES:
        raise ValueError("serialized input artifact exceeds ceiling")
    output_path.write_bytes(data)
    print(json.dumps(dict(path=str(output_path), sha256=sha(data), bytes=len(data), cases=len(records), unique_blobs=len(blobs), raw_blob_bytes=raw_blob_bytes, voxel_visits=visits), sort_keys=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--contract", type=pathlib.Path, required=True)
    parser.add_argument("--protected", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    write_inputs(args.contract, args.protected, args.output)


if __name__ == "__main__":
    main()
