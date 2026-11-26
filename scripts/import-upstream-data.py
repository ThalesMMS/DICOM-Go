#!/usr/bin/env python3
"""Import pinned DCMTK/GDCM test tables into testdata/upstream with provenance.

The script never copies production source code. It converts selected test
tables and data files from local reference checkouts into JSON (or copies data
files verbatim) and records, for every artifact, the upstream commit, source
file, line range, SHA-256 of the source file and of the derived artifact.

Usage (from dicom-go-dev):

    python3 scripts/import-upstream-data.py --reference-root ../..

The reference root must contain checkouts named dcmtk/ and GDCM/ at the pinned
commits below. Re-running with identical inputs is byte-for-byte reproducible.
Entries in THIRD_PARTY_PROVENANCE.json that this script does not own are kept.
"""

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys

OWNER = "scripts/import-upstream-data.py"
MODULE_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MANIFEST = os.path.join(MODULE_ROOT, "THIRD_PARTY_PROVENANCE.json")
OUT = os.path.join(MODULE_ROOT, "testdata", "upstream")

PINS = {
    "gdcm-2cd05d13": ("GDCM", "2cd05d13222b175e791b5ae3c08feeabcc1ed1f0"),
    "dcmtk-8d68392d": ("dcmtk", "8d68392db66cd69e13dc878dc9889681062ad479"),
    "vtk-ac53123e": ("VTK", "ac53123e515e6e97adf79d9a225101303049817e"),
}

UPSTREAMS = {
    "gdcm-2cd05d13": {
        "project": "GDCM (Grassroots DICOM)",
        "repository": "https://github.com/malaterre/GDCM",
        "commit": PINS["gdcm-2cd05d13"][1],
        "license": "BSD-3-Clause",
        "license_file": "testdata/upstream/gdcm/LICENSE.GDCM",
        "license_source": "Copyright.txt",
        "notice_heading": "### GDCM — test tables",
    },
    "dcmtk-8d68392d": {
        "project": "DCMTK",
        "repository": "https://github.com/DCMTK/dcmtk",
        "commit": PINS["dcmtk-8d68392d"][1],
        "license": "BSD-3-Clause",
        "license_file": "testdata/upstream/dcmtk/LICENSE.DCMTK",
        "license_source": "COPYRIGHT",
        "notice_heading": "### DCMTK — test data and vectors",
    },
    "vtk-ac53123e": {
        "project": "VTK (Visualization Toolkit)",
        "repository": "https://github.com/Kitware/VTK",
        "commit": PINS["vtk-ac53123e"][1],
        "license": "BSD-3-Clause",
        "license_file": "testdata/upstream/vtk/LICENSE.VTK",
        "license_source": "Copyright.txt",
        "notice_heading": "### VTK — numerical fixtures",
    },
}


def sha256_bytes(b):
    return hashlib.sha256(b).hexdigest()


def sha256_file(p):
    with open(p, "rb") as f:
        return sha256_bytes(f.read())


class Importer:
    def __init__(self, ref_root):
        self.ref_root = ref_root
        self.artifacts = []

    def checkout(self, up):
        d = os.path.join(self.ref_root, PINS[up][0])
        head = subprocess.run(["git", "-C", d, "rev-parse", "HEAD"], capture_output=True, text=True,
                              check=True).stdout.strip()
        if head != PINS[up][1]:
            sys.exit(f"{d}: HEAD {head} does not match pinned {PINS[up][1]}")
        return d

    def read(self, up, rel):
        # Read the committed blob, not the working tree: checkouts with
        # core.autocrlf would otherwise change bytes and hashes.
        return subprocess.run(["git", "-C", self.checkout(up), "show", f"{PINS[up][1]}:{rel}"],
                              capture_output=True, check=True).stdout

    def listdir(self, up, rel):
        out = subprocess.run(["git", "-C", self.checkout(up), "ls-tree", "--name-only",
                              f"{PINS[up][1]}:{rel}"], capture_output=True, text=True, check=True).stdout
        return sorted(out.split())

    def source(self, up, rel, first, last):
        data = self.read(up, rel)
        n = data.count(b"\n") + (0 if data.endswith(b"\n") else 1)
        if not (1 <= first <= last <= n):
            sys.exit(f"{rel}: bad line range {first}-{last} (file has {n})")
        return {"file": rel, "lines": f"{first}-{last}", "sha256": sha256_bytes(data),
                "url": f"{UPSTREAMS[up]['repository']}/blob/{PINS[up][1]}/{rel}#L{first}-L{last}"}

    def write(self, up, rel_out, payload, sources, transformation):
        path = os.path.join(OUT, rel_out)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        if isinstance(payload, (bytes, bytearray)):
            data = bytes(payload)
        else:
            data = (json.dumps(payload, indent=1, ensure_ascii=False) + "\n").encode("utf-8")
        with open(path, "wb") as f:
            f.write(data)
        self.artifacts.append({
            "path": "testdata/upstream/" + rel_out.replace(os.sep, "/"),
            "upstream": up,
            "sha256": sha256_bytes(data),
            "distribution": "test-data",
            "transformation": transformation,
            "sources": sources,
            "generated_by": OWNER,
        })

    def license(self, up):
        meta = UPSTREAMS[up]
        data = self.read(up, meta["license_source"])
        dest = os.path.join(MODULE_ROOT, meta["license_file"])
        os.makedirs(os.path.dirname(dest), exist_ok=True)
        with open(dest, "wb") as f:
            f.write(data)
        meta["license_sha256"] = sha256_bytes(data)


def lines_of(data):
    return data.decode("utf-8", errors="strict").split("\n")


def span(lines, start_pat, end_pat="};"):
    """Return 1-based [first,last] of the C array starting at start_pat."""
    first = next(i for i, l in enumerate(lines) if start_pat in l)
    last = next(i for i in range(first, len(lines)) if lines[i].strip().startswith(end_pat))
    return first + 1, last + 1


# ---------------------------------------------------------------------- GDCM
GEOM = re.compile(r"/\*\s*gdcm\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s*\*/")


def gdcm_md5_table(imp, rel, array, out, note):
    ls = lines_of(imp.read("gdcm-2cd05d13", rel))
    first, last = span(ls, f"{array}[][2] =")
    rows, geom = [], None
    for l in ls[first:last - 1]:
        s = l.strip()
        m = GEOM.match(s)
        if m:
            geom = [int(x) for x in m.groups()]
            continue
        m = re.match(r'\{\s*"([0-9a-f]{32})"\s*,\s*"([^"]+)"\s*\}\s*,?\s*(//\s*(.*))?$', s)
        if not m:
            if s.startswith("{") and "nullptr" not in s:
                sys.exit(f"{rel}: unparsed row {s!r}")
            continue
        row = {"file": m.group(2), "md5": m.group(1)}
        if geom:
            row["gdcm_dimensions"] = {"columns": geom[0], "rows": geom[1], "frames": geom[2],
                                      "bits_allocated": geom[3], "samples_per_pixel": geom[4]}
        if m.group(4):
            row["upstream_comment"] = m.group(4).strip()
        rows.append(row)
        geom = None
    imp.write("gdcm-2cd05d13", out, {"description": note, "entries": rows},
              [imp.source("gdcm-2cd05d13", rel, first, last)],
              "Active rows of the C table converted to JSON; commented-out rows and the "
              "sentinel are omitted. The '/* gdcm C R F B S */' comment preceding a row is kept "
              "as gdcm_dimensions.")
    return len(rows)


def gdcm_pair_table(imp, rel, array, out, note, value_kind):
    ls = lines_of(imp.read("gdcm-2cd05d13", rel))
    first, last = span(ls, f"{array}[")
    rows = []
    for l in ls[first:last - 1]:
        s = l.strip()
        if value_kind == "uid":
            m = re.match(r'\{\s*"([^"]+)"\s*,\s*"([0-9.]*)"\s*\}', s)
        else:
            m = re.match(r'\{\s*"([^"]+)"\s*,\s*(\d+)\s*\}', s)
        if not m:
            if s.startswith("{") and "nullptr" not in s:
                sys.exit(f"{rel}: unparsed row {s!r}")
            continue
        rows.append({"file": m.group(1),
                     value_kind: m.group(2) if value_kind == "uid" else int(m.group(2))})
    imp.write("gdcm-2cd05d13", out, {"description": note, "entries": rows},
              [imp.source("gdcm-2cd05d13", rel, first, last)],
              "Active rows of the C table converted to JSON; commented-out rows and the sentinel are omitted.")
    return len(rows)


