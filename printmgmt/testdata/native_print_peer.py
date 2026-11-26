"""Pinned independent synthetic native Print Management peer.
Protocol/value evidence only; no printer fonts, optics, scale or clinical proof.
"""
import hashlib
import json
import pathlib
import struct
import sys
import pydicom
import pynetdicom
from pydicom.dataset import Dataset
from pydicom.sequence import Sequence
from pydicom.filereader import read_dataset
from pydicom.uid import ExplicitVRLittleEndian, generate_uid
from pynetdicom import AE, evt
from pynetdicom.sop_class import BasicGrayscalePrintManagementMeta as META
from pynetdicom.sop_class import BasicFilmSession as SESSION
from pynetdicom.sop_class import BasicFilmBox as FILM
from pynetdicom.sop_class import BasicGrayscaleImageBox as BOX
LUT = '1.2.840.10008.5.1.1.23'
ANN = '1.2.840.10008.5.1.1.15'
assert pydicom.__version__ == '3.0.2' and pynetdicom.__version__ == '3.0.4'

def ae_base():
    ae = AE(ae_title='NATIVEPEER')
    ae.acse_timeout = ae.dimse_timeout = ae.network_timeout = 5
    return ae

def ref(cls, uid):
    ds = Dataset()
    ds.ReferencedSOPClassUID, ds.ReferencedSOPInstanceUID = cls, uid
    return ds

