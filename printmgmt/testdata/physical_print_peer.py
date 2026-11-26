"""Synthetic independent physical-presentation peer; protocol/plan evidence only.
Pinned pydicom 3.0.2, pynetdicom 3.0.4. No physical printer or scale measurement.
"""
import hashlib
import json
import sys
import pydicom
import pynetdicom
from pydicom.dataset import Dataset
from pydicom.sequence import Sequence
from pydicom.uid import ExplicitVRLittleEndian, generate_uid
from pynetdicom import AE, evt
from pynetdicom.sop_class import BasicGrayscalePrintManagementMeta as META
from pynetdicom.sop_class import BasicFilmSession as SESSION
from pynetdicom.sop_class import BasicFilmBox as FILM
from pynetdicom.sop_class import BasicGrayscaleImageBox as BOX
assert pydicom.__version__ == '3.0.2' and pynetdicom.__version__ == '3.0.4'

def pixels():
    item = Dataset()
    item.SamplesPerPixel = 1
    item.PhotometricInterpretation = 'MONOCHROME2'
    item.Rows, item.Columns = 4, 10
    item.BitsAllocated = item.BitsStored = 8
    item.HighBit = 7
    item.PixelRepresentation = 0
    item.PixelData = bytes(range(40))
    return item

def ae_base():
    ae = AE(ae_title='PEERPRINT')
    ae.acse_timeout = ae.dimse_timeout = ae.network_timeout = 5
    return ae

def run_scp():
    log = []
    ae = ae_base()
    ae.add_supported_context(META, ExplicitVRLittleEndian)
    def create(event):
        cls = str(event.request.AffectedSOPClassUID)
        uid = str(event.request.AffectedSOPInstanceUID or generate_uid())
        log.append({'op': 'N-CREATE', 'class': cls})
        reply = Dataset()
        reply.AffectedSOPInstanceUID = uid
        if cls == str(FILM):
            ref = Dataset()
            ref.ReferencedSOPClassUID = BOX
            ref.ReferencedSOPInstanceUID = generate_uid()
            reply.ReferencedImageBoxSequence = Sequence([ref])
        return 0, reply
    def set_image(event):
        ds = event.modification_list
        width, behavior = float(ds.RequestedImageSize), str(ds.RequestedDecimateCropBehavior)
        item = ds.BasicGrayscaleImageSequence[0]
        status = 0
        if width > 100 or width * int(item.Rows) / int(item.Columns) > 80:
            status = {'FAIL': 0xC603, 'CROP': 0xB609, 'DECIMATE': 0xB60A}[behavior]
        log.append({'op': 'N-SET', 'width': width, 'behavior': behavior,
                    'width_vr': ds['RequestedImageSize'].VR,
                    'behavior_vr': ds['RequestedDecimateCropBehavior'].VR,
                    'status': status, 'sha256': hashlib.sha256(item.PixelData).hexdigest()})
        return status, None
    def action(event):
        log.append({'op': 'N-ACTION'})
        return 0, None
    def delete(event):
        log.append({'op': 'N-DELETE'})
        return 0
    server = ae.start_server(('127.0.0.1', 0), block=False,
             evt_handlers=[(evt.EVT_N_CREATE, create), (evt.EVT_N_SET, set_image),
                           (evt.EVT_N_ACTION, action), (evt.EVT_N_DELETE, delete)])
    print(server.server_address[1], flush=True)
    assert sys.stdin.readline().strip() == 'DONE'
    server.shutdown()
    print(json.dumps(log), flush=True)

def run_scu():
    port = int(sys.stdin.readline())
    ae = ae_base()
    ae.add_requested_context(META, ExplicitVRLittleEndian)
    assoc = ae.associate('127.0.0.1', port, ae_title='PRINTER')
    assert assoc.is_established
    try:
        session = Dataset()
        session.NumberOfCopies = '1'
        session_uid = generate_uid()
        status, _ = assoc.send_n_create(session, SESSION, session_uid, meta_uid=META)
        assert status.Status == 0
        film = Dataset()
        film.ImageDisplayFormat = 'STANDARD\\1,1'
        film.MagnificationType = 'BILINEAR'
        ref = Dataset()
        ref.ReferencedSOPClassUID, ref.ReferencedSOPInstanceUID = SESSION, session_uid
        film.ReferencedFilmSessionSequence = Sequence([ref])
        film_uid = generate_uid()
        status, reply = assoc.send_n_create(film, FILM, film_uid, meta_uid=META)
        assert status.Status == 0
        box_uid = reply.ReferencedImageBoxSequence[0].ReferencedSOPInstanceUID
        statuses = []
        for width, behavior, expected in [(100, 'FAIL', 0), (200, 'FAIL', 0xC603),
                                          (200, 'CROP', 0xB609), (200, 'DECIMATE', 0xB60A)]:
            ds = Dataset()
            ds.ImageBoxPosition = 1
            ds.RequestedImageSize = str(width)
            ds.RequestedDecimateCropBehavior = behavior
            ds.BasicGrayscaleImageSequence = Sequence([pixels()])
            status, _ = assoc.send_n_set(ds, BOX, box_uid, meta_uid=META)
            statuses.append(int(status.Status))
            assert status.Status == expected, hex(status.Status)
            # Refused N-SET must leave the last accepted fit plan usable.
            status, _ = assoc.send_n_action(None, 1, FILM, film_uid, meta_uid=META)
            assert status.Status == 0
        status = assoc.send_n_delete(SESSION, session_uid, meta_uid=META)
        assert status.Status == 0
        print(json.dumps({'statuses': statuses, 'sha256': hashlib.sha256(pixels().PixelData).hexdigest()}), flush=True)
    finally:
        assoc.release()

if sys.argv[1] == 'scp': run_scp()
elif sys.argv[1] == 'scu': run_scu()
else: raise ValueError('unsupported fixture mode')
