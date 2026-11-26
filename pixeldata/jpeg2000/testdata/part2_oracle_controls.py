"""Independent control vectors for the bounded Annex J oracle."""
import argparse
import copy
import importlib.util
import math
import os
from pathlib import Path
import struct
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True

parser = argparse.ArgumentParser()
parser.add_argument("--encoder", required=True)
parser.add_argument("--decoder", required=True)
args = parser.parse_args()
spec = importlib.util.spec_from_file_location("part2_oracle", Path(__file__).with_name("part2_oracle.py"))
oracle = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = oracle
spec.loader.exec_module(oracle)


def marker(code, body):
    return struct.pack(">HH", code, len(body)+2)+body


def attach_header(codestream, inverse, offsets):
    cursor, header = 2, bytearray(codestream[:2])
    while True:
        code, length = struct.unpack_from(">HH", codestream, cursor)
        if code == 0xFF90:
            break
        segment = bytearray(codestream[cursor:cursor+length+2])
        if code == 0xFF51:
            segment[4:6] = struct.pack(">H", 0x8100)
        if code == 0xFF52:
            segment[8] = 2
        header.extend(segment)
        cursor += length+2
    header.extend(marker(0xFF78, struct.pack(">HBB", 2, 0x87, 0x87)))
    # Independent two-component dependency with arbitrary integer
    # prediction coefficient and power-of-two divisor.
    header.extend(marker(0xFF74, struct.pack(">HHHii", 0, 0x0407, 0, *inverse)))
    header.extend(marker(0xFF74, struct.pack(">HHHii", 0, 0x0609, 0, *offsets)))
    mcc = struct.pack(">HBHHBHBBHBB", 0, 3, 0, 1, 0, 2, 0, 1, 2, 0, 1)+bytes([1, 9, 7])
    header.extend(marker(0xFF75, mcc))
    header.extend(marker(0xFF77, bytes([1, 3])))
    return bytes(header)+codestream[cursor:]