def lut_table(bits):
    ds, item = Dataset(), Dataset()
    count = 1 << bits
    item.add_new(0x00283002, 'US', [count, 0, 12])
    item.add_new(0x00283006, 'OW', struct.pack('<' + 'H' * count,
                 *[i * 4095 // (count - 1) for i in range(count)]))
    ds.PresentationLUTSequence = Sequence([item])
    return ds

def pixels(bits):
    ds = Dataset()
    ds.SamplesPerPixel, ds.PhotometricInterpretation = 1, 'MONOCHROME2'
    ds.Rows, ds.Columns = 4, 10
    ds.BitsAllocated, ds.BitsStored = (8 if bits == 8 else 16), bits
    ds.HighBit, ds.PixelRepresentation = bits - 1, 0
    ds.PixelData = bytes(range(40)) if bits == 8 else struct.pack('<40H', *[i * 99 for i in range(40)])
    return ds

def run_scp(mode):
    log, sessions, annotation_charsets = [], {}, {}
    ae = ae_base()
    ae.add_supported_context(META, ExplicitVRLittleEndian)
    if mode != 'scp-reject-context':
        ae.add_supported_context(LUT, ExplicitVRLittleEndian)
        ae.add_supported_context(ANN, ExplicitVRLittleEndian)
    def create(event):
        cls = str(event.request.AffectedSOPClassUID)
        uid = str(event.request.AffectedSOPInstanceUID or generate_uid())
        ds, reply = event.attribute_list, Dataset()
        record = {'op': 'N-CREATE', 'class': cls, 'context': str(event.context.abstract_syntax)}
        reply.AffectedSOPInstanceUID = uid
        status = 0
        if cls == LUT:
            if 'PresentationLUTSequence' in ds:
                item = ds.PresentationLUTSequence[0]
                record.update(descriptor=list(item.LUTDescriptor), descriptor_vr=item['LUTDescriptor'].VR,
                              data_vr=item['LUTData'].VR, lut_sha=hashlib.sha256(item.LUTData).hexdigest())
            else:
                record.update(shape=str(ds.PresentationLUTShape), shape_vr=ds['PresentationLUTShape'].VR)
            if mode == 'scp-ignore-lut': status = 0x0107
        elif cls == str(SESSION):
            sessions[uid] = str(ds.get('SpecificCharacterSet', ''))
            record['charset'] = sessions[uid]
            if mode == 'scp-ignore-session': status = 0x0107
        elif cls == str(FILM):
            reply.ReferencedImageBoxSequence = Sequence([ref(BOX, generate_uid())])
            reply.ReferencedBasicAnnotationBoxSequence = Sequence([ref(ANN, generate_uid()), ref(ANN, generate_uid())])
            charset = sessions[str(ds.ReferencedFilmSessionSequence[0].ReferencedSOPInstanceUID)]
            for item in reply.ReferencedBasicAnnotationBoxSequence:
                annotation_charsets[str(item.ReferencedSOPInstanceUID)] = charset
            record['lut_ref'] = str(ds.ReferencedPresentationLUTSequence[0].ReferencedSOPInstanceUID) if 'ReferencedPresentationLUTSequence' in ds else ''
            record['format'] = str(ds.get('AnnotationDisplayFormatID', ''))
            if mode == 'scp-ignore-film': status = 0x0107
        record['status'] = status
        log.append(record)
        return status, reply
    def set_obj(event):
        cls, ds = str(event.request.RequestedSOPClassUID), event.modification_list
        record = {'op': 'N-SET', 'class': cls, 'context': str(event.context.abstract_syntax)}
        status = 0
        if cls == ANN:
            # Native Text String inherits Film Session charset. The DIMSE
            # dataset has no standalone charset; decode with that context.
            charset = annotation_charsets[str(event.request.RequestedSOPInstanceUID)]
            stream = event.request.ModificationList
            stream.seek(0)
            ds = read_dataset(stream, False, True,
                              parent_encoding='utf-8' if charset == 'ISO_IR 192' else 'iso8859')
            record.update(position=int(ds.AnnotationPosition), text=str(ds.TextString), text_vr=ds['TextString'].VR,
                          position_vr=ds['AnnotationPosition'].VR)
            if mode == 'scp-ignore-annotation': status = 0x0107
        else:
            item = ds.BasicGrayscaleImageSequence[0]
            record.update(bits=int(item.BitsStored), sha=hashlib.sha256(item.PixelData).hexdigest())
        record['status'] = status
        log.append(record)
        return status, None
    def action(event):
        log.append({'op': 'N-ACTION', 'class': str(event.request.RequestedSOPClassUID)})
        return 0, None
    def delete(event):
        log.append({'op': 'N-DELETE', 'class': str(event.request.RequestedSOPClassUID)})
        return 0
    server = ae.start_server(('127.0.0.1', 0), block=False,
             evt_handlers=[(evt.EVT_N_CREATE, create), (evt.EVT_N_SET, set_obj),
                           (evt.EVT_N_ACTION, action), (evt.EVT_N_DELETE, delete)])
    print(server.server_address[1], flush=True)
    assert sys.stdin.readline().strip() == 'DONE'
    server.shutdown()
    print(json.dumps(log), flush=True)

def run_scu():
    port = int(sys.stdin.readline())
    ae = ae_base()
    for cls in (META, LUT, ANN): ae.add_requested_context(cls, ExplicitVRLittleEndian)
    assoc = ae.associate('127.0.0.1', port, ae_title='PRINTER')
    assert assoc.is_established
    statuses = []
    def check(s, expected):
        actual = int(s.Status)
        statuses.append(actual)
        assert actual == expected, (hex(actual), hex(expected))
    try:
        u8, u12 = generate_uid(), generate_uid()
        s, _ = assoc.send_n_create(lut_table(8), LUT, u8); check(s, 0)
        s, _ = assoc.send_n_create(lut_table(12), LUT, u12); check(s, 0)
        s, _ = assoc.send_n_create(lut_table(8), LUT, u8); check(s, 0x0111)
        invalid = lut_table(8)
        invalid.PresentationLUTSequence[0].LUTDescriptor = [256, 1, 12]
        s, _ = assoc.send_n_create(invalid, LUT, generate_uid()); check(s, 0x0106)
        session = Dataset()
        session.NumberOfCopies, session.SpecificCharacterSet = '1', 'ISO_IR 192'
        session_uid, film_uid = generate_uid(), generate_uid()
        s, _ = assoc.send_n_create(session, SESSION, session_uid, meta_uid=META); check(s, 0)
        unsupported = Dataset()
        unsupported.ReferencedPresentationLUTSequence = Sequence([ref(LUT, u8)])
        s, _ = assoc.send_n_set(unsupported, SESSION, session_uid, meta_uid=META); check(s, 0x0107)
        film = Dataset()
        film.ImageDisplayFormat, film.AnnotationDisplayFormatID = 'STANDARD\\2,1', 'SYNTHETIC'
        film.ReferencedFilmSessionSequence = Sequence([ref(SESSION, session_uid)])
        film.ReferencedPresentationLUTSequence = Sequence([ref(LUT, u8)])
        film.MinDensity, film.ReflectedAmbientLight = 0, 0
        s, reply = assoc.send_n_create(film, FILM, film_uid, meta_uid=META); check(s, 0)
        boxes = reply.ReferencedImageBoxSequence
        anns = reply.ReferencedBasicAnnotationBoxSequence
        assert len(boxes) == 2 and len(anns) == 2
        a = Dataset()
        # pynetdicom's standalone dataset writer does not inherit a prior
        # Film Session encoding. Supply the LO bytes in its declared charset.
        a.AnnotationPosition, a.TextString = 2, 'SYNTHÉTIQUE'.encode('utf-8')
        s, _ = assoc.send_n_set(a, ANN, anns[1].ReferencedSOPInstanceUID); check(s, 0)
        a.TextString = 'A' * 65
        s, _ = assoc.send_n_set(a, ANN, anns[1].ReferencedSOPInstanceUID); check(s, 0x0106)
        for i, bits in enumerate((8, 12)):
            ds = Dataset()
            ds.ImageBoxPosition = i + 1
            ds.BasicGrayscaleImageSequence = Sequence([pixels(bits)])
            if bits == 12: ds.ReferencedPresentationLUTSequence = Sequence([ref(LUT, u12)])
            s, _ = assoc.send_n_set(ds, BOX, boxes[i].ReferencedSOPInstanceUID, meta_uid=META); check(s, 0)
        ds = Dataset()
        ds.ImageBoxPosition = 1
        ds.BasicGrayscaleImageSequence = Sequence([pixels(12)])
        s, _ = assoc.send_n_set(ds, BOX, boxes[0].ReferencedSOPInstanceUID, meta_uid=META); check(s, 0x0106)
        s = assoc.send_n_delete(LUT, u8); check(s, 0x0110)
        changed = Dataset()
        changed.ReferencedPresentationLUTSequence = Sequence([ref(LUT, u12)])
        s, _ = assoc.send_n_set(changed, FILM, film_uid, meta_uid=META); check(s, 0x0106)
        s, _ = assoc.send_n_action(None, 1, FILM, film_uid, meta_uid=META); check(s, 0)
        s = assoc.send_n_delete(SESSION, session_uid, meta_uid=META); check(s, 0)
        s = assoc.send_n_delete(LUT, u8); check(s, 0)
        s = assoc.send_n_delete(LUT, u12); check(s, 0)
        s = assoc.send_n_delete(LUT, u12); check(s, 0x0112)
        print(json.dumps({'statuses': statuses,
              'pixel_sha': [hashlib.sha256(pixels(b).PixelData).hexdigest() for b in (8, 12)],
              'lut_sha': [hashlib.sha256(lut_table(b).PresentationLUTSequence[0].LUTData).hexdigest() for b in (8, 12)]}), flush=True)
    finally:
        assoc.release()

def write_image(path):
    from pydicom.dataset import FileDataset, FileMetaDataset
    from pydicom.uid import SecondaryCaptureImageStorage
    meta = FileMetaDataset()
    meta.MediaStorageSOPClassUID = SecondaryCaptureImageStorage
    meta.MediaStorageSOPInstanceUID = '1.2.826.0.1.3680043.10.543.1462.1'
    meta.TransferSyntaxUID = ExplicitVRLittleEndian
    meta.ImplementationClassUID = '1.2.826.0.1.3680043.10.543.1462.2'
    ds = FileDataset(path, {}, file_meta=meta, preamble=b'\0' * 128)
    ds.SOPClassUID, ds.SOPInstanceUID = meta.MediaStorageSOPClassUID, meta.MediaStorageSOPInstanceUID
    ds.StudyInstanceUID, ds.SeriesInstanceUID = '1.2.826.0.1.3680043.10.543.1462.3', '1.2.826.0.1.3680043.10.543.1462.4'
    ds.PatientName, ds.PatientID, ds.Modality = 'SYNTHETIC', 'SYNTHETIC1462', 'OT'
    ds.StudyDate, ds.StudyTime, ds.InstanceNumber = '20000101', '000000', '1'
    for elem in pixels(8): ds.add(elem)
    ds.WindowCenter, ds.WindowWidth = 20, 40
    ds.save_as(path, enforce_file_format=True)

def find_stored_print(directory):
    matches = []
    for path in pathlib.Path(directory).iterdir():
        if not path.is_file(): continue
        try:
            ds = pydicom.dcmread(path, stop_before_pixels=True)
        except (pydicom.errors.InvalidDicomError, OSError):
            continue
        if str(ds.get('SOPClassUID', '')) == '1.2.840.10008.5.1.1.27': matches.append(str(path))
    assert len(matches) == 1, matches
    print(matches[0], flush=True)

mode = sys.argv[1]
if mode.startswith('scp'): run_scp(mode)
elif mode == 'scu': run_scu()
elif mode == 'write-image': write_image(sys.argv[2])
elif mode == 'find-stored-print': find_stored_print(sys.argv[2])
else: raise ValueError('unsupported fixture mode')
