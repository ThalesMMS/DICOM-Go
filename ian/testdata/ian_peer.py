"""Independent IAN peer; pydicom 3.0.2 / pynetdicom 3.0.4. Synthetic data only.

scu: pynetdicom sends Instance Availability Notifications to the dicom-go SCP
     whose port arrives on stdin.
scp: pynetdicom serves the Instance Availability Notification SOP Class and
     records every N-CREATE; the summary is printed as JSON when stdin says
     DONE.
"""
import json
import os
import sys

import pydicom
import pynetdicom
from pydicom.dataset import Dataset
from pydicom.sequence import Sequence
from pydicom.uid import ExplicitVRLittleEndian, ImplicitVRLittleEndian
from pynetdicom import AE, evt
from pynetdicom.sop_class import InstanceAvailabilityNotification as IAN

if os.environ.get("DICOMGO_PEER_DEBUG"):
    from pynetdicom import debug_logger
    debug_logger()

assert pydicom.__version__ == "3.0.2" and pynetdicom.__version__ == "3.0.4"
STUDY = "1.2.826.0.1.3680043.10.1168.5"
CT = "1.2.840.10008.5.1.4.1.1.2"
MPPS = "1.2.840.10008.3.1.2.3.3"


def notification(availability="ONLINE"):
    ds = Dataset()
    step = Dataset()
    step.ReferencedSOPClassUID = MPPS
    step.ReferencedSOPInstanceUID = f"{STUDY}.9"
    step.PerformedWorkitemCodeSequence = Sequence()
    ds.ReferencedPerformedProcedureStepSequence = Sequence([step])
    ds.StudyInstanceUID = STUDY
    series = []
    for s in range(2):
        item = Dataset()
        item.SeriesInstanceUID = f"{STUDY}.2.{s + 1}"
        sops = []
        for i in range(2):
            sop = Dataset()
            sop.ReferencedSOPClassUID = CT
            sop.ReferencedSOPInstanceUID = f"{STUDY}.3.{s + 1}.{i + 1}"
            sop.InstanceAvailability = availability
            sop.RetrieveAETitle = "PEERARCH"
            sops.append(sop)
        item.ReferencedSOPSequence = Sequence(sops)
        series.append(item)
    ds.ReferencedSeriesSequence = Sequence(series)
    return ds


def run_scu(port):
    ae = AE(ae_title="PEERIAN")
    ae.acse_timeout = ae.dimse_timeout = ae.network_timeout = 5
    ae.add_requested_context(IAN, [ExplicitVRLittleEndian, ImplicitVRLittleEndian])
    assoc = ae.associate("127.0.0.1", port, ae_title="IANSCP")
    assert assoc.is_established
    try:
        status, _ = assoc.send_n_create(notification(), IAN, f"{STUDY}.8.1")
        assert status.Status == 0x0000, hex(status.Status)
        # The same notification UID again: 0111H Duplicate SOP Instance.
        status, _ = assoc.send_n_create(notification(), IAN, f"{STUDY}.8.1")
        assert status.Status == 0x0111, hex(status.Status)
        # An Instance Availability outside the enumerated values: 0106H.
        status, _ = assoc.send_n_create(notification("SOMEWHERE"), IAN, f"{STUDY}.8.2")
        assert status.Status == 0x0106, hex(status.Status)
        # No Study Instance UID: 0120H Missing Attribute.
        missing = notification()
        del missing.StudyInstanceUID
        status, _ = assoc.send_n_create(missing, IAN, f"{STUDY}.8.3")
        assert status.Status == 0x0120, hex(status.Status)
    finally:
        assoc.release()
    print("OK", flush=True)


def run_scp():
    log = []

    def on_create(event):
        ds = event.attribute_list
        series = ds.ReferencedSeriesSequence
        sops = [sop for s in series for sop in s.ReferencedSOPSequence]
        log.append({
            "class": str(event.request.AffectedSOPClassUID),
            "uid": str(event.request.AffectedSOPInstanceUID),
            "study": str(ds.StudyInstanceUID),
            "series": len(series),
            "instances": len(sops),
            "availability": sorted({str(sop.InstanceAvailability) for sop in sops}),
            "retrieve": sorted({str(sop.RetrieveAETitle) for sop in sops}),
            "pps": [str(p.ReferencedSOPInstanceUID) for p in ds.ReferencedPerformedProcedureStepSequence],
        })
        return 0x0000, None

    ae = AE(ae_title="PEERIANSCP")
    ae.add_supported_context(IAN, [ExplicitVRLittleEndian, ImplicitVRLittleEndian])
    server = ae.start_server(("127.0.0.1", 0), block=False, evt_handlers=[(evt.EVT_N_CREATE, on_create)])
    print("PORT", server.server_address[1], flush=True)
    assert sys.stdin.readline().strip() == "DONE"
    server.shutdown()
    print(json.dumps(log), flush=True)


if sys.argv[1] == "scu":
    run_scu(int(sys.stdin.readline()))
else:
    run_scp()