with tempfile.TemporaryDirectory(prefix="annex-j-independent-controls-") as directory:
    base = Path(directory)
    planes = [[-7, -3, 0, 4, 9, 14], [6, -6, 3, -3, 5, -5]]
    raw = base/"planes.raw"
    raw.write_bytes(struct.pack("<"+"b"*12, *(planes[0]+planes[1])))
    coded = base/"planes.j2k"
    completed = subprocess.run([args.encoder, "-i", str(raw), "-o", str(coded),
                                "-F", "3,2,2,8,s", "-n", "1", "-mct", "0"],
                               capture_output=True)
    assert completed.returncode == 0, completed.stdout+completed.stderr
    direct = coded.read_bytes()
    for coefficients, offsets in [((1, 1), (0, 0)), ((-3, 2), (13, -7)), ((0, 4), (-9, 4))]:
        stream = attach_header(direct, coefficients, offsets)
        descriptor, decoded = oracle.decode(stream, args.decoder)
        p0 = [value+offsets[0] for value in planes[0]]
        p1 = [planes[1][i]+offsets[1]+(2*coefficients[0]*p0[i]+coefficients[1])//(2*coefficients[1])
              for i in range(6)]
        assert decoded == [p0, p1], (coefficients, offsets, decoded, [p0, p1])
        print("independent OpenJPEG-coded dependency control passed", coefficients, offsets)
    stream = attach_header(direct, (1, 1), (0, 0))
    # A nonidentity irreversible matrix has different source and output
    # component order. This catches a decoder hard-coded to cumulative sums.
    floating = base/"floating.j2k"
    completed = subprocess.run([args.encoder, "-i", str(raw), "-o", str(floating),
                                "-F", "3,2,2,8,s", "-n", "2", "-mct", "0", "-I"],
                               capture_output=True)
    assert completed.returncode == 0, completed.stdout+completed.stderr
    irreversible = bytearray(attach_header(floating.read_bytes(), (1, 1), (0, 0)))
    descriptor = oracle.parse_header(stream)
    replacements = {}
    # Locate the variable-length header without assuming reversible QCD length.
    cursor, ranges = 2, []
    while True:
        code, length = struct.unpack_from(">HH", irreversible, cursor)
        if code == 0xff90:
            break
        ranges.append((code, cursor, cursor+length+2))
        cursor += length+2
    matrices = [entry for entry in ranges if entry[0] == 0xff74]
    start, end = matrices[0][1:]
    replacements[start] = (end, marker(0xff74, struct.pack(">HHHffff", 0, 0x0907, 0, 0, 1, -2, 0.5)))
    start, end = matrices[1][1:]
    replacements[start] = (end, marker(0xff74, struct.pack(">HHHii", 0, 0x0609, 0, 5, -3)))
    for code, start, end in ranges:
        if code == 0xff75:
            payload = bytearray(irreversible[start+4:end])
            payload[7], payload[-3] = 1, 0
            replacements[start] = (end, marker(code, payload))
    rebuilt = bytearray(irreversible[:2])
    for _, start, end in ranges:
        rebuilt.extend(replacements.get(start, (end, irreversible[start:end]))[1])
    rebuilt.extend(irreversible[cursor:])
    parsed = oracle.parse_header(bytes(rebuilt))
    assert parsed.transform == 1 and parsed.coefficients == (0, 1, -2, 0.5)
    environment = dict(os.environ, DICOM_GO_PART2_FLOAT_DIR=str(base))
    completed = subprocess.run([os.environ["DICOM_GO_OPENJPEG_FLOAT"], "-i", str(floating),
                                "-o", str(base/"control.pgx")], env=environment, capture_output=True)
    assert completed.returncode == 0, completed.stdout+completed.stderr
    float_planes = [[value[0] for value in struct.iter_unpack("<f", (base/f"component{i}.f32").read_bytes())]
                    for i in range(2)]
    assert any(abs(value-round(value)) > 0.001 for plane in float_planes for value in plane)
    _, restored = oracle.decode(bytes(rebuilt), args.decoder)
    expected = [[value+5 for value in float_planes[1]],
                [-2*float_planes[0][i]+0.5*float_planes[1][i]-3 for i in range(6)]]
    assert restored == expected, (restored, expected)
    rounded_early = oracle.reconstruct(parsed, [[math.floor(value+0.5) for value in plane]
                                               for plane in float_planes])
    assert rounded_early != restored, "control must detect premature integer conversion"
    print("independent OpenJPEG-coded nonidentity float matrix control passed")
    print("premature integer conversion produces a different control result")
    permuted_stream = bytearray(rebuilt)
    mcc_start = next(start for code, start, end in parsed.ranges if code == 0xff75)
    permuted_stream[mcc_start+14:mcc_start+16] = bytes([1, 0])
    permuted_stream[mcc_start+18:mcc_start+20] = bytes([1, 0])
    permuted = oracle.parse_header(bytes(permuted_stream))
    assert permuted.inputs == permuted.outputs == (1, 0)
    assert oracle.reconstruct(permuted, planes) == [
        [-2*planes[1][i]+0.5*planes[0][i]-3 for i in range(6)],
        [value+5 for value in planes[0]],
    ]
    print("independent component permutation control passed")
    descriptor = oracle.parse_header(stream)
    cases = {}
    def altered(offset, value):
        copy = bytearray(stream)
        copy[offset] = value
        return copy
    cases["wrong extension bits"] = altered(6, 0x80)
    positions = {code:start for code,start,end in descriptor.ranges}
    cases["SGcod 1"] = altered(positions[0xFF52]+8, 1)
    cases["MCO absent index"] = altered(positions[0xFF77]+5, 12)
    cases["wrong CBD count"] = altered(positions[0xFF78]+5, 3)
    cases["duplicate MCC input"] = altered(positions[0xFF75]+15, 0)
    cases["unsigned coded component"] = altered(42, 7)
    cases["truncated"] = stream[:-3]
    cases["trailing payload"] = stream+b"\0\1"
    for title, data in cases.items():
        try:
            oracle.parse_header(data)
        except (oracle.ProfileError, struct.error):
            print("rejected invalid control", title)
        else:
            raise AssertionError("oracle admitted "+title)

    import pydicom
    from pydicom.dataset import FileDataset, FileMetaDataset
    from pydicom.encaps import encapsulate
    meta = FileMetaDataset()
    meta.TransferSyntaxUID = "1.2.840.10008.1.2.4.92"
    meta.MediaStorageSOPClassUID = "1.2.840.10008.5.1.4.1.1.7.2"
    meta.MediaStorageSOPInstanceUID = "2.25.184101"
    meta.ImplementationClassUID = "2.25.184102"
    dataset = FileDataset("", {}, file_meta=meta, preamble=bytes(128))
    dataset.SOPClassUID = meta.MediaStorageSOPClassUID
    dataset.SOPInstanceUID = meta.MediaStorageSOPInstanceUID
    dataset.Rows, dataset.Columns, dataset.NumberOfFrames = 2, 3, 2
    dataset.SamplesPerPixel, dataset.PhotometricInterpretation = 1, "MONOCHROME2"
    dataset.BitsAllocated, dataset.BitsStored, dataset.HighBit, dataset.PixelRepresentation = 8, 8, 7, 1
    dataset.PixelData = encapsulate([stream], has_bot=False)
    dataset["PixelData"].is_undefined_length = True
    part10 = base/"valid.dcm"
    pydicom.dcmwrite(part10, dataset, enforce_file_format=True)
    oracle.read_part10(part10)
    print("independent pydicom-created valid Part 10 accepted")
    native = copy.deepcopy(dataset)
    native.file_meta.TransferSyntaxUID = "1.2.840.10008.1.2.1"
    expected = planes[0] + [planes[0][i]+planes[1][i] for i in range(6)]
    native.PixelData = struct.pack("<"+"b"*len(expected), *expected)
    native["PixelData"].is_undefined_length = False
    native_path = base/"source.dcm"
    pydicom.dcmwrite(native_path, native, enforce_file_format=True)
    command = [sys.executable, str(Path(__file__).with_name("part2_oracle.py")),
               "--decoder", args.decoder, "--part10", "--input", str(part10),
               "--source-native", str(native_path)]
    completed = subprocess.run(command, capture_output=True)
    assert completed.returncode == 0, completed.stdout+completed.stderr
    changed = bytearray(native.PixelData)
    changed[0] += 1
    native.PixelData = bytes(changed)
    pydicom.dcmwrite(native_path, native, enforce_file_format=True)
    completed = subprocess.run(command, capture_output=True)
    assert completed.returncode != 0 and b"maximum reconstruction error 1 > 0" in completed.stderr
    print("wrong expected native sample rejected by end-to-end comparison")
    cases = [
        ("frame count", "NumberOfFrames", 3),
        ("bit count", "BitsStored", 7),
        ("high bit", "HighBit", 3),
        ("representation", "PixelRepresentation", 0),
        ("photometric", "PhotometricInterpretation", "RGB"),
    ]
    for name, attribute, value in cases:
        before = getattr(dataset, attribute)
        setattr(dataset, attribute, value)
        bad = base/f"invalid-{attribute}.dcm"
        pydicom.dcmwrite(bad, dataset, enforce_file_format=True)
        try:
            oracle.read_part10(bad)
        except oracle.ProfileError:
            print("rejected DICOM mismatch", name)
        else:
            raise AssertionError("oracle admitted DICOM "+name)
        setattr(dataset, attribute, before)
    encoded = bytearray(part10.read_bytes())
    identity = b"2.25.184101"
    assert encoded.count(identity) == 2
    position = encoded.rindex(identity)
    encoded[position+len(identity)-1] = ord("3")
    bad = base/"invalid-identity.dcm"
    bad.write_bytes(encoded)
    try:
        oracle.read_part10(bad)
    except oracle.ProfileError:
        print("rejected DICOM mismatch identity")
    else:
        raise AssertionError("oracle admitted mismatched SOP identity")
