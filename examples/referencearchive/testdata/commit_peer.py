"""Independent Storage Commitment requestor; pydicom 3.0.2 / pynetdicom 3.0.4.

Stores synthetic instances, asks the archive to commit them plus one it never
received, releases the association, receives the N-EVENT-REPORT on a separate
association, then retrieves the committed instances and compares them.
"""
import hashlib
import os
import sys
import threading

import numpy as np
import pydicom
import pynetdicom
from pydicom.dataset import Dataset, FileMetaDataset
from pydicom.uid import ExplicitVRLittleEndian, ImplicitVRLittleEndian
from pynetdicom import AE, evt, build_role

if os.environ.get("DICOMGO_PEER_DEBUG"):
    from pynetdicom import debug_logger
    debug_logger()
from pynetdicom.sop_class import (
    SecondaryCaptureImageStorage as SC,
    StudyRootQueryRetrieveInformationModelGet as GET,
    StorageCommitmentPushModel as COMMIT,
)

assert pydicom.__version__ == "3.0.2" and pynetdicom.__version__ == "3.0.4"
prefix = "1.2.826.0.1.3680043.10.946"
stored = {}
for i in range(2):
    ds = Dataset()
    ds.file_meta = FileMetaDataset()
    ds.file_meta.TransferSyntaxUID = ExplicitVRLittleEndian
    ds.SOPClassUID = SC
    ds.SOPInstanceUID = f"{prefix}.9.{i + 1}"
    ds.StudyInstanceUID = f"{prefix}.1"
    ds.SeriesInstanceUID = f"{prefix}.1.1"
    ds.PatientID = "SYNTHETIC"
    ds.PatientName = "SYNTHETIC^ONLY"
    ds.StudyDate = "20260925"
    ds.Modality = "OT"
    ds.Rows, ds.Columns = 13, 11
    ds.SamplesPerPixel = 1
    ds.PhotometricInterpretation = "MONOCHROME2"
    ds.BitsAllocated, ds.BitsStored, ds.HighBit, ds.PixelRepresentation = 16, 12, 11, 0
    ds.PixelData = ((np.arange(143, dtype=np.uint32) * 7 + i * 31) % 4096).astype("<u2").tobytes()
    stored[str(ds.SOPInstanceUID)] = ds
missing = f"{prefix}.9.99"

events = []
received = threading.Event()


def on_event(event):
    events.append((event.request.EventTypeID, event.event_information))
    received.set()
    return 0x0000, None


callback = AE(ae_title="COMMITSCU")
callback.add_supported_context(COMMIT, [ExplicitVRLittleEndian, ImplicitVRLittleEndian], scu_role=True, scp_role=True)
callback.acse_timeout = callback.dimse_timeout = callback.network_timeout = 5
server = callback.start_server(("127.0.0.1", 0), block=False, evt_handlers=[(evt.EVT_N_EVENT_REPORT, on_event)])
print("PORT", server.server_address[1], flush=True)
archive_port = int(sys.stdin.readline())

retrieved = {}


def on_store(event):
    ds = event.dataset
    ds.file_meta = event.file_meta
    retrieved[str(ds.SOPInstanceUID)] = ds
    return 0x0000


ae = AE(ae_title="COMMITSCU")
ae.acse_timeout = ae.dimse_timeout = ae.network_timeout = 5
for sop in [SC, GET, COMMIT]:
    ae.add_requested_context(sop, ExplicitVRLittleEndian)


def associate():
    assoc = ae.associate("127.0.0.1", archive_port, ae_title="REFERENCEARCHIVE",
                         ext_neg=[build_role(SC, scu_role=True, scp_role=True)],
                         evt_handlers=[(evt.EVT_C_STORE, on_store)])
    assert assoc.is_established
    return assoc


def request_commitment(uid, uids):
    received.clear()
    events.clear()
    action = Dataset()
    action.TransactionUID = uid
    items = []
    for sop_uid in uids:
        item = Dataset()
        item.ReferencedSOPClassUID = SC
        item.ReferencedSOPInstanceUID = sop_uid
        items.append(item)
    action.ReferencedSOPSequence = items
    assoc = associate()
    status, _ = assoc.send_n_action(action, 1, COMMIT, "1.2.840.10008.1.20.1.1")
    assert status.Status == 0x0000, hex(status.Status)
    # The result must come on a separate association.
    assoc.release()
    assert received.wait(20), "no N-EVENT-REPORT"
    event_type, info = events[0]
    assert info.TransactionUID == uid
    return event_type, info


try:
    assoc = associate()
    for ds in stored.values():
        assert assoc.send_c_store(ds).Status == 0x0000
    assoc.release()

    event_type, info = request_commitment(f"{prefix}.7.1", list(stored) + [missing])
    assert event_type == 2
    assert {str(r.ReferencedSOPInstanceUID) for r in info.ReferencedSOPSequence} == set(stored)
    assert [(str(f.ReferencedSOPInstanceUID), f.FailureReason) for f in info.FailedSOPSequence] == [(missing, 0x0112)]

    event_type, info = request_commitment(f"{prefix}.7.2", list(stored))
    assert event_type == 1
    assert "FailedSOPSequence" not in info
    assert {str(r.ReferencedSOPInstanceUID) for r in info.ReferencedSOPSequence} == set(stored)

    # What was committed is what a retrieve returns.
    assoc = associate()
    query = Dataset()
    query.QueryRetrieveLevel = "STUDY"
    query.StudyInstanceUID = f"{prefix}.1"
    results = list(assoc.send_c_get(query, GET))
    assoc.release()
    assert results[-1][0].Status == 0x0000
    assert set(retrieved) == set(stored)
    for uid, original in stored.items():
        got = retrieved[uid]
        for keyword in ["SOPClassUID", "StudyInstanceUID", "SeriesInstanceUID", "PatientID", "Rows", "Columns", "BitsStored"]:
            assert getattr(got, keyword) == getattr(original, keyword), keyword
        assert hashlib.sha256(got.PixelData).digest() == hashlib.sha256(original.PixelData).digest()
    print("OK", flush=True)
finally:
    server.shutdown()
