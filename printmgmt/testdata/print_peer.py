"""Independent Print Management peer; pydicom 3.0.2 / pynetdicom 3.0.4.

scu: pynetdicom drives the dicom-go SCP (port on stdin) through a grayscale
     session and prints the SHA-256 of the pixels it sent.
scp: pynetdicom serves the Basic Grayscale Print Management Meta SOP Class,
     with handlers that check the command SOP Classes against the Meta SOP
     Class context, create the image boxes and record every command; the log
     is printed as JSON when stdin says DONE.
"""
import hashlib
import json
import os
import sys

import numpy as np
import pydicom
import pynetdicom
from pydicom.dataset import Dataset
from pydicom.sequence import Sequence
from pydicom.uid import ExplicitVRLittleEndian, ImplicitVRLittleEndian, generate_uid
from pynetdicom import AE, evt

if os.environ.get("DICOMGO_PEER_DEBUG"):
    from pynetdicom import debug_logger
    debug_logger()
from pynetdicom.sop_class import (
    BasicGrayscalePrintManagementMeta as META,
    BasicFilmSession as SESSION,
    BasicFilmBox as FILMBOX,
    BasicGrayscaleImageBox as IMAGEBOX,
    Printer as PRINTER,
)

assert pydicom.__version__ == "3.0.2" and pynetdicom.__version__ == "3.0.4"
PRINTER_INSTANCE = "1.2.840.10008.5.1.1.17"


def image_item(rows, columns, seed):
    item = Dataset()
    item.SamplesPerPixel = 1
    item.PhotometricInterpretation = "MONOCHROME2"
    item.Rows, item.Columns = rows, columns
    item.BitsAllocated, item.BitsStored, item.HighBit, item.PixelRepresentation = 8, 8, 7, 0
    item.PixelData = ((np.arange(rows * columns) * 3 + seed) % 256).astype(np.uint8).tobytes()
    return item


def run_scu(port):
    ae = AE(ae_title="PEERMOD")
    ae.acse_timeout = ae.dimse_timeout = ae.network_timeout = 5
    ae.add_requested_context(META, [ExplicitVRLittleEndian, ImplicitVRLittleEndian])
    assoc = ae.associate("127.0.0.1", port, ae_title="PRINTER")
    assert assoc.is_established
    sent = []
    try:
        session = Dataset()
        session.NumberOfCopies = "1"
        session.MediumType = "PAPER"
        session.FilmSessionLabel = "SYNTHETIC PEER"
        session_uid = generate_uid()
        status, reply = assoc.send_n_create(session, SESSION, session_uid, meta_uid=META)
        assert status.Status == 0x0000, hex(status.Status)
        box = Dataset()
        box.ImageDisplayFormat = "STANDARD\\2,1"
        ref = Dataset()
        ref.ReferencedSOPClassUID = SESSION
        ref.ReferencedSOPInstanceUID = session_uid
        box.ReferencedFilmSessionSequence = Sequence([ref])
        box.FilmOrientation = "PORTRAIT"
        box.FilmSizeID = "8INX10IN"
        film_uid = generate_uid()
        status, reply = assoc.send_n_create(box, FILMBOX, film_uid, meta_uid=META)
        assert status.Status == 0x0000, hex(status.Status)
        boxes = reply.ReferencedImageBoxSequence
        assert len(boxes) == 2 and all(b.ReferencedSOPClassUID == IMAGEBOX for b in boxes)
        for position, image_box in enumerate(boxes, start=1):
            update = Dataset()
            update.ImageBoxPosition = position
            item = image_item(4, 5 + position, position * 40)
            update.BasicGrayscaleImageSequence = Sequence([item])
            status, _ = assoc.send_n_set(update, IMAGEBOX, image_box.ReferencedSOPInstanceUID, meta_uid=META)
            assert status.Status == 0x0000, hex(status.Status)
            sent.append(hashlib.sha256(item.PixelData).hexdigest())
        # A wrong position is refused.
        wrong = Dataset()
        wrong.ImageBoxPosition = 2
        wrong.BasicGrayscaleImageSequence = Sequence([image_item(2, 2, 0)])
        status, _ = assoc.send_n_set(wrong, IMAGEBOX, boxes[0].ReferencedSOPInstanceUID, meta_uid=META)
        assert status.Status == 0x0106, hex(status.Status)
        status, _ = assoc.send_n_action(None, 1, FILMBOX, film_uid, meta_uid=META)
        assert status.Status == 0x0000, hex(status.Status)
        status, printer = assoc.send_n_get([0x21100010], PRINTER, PRINTER_INSTANCE, meta_uid=META)
        assert status.Status == 0x0000 and printer.PrinterStatus == "NORMAL", status
        status = assoc.send_n_delete(SESSION, session_uid, meta_uid=META)
        assert status.Status == 0x0000, hex(status.Status)
    finally:
        assoc.release()
    print("OK " + ",".join(sent), flush=True)


