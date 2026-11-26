"""Independent Query/Retrieve requestor for qrscp; pydicom 3.0.2 / pynetdicom 3.0.4.

Usage: qr_peer.py QRSCP_PORT MOVE_PORT STUDY_A STUDY_B

pynetdicom queries the dicom-go qrscp with Study Root and Patient Root C-FIND,
retrieves STUDY_A with C-GET and moves STUDY_B with C-MOVE to its own Storage
SCP (AE PYDEST on MOVE_PORT). It prints one JSON summary line. Synthetic data
only.
"""
import json
import os
import sys

import pydicom
import pynetdicom
from pydicom.dataset import Dataset
from pynetdicom import AE, StoragePresentationContexts, build_role, evt
from pynetdicom.sop_class import (
    BasicTextSRStorage,
    PatientRootQueryRetrieveInformationModelFind,
    SecondaryCaptureImageStorage,
    StudyRootQueryRetrieveInformationModelFind,
    StudyRootQueryRetrieveInformationModelGet,
    StudyRootQueryRetrieveInformationModelMove,
)

if os.environ.get("DICOMGO_PEER_DEBUG"):
    from pynetdicom import debug_logger

    debug_logger()

assert pydicom.__version__ == "3.0.2" and pynetdicom.__version__ == "3.0.4"

port, move_port, study_a, study_b = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3], sys.argv[4]
summary = {}


def find(model, identifier):
    ae = AE(ae_title="PYSCU")
    ae.add_requested_context(model)
    assoc = ae.associate("127.0.0.1", port, ae_title="QRSCP")
    assert assoc.is_established, "C-FIND association"
    matches, final = [], None
    for status, ds in assoc.send_c_find(identifier, model):
        if status and status.Status in (0xFF00, 0xFF01):
            matches.append(ds)
        elif status:
            final = status.Status
    assoc.release()
    return matches, final


query = Dataset()
query.QueryRetrieveLevel = "STUDY"
query.StudyInstanceUID = ""
query.PatientID = "SYNTH-*"
query.ModalitiesInStudy = ""
query.NumberOfStudyRelatedInstances = ""
matches, final = find(StudyRootQueryRetrieveInformationModelFind, query)
summary["study_matches"] = sorted(str(ds.StudyInstanceUID) for ds in matches)
summary["study_instances"] = sorted(str(ds.NumberOfStudyRelatedInstances) for ds in matches)
summary["study_final"] = final

query = Dataset()
query.QueryRetrieveLevel = "PATIENT"
query.PatientID = ""
query.PatientName = ""
matches, final = find(PatientRootQueryRetrieveInformationModelFind, query)
summary["patients"] = sorted(str(ds.PatientID) for ds in matches)

received_get = []


def on_get_store(event):
    received_get.append(str(event.request.AffectedSOPInstanceUID))
    return 0x0000


ae = AE(ae_title="PYSCU")
ae.add_requested_context(StudyRootQueryRetrieveInformationModelGet)
roles = []
for storage in (SecondaryCaptureImageStorage, BasicTextSRStorage):
    ae.add_requested_context(storage)
    roles.append(build_role(storage, scp_role=True))
assoc = ae.associate("127.0.0.1", port, ae_title="QRSCP", ext_neg=roles, evt_handlers=[(evt.EVT_C_STORE, on_get_store)])
assert assoc.is_established, "C-GET association"
query = Dataset()
query.QueryRetrieveLevel = "STUDY"
query.StudyInstanceUID = study_a
get_final = None
for status, _ in assoc.send_c_get(query, StudyRootQueryRetrieveInformationModelGet):
    if status:
        get_final = status.Status
assoc.release()
summary["get_final"] = get_final
summary["get_received"] = sorted(received_get)

received_move = []


def on_move_store(event):
    received_move.append(str(event.request.AffectedSOPInstanceUID))
    return 0x0000


destination = AE(ae_title="PYDEST")
destination.supported_contexts = StoragePresentationContexts
server = destination.start_server(("127.0.0.1", move_port), block=False, evt_handlers=[(evt.EVT_C_STORE, on_move_store)])
try:
    ae = AE(ae_title="PYSCU")
    ae.add_requested_context(StudyRootQueryRetrieveInformationModelMove)
    assoc = ae.associate("127.0.0.1", port, ae_title="QRSCP")
    assert assoc.is_established, "C-MOVE association"
    query = Dataset()
    query.QueryRetrieveLevel = "STUDY"
    query.StudyInstanceUID = study_b
    move_final = None
    for status, _ in assoc.send_c_move(query, "PYDEST", StudyRootQueryRetrieveInformationModelMove):
        if status:
            move_final = status.Status
    assoc.release()
finally:
    server.shutdown()
summary["move_final"] = move_final
summary["move_received"] = sorted(received_move)
print(json.dumps(summary), flush=True)
