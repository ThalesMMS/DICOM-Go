"""Independent MPPS peer; pydicom 3.0.2 / pynetdicom 3.0.4. Synthetic data only.

scu: pynetdicom drives the dicom-go SCP whose port arrives on stdin.
scp: pynetdicom serves MPPS with handlers that enforce the PS3.4 F.7
     lifecycle and records every command; the summary is printed as JSON when
     stdin says DONE.
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

if os.environ.get("DICOMGO_PEER_DEBUG"):
    from pynetdicom import debug_logger
    debug_logger()
from pynetdicom.sop_class import ModalityPerformedProcedureStep as MPPS

assert pydicom.__version__ == "3.0.2" and pynetdicom.__version__ == "3.0.4"
STUDY = "1.2.826.0.1.3680043.10.947.5"
CT = "1.2.840.10008.5.1.4.1.1.2"


def create_attributes(status="IN PROGRESS"):
    ds = Dataset()
    step = Dataset()
    step.StudyInstanceUID = STUDY
    step.ReferencedStudySequence = Sequence()
    step.AccessionNumber = "ACC947"
    step.RequestedProcedureID = "RP947"
    step.RequestedProcedureDescription = "SYNTHETIC"
    step.ScheduledProcedureStepID = "SPS947"
    step.ScheduledProcedureStepDescription = "SYNTHETIC"
    step.ScheduledProtocolCodeSequence = Sequence()
    ds.ScheduledStepAttributesSequence = Sequence([step])
    ds.PatientName = "SYNTHETIC^PEER"
    ds.PatientID = "PEER947"
    ds.PatientBirthDate = ""
    ds.PatientSex = ""
    ds.ReferencedPatientSequence = Sequence()
    ds.PerformedProcedureStepID = "PPS-PEER"
    ds.PerformedStationAETitle = "PEERMOD"
    ds.PerformedStationName = ""
    ds.PerformedLocation = ""
    ds.PerformedProcedureStepStartDate = "20260925"
    ds.PerformedProcedureStepStartTime = "120000"
    ds.PerformedProcedureStepStatus = status
    ds.PerformedProcedureStepDescription = ""
    ds.PerformedProcedureTypeDescription = ""
    ds.ProcedureCodeSequence = Sequence()
    ds.PerformedProcedureStepEndDate = ""
    ds.PerformedProcedureStepEndTime = ""
    ds.Modality = "CT"
    ds.StudyID = ""
    ds.PerformedProtocolCodeSequence = Sequence()
    ds.PerformedSeriesSequence = Sequence()
    return ds


def performed_series(count):
    items = []
    for i in range(count):
        item = Dataset()
        item.PerformingPhysicianName = ""
        item.ProtocolName = "SYNTHETIC"
        item.OperatorsName = ""
        item.SeriesInstanceUID = f"{STUDY}.2.{i + 1}"
        item.SeriesDescription = ""
        item.RetrieveAETitle = ""
        image = Dataset()
        image.ReferencedSOPClassUID = CT
        image.ReferencedSOPInstanceUID = f"{STUDY}.3.{i + 1}"
        item.ReferencedImageSequence = Sequence([image])
        item.ReferencedNonImageCompositeSOPInstanceSequence = Sequence()
        items.append(item)
    return Sequence(items)


def run_scu(port):
    ae = AE(ae_title="PEERMOD")
    ae.acse_timeout = ae.dimse_timeout = ae.network_timeout = 5
    ae.add_requested_context(MPPS, [ExplicitVRLittleEndian, ImplicitVRLittleEndian])
    assoc = ae.associate("127.0.0.1", port, ae_title="RIS")
    assert assoc.is_established
    uid = f"{STUDY}.9.1"
    try:
        status, _ = assoc.send_n_create(create_attributes(), MPPS, uid)
        assert status.Status == 0x0000, hex(status.Status)
        progress = Dataset()
        progress.PerformedSeriesSequence = performed_series(1)
        status, _ = assoc.send_n_set(progress, MPPS, uid)
        assert status.Status == 0x0000, hex(status.Status)
        final = Dataset()
        final.PerformedProcedureStepStatus = "COMPLETED"
        final.PerformedProcedureStepEndDate = "20260925"
        final.PerformedProcedureStepEndTime = "121500"
        final.PerformedSeriesSequence = performed_series(2)
        status, _ = assoc.send_n_set(final, MPPS, uid)
        assert status.Status == 0x0000, hex(status.Status)
        # No update after a terminal state: 0110H with Error ID A710.
        status, _ = assoc.send_n_set(progress, MPPS, uid)
        assert status.Status == 0x0110 and status.ErrorID == 0xA710, status
        # N-CREATE accepts only IN PROGRESS: 0106H.
        status, _ = assoc.send_n_create(create_attributes("COMPLETED"), MPPS, f"{STUDY}.9.2")
        assert status.Status == 0x0106, hex(status.Status)
        # Attributes N-SET may not change (Table F.7.2-1 "Not allowed") are
        # refused as invalid, before the step state is considered.
        other = Dataset()
        other.PatientName = "OTHER^NAME"
        status, _ = assoc.send_n_set(other, MPPS, f"{STUDY}.9.1")
        assert status.Status == 0x0106, hex(status.Status)
        status, _ = assoc.send_n_set(progress, MPPS, f"{STUDY}.9.404")
        assert status.Status == 0x0112, hex(status.Status)
    finally:
        assoc.release()
    print("OK", flush=True)


def run_scp():
    steps, log = {}, []

    def on_create(event):
        uid = str(event.request.AffectedSOPInstanceUID)
        ds = event.attribute_list
        record = {"op": "N-CREATE", "class": str(event.request.AffectedSOPClassUID), "uid": uid, "status": str(ds.PerformedProcedureStepStatus),
                  "patient": str(ds.PatientName), "modality": str(ds.Modality), "steps": len(ds.ScheduledStepAttributesSequence)}
        log.append(record)
        if ds.PerformedProcedureStepStatus != "IN PROGRESS":
            return 0x0106, None
        if uid in steps:
            return 0x0111, None
        steps[uid] = ds
        return 0x0000, ds

    def on_set(event):
        uid = str(event.request.RequestedSOPInstanceUID)
        mods = event.modification_list
        record = {"op": "N-SET", "class": str(event.request.RequestedSOPClassUID), "uid": uid, "tags": sorted(str(e.tag) for e in mods)}
        if "PerformedProcedureStepStatus" in mods:
            record["status"] = str(mods.PerformedProcedureStepStatus)
        if "PerformedSeriesSequence" in mods:
            record["series"] = len(mods.PerformedSeriesSequence)
            record["images"] = sum(len(s.ReferencedImageSequence) for s in mods.PerformedSeriesSequence)
        log.append(record)
        if uid not in steps:
            return 0x0112, None
        current = steps[uid]
        if current.PerformedProcedureStepStatus != "IN PROGRESS":
            status = Dataset()
            status.Status = 0x0110
            status.ErrorComment = "Performed Procedure Step Object may no longer be updated"
            status.ErrorID = 0xA710
            return status, None
        for element in mods:
            current[element.tag] = element
        return 0x0000, current

    ae = AE(ae_title="PEERRIS")
    ae.add_supported_context(MPPS, [ExplicitVRLittleEndian, ImplicitVRLittleEndian])
    server = ae.start_server(("127.0.0.1", 0), block=False, evt_handlers=[(evt.EVT_N_CREATE, on_create), (evt.EVT_N_SET, on_set)])
    print("PORT", server.server_address[1], flush=True)
    assert sys.stdin.readline().strip() == "DONE"
    server.shutdown()
    print(json.dumps(log), flush=True)


if sys.argv[1] == "scu":
    run_scu(int(sys.stdin.readline()))
else:
    run_scp()