def run_scp():
    log, state = [], {"boxes": {}}

    def check_context(event, cls, allowed):
        return str(event.context.abstract_syntax) == str(META) and cls in allowed

    def on_create(event):
        cls = str(event.request.AffectedSOPClassUID)
        ds = event.attribute_list
        entry = {"op": "N-CREATE", "class": cls, "context": str(event.context.abstract_syntax)}
        log.append(entry)
        # The Go side asserts the recorded context of every command.
        entry["allowed"] = check_context(event, cls, [str(SESSION), str(FILMBOX)])
        uid = str(event.request.AffectedSOPInstanceUID or generate_uid())
        if cls == str(SESSION):
            entry["copies"] = str(ds.get("NumberOfCopies", ""))
            entry["medium"] = str(ds.get("MediumType", ""))
            state["session"] = uid
            reply = Dataset()
            # pynetdicom takes an SCP-assigned UID from AffectedSOPInstanceUID.
            reply.AffectedSOPInstanceUID = uid
            return 0x0000, reply
        entry["format"] = str(ds.ImageDisplayFormat)
        entry["session_ref"] = str(ds.ReferencedFilmSessionSequence[0].ReferencedSOPInstanceUID) == state.get("session")
        columns, rows = [int(v) for v in str(ds.ImageDisplayFormat).split("\\")[1].split(",")]
        items = []
        for _ in range(columns * rows):
            item = Dataset()
            item.ReferencedSOPClassUID = IMAGEBOX
            item.ReferencedSOPInstanceUID = generate_uid()
            state["boxes"][str(item.ReferencedSOPInstanceUID)] = len(state["boxes"]) + 1
            items.append(item)
        reply = Dataset()
        reply.AffectedSOPInstanceUID = uid
        reply.ImageDisplayFormat = ds.ImageDisplayFormat
        reply.ReferencedImageBoxSequence = Sequence(items)
        return 0x0000, reply

    def on_set(event):
        cls = str(event.request.RequestedSOPClassUID)
        mods = event.modification_list
        entry = {"op": "N-SET", "class": cls, "context": str(event.context.abstract_syntax)}
        log.append(entry)
        if cls == str(IMAGEBOX):
            uid = str(event.request.RequestedSOPInstanceUID)
            entry["position_ok"] = state["boxes"].get(uid) == int(mods.ImageBoxPosition)
            item = mods.BasicGrayscaleImageSequence[0]
            entry["rows"], entry["columns"] = int(item.Rows), int(item.Columns)
            entry["bits"] = [int(item.BitsAllocated), int(item.BitsStored), int(item.HighBit), int(item.PixelRepresentation)]
            entry["photometric"] = str(item.PhotometricInterpretation)
            entry["sha256"] = hashlib.sha256(item.PixelData[: item.Rows * item.Columns * (item.BitsAllocated // 8)]).hexdigest()
        return 0x0000, mods

    def on_action(event):
        log.append({"op": "N-ACTION", "class": str(event.request.RequestedSOPClassUID), "type": int(event.action_type), "context": str(event.context.abstract_syntax)})
        return 0x0000, None

    def on_get(event):
        log.append({"op": "N-GET", "class": str(event.request.RequestedSOPClassUID), "instance": str(event.request.RequestedSOPInstanceUID)})
        ds = Dataset()
        ds.PrinterStatus = "NORMAL"
        ds.PrinterStatusInfo = "NORMAL"
        return 0x0000, ds

    def on_delete(event):
        log.append({"op": "N-DELETE", "class": str(event.request.RequestedSOPClassUID)})
        return 0x0000

    ae = AE(ae_title="PEERPRINT")
    ae.add_supported_context(META, [ExplicitVRLittleEndian, ImplicitVRLittleEndian])
    server = ae.start_server(("127.0.0.1", 0), block=False, evt_handlers=[
        (evt.EVT_N_CREATE, on_create), (evt.EVT_N_SET, on_set), (evt.EVT_N_ACTION, on_action),
        (evt.EVT_N_GET, on_get), (evt.EVT_N_DELETE, on_delete)])
    print("PORT", server.server_address[1], flush=True)
    assert sys.stdin.readline().strip() == "DONE"
    server.shutdown()
    print(json.dumps(log), flush=True)


if sys.argv[1] == "scu":
    run_scu(int(sys.stdin.readline()))
else:
    run_scp()
