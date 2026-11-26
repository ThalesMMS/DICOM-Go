"""Build the synthetic inputs of the dcmqi TID 1500 fixture.

Writes, into the directory given as the first argument, a three-slice 4x4
synthetic CT series (ct1.dcm..ct3.dcm) and a binary Segmentation of it
(seg.dcm, built with highdicom), with fixed UIDs under the
1.2.826.0.1.3680043.8.498 test root and no patient data. dcmqi's
tid1500writer then reads them with dcmqi_tid1500.json to write
dcmqi_tid1500.dcm (see dcmqi_tid1500.md for the command and provenance).
"""

import os
import sys

import highdicom as hd
import numpy as np
from pydicom.dataset import Dataset, FileMetaDataset
from pydicom.sr.codedict import codes
from pydicom.uid import CTImageStorage, ExplicitVRLittleEndian

ROOT = "1.2.826.0.1.3680043.8.498.20260926.2"
STUDY = ROOT + "0"
CT_SERIES = ROOT + "1"
SEG_SERIES = ROOT + "2"
SEG_INSTANCE = ROOT + "3"
FRAME_OF_REFERENCE = ROOT + "4"


def ct_slice(index):
    ds = Dataset()
    ds.file_meta = FileMetaDataset()
    ds.file_meta.MediaStorageSOPClassUID = CTImageStorage
    ds.file_meta.MediaStorageSOPInstanceUID = f"{ROOT}5.{index}"
    ds.file_meta.TransferSyntaxUID = ExplicitVRLittleEndian
    ds.SOPClassUID = CTImageStorage
    ds.SOPInstanceUID = ds.file_meta.MediaStorageSOPInstanceUID
    ds.PatientName = "Synthetic^Phantom"
    ds.PatientID = "SYNTH-TID1500"
    ds.PatientBirthDate = ""
    ds.PatientSex = "O"
    ds.StudyInstanceUID = STUDY
    ds.StudyID = "1"
    ds.StudyDate = "20260926"
    ds.StudyTime = "120000"
    ds.AccessionNumber = ""
    ds.ReferringPhysicianName = ""
    ds.SeriesInstanceUID = CT_SERIES
    ds.SeriesNumber = 1
    ds.Modality = "CT"
    ds.Manufacturer = "dicom-go synthetic"
    ds.FrameOfReferenceUID = FRAME_OF_REFERENCE
    ds.PositionReferenceIndicator = ""
    ds.InstanceNumber = index
    ds.ImageType = ["ORIGINAL", "PRIMARY", "AXIAL"]
    ds.ImagePositionPatient = [0.0, 0.0, float(index - 1) * 2.0]
    ds.ImageOrientationPatient = [1.0, 0.0, 0.0, 0.0, 1.0, 0.0]
    ds.PixelSpacing = [1.0, 1.0]
    ds.SliceThickness = 2.0
    ds.SamplesPerPixel = 1
    ds.PhotometricInterpretation = "MONOCHROME2"
    ds.Rows = 4
    ds.Columns = 4
    ds.BitsAllocated = 16
    ds.BitsStored = 16
    ds.HighBit = 15
    ds.PixelRepresentation = 1
    ds.RescaleIntercept = 0
    ds.RescaleSlope = 1
    ds.KVP = 120
    ds.PixelData = (np.arange(16, dtype=np.int16).reshape(4, 4) * index).tobytes()
    return ds


def main(out):
    os.makedirs(out, exist_ok=True)
    slices = [ct_slice(i) for i in (1, 2, 3)]
    for i, ds in enumerate(slices, start=1):
        ds.save_as(os.path.join(out, f"ct{i}.dcm"), enforce_file_format=True)
    mask = np.zeros((3, 4, 4), dtype=bool)
    mask[:, 1:3, 1:3] = True
    seg = hd.seg.Segmentation(
        source_images=slices,
        pixel_array=mask,
        segmentation_type=hd.seg.SegmentationTypeValues.BINARY,
        segment_descriptions=[
            hd.seg.SegmentDescription(
                segment_number=1,
                segment_label="Lesion",
                segmented_property_category=codes.SCT.MorphologicallyAbnormalStructure,
                segmented_property_type=codes.SCT.Neoplasm,
                algorithm_type=hd.seg.SegmentAlgorithmTypeValues.MANUAL,
            )
        ],
        series_instance_uid=SEG_SERIES,
        series_number=2,
        sop_instance_uid=SEG_INSTANCE,
        instance_number=1,
        manufacturer="dicom-go synthetic",
        manufacturer_model_name="dcmqi fixture",
        software_versions="1",
        device_serial_number="1",
        content_date="20260926",
        content_time="120000",
    )
    seg.save_as(os.path.join(out, "seg.dcm"))


if __name__ == "__main__":
    main(sys.argv[1])