def gdcm_blacklist(imp):
    rel = "Testing/Source/Data/CMakeLists.txt"
    ls = lines_of(imp.read("gdcm-2cd05d13", rel))
    groups, cond, pending_cond, first, last = [], None, "always", None, None
    comment, after_entry = [], False
    for i, l in enumerate(ls):
        s = l.strip()
        m = re.match(r"if\((.*)\)$", s)
        if m and cond is None:
            pending_cond = m.group(1)
        if s == "endif()" and cond is None:
            pending_cond = "always"
        if s.startswith("set(BLACK_LIST_READER"):
            cond, comment, after_entry = pending_cond, [], False
            first = first or i + 1
            continue
        if cond is None:
            continue
        if s == ")":
            cond, last = None, i + 1
            continue
        if s.startswith("#"):
            text = s.lstrip("#").strip()
            if re.search(r"\.(dcm|acr)$", text):
                continue  # commented-out file name, not an explanation
            if after_entry:
                comment, after_entry = [], False
            comment.append(text)
            continue
        if s.startswith("${") or not s:
            continue
        name, _, trailing = s.partition("#")
        groups.append({"file": name.strip(), "condition": cond,
                       "reason": trailing.strip() or " ".join(comment)})
        after_entry = True
    imp.write("gdcm-2cd05d13", "gdcm/black_list_reader.json",
              {"description": "Files GDCM's reader tests skip (BLACK_LIST_READER), with the CMake "
                              "condition that adds them and the adjacent upstream comment.",
               "entries": groups},
              [imp.source("gdcm-2cd05d13", rel, first, last)],
              "Names from every set(BLACK_LIST_READER ...) block, the enclosing if() condition "
              "('always' at top level) and the nearest preceding or trailing comment.")
    return len(groups)


def gdcm_overlay3(imp):
    rel = "Testing/Source/MediaStorageAndFileFormat/Cxx/TestOverlay3.cxx"
    ls = lines_of(imp.read("gdcm-2cd05d13", rel))
    first, last = span(ls, "overlay3array[] =")
    rows, corpus = [], None
    for l in ls[first:last - 1]:
        s = l.strip()
        if s.startswith("// gdcm"):
            corpus = s[3:].strip()
            continue
        m = re.match(r'\{\s*"([0-9a-f]{32})"\s*,\s*"([^"]+)"\s*,\s*(\d+)\s*,\s*Overlay::(\w+)', s)
        if m:
            rows.append({"corpus": corpus, "file": m.group(2), "overlay_index": int(m.group(3)),
                         "type": m.group(4), "unpacked_md5": m.group(1)})
    imp.write("gdcm-2cd05d13", "gdcm/overlay3.json",
              {"description": "MD5 of each unpacked overlay (one byte per pixel, as produced by "
                              "gdcm::Overlay::GetUnpackBuffer) for TestOverlay3.",
               "entries": rows},
              [imp.source("gdcm-2cd05d13", rel, first, last)],
              "Active rows of overlay3array converted to JSON with the corpus section comment; "
              "commented-out rows and the sentinel are omitted.")
    return len(rows)


# --------------------------------------------------------------------- DCMTK
def dcmtk_verbatim(imp, rel, out, note):
    data = imp.read("dcmtk-8d68392d", rel)
    n = data.count(b"\n") + (0 if data.endswith(b"\n") else 1)
    imp.write("dcmtk-8d68392d", out, data, [imp.source("dcmtk-8d68392d", rel, 1, max(n, 1))],
              "Verbatim copy of an upstream data file. " + note)


def dcmtk_c_byte_arrays(imp, rel, out, note):
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    arrays, sources = {}, []
    i = 0
    while i < len(ls):
        m = re.match(r"\s*(?:static\s+)?(?:const\s+)?unsigned char\s+(\w+)\s*\[\s*\]\s*=\s*\{", ls[i])
        if not m:
            i += 1
            continue
        j = i
        while "};" not in ls[j]:
            j += 1
        body = " ".join(ls[i:j + 1])
        body = body[body.index("{") + 1:body.rindex("}")]
        vals = [int(x, 16) for x in re.findall(r"0[xX]([0-9a-fA-F]{1,2})\b", body)]
        rest = re.sub(r"0[xX][0-9a-fA-F]{1,2}\b", "", body).replace(",", "").strip()
        if rest:
            sys.exit(f"{rel}:{i + 1}: unexpected tokens in {m.group(1)}: {rest[:40]!r}")
        arrays[m.group(1)] = {"lines": f"{i + 1}-{j + 1}", "hex": bytes(vals).hex()}
        i = j + 1
    first = min(int(a["lines"].split("-")[0]) for a in arrays.values())
    last = max(int(a["lines"].split("-")[1]) for a in arrays.values())
    imp.write("dcmtk-8d68392d", out, {"description": note, "arrays": arrays},
              [imp.source("dcmtk-8d68392d", rel, first, last)],
              "Every 'unsigned char name[] = {...}' initializer converted to lowercase hex; "
              "comments and C code are not copied.")
    return len(arrays)


def dcmtk_cielab(imp):
    rel = "dcmiod/tests/tcielabutil.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = span(ls, "referenceColors[] =")
    rows = []
    for l in ls[first:last - 1]:
        m = re.match(r'\s*\{\s*"([^"]+)"\s*,\s*(\d+),\s*(\d+),\s*(\d+),\s*([-\d.]+),\s*([-\d.]+),\s*([-\d.]+)\s*\}', l)
        if m:
            rows.append({"name": m.group(1), "srgb": [int(m.group(k)) for k in (2, 3, 4)],
                         "lab_d50": [float(m.group(k)) for k in (5, 6, 7)]})
    imp.write("dcmtk-8d68392d", "dcmtk/cielab_reference_colors.json",
              {"description": "8-bit sRGB colors and D50 CIELab (ICC PCS) values computed by the CSS "
                              "Color Module Level 4 sample code, as tabulated by DCMTK tcielabutil.",
               "entries": rows},
              [imp.source("dcmtk-8d68392d", rel, first, last)],
              "Rows of referenceColors converted to JSON.")
    return len(rows)


def dcmtk_ftoa(imp):
    """Expected outputs of the current OFStandard::ftoa routine (tftoa.cc)."""
    rel = "ofstd/tests/tftoa.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = span(ls, "static const tuple values[] =")
    flags = {"ftoa_zeropad": "zeropad", "ftoa_leftadj": "leftadj", "ftoa_alternate": "alternate",
             "ftoa_format_e": "format_e", "ftoa_format_f": "format_f", "ftoa_uppercase": "uppercase"}
    rows, branch = [], None
    for l in ls[first:last - 1]:
        s = l.strip()
        # Only the first #if branch: the others test the legacy routine.
        if s.startswith("#if"):
            branch = 0
            continue
        if s.startswith("#elif") or s.startswith("#else"):
            branch += 1
            continue
        if s.startswith("#endif"):
            branch = None
            continue
        if branch not in (None, 0):
            continue
        m = re.match(r'tuple\((.*),\s*(.*?),\s*(-?\d+),\s*(-?\d+),\s*"(.*)"\s*\)', s)
        if not m:
            continue
        value = m.group(1).strip()
        if "infinity" in value:
            value = "-inf" if value.startswith("-") else "inf"
        elif "NaN" in value:
            value = "nan"
        names = [] if m.group(2).strip() == "0" else [flags[f.strip().split("::")[-1]] for f in m.group(2).split("|")]
        rows.append({"value": value, "flags": names, "width": int(m.group(3)),
                     "precision": int(m.group(4)), "output": m.group(5)})
    imp.write("dcmtk-8d68392d", "dcmtk/ftoa_vectors.json",
              {"source": "DCMTK ofstd/tests/tftoa.cc at 8d68392db66cd69e13dc878dc9889681062ad479 "
                         "(BSD-3-Clause), expected outputs of the current (non-legacy) routine",
               "buffer_size": 50, "vectors": rows},
              [imp.source("dcmtk-8d68392d", rel, first, last)],
              "Rows of the first #if branch of values[] converted to JSON; the infinity and NaN "
              "expressions become the strings inf, -inf and nan.")
    return len(rows)


