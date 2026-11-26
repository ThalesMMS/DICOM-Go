"""Own offline oracle runner. Never admits unmanifested/vendor/PHI files.

Compile gdcm_oracle.cpp against a separately installed GDCM. It uses public
APIs and each invocation has a 10 s deadline. Large declared workloads are
refused before starting GDCM, which has no equivalent decode budget API here.
"""
import argparse
import hashlib
import json
import pathlib
import platform
import subprocess
import tempfile


def run(executable, directory):
    manifest = json.loads((directory / "manifest.json").read_text(encoding="utf-8"))
    results = []
    with tempfile.TemporaryDirectory(prefix="quirk-gdcm-") as tmp:
        for f in manifest["Fixtures"]:
            path = directory / f["File"]
            wire = path.read_bytes()
            if len(wire) != f["Bytes"] or len(wire) > 65536 or hashlib.sha256(wire).hexdigest() != f["SourceSHA256"]:
                raise ValueError("authored fixture admission failed: " + f["ID"])
            row = {"ID": f["ID"], "SourceSHA256": f["SourceSHA256"]}
            results.append(row)
            o = f["Oracle"]
            if o["Frames"] > 4 or o["Rows"] * o["Columns"] * o["Frames"] > 4096 or f["ID"] == "i32.XADC.7.215MegaBytes":
                row["Status"] = "not-run:declared-budget"
                continue
            raw = pathlib.Path(tmp) / (f["ID"] + ".raw")
            try:
                proc = subprocess.run([str(executable), str(path), str(raw)], capture_output=True, timeout=10)
            except subprocess.TimeoutExpired:
                row["Status"] = "timeout"
                continue
            row["ExitCode"] = proc.returncode
            row["SourceUnchanged"] = path.read_bytes() == wire
            if not row["SourceUnchanged"]:
                raise ValueError("oracle changed source")
            if proc.returncode != 0:
                row["Status"] = {3: "reject:input-budget", 4: "reject:image-read", 5: "reject:decode-budget", 6: "reject:color-conversion", 7: "reject:decode", 8: "reject:output"}.get(proc.returncode, "process-failure")
                continue
            try:
                observed = json.loads(proc.stdout.decode("utf-8"))
            except (ValueError, UnicodeError):
                row["Status"] = "invalid-oracle-output"
                continue
            decoded = raw.read_bytes()
            if len(decoded) > 65536:
                raise ValueError("oracle exceeded raw output budget")
            row["Status"] = "decoded"
            row["Observed"] = observed
            row["PixelSHA256"] = hashlib.sha256(decoded).hexdigest()
            row["PixelsMatchAuthored"] = decoded.hex() == o["PixelHex"]
            fields = ("Rows", "Columns", "Frames", "Samples", "BitsAllocated", "BitsStored", "HighBit", "PixelRepresentation")
            row["StoredMetadataMatches"] = all(observed[k] == o[k] for k in fields) and observed["SourcePhotometric"].strip() == o["Photometric"]
            row["StoredGeometryMatches"] = observed["DataSpacing"] == o["Spacing"] and observed["DataOrientation"] == o["Orientation"] and observed["DataPosition"] == o["Position"]
            # GDCM ImageReader may use SOP-specific defaults. Never count a
            # raw Pixel hash as proof of geometry equivalence.
            row["ImageGeometryMatches"] = observed["SpacingXYZ"][:2] == list(reversed(o["Spacing"])) and observed["Origin"] == o["Position"] and observed["DirectionCosines"] == o["Orientation"]
    return {"Schema": 1, "Runtime": platform.system() + "/" + platform.machine(), "Oracle": "GDCM ImageReader public APIs; separate stored-tag and image geometry", "Results": results}


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--oracle", required=True, type=pathlib.Path)
    parser.add_argument("--dir", default=pathlib.Path(__file__).parent, type=pathlib.Path)
    parser.add_argument("--output", required=True, type=pathlib.Path)
    args = parser.parse_args()
    report = run(args.oracle.resolve(), args.dir.resolve())
    args.output.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    for row in report["Results"]:
        print(row["ID"], row["Status"], "pixels=" + str(row.get("PixelsMatchAuthored")), "metadata=" + str(row.get("StoredMetadataMatches")), "stored-geometry=" + str(row.get("StoredGeometryMatches")))
