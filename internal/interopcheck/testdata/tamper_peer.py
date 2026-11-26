"""Independent tampering peer; pydicom 3.0.2 / pynetdicom 3.0.4.

Storage SCP and Study Root C-GET SCP that always answers success. The calling
AE title of each association selects a negative control, applied with pydicom
independently of dicom-go:

  none              persist and return the received dataset unchanged
  delete-attribute  drop Study Description
  relabel-syntax    declare Explicit VR Big Endian over little-endian bytes
  distant-sample    flip bits of the last sample
  remove-frame      drop the last frame and decrement Number of Frames
  corrupt-sequence  alter the Code Value in the first coded item

C-STORE saves <dir>/<mode>.dcm; C-GET returns the stored dataset with the same
mutation (relabel-syntax has no wire form and is refused for C-GET). Prints
"PORT n" when ready; stops when stdin closes.
"""
import copy
import os
import sys

import pydicom
import pynetdicom
from pynetdicom import AE, evt
from pydicom.uid import ExplicitVRLittleEndian
from pynetdicom.sop_class import (
    MultiFrameGrayscaleWordSecondaryCaptureImageStorage as STORAGE,
    StudyRootQueryRetrieveInformationModelGet as GET,
)

if os.environ.get("DICOMGO_PEER_DEBUG"):
    from pynetdicom import debug_logger
    debug_logger()

assert pydicom.__version__ == "3.0.2" and pynetdicom.__version__ == "3.0.4"
MODES = {"none", "delete-attribute", "relabel-syntax", "distant-sample", "remove-frame", "corrupt-sequence"}
LE = b"1.2.840.10008.1.2.1\x00"
BE = b"1.2.840.10008.1.2.2\x00"
out_dir = sys.argv[1]
stored = {}


def mode_of(event):
    mode = event.assoc.requestor.ae_title.strip()
    if mode not in MODES:
        raise ValueError("unknown mode")
    return mode


def mutate(ds, mode):
    ds = copy.deepcopy(ds)
    if mode == "delete-attribute":
        del ds.StudyDescription
    elif mode in ("distant-sample", "remove-frame"):
        pixels = bytearray(ds.PixelData)
        if mode == "distant-sample":
            pixels[-2] ^= 0x5A
        else:
            frames = int(ds.NumberOfFrames)
            frame = len(pixels) // frames
            del pixels[-frame:]
            ds.NumberOfFrames = frames - 1
        ds.PixelData = bytes(pixels)
    elif mode == "corrupt-sequence":
        item = ds.ProcedureCodeSequence[0]
        item.CodeValue = chr(ord(item.CodeValue[0]) ^ 1) + item.CodeValue[1:]
    return ds


def on_store(event):
    try:
        mode = mode_of(event)
        ds = event.dataset
        ds.file_meta = event.file_meta
        path = os.path.join(out_dir, mode + ".dcm")
        mutate(ds, mode).save_as(path, enforce_file_format=True)
        if mode == "relabel-syntax":
            with open(path, "rb") as f:
                data = f.read()
            assert data.count(LE) == 1
            with open(path, "wb") as f:
                f.write(data.replace(LE, BE, 1))
        stored[mode] = ds
        return 0x0000
    except Exception as exc:
        print("store failed:", type(exc).__name__, file=sys.stderr, flush=True)
        return 0xA700


def on_get(event):
    mode = mode_of(event)
    if mode == "relabel-syntax" or mode not in stored:
        yield 0
        return
    yield 1
    yield 0xFF00, mutate(stored[mode], mode)


ae = AE(ae_title="PY_TAMPER")
ae.add_supported_context(STORAGE, ExplicitVRLittleEndian, scu_role=True, scp_role=True)
ae.add_supported_context(GET, ExplicitVRLittleEndian)
server = ae.start_server(("127.0.0.1", 0), block=False, evt_handlers=[(evt.EVT_C_STORE, on_store), (evt.EVT_C_GET, on_get)])
print("PORT", server.server_address[1], flush=True)
try:
    sys.stdin.read()
finally:
    server.shutdown()
