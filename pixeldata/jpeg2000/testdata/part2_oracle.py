"""Independent bounded Annex J decoder, using OpenJPEG for spatial coding.

pydicom reads original DICOM Part 10. No production Go parser, operator, or
expected sample values influence reconstruction. OpenJPEG itself does not
support the complete SGcod=2 stream.
"""
import argparse
from dataclasses import dataclass
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import shutil
import struct
import subprocess
import sys
import tempfile


class ProfileError(ValueError):
    """Malformed input or data outside the supported profile."""


def require(condition, message):
    if not condition:
        raise ProfileError(message)


@dataclass(frozen=True)
class Header:
    width: int
    height: int
    count: int
    depths: tuple
    signed: tuple
    coded_depths: tuple
    coded_signed: tuple
    reversible: bool
    transform: int
    coefficients: tuple
    offsets: tuple
    inputs: tuple
    outputs: tuple
    ranges: tuple
    tile_start: int
    end: int


def read_array(payload):
    require(len(payload) >= 6, "truncated MCT")
    part, flags, last = struct.unpack_from(">HHH", payload)
    require(part == last == 0 and flags & 0xF000 == 0, "unsupported split MCT")
    purpose, representation, index = (flags >> 8) & 3, (flags >> 10) & 3, flags & 255
    require(purpose <= 2 and index != 0, "invalid MCT array index/purpose")
    code, width = (("h", 2), ("i", 4), ("f", 4), ("d", 8))[representation]
    require((len(payload)-6) % width == 0, "incomplete MCT coefficient")
    values = tuple(v[0] for v in struct.iter_unpack(">"+code, payload[6:]))
    require(all(math.isfinite(v) for v in values), "nonfinite MCT coefficient")
    return index, purpose, representation, values


