"""Independent exhaustive G.711 reference vectors from CPython 3.12 audioop.

DICOM AB is before telephone even-bit inversion, so adapt it for audioop.
No codec source/tables are copied. Run --generate to write the own test outputs;
the default verifies both checked-in files against the independent decoder.
"""
import argparse
import audioop
import hashlib
import json
from pathlib import Path
import sys

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--generate", action="store_true")
args = parser.parse_args()
root = Path(__file__).resolve().parent
result = {"python": sys.version.split()[0], "oracle": "CPython audioop", "vectors": {}}
for law in ("AB", "MB"):
    encoded = bytes((code ^ 0x55 if law == "AB" else code) for code in range(256))
    pcm = audioop.alaw2lin(encoded, 2) if law == "AB" else audioop.ulaw2lin(encoded, 2)
    if sys.byteorder != "little":
        pcm = audioop.byteswap(pcm, 2)
    path = root / f"audio-{law}-pcm16le.bin"
    if args.generate:
        path.write_bytes(pcm)
    if path.read_bytes() != pcm:
        raise SystemExit(f"Mismatch: {path.name}")
    result["vectors"][law] = {"codes": 256, "bytes": len(pcm), "sha256": hashlib.sha256(pcm).hexdigest()}
print(json.dumps(result, indent=2))