def dcmtk_tparser_wrong_vr(imp):
    """Expand tparser.cc wrongExplicitVRinDataset_* vectors (dctmacro.h macros)."""
    rel = "dcmdata/tests/tparser.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first = next(i for i, l in enumerate(ls) if "wrongExplicitVRinDataset_unknownTag1(" in l) + 1
    last = next(i for i, l in enumerate(ls) if l.strip() == "#undef WRONG_EXPLICIT_VR_COMMON") + 1

    def tag(g, e):
        return g.to_bytes(2, "little") + e.to_bytes(2, "little")

    def short(g, e, vr, n):
        return tag(g, e) + vr.encode() + n.to_bytes(2, "little")

    def long(g, e, vr, n):
        return tag(g, e) + vr.encode() + bytes(2) + n.to_bytes(4, "little")

    common = (short(0x0006, 0x0006, "PN", 4) + b"ABCD" + long(0x0006, 0x0008, "UT", 4) + b"EFGH"
              + short(0x0010, 0x0010, "ST", 4) + b"IJKL" + short(0x0010, 0x0040, "CS", 2) + b"O ")
    default = common + long(0x0010, 0x1005, "UN", 4) + b"0815" + short(0x0028, 0x7FE0, "LO", 4) + b"4242"
    dictlen = common + short(0x0010, 0x1005, "UN", 4) + b"0815" + long(0x0028, 0x7FE0, "LO", 4) + b"4242"
    imp.write("dcmtk-8d68392d", "dcmtk/tparser_wrong_explicit_vr.json", {
        "description": "Explicit VR Little Endian data sets whose elements carry wrong but valid VRs "
                       "(DCMTK tparser wrongExplicitVRinDataset). 'default' uses the length field size of "
                       "the VR written in the file; 'dict_length' uses the size of the dictionary VR. "
                       "Without dictionary preferences DCMTK reads 'default' and rejects 'dict_length', "
                       "keeping the written VRs PN, UT, ST, CS, UN and LO.",
        "transfer_syntax": "1.2.840.10008.1.2.1",
        "default": default.hex(), "dict_length": dictlen.hex(),
        "expected_vrs_as_written": {"(0006,0006)": "PN", "(0006,0008)": "UT", "(0010,0010)": "ST",
                                    "(0010,1005)": "UN", "(0028,7FE0)": "LO"},
        "expected_vrs_dictionary_preferred": {"(0006,0006)": "PN", "(0006,0008)": "UT", "(0010,0010)": "PN",
                                              "(0010,1005)": "PN", "(0028,7FE0)": "UR"},
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "The two byte arrays built by the WRONG_EXPLICIT_VR_COMMON macros, expanded with the "
        "dctmacro.h TAG_AND_LENGTH (tag, VR, reserved 0000, 32-bit length) and "
        "TAG_AND_LENGTH_SHORT (tag, VR, 16-bit length) layouts in little endian; expectations from "
        "testExplicitVRinDataset.")


# DCMTK value vectors (#1053). Only literal inputs and expected results are
# extracted from the OFCHECK assertions; the test programs themselves are not
# copied. Byte strings are stored with every byte mapped to the code point of
# the same number (ISO 8859-1), so ASCII values read as written.
C_STRING = r'"((?:[^"\\]|\\.)*)"'
C_ESCAPES = {"n": 0x0A, "r": 0x0D, "t": 0x09, "v": 0x0B, "f": 0x0C, "a": 0x07, "b": 0x08,
             "\\": 0x5C, '"': 0x22, "'": 0x27, "?": 0x3F}


def c_bytes(literal):
    """Decode the body of a C string or character literal to bytes."""
    out, i = bytearray(), 0
    while i < len(literal):
        ch = literal[i]
        if ch != "\\":
            out += ch.encode("latin-1")
            i += 1
            continue
        nxt = literal[i + 1]
        if nxt in "01234567":
            m = re.match(r"[0-7]{1,3}", literal[i + 1:])
            out.append(int(m.group(0), 8))
            i += 1 + len(m.group(0))
        elif nxt == "x":
            m = re.match(r"[0-9a-fA-F]+", literal[i + 2:])
            out.append(int(m.group(0), 16))
            i += 2 + len(m.group(0))
        else:
            out.append(C_ESCAPES[nxt])
            i += 2
    return bytes(out)


def c_text(literal, length=None):
    b = c_bytes(literal)
    if length is not None:
        if length > len(b) + 1:
            sys.exit(f"string literal shorter than explicit length {length}")
        b = (b + b"\0")[:length]
    return b.decode("latin-1")


def strip_c_comments(s):
    return re.sub(r"/\*.*?\*/", "", s)


def statements(ls, first, last):
    """Yield (line, statement) for the ';'-terminated statements of lines
    first..last (1-based), joining continuation lines and skipping // lines."""
    buf, start, in_block = "", None, False
    for n in range(first, last + 1):
        s = ls[n - 1].strip()
        if in_block or (not buf and s.startswith("/*")):
            in_block = "*/" not in s
            continue
        if not buf and (not s or s.startswith("//") or s.startswith("#")):
            continue
        if not buf:
            start = n
        buf = (buf + " " + s).strip()
        if buf.endswith(";") or buf.endswith("{") or buf.endswith("}") or buf.startswith("OFTEST("):
            yield start, buf
            buf = ""


def c_args(text):
    """Split a C argument list of literals into Python values."""
    text = strip_c_comments(text)
    out, pos = [], 0
    tok = re.compile(r'\s*(' + C_STRING + r'|OFTrue|OFFalse|DSRTypes::CVT_\w+|[-+]?[0-9][0-9.eE+-]*|\w+)\s*(,|$)')
    while pos < len(text):
        m = tok.match(text, pos)
        if not m:
            sys.exit(f"cannot parse arguments {text!r}")
        t = m.group(1)
        if m.group(2) is not None:
            out.append(c_text(m.group(2)))
        elif t in ("OFTrue", "OFFalse"):
            out.append(t == "OFTrue")
        else:
            out.append(t)
        pos = m.end()
    return out


def dcmtk_source_span(ls, start_pat, end_pat):
    first = next(i for i, l in enumerate(ls) if start_pat in l) + 1
    last = next(i for i in range(first, len(ls)) if end_pat in ls[i]) + 1
    return first, last


DCMTK_CHVAL_VR = {
    "ApplicationEntity": "AE", "AgeString": "AS", "CodeString": "CS", "Date": "DA", "DateTime": "DT",
    "DecimalString": "DS", "IntegerString": "IS", "LongString": "LO", "LongText": "LT", "PersonName": "PN",
    "ShortString": "SH", "ShortText": "ST", "Time": "TM", "UnlimitedCharacters": "UC",
    "UniqueIdentifier": "UI", "UniversalResourceIdentifierOrLocator": "UR", "UnlimitedText": "UT",
}


def dcmtk_chval(imp):
    """checkStringValue() verdicts of dcmdata/tests/tchval.cc."""
    rel = "dcmdata/tests/tchval.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = dcmtk_source_span(ls, "OFTEST(dcmdata_checkStringValue)", "CHECK_BAD ( \"UT-04\"")
    row = re.compile(r'^CHECK_(GOOD|BAD)\s*\(\s*"([A-Z]{2}-\d+)"\s*,\s*Dcm(\w+)::checkStringValue\((.*)\)\s*\)\s*;?$')
    rows, comment, huge = [], None, None
    for n in range(first, last + 1):
        s = ls[n - 1].strip()
        if s.startswith("//"):
            text = s.lstrip("/").strip()
            comment = None if text.startswith("CHECK_") else text
            continue
        m = re.match(r"OFString hugeString\((\d+) \* (\d+), '(.)'\);", s)
        if m:
            huge = {"fill": m.group(3), "length": int(m.group(1)) * int(m.group(2)), "set": []}
            continue
        m = re.match(r"hugeString\[hugeString\.length\(\) (/ 2|- 1)\] = '(\\?.)';", s)
        if m:
            index = huge["length"] // 2 if m.group(1) == "/ 2" else huge["length"] - 1
            huge["set"] = huge["set"] + [{"index": index, "char": c_text(m.group(2))}]
            continue
        if not s.startswith("CHECK_"):
            comment = None if s else comment
            continue
        m = row.match(s)
        if not m:
            sys.exit(f"{rel}:{n}: unparsed row {s!r}")
        vr = DCMTK_CHVAL_VR[m.group(3)]
        args = c_args(m.group(4))
        entry = {"line": n, "id": m.group(2), "vr": vr, "expect": m.group(1).lower()}
        if args[0] == "hugeString":
            entry["generated"] = json.loads(json.dumps(huge))
        else:
            entry["value"] = args[0]
        strings = [a for a in args[1:] if isinstance(a, str)]
        if vr in ("LT", "ST", "UT", "UR"):
            if strings:
                entry["charset"] = strings[0]
        else:
            if strings:
                entry["vm"] = strings[0]
            if len(strings) > 1:
                entry["charset"] = strings[1]
        flags = [a for a in args[1:] if isinstance(a, bool)]
        if flags:
            entry["old_format"] = flags[0]
        if comment:
            entry["upstream_comment"] = comment
        comment = None
        rows.append(entry)
    imp.write("dcmtk-8d68392d", "dcmtk/chval_vectors.json", {
        "description": "Verdicts of DCMTK's checkStringValue() for single VR values (tchval.cc). "
                       "'vm' is the value multiplicity checked (DCMTK's default is '1-n' when absent), "
                       "'charset' the Specific Character Set the value is checked under, 'old_format' "
                       "whether the ACR-NEMA date/time formats are accepted. 'generated' describes a "
                       "value built at run time: 'length' copies of 'fill' with 'set' characters "
                       "replaced.",
        "entries": rows,
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "The literal arguments and GOOD/BAD verdict of every active CHECK_GOOD/CHECK_BAD row, with "
        "the line number; commented-out rows are omitted. String bytes are stored as the code "
        "points of the same numbers (ISO 8859-1). The generated UT string is described by its "
        "construction parameters.")
    return len(rows)


def dcmtk_vrds(imp):
    """DcmDecimalString parse/format vectors of dcmdata/tests/tvrds.cc."""
    rel = "dcmdata/tests/tvrds.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = dcmtk_source_span(ls, "OFTEST(dcmdata_decimalString_1)", 'OFCHECK(testStr == "0.1");')
    parse, puts, cur, last_put = [], [], None, None
    put_string = re.compile(r'decStr\.(putString|putOFStringArray)\((?:OFString\()?' + C_STRING +
                            r'(?:\s*,\s*(\d+))?\s*\)+\.good\(\)')
    for n, s in statements(ls, first, last):
        m = put_string.search(s)
        if m:
            cur = {"line": n, "input": c_text(m.group(2), int(m.group(3)) if m.group(3) else None)}
            parse.append(cur)
            continue
        m = re.search(r"getFloat64Vector\(doubleVals\)\.(good|bad)\(\)", s)
        if m:
            cur["float64_ok"] = m.group(1) == "good"
            continue
        m = re.search(r"OFCHECK_EQUAL\(doubleVals\.size\(\), (\d+)\)", s)
        if m:
            cur["float64_count"] = int(m.group(1))
            continue
        m = re.search(r"OFCHECK_EQUAL\(doubleVals\[(\d+)\], ([-+0-9.eE]+)\)", s)
        if m:
            values = cur.setdefault("float64", [])
            if int(m.group(1)) != len(values):
                sys.exit(f"{rel}:{n}: out-of-order value index")
            values.append(m.group(2))
            continue
        m = re.search(r"OFCHECK_EQUAL\(decStr\.getVM\(\), (\d+)\)", s)
        if m:
            cur["vm"] = int(m.group(1))
            continue
        if s == "decStr.clear();":
            puts.append({"line": n, "op": "clear"})
            continue
        m = re.search(r"decStr\.putFloat64(Prec)?\(([^,]+),\s*(\d+)(?:,\s*(\d+))?(?:,\s*(OFTrue|OFFalse))?\)\.(good|bad)\(\)", s)
        if m:
            last_put = {"line": n, "op": "put", "value": m.group(2).strip(), "position": int(m.group(3)),
                        "ok": m.group(6) == "good"}
            if m.group(4):
                last_put["precision"] = int(m.group(4))
            if m.group(5):
                last_put["remove_trailing_zeros"] = m.group(5) == "OFTrue"
            puts.append(last_put)
            continue
        m = re.search(r"item\.putAndInsertFloat64\(DcmTag\(DCM_\w+, EVR_DS\), ([-+0-9.eE]+)\)\.good\(\)", s)
        if m:
            last_put = {"line": n, "op": "put_new_element", "value": m.group(1), "position": 0, "ok": True}
            puts.append(last_put)
            continue
        m = re.search(r"OFCHECK\(testStr == " + C_STRING + r"\)", s)
        if m:
            last_put["string"] = c_text(m.group(1))
            continue
        if re.search(r"getOFStringArray|findAndGetOFStringArray|^OFTEST|^DcmDecimalString|^OFVector|^OFString|^DcmItem|^[{}]$", s):
            continue
        sys.exit(f"{rel}:{n}: unparsed statement {s!r}")
    imp.write("dcmtk-8d68392d", "dcmtk/vrds_vectors.json", {
        "description": "DCMTK DcmDecimalString vectors (tvrds.cc). 'parse': a value string, the "
                       "resulting VM and the Float64 values (getFloat64Vector), or float64_ok false "
                       "with the count of values read before the invalid one. 'format': putFloat64 / "
                       "putFloat64Prec steps on one element; 'string' is the whole value string after the "
                       "step ('precision' defaults to 6 significant digits; 'clear' empties the element).",
        "parse": parse, "format": puts,
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "Literal inputs and expected results of the OFCHECK assertions, in source order. Values "
        "passed with an explicit length keep their embedded NUL byte.")
    return len(parse) + len(puts)


def dcmtk_vrdatim(imp):
    """DA/TM/DT vectors of dcmdata/tests/tvrdatim.cc."""
    rel = "dcmdata/tests/tvrdatim.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = dcmtk_source_span(ls, "OFTEST(dcmdata_dateTime)", 'OFCHECK_EQUAL(string, "2002-04-10 00:00");')
    vrs = {"dcmDate": "DA", "dcmTime": "TM", "dcmDateTime": "DT"}
    comps, entries, tzs, cur = {}, [], [], None
    for n, s in statements(ls, first, last):
        m = re.match(r"OF(Date|Time) (\w+)\((\d+), (\d+), (\d+)\);", s)
        if m:
            comps[m.group(2)] = [int(m.group(k)) for k in (3, 4, 5)]
            continue
        m = re.match(r"OFDateTime (\w+)\((\w+), (\w+)\);", s)
        if m:
            comps[m.group(1)] = comps[m.group(2)] + comps[m.group(3)]
            continue
        m = re.match(r"(dcm\w+)\.setOF\w+\((\w+)\);", s)
        if m:
            cur = {"line": n, "vr": vrs[m.group(1)], "components": comps[m.group(2)]}
            entries.append(cur)
            continue
        m = re.match(r"(dcm\w+)\.putString\(" + C_STRING + r"\);", s)
        if m:
            cur = {"line": n, "vr": vrs[m.group(1)], "value": c_text(m.group(2))}
            entries.append(cur)
            continue
        m = re.match(r'CHECK_EQUAL\("\(\w{4},\w{4}\) \w\w \[([^\]]*)\]', s)
        if m:
            if "value" not in cur:
                cur["dicom"] = m.group(1)
            continue
        m = re.match(r"OFCHECK\(dcm\w+\.getOF\w+\(\w+\)\.(good|bad)\(\)\);", s)
        if m:
            cur["parse_ok"] = m.group(1) == "good"
            continue
        m = re.match(r"CHECK_STREAM_EQUAL\(\w+, " + C_STRING + r"\);", s)
        if m:
            cur["printed"] = c_text(m.group(1))
            continue
        m = re.match(r"OFCHECK\(\w+\.getISOFormatted\w+\(string,(.*)\)\);", s)
        if m:
            flags = {name: v == "OFTrue" for v, name in
                     re.findall(r"(OFTrue|OFFalse)\s*/\*(\w+)\*/", m.group(1))}
            cur.setdefault("iso", []).append({"flags": flags})
            continue
        m = re.match(r"OFCHECK_EQUAL\(string, " + C_STRING + r"\);", s)
        if m:
            cur["iso"][-1]["output"] = c_text(m.group(1))
            continue
        m = re.match(r"OFCHECK\(DcmTime::getTimeZoneFromString\(" + C_STRING + r", timeZone\)\.(good|bad)\(\)\);", s)
        if m:
            tzs.append({"line": n, "value": c_text(m.group(1)), "ok": m.group(2) == "good"})
            continue
        m = re.match(r"OFCHECK_EQUAL\(timeZone, ([-+0-9.]+)\);", s)
        if m:
            tzs[-1]["hours"] = float(m.group(1))
            continue
        if re.match(r"(OFCHECK_EQUAL\(\w+, cur\w+\)|dcm\w+\.print|if |OFCHECK_FAIL|return;|double |OFDate |OFTime |"
                    r"OFDateTime |OFString |Dcm\w+ |OFOStringStream|OFTEST|[{}]$)", s):
            continue
        sys.exit(f"{rel}:{n}: unparsed statement {s!r}")
    imp.write("dcmtk-8d68392d", "dcmtk/vrdatim_vectors.json", {
        "description": "DCMTK DcmDate/DcmTime/DcmDateTime vectors (tvrdatim.cc). 'components' "
                       "(year, month, day, hour, minute, second) encode as 'dicom'; a 'value' parses "
                       "(parse_ok) to the time printed as 'printed' or formatted as 'iso' under the "
                       "given flags. 'time_zones' are getTimeZoneFromString results in hours.",
        "values": entries, "time_zones": tzs,
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "Literal inputs and expected results of the OFCHECK assertions in source order; the "
        "printed element dumps are reduced to their bracketed value.")
    return len(entries) + len(tzs)


def dcmtk_vrpn(imp):
    """Person Name component vectors of dcmdata/tests/tvrpn.cc."""
    rel = "dcmdata/tests/tvrpn.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first = next(i for i, l in enumerate(ls) if "OFTEST(dcmdata_personName)" in l) + 1
    last = max(i + 1 for i, l in enumerate(ls) if 'OFCHECK_EQUAL(s, "")' in l)
    groups, comps, cur = [], [], None
    names = {"l": "family", "f": "given", "m": "middle", "p": "prefix", "s": "suffix"}
    for n, s in statements(ls, first, last):
        m = re.match(r"OFCHECK\( DcmPersonName::getComponentGroup\(" + C_STRING + r", (\d+), resultStr\)\.(good|bad)\(\) \);", s)
        if m:
            cur = {"line": n, "value": c_text(m.group(1)), "group": int(m.group(2)), "ok": m.group(3) == "good"}
            groups.append(cur)
            continue
        m = re.match(r"OFCHECK_EQUAL\(resultStr, " + C_STRING + r"\);", s)
        if m:
            cur["result"] = c_text(m.group(1))
            continue
        m = re.match(r"OFCHECK\( DcmPersonName::getNameComponentsFromString\(" + C_STRING + r", l,f,m,p,s, (\d+)\)\.good\(\)\);", s)
        if m:
            cur = {"line": n, "value": c_text(m.group(1)), "group": int(m.group(2)), "components": {}}
            comps.append(cur)
            continue
        m = re.match(r"OFCHECK_EQUAL\(([lfmps]), " + C_STRING + r"\);", s)
        if m:
            cur["components"][names[m.group(1)]] = c_text(m.group(2))
            continue
        if re.match(r"(OFString |OFTEST|[{}]$)", s):
            continue
        sys.exit(f"{rel}:{n}: unparsed statement {s!r}")
    imp.write("dcmtk-8d68392d", "dcmtk/vrpn_vectors.json", {
        "description": "DCMTK DcmPersonName vectors (tvrpn.cc): the component group string returned for "
                       "a group index ('ok' false when DCMTK reports the group as absent) and the five "
                       "name components of a group. Strings are ISO 8859-1.",
        "component_groups": groups, "name_components": comps,
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "Literal inputs and expected results of the OFCHECK assertions in source order; the "
        "repeated final block of the upstream test is kept as written.")
    return len(groups) + len(comps)


def dcmtk_vrui(imp):
    """UI padding vectors of dcmdata/tests/tvrui.cc."""
    rel = "dcmdata/tests/tvrui.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = dcmtk_source_span(ls, "OFTEST(dcmdata_uniqueIdentifier_1)", 'OFCHECK_EQUAL(value, "5.6.7.8 ");')
    rows, cur, auto, pending = [], None, None, None
    for n, s in statements(ls, first, last):
        m = re.match(r"dcmEnableAutomaticInputDataCorrection\.set\((OFTrue|OFFalse)\);", s)
        if m:
            auto = m.group(1) == "OFTrue"
            continue
        m = re.match(r"OFCHECK\(sopInstanceUID\.putString\(" + C_STRING + r"(?:, (\d+))?\)\.good\(\)\);", s)
        if m:
            cur = {"line": n, "input": c_text(m.group(1), int(m.group(2)) if m.group(2) else None),
                   "automatic_correction": auto, "values": []}
            rows.append(cur)
            continue
        m = re.match(r"OFCHECK_EQUAL\(sopInstanceUID\.getNumberOfValues\(\), (\d+)\);", s)
        if m:
            cur["vm"] = int(m.group(1))
            continue
        m = re.match(r"OFCHECK\(sopInstanceUID\.checkValue\(" + C_STRING + r"\)\.(good|bad)\(\)\);", s)
        if m:
            cur["check_vm"] = c_text(m.group(1))
            cur["check_ok"] = m.group(2) == "good"
            continue
        m = re.match(r"OFCHECK\(sopInstanceUID\.getOFString\(value, (\d+)(.*)\)\.good\(\)\);", s)
        if m:
            pending = {"index": int(m.group(1)), "normalize": "OFFalse" not in strip_c_comments(m.group(2))}
            continue
        m = re.match(r"OFCHECK_EQUAL\(value, (?:OFString\()?" + C_STRING + r"(?:, (\d+)\))?\);", s)
        if m:
            pending["value"] = c_text(m.group(1), int(m.group(2)) if m.group(2) else None)
            cur["values"].append(pending)
            continue
        if re.match(r"(OFString |DcmUniqueIdentifier |OFTEST|[{}]$)", s):
            continue
        sys.exit(f"{rel}:{n}: unparsed statement {s!r}")
    imp.write("dcmtk-8d68392d", "dcmtk/vrui_vectors.json", {
        "description": "DCMTK DcmUniqueIdentifier padding vectors (tvrui.cc): a value string stored with "
                       "or without DCMTK's automatic input data correction, its VM, the checkValue() "
                       "verdict for 'check_vm', and the values read back with and without "
                       "normalization (padding removal).",
        "entries": rows,
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "Literal inputs and expected results of the OFCHECK assertions in source order; values "
        "passed with an explicit length keep their NUL bytes.")
    return len(rows)


def dcmtk_match(imp):
    """Attribute matching vectors of dcmdata/tests/tmatch.cc."""
    rel = "dcmdata/tests/tmatch.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = dcmtk_source_span(ls, "OFTEST(dcmdata_attribute_matching)", 'OFCHECK(!DcmAttributeMatching( EVR_PN )')
    kinds = {"wildcards": "wildcard", "date": "date", "time": "time", "datetime": "datetime",
             "uids": "uid_list", "singlevalue": "single_value"}
    rows, vr = [], None
    for n, s in statements(ls, first, last):
        m = re.match(r"OFCHECK\((!?)test_(\w+)\(\s*" + C_STRING + r"\s*(?:,\s*" + C_STRING + r"\s*)?\)\s*\);", s)
        if m:
            row = {"line": n, "kind": kinds[m.group(2)], "query": c_text(m.group(3))}
            if m.group(4) is not None:
                row["candidate"] = c_text(m.group(4))
                row["match"] = m.group(1) != "!"
            else:
                row["valid_query"] = m.group(1) != "!"
            rows.append(row)
            continue
        m = re.match(r"(?:DcmAttributeMatching )?match(?:\( | = DcmAttributeMatching\( )EVR_(\w\w) \);", s)
        if m:
            vr = m.group(1)
            continue
        m = re.match(r"OFCHECK\((!?)(?:match|DcmAttributeMatching\( EVR_(\w\w) \))\( " + C_STRING + r", \d+, " +
                     C_STRING + r", \d+ \)\);", s)
        if m:
            rows.append({"line": n, "kind": "vr", "vr": m.group(2) or vr, "query": c_text(m.group(3)),
                         "candidate": c_text(m.group(4)), "match": m.group(1) != "!"})
            continue
        if s in ("OFCHECK(match);", "OFCHECK(!!match);") or re.match(r"(OFTEST|[{}]$)", s):
            continue
        sys.exit(f"{rel}:{n}: unparsed statement {s!r}")
    imp.write("dcmtk-8d68392d", "dcmtk/match_vectors.json", {
        "description": "DCMTK DcmAttributeMatching vectors (tmatch.cc): whether a query value is a "
                       "valid date/time/datetime range query ('valid_query'), and whether a query "
                       "matches a candidate ('match') under wild card, date, time, datetime, UID list, "
                       "single value or VR-selected matching.",
        "entries": rows,
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "Literal inputs and expected results of the OFCHECK assertions in source order.")
    return len(rows)


def dcmtk_frmsiz(imp):
    """Uncompressed frame size vectors of dcmdata/tests/tfrmsiz.cc."""
    rel = "dcmdata/tests/tfrmsiz.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = dcmtk_source_span(ls, "OFTEST(dcmdata_uncompressedFrameSize)", "DcmRLEDecoderRegistration::cleanup();")
    attrs, words, codec, registered = {}, None, "native", False
    sizes, decoded, pending = [], [], False
    for n, s in statements(ls, first, last):
        m = re.search(r"putAndInsertUint16\(DCM_(\w+), (\d+)\)", s)
        if m:
            attrs[m.group(1)] = int(m.group(2))
            continue
        m = re.search(r"putAndInsertString\(DCM_(\w+), " + C_STRING + r"\)", s)
        if m:
            attrs[m.group(1)] = c_text(m.group(2))
            continue
        m = re.search(r"OFCHECK\((\d+) == px->decodedBitsAllocated\((\d+), (\d+)\)\);", s)
        if m:
            decoded.append({"line": n, "encoding": codec, "bits_allocated": int(m.group(2)),
                            "bits_stored": int(m.group(3)), "decoded_bits_allocated": int(m.group(1))})
            continue
        m = re.search(r"createUint16Array\((\d+), pxdata\)", s)
        if m:
            words = int(m.group(1))
            continue
        if "putOriginalRepresentation(EXS_RLELossless" in s:
            codec = "rle_lossless"
            continue
        if "DcmRLEDecoderRegistration::registerCodecs();" in s:
            registered = True
            continue
        row = {"line": n, "encoding": codec, "attributes": dict(sorted(attrs.items())), "pixel_data_words": words}
        if codec != "native":
            row["decoder_registered"] = registered
        if re.search(r"OFCHECK\(px->getUncompressedFrameSize\(&dset, frameSize, OFTrue\)\.good\(\)\);", s):
            pending = row
            continue
        m = re.search(r"OFCHECK\((\d+) == frameSize\);", s)
        if m:
            pending["frame_size"] = int(m.group(1))
            sizes.append(pending)
            continue
        if re.search(r"OFCHECK\(EC_InvalidValue == px->getUncompressedFrameSize\(&dset, frameSize, OFTrue\)\);", s):
            row["frame_size"] = None
            sizes.append(row)
            continue
        if re.match(r"(DcmDataset |DcmPixelData |Uint32 |Uint16 |DcmRLERepresentationParameter |DcmPixelSequence |"
                    r"DcmPixelItem |OFCHECK\(NULL|OFCHECK\(dset\.insert|OFCHECK\(\(NULL|pixelSequence->insert|if |OFTEST|"
                    r"DcmRLEDecoderRegistration::cleanup\(\);|[{}]$)", s):
            continue
        sys.exit(f"{rel}:{n}: unparsed statement {s!r}")
    imp.write("dcmtk-8d68392d", "dcmtk/frmsiz_vectors.json", {
        "description": "DCMTK getUncompressedFrameSize() and decodedBitsAllocated() vectors (tfrmsiz.cc). "
                       "Each frame size row gives the Image Pixel attributes in effect, the Pixel Data "
                       "length in 16-bit words when set, the encoding (native or RLE Lossless, with "
                       "whether DCMTK's RLE decoder was registered) and the size in bytes, or null when "
                       "DCMTK refuses the computation.",
        "frame_sizes": sizes, "decoded_bits_allocated": decoded,
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "Attribute values set by the test and the expected results of its OFCHECK assertions, "
        "replayed in source order.")
    return len(sizes) + len(decoded)


def dcmtk_genuid(imp):
    """UID generation parameters of dcmdata/tests/tgenuid.cc."""
    rel = "dcmdata/tests/tgenuid.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = dcmtk_source_span(ls, "OFTEST(dcmdata_generateUniqueIdentifier)", "if (! generated_uid_is_not_unique) break;")
    buf = next(int(m.group(1)) for l in ls[first - 1:last] for m in [re.search(r"char uid\[(\d+)\];", l)] if m)
    count = next(int(m.group(1)) for l in ls[first - 1:last] for m in [re.search(r"i < (\d+);", l)] if m)
    imp.write("dcmtk-8d68392d", "dcmtk/genuid_vectors.json", {
        "description": "DCMTK dcmGenerateUniqueIdentifier() check (tgenuid.cc): 'count' UIDs generated "
                       "in quick succession must all differ; each fits a buffer of 'buffer_size' bytes "
                       "including the terminating NUL (a 64-byte UI value).",
        "count": count, "buffer_size": buf,
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "The loop count and the UID buffer size of the test.")
    return 1


def dcmtk_atof(imp):
    """OFStandard::atof vectors of ofstd/tests/tatof.cc."""
    rel = "ofstd/tests/tatof.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = span(ls, "const ValuePair vp[] =")
    branches = {"#ifdef ENABLE_OLD_OFSTD_ATOF_IMPLEMENTATION": "old",
                "#elif defined(ENABLE_IOSTREAM_BASED_ATOF_IMPLEMENTATION)": "iostream", "#else": "cstdio"}
    rows, impl = [], None
    for n in range(first, last + 1):
        s = ls[n - 1].strip()
        if s.startswith("#"):
            impl = None if s == "#endif" else branches[s]
            continue
        m = re.match(r"\{\s*" + C_STRING + r"\s*,\s*(.+?)\s*,\s*(OFTrue|OFFalse)\s*\},?$", s)
        if not m:
            if s.startswith("{\"") or s.startswith("{ \""):
                sys.exit(f"{rel}:{n}: unparsed row {s!r}")
            continue
        expr = m.group(2)
        row = {"line": n, "input": c_text(m.group(1)), "ok": m.group(3) == "OFTrue"}
        if expr in ("HUGE_VAL", "OFnumeric_limits<double>::infinity()"):
            row["value"] = "inf"
        elif expr == "-OFnumeric_limits<double>::infinity()":
            row["value"] = "-inf"
        elif expr == "OFnumeric_limits<double>::quiet_NaN()":
            row["value"] = "nan"
        elif re.fullmatch(r"[-+]?[0-9.]+(?:[eE][-+]?[0-9]+)?F?", expr):
            row["value"] = expr.rstrip("F")
            if expr.endswith("F"):
                row["float_literal"] = True
        else:
            sys.exit(f"{rel}:{n}: unknown value expression {expr!r}")
        if impl:
            row["implementation"] = impl
        rows.append(row)
    locales, loc, locale_last = [], None, last
    for n in range(last + 1, len(ls) + 1):
        s = ls[n - 1].strip()
        m = re.match(r"std::setlocale\(LC_ALL, " + C_STRING + r"\);", s)
        if m:
            loc = c_text(m.group(1))
            locale_last = n
            continue
        m = re.match(r"d2 = OFStandard::atof\(" + C_STRING + r", &r\);", s)
        if m:
            locales.append({"line": n, "locale": loc, "input": c_text(m.group(1))})
            continue
        m = re.match(r"OFCHECK_EQUAL\(([-+0-9.eE]+), d2\);", s)
        if m:
            locales[-1]["value"] = m.group(1)
    imp.write("dcmtk-8d68392d", "dcmtk/atof_vectors.json", {
        "description": "DCMTK OFStandard::atof vectors (tatof.cc): the input, whether conversion "
                       "succeeds and the expected double (inf, -inf, nan or a decimal literal; "
                       "'float_literal' marks a C float constant, i.e. the value rounded to binary32). "
                       "Converted values must agree to 9 significant digits. 'implementation' marks the "
                       "row of one of DCMTK's alternative atof implementations (old, iostream, cstdio). "
                       "'locale_independent' conversions must not depend on the C locale.",
        "entries": rows, "locale_independent": locales,
    }, [imp.source("dcmtk-8d68392d", rel, first, locale_last)],
        "Rows of vp[] (commented-out rows omitted; every #if branch kept and labelled) and the "
        "literal inputs and results of the locale checks.")
    return len(rows) + len(locales)


def dcmtk_ofdatim(imp):
    """OFDate/OFTime/OFDateTime setter vectors of ofstd/tests/tofdatim.cc."""
    rel = "ofstd/tests/tofdatim.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = dcmtk_source_span(ls, "OFTEST(ofstd_OFDate)", 'OFCHECK(!dateTime2.setISOFormattedDateTime("20001231101530-0230  "));')
    setter = re.compile(r"OFCHECK\((!?)\s*(\w+)\.(setDate|setTime|setDateTime|setTimeZone|setSecond|setISOFormattedDate|"
                        r"setISOFormattedTime|setISOFormattedTimeZone|setISOFormattedDateTime)\((.*)\)\);$")
    rows, last_by_var = [], {}
    for n, s in statements(ls, first, last):
        m = setter.match(s)
        if m:
            args = c_args(m.group(4))
            row = {"line": n, "method": m.group(3), "ok": m.group(1) != "!"}
            if m.group(3).startswith("setISOFormatted"):
                row["value"] = args[0]
            elif any(re.fullmatch(r"[A-Za-z_]\w*", a) for a in args):
                continue  # the arguments are objects, not literals
            else:
                row["args"] = [float(a) if "." in a else int(a) for a in args]
            rows.append(row)
            last_by_var[m.group(2)] = row
            continue
        m = re.match(r"OFCHECK_EQUAL\((\w+)\.getTimeZone\(\), ([-+]?[0-9.]+)\);", s)
        if m and last_by_var.get(m.group(1), {}).get("method", "").startswith("setISOFormatted"):
            last_by_var[m.group(1)]["time_zone_hours"] = float(m.group(2))
    imp.write("dcmtk-8d68392d", "dcmtk/ofdatim_vectors.json", {
        "description": "DCMTK OFDate/OFTime/OFDateTime setter vectors (tofdatim.cc): whether a date, "
                       "time, time zone or date/time given as numbers ('args': year, month, day, hour, "
                       "minute, second, time zone in hours) or as an ISO 8601 / DICOM formatted string "
                       "('value') is accepted, and the time zone in hours a formatted time yields.",
        "entries": rows,
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "Literal arguments and verdicts of the setter assertions and the time zones checked right "
        "after them, in source order; assertions on object state (comparisons, getters and "
        "formatted output of earlier values) are not imported.")
    return len(rows)


def dcmtk_srcodvl(imp):
    """Coded entry vectors of dcmsr/tests/tsrcodvl.cc."""
    rel = "dcmsr/tests/tsrcodvl.cc"
    ls = lines_of(imp.read("dcmtk-8d68392d", rel))
    first, last = dcmtk_source_span(ls, "OFTEST(dcmsr_validCompleteOrEmptyCode)", 'OFCHECK_EQUAL(codedEntry.getCodeMeaning(), "HIPAA Privacy Rule");')
    types = {"CVT_Short": "short", "CVT_Long": "long", "CVT_URN": "urn", "CVT_auto": "auto"}

    def code(args):
        strings = [a for a in args if isinstance(a, str) and not a.startswith("DSRTypes::")]
        out = {"code_value": strings[0], "coding_scheme_designator": strings[1]}
        if len(strings) == 4:
            out["coding_scheme_version"] = strings[2]
        out["code_meaning"] = strings[-1]
        cvt = [a for a in args if isinstance(a, str) and a.startswith("DSRTypes::")]
        out["code_value_type"] = types[cvt[0].split("::")[1]] if cvt else "auto"
        checks = [a for a in args if isinstance(a, bool)]
        if checks:
            out["check"] = checks[0]
        return out

    rows, named, basic, test, cur = [], {}, {}, None, None
    for n, s in statements(ls, first, last):
        s = strip_c_comments(s)
        m = re.match(r"OFTEST\((\w+)\)", s)
        if m:
            test = m.group(1)
            continue
        m = re.match(r"const DSR(CodedEntryValue|BasicCodedEntry) (code\d)\((.*)\);$", s)
        if m:
            entry = {"line": n, "test": test, "operation": "construct", **code(c_args(m.group(3)))}
            if m.group(1) == "BasicCodedEntry":
                basic[m.group(2)] = entry
            else:
                named[m.group(2)] = entry
                rows.append(entry)
            continue
        m = re.match(r"OFCHECK\((!?)(code\d|codedEntry)\.(isValid|isComplete|isEmpty|usesEnhancedEncodingMode)\(\)\);$", s)
        if m:
            target = named[m.group(2)] if m.group(2) != "codedEntry" else cur
            key = {"isValid": "valid", "isComplete": "complete", "isEmpty": "empty",
                   "usesEnhancedEncodingMode": "enhanced_encoding"}[m.group(3)]
            target[key] = m.group(1) != "!"
            continue
        m = re.match(r"DSRCodedEntryValue codedEntry\((.*)\);$", s)
        if m:
            args = c_args(m.group(1))
            src = dict(basic[args[0]]) if args[0] in basic else code(args)
            cur = {**src, "line": n, "test": test, "operation": "construct"}
            rows.append(cur)
            continue
        m = re.match(r"codedEntry = (code\d);$", s)
        if m:
            cur = {**basic[m.group(1)], "line": n, "test": test, "operation": "assign"}
            rows.append(cur)
            continue
        m = re.match(r"OFCHECK\(codedEntry\.setCode\((.*)\)\.good\(\)\);$", s)
        if m:
            args = c_args(m.group(1))
            src = dict(basic[args[0]]) if args[0] in basic else code(args)
            if args[0] in basic and len(args) > 1:
                src["check"] = args[1]
            cur = {**src, "line": n, "test": test, "operation": "set_code", "set_ok": True}
            rows.append(cur)
            continue
        if s == "codedEntry.clear();":
            cur = {"line": n, "test": test, "operation": "clear", "code_value": "", "coding_scheme_designator": "",
                   "code_meaning": "", "code_value_type": "auto"}
            rows.append(cur)
            continue
        m = re.match(r"OFCHECK_EQUAL\(codedEntry\.getCodeValueType\(\), DSRTypes::(CVT_\w+)\);$", s)
        if m:
            cur["resulting_type"] = types[m.group(1)]
            continue
        m = re.match(r"OFCHECK_EQUAL\(codedEntry\.get(CodeValue|CodingSchemeDesignator|CodingSchemeVersion|CodeMeaning)\(\), " + C_STRING + r"\s*\);$", s)
        if m:
            key = re.sub(r"(?<!^)([A-Z])", r"_\1", m.group(1)).lower()
            cur.setdefault("read_back", {})[key] = c_text(m.group(2))
            continue
        m = re.match(r"OFCHECK\(codedEntry\.writeSequence\(dataset, DCM_\w+\)\.good\(\)\);$", s)
        if m:
            cur["written_attributes"] = {}
            continue
        m = re.match(r"OFCHECK\((!?)dataset\.tagExists(?:WithValue)?\(DCM_(\w+), OFTrue\s*\)\);$", s)
        if m:
            cur["written_attributes"][m.group(2)] = m.group(1) != "!"
            continue
        m = re.match(r"OFCHECK\((!?)\(?codedEntry (==|!=) DSRCodedEntryValue\((.*?)\)\)?\);$", s)
        if m:
            equal = (m.group(2) == "==") != (m.group(1) == "!")
            cur.setdefault("equals", []).append({**code(c_args(m.group(3))), "equal": equal})
            continue
        if re.match(r"(DcmDataset |dataset\.clear\(\);|if |OFCHECK_FAIL|return;|[{}]$)", s):
            continue
        sys.exit(f"{rel}:{n}: unparsed statement {s!r}")
    imp.write("dcmtk-8d68392d", "dcmtk/srcodvl_vectors.json", {
        "description": "DCMTK DSRCodedEntryValue vectors (tsrcodvl.cc). Each row is a code built, "
                       "assigned, set or cleared with its code value type ('auto' lets DCMTK choose "
                       "Code Value, Long Code Value or URN Code Value) and optional check flag, followed "
                       "by the expected results: valid/complete/empty, the resulting type, the attributes "
                       "a written Code Sequence item contains, the values read back and equality with "
                       "other codes.",
        "entries": rows,
    }, [imp.source("dcmtk-8d68392d", rel, first, last)],
        "Literal arguments and expected results of the OFCHECK assertions in source order.")
    return len(rows)


DCMTK_VALUE_VECTORS = (dcmtk_chval, dcmtk_vrds, dcmtk_vrdatim, dcmtk_vrpn, dcmtk_vrui, dcmtk_match,
                       dcmtk_frmsiz, dcmtk_genuid, dcmtk_atof, dcmtk_ofdatim, dcmtk_srcodvl)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--reference-root", default=os.path.join(MODULE_ROOT, "..", ".."))
    args = ap.parse_args()
    imp = Importer(os.path.abspath(args.reference_root))

    if os.path.isdir(OUT):
        for sub in ("gdcm", "dcmtk", "vtk"):
            d = os.path.join(OUT, sub)
            if os.path.isdir(d):
                shutil.rmtree(d)
    for up in UPSTREAMS:
        imp.license(up)

    counts = {}
    counts["md5_data_images"] = gdcm_md5_table(
        imp, "Testing/Source/Data/gdcmMD5DataImages.cxx", "gdcmMD5DataImages",
        "gdcm/md5_data_images.json",
        "MD5 of the decoded pixel buffer GDCM produces for each gdcmData image (TestImageReader).")
    counts["md5_data_broken_images"] = gdcm_md5_table(
        imp, "Testing/Source/Data/gdcmMD5DataBrokenImages.cxx", "gdcmMD5DataBrokenImages",
        "gdcm/md5_data_broken_images.json",
        "MD5 of the complete file GDCM writes after repairing each broken input (TestWriter).")
    counts["media_storage"] = gdcm_pair_table(
        imp, "Testing/Source/Data/gdcmMediaStorageDataFiles.cxx", "gdcmMediaStorageDataFiles",
        "gdcm/media_storage.json", "SOP Class UID GDCM assigns to each gdcmData file (empty when unknown).", "uid")
    counts["stream_offsets"] = gdcm_pair_table(
        imp, "Testing/Source/Data/gdcmStreamOffsetDataFiles.cxx", "gdcmStreamOffsetDataFiles",
        "gdcm/stream_offsets.json", "Byte offset where GDCM stops reading before Pixel Data (TestReaderUpToTag).", "offset")
    counts["selected_tags_offsets"] = gdcm_pair_table(
        imp, "Testing/Source/Data/gdcmSelectedTagsOffsetDataFiles.cxx", "gdcmSelectedTagsOffsetDataFiles",
        "gdcm/selected_tags_offsets.json", "Byte offset after reading selected tags (TestReaderSelectedTags).", "offset")
    counts["selected_private_group_offsets"] = gdcm_pair_table(
        imp, "Testing/Source/Data/gdcmSelectedPrivateGroupOffsetDataFiles.cxx",
        "gdcmSelectedPrivateGroupOffsetDataFiles", "gdcm/selected_private_group_offsets.json",
        "Byte offset after reading a selected private group (TestReaderSelectedPrivateGroup).", "offset")
    counts["black_list_reader"] = gdcm_blacklist(imp)
    counts["overlay3"] = gdcm_overlay3(imp)

    for name in ("camera", "monitor", "printer", "scanner"):
        dcmtk_verbatim(imp, f"dcmimgle/data/{name}.lut", f"dcmtk/lut/{name}.lut",
                       "Characteristic curve (DDL to luminance/optical density) used by DCMTK's GSDF tests.")
    for d in ("dcmwlm/data/wlistdb/OFFIS", "dcmwlm/data/wlistqry"):
        for fn in imp.listdir("dcmtk-8d68392d", d):
            if fn.endswith(".dump") or fn == "README":
                sub = "wlistdb" if "wlistdb" in d else "wlistqry"
                dcmtk_verbatim(imp, f"{d}/{fn}", f"dcmtk/{sub}/{fn}",
                               "Fictitious worklist data from DCMTK's example database; no real patient data.")
    counts["cielab"] = dcmtk_cielab(imp)
    counts["ftoa"] = dcmtk_ftoa(imp)
    dcmtk_tparser_wrong_vr(imp)
    for fn in DCMTK_VALUE_VECTORS:
        counts[fn.__name__[len("dcmtk_"):]] = fn(imp)
    counts["gb18030"] = dcmtk_c_byte_arrays(
        imp, "oficonv/tests/tables_gb18030.h", "dcmtk/oficonv_gb18030.json",
        "GB18030 byte sequences and their UTF-8 conversions from DCMTK's oficonv unit tests.")
    counts["gb2312"] = dcmtk_c_byte_arrays(
        imp, "oficonv/tests/tables_gb2312.h", "dcmtk/oficonv_gb2312.json",
        "GB2312 byte sequences and their UTF-8 conversions from DCMTK's oficonv unit tests.")

    manifest = {"schema": 1, "upstreams": {}, "artifacts": []}
    if os.path.exists(MANIFEST):
        with open(MANIFEST, encoding="utf-8") as f:
            manifest = json.load(f)
    manifest["artifacts"] = [a for a in manifest["artifacts"] if a.get("generated_by") != OWNER]
    manifest["artifacts"] += imp.artifacts
    manifest["artifacts"].sort(key=lambda a: a["path"])
    for up, meta in UPSTREAMS.items():
        manifest["upstreams"][up] = meta
    manifest["upstreams"] = dict(sorted(manifest["upstreams"].items()))
    with open(MANIFEST, "w", encoding="utf-8", newline="\n") as f:
        json.dump(manifest, f, indent=1, ensure_ascii=False)
        f.write("\n")
    print(json.dumps(counts, indent=1))
    print(f"{len(imp.artifacts)} artifacts")


if __name__ == "__main__":
    main()