def parse_header(data):
    require(data[:2] == b"\xff\x4f", "missing SOC")
    segments, ranges, cursor = {}, [], 2
    allowed = {0xFF51, 0xFF52, 0xFF5C, 0xFF78, 0xFF74, 0xFF75, 0xFF77, 0xFF64}
    while True:
        require(len(data)-cursor >= 4, "truncated marker")
        marker, length = struct.unpack_from(">HH", data, cursor)
        if marker == 0xFF90:
            break
        require(marker in allowed and 2 <= length <= len(data)-cursor-2,
                "unknown extension or invalid segment length")
        payload = data[cursor+4:cursor+2+length]
        segments.setdefault(marker, []).append(payload)
        ranges.append((marker, cursor, cursor+2+length))
        cursor += 2+length
    for marker in (0xFF51, 0xFF52, 0xFF5C, 0xFF78, 0xFF75, 0xFF77):
        require(len(segments.get(marker, ())) == 1, f"missing/duplicate marker {marker:#x}")
    siz = segments[0xFF51][0]
    require(len(siz) >= 36, "truncated SIZ")
    flags, width, height, x0, y0, tw, th, tx, ty, count = struct.unpack(">H8IH", siz[:36])
    require(flags == 0x8100, "SIZ must declare only Part 2 Annex J")
    require(2 <= count <= 16 and len(siz) == 36+3*count, "component count")
    require(0 < width <= 65535 and 0 < height <= 65535 and width*height*count <= 2097152,
            "collection sample budget")
    require((x0, y0, tw, th, tx, ty) == (0, 0, width, height, 0, 0), "tile geometry")
    coded_depths = tuple((siz[36+3*i] & 127)+1 for i in range(count))
    coded_signed = tuple(bool(siz[36+3*i] & 128) for i in range(count))
    require(all(1 <= depth <= 17 for depth in coded_depths), "coded precision")
    require(all(coded_signed), "only signed coded components are supported")
    require(all(siz[37+3*i:39+3*i] == b"\x01\x01" for i in range(count)), "subsampling")
    cod = segments[0xFF52][0]
    require(len(cod) == 10 and cod[:5] == bytes([0, 0, 0, 1, 2]), "coding style/SGcod")
    require(cod[5] <= 16 and cod[6:9] == bytes([4, 4, 0]) and cod[9] in (0, 1), "spatial coding style")
    spatial_reversible = bool(cod[9])
    qcd = segments[0xFF5C][0]
    bands = 1+3*cod[5]
    require(len(qcd) == 1 + bands*(1 if spatial_reversible else 2), "quantization size")
    require(qcd[0] & 31 == (0 if spatial_reversible else 2), "quantization style")
    cbd = segments[0xFF78][0]
    require(len(cbd) == count+2 and struct.unpack_from(">H", cbd)[0] == count, "CBD count")
    depths = tuple((v & 127)+1 for v in cbd[2:])
    signs = tuple(bool(v & 128) for v in cbd[2:])
    require(len(set(depths)) == len(set(signs)) == 1 and 1 <= depths[0] <= 16,
            "mixed reconstructed precision")
    arrays = {}
    for payload in segments.get(0xFF74, ()):
        index, purpose, representation, values = read_array(payload)
        require(index not in arrays, "duplicate MCT array")
        arrays[index] = purpose, representation, values
    mcc = segments[0xFF75][0]
    require(len(mcc) >= 13, "truncated MCC")
    part, index, last, collections = struct.unpack_from(">HBHH", mcc)
    require(part == last == 0 and collections == 1 and index != 0, "MCC stage profile")
    kind, pos, lists = mcc[7], 8, []
    require(kind in (0, 1), "unsupported transformation kind")
    for _ in range(2):
        require(pos+2 <= len(mcc), "truncated MCC indices")
        raw_count = struct.unpack_from(">H", mcc, pos)[0]
        pos += 2
        entries, index_width = raw_count & 32767, 2 if raw_count & 32768 else 1
        require(entries == count and pos+entries*index_width <= len(mcc), "MCC index count")
        ids = tuple(int.from_bytes(mcc[pos+i*index_width:pos+(i+1)*index_width], "big") for i in range(entries))
        require(sorted(ids) == list(range(count)), "MCC duplicate/out-of-range indices")
        lists.append(ids)
        pos += entries*index_width
    require(len(mcc)-pos == 3, "MCC transform parameters")
    parameters = int.from_bytes(mcc[pos:], "big")
    require(parameters & ~0x1FFFF == 0, "reserved MCC flags")
    reversible = bool(parameters & 0x10000)
    require(reversible == spatial_reversible, "mixed reversible transform profile")
    transform_id, offset_id = parameters & 255, (parameters >> 8) & 255
    require(transform_id in arrays and offset_id in arrays and transform_id != offset_id, "missing transform array")
    purpose, representation, matrix = arrays[transform_id]
    require(purpose == kind, "MCT/MCC transformation kind mismatch")
    purpose, offset_representation, offsets = arrays[offset_id]
    require(purpose == 2 and len(offsets) == count, "MCT offset count/purpose")
    require(set(arrays) == {transform_id, offset_id}, "unused transform arrays")
    if kind == 0:
        require(reversible and representation in (0, 1) and offset_representation in (0, 1), "dependency integer arrays")
        require(len(matrix) == (count-1)*(count+2)//2, "dependency coefficient count")
        position = 0
        for row in range(1, count):
            divisor = matrix[position+row]
            require(divisor > 0 and divisor & (divisor-1) == 0, "dependency divisor")
            position += row+1
    else:
        require(not reversible and len(matrix) == count*count, "decorrelation matrix")
    require(segments[0xFF77] == [bytes([1, index])], "MCO stage reference")
    tile_start = cursor
    require(len(data)-cursor >= 16, "truncated tile")
    marker, length, tile, tile_length, part, parts = struct.unpack_from(">HHHIBB", data, cursor)
    require(marker == 0xFF90 and length == 10 and tile == part == 0 and parts == 1, "tile header")
    require(tile_length >= 14 and tile_length <= len(data)-cursor-2, "tile length")
    require(data[cursor+12:cursor+14] == b"\xff\x93", "missing SOD")
    end = cursor+tile_length+2
    require(data[end-2:end] == b"\xff\xd9", "missing EOC")
    require(end == len(data) or end & 1 and data[end:] == b"\0", "trailing codestream bytes")
    return Header(width, height, count, depths, signs, coded_depths, coded_signed, reversible,
                  kind, matrix, offsets, *lists, tuple(ranges), tile_start, end)


def coded_stream(data, header):
    out = bytearray(data[:2])
    for marker, start, end in header.ranges:
        if marker in (0xFF74, 0xFF75, 0xFF77, 0xFF78):
            continue
        segment = bytearray(data[start:end])
        if marker == 0xFF51:
            segment[4:6] = b"\0\0"
        if marker == 0xFF52:
            segment[8] = 0
        out.extend(segment)
    out.extend(data[header.tile_start:header.end])
    return out


def decode(data, decoder):
    header = parse_header(data)
    with tempfile.TemporaryDirectory(prefix="dicomgo-part2-oracle-") as directory:
        base = Path(directory)
        source, output = base/"components.j2k", base/"decoded.pgx"
        source.write_bytes(coded_stream(data, header))
        environment = dict(os.environ)
        if not header.reversible:
            decoder = environment.get("DICOM_GO_OPENJPEG_FLOAT", "")
            require(decoder, "irreversible decoding requires the pinned float-coefficient extractor")
            environment["DICOM_GO_PART2_FLOAT_DIR"] = str(base)
        run = subprocess.run([decoder, "-i", str(source), "-o", str(output)],
                             capture_output=True, env=environment)
        require(run.returncode == 0, f"OpenJPEG failed: {(run.stdout+run.stderr).decode(errors='replace')}")
        coded = []
        for component in range(header.count):
            if not header.reversible:
                floats = base/f"component{component}.f32"
                require(floats.is_file(), "float extractor output missing")
                plane = [value[0] for value in struct.iter_unpack("<f", floats.read_bytes())]
                require(len(plane) == header.width*header.height and
                        all(math.isfinite(value) for value in plane), "invalid float coefficient plane")
                coded.append(plane)
                continue
            blob = (base/f"decoded_{component}.pgx").read_bytes()
            line, pixels = blob.split(b"\n", 1)
            magic, byteorder, sign, bits, width, height = line.split()
            require(magic == b"PG" and byteorder in (b"ML", b"LM"), "PGX encoding")
            require((int(width), int(height), int(bits)) ==
                    (header.width, header.height, header.coded_depths[component]), "PGX dimensions")
            require((sign == b"-") == header.coded_signed[component], "PGX signedness")
            signed, depth = header.coded_signed[component], header.coded_depths[component]
            code = ("b" if signed else "B") if depth <= 8 else ("h" if signed else "H") if depth <= 16 else ("i" if signed else "I")
            plane = [v[0] for v in struct.iter_unpack((">" if byteorder == b"ML" else "<")+code, pixels)]
            require(len(plane) == header.width*header.height, "PGX sample count")
            coded.append(plane)
    return header, reconstruct(header, coded)


def reconstruct(header, coded):
    """Apply only the decoded marker semantics to the coded components."""
    require(len(coded) == header.count and
            all(len(plane) == header.width*header.height for plane in coded), "coded plane dimensions")
    ordered, decoded, pos = [coded[i] for i in header.inputs], [], 0
    for row in range(header.count):
        if header.transform == 0:
            coefficients = header.coefficients[pos:pos+row] if row else ()
            divisor = header.coefficients[pos+row] if row else 1
            if row:
                pos += row+1
            decoded.append([ordered[row][i]+header.offsets[row]+
                            (2*sum(coefficients[k]*decoded[k][i] for k in range(row))+divisor)//(2*divisor)
                            for i in range(header.width*header.height)])
        else:
            coefficients = header.coefficients[row*header.count:(row+1)*header.count]
            decoded.append([sum(coefficients[k]*ordered[k][i] for k in range(header.count))+header.offsets[row]
                            for i in range(header.width*header.height)])
    result = [None]*header.count
    for row, index in enumerate(header.outputs):
        result[index] = decoded[row]
    return result


def read_part10(path):
    import pydicom
    from io import BytesIO
    from pydicom.encaps import generate_fragments, parse_basic_offsets
    require(pydicom.__version__ == "3.0.2", "pydicom version must be 3.0.2")
    dataset = pydicom.dcmread(path)
    syntax = str(dataset.file_meta.TransferSyntaxUID)
    require(syntax in ("1.2.840.10008.1.2.4.92", "1.2.840.10008.1.2.4.93"), "Part 2 transfer syntax")
    require(dataset[0x7FE00010].is_undefined_length, "encapsulated Pixel Data length")
    require(dataset.SOPInstanceUID == dataset.file_meta.MediaStorageSOPInstanceUID, "SOP identity mismatch")
    require(dataset.SOPClassUID == dataset.file_meta.MediaStorageSOPClassUID, "SOP class mismatch")
    stream = BytesIO(dataset.PixelData)
    require(not parse_basic_offsets(stream), "collection BOT must be empty")
    fragments = list(generate_fragments(stream))
    require(len(fragments) == 1, "expected one component collection")
    header = parse_header(fragments[0])
    require(dataset.SamplesPerPixel == 1 and "PlanarConfiguration" not in dataset, "monochrome layout")
    require(dataset.PhotometricInterpretation in ("MONOCHROME1", "MONOCHROME2"), "photometric interpretation")
    require((dataset.Rows, dataset.Columns, int(dataset.NumberOfFrames)) ==
            (header.height, header.width, header.count), "DICOM dimensions/frame count")
    require(dataset.BitsAllocated in (8, 16) and dataset.BitsStored <= dataset.BitsAllocated, "allocated bits")
    require(dataset.HighBit == dataset.BitsStored-1 and dataset.BitsStored == header.depths[0] and
            dataset.PixelRepresentation == int(header.signed[0]), "stored precision/sign")
    require(syntax.endswith(".93") or header.reversible, "lossless UID with irreversible transform")
    if not header.reversible:
        require(dataset.get("LossyImageCompression") == "01", "missing lossy history")
        methods = dataset.get("LossyImageCompressionMethod")
        require("ISO_15444_1" in ([methods] if isinstance(methods, str) else list(methods or [])), "missing lossy method")
    return dataset, fragments[0]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--decoder", required=True)
    parser.add_argument("--input", required=True)
    parser.add_argument("--expected")
    parser.add_argument("--allocated", type=int)
    parser.add_argument("--part10", action="store_true")
    parser.add_argument("--source-native")
    parser.add_argument("--output")
    parser.add_argument("--limit", type=int, default=0)
    args = parser.parse_args()
    require(args.limit >= 0, "negative comparison limit")
    if args.part10:
        dataset, data = read_part10(args.input)
        allocated = dataset.BitsAllocated
    else:
        data, allocated = Path(args.input).read_bytes(), args.allocated
    header, observed_frames = decode(data, args.decoder)
    output_bits, output_signed = header.depths[0], header.signed[0]
    minimum = -(1 << (output_bits-1)) if output_signed else 0
    maximum = (1 << (output_bits-1))-1 if output_signed else (1 << output_bits)-1
    # Part 1 sample reconstruction rounds to the nearest code and clamps
    # to the reconstructed CBD precision after the component transform.
    observed_frames = [[min(maximum, max(minimum, math.floor(value+0.5)))
                        for value in plane] for plane in observed_frames]
    if args.source_native:
        import pydicom
        source = pydicom.dcmread(args.source_native)
        require(source.file_meta.TransferSyntaxUID.is_little_endian and
                not source.file_meta.TransferSyntaxUID.is_compressed, "native little-endian source required")
        for field in ("Rows", "Columns", "NumberOfFrames", "BitsStored", "BitsAllocated",
                      "HighBit", "PixelRepresentation", "PhotometricInterpretation", "SamplesPerPixel"):
            require(source.get(field) == dataset.get(field), f"source layout differs: {field}")
        native = source.pixel_array
        require(native.shape == (header.count, header.height, header.width), "native shape")
        expected = native.flatten().tolist()
        if header.reversible:
            require(source.SOPInstanceUID == dataset.SOPInstanceUID, "lossless identity changed")
        else:
            require(source.SOPInstanceUID != dataset.SOPInstanceUID, "lossy identity unchanged")
    else:
        require(args.expected and allocated in (8, 16), "raw expected samples required")
        fmt = "<" + (("b" if header.signed[0] else "B") if allocated == 8 else ("h" if header.signed[0] else "H"))
        expected = [v[0] for v in struct.iter_unpack(fmt, Path(args.expected).read_bytes())]
    observed = [v for plane in observed_frames for v in plane]
    require(len(observed) == len(expected), "reference sample count")
    error = max(abs(a-b) for a,b in zip(observed, expected))
    require(error <= args.limit, f"maximum reconstruction error {error} > {args.limit}")
    squared_error = sum((a-b)**2 for a, b in zip(observed, expected))/len(expected)
    spatial_decoder = args.decoder if header.reversible else os.environ["DICOM_GO_OPENJPEG_FLOAT"]
    decoder_path = Path(shutil.which(spatial_decoder) or spatial_decoder)
    result = dict(decoder="independent Python Annex J + OpenJPEG spatial decoder",
                  input_sha256=hashlib.sha256(Path(args.input).read_bytes()).hexdigest(),
                  codestream_sha256=hashlib.sha256(data).hexdigest(),
                  reference_sha256=hashlib.sha256(Path(args.source_native or args.expected).read_bytes()).hexdigest(),
                  oracle_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                  spatial_decoder_sha256=hashlib.sha256(decoder_path.read_bytes()).hexdigest(),
                  spatial_precision="integer" if header.reversible else "unrounded float32",
                  python=sys.version.split()[0], platform=platform.platform(),
                  samples=len(expected), frames=header.count, bits=header.depths[0], signed=header.signed[0],
                  reversible=header.reversible, max_error=error, limit=args.limit, part10=args.part10,
                  mean_absolute_error=sum(abs(a-b) for a, b in zip(observed, expected))/len(expected),
                  psnr_db=10*math.log10((maximum-minimum)**2/squared_error) if squared_error else None)
    if args.part10:
        import pydicom
        import numpy
        result.update(pydicom=pydicom.__version__, numpy=numpy.__version__)
    if args.output:
        Path(args.output).write_text(json.dumps(result, sort_keys=True, indent=2)+"\n")
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    main()
