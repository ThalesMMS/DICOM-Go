// Writes the synthetic TID 1500 Measurement Report that sr tests use as the
// DCMTK oracle (testdata/dcmtk_tid1500.dcm): DCMTK's own template classes
// (dcmsr/cmr) build the content tree, and TID1500_MeasurementReport::isValid
// must hold. The measurement values mirror dcmtkOracleReport in
// measurement_report_standard_test.go.
//
// Build and run (MSYS2 UCRT64, DCMTK 3.7.0):
//   g++ -O1 dcmtk_tid1500_oracle.cxx -o dcmtk_tid1500_oracle \
//       -lcmr -ldcmsr -ldcmdata -loflog -lofstd
//   ./dcmtk_tid1500_oracle dcmtk_tid1500.dcm
#include "dcmtk/config/osconfig.h"
#include "dcmtk/dcmdata/dctk.h"
#include "dcmtk/dcmsr/dsrdoc.h"
#include "dcmtk/dcmsr/cmr/tid1500.h"
#include "dcmtk/dcmsr/cmr/cid5000.h"
#include "dcmtk/dcmsr/cmr/cid7021.h"
#include "dcmtk/dcmsr/cmr/cid7181.h"
#include "dcmtk/dcmsr/cmr/cid7469.h"
#include "dcmtk/dcmsr/cmr/cid7551.h"
#include <cstdio>

#define REQUIRE(cond)                                                        \
  do {                                                                       \
    OFCondition status = (cond);                                             \
    if (status.bad()) {                                                      \
      std::fprintf(stderr, "%s: %s\n", #cond, status.text());                \
      return 1;                                                              \
    }                                                                        \
  } while (0)

static const char *CTImage = "1.2.826.0.1.3680043.8.498.20260926.1";
static const char *Segmentation = "1.2.826.0.1.3680043.8.498.20260926.2";

int main(int argc, char *argv[]) {
  if (argc < 2) return 2;
  TID1500_MeasurementReport report(CMR_CID7021::ImagingMeasurementReport);
  REQUIRE(report.setLanguage(CID5000_Language(CID5000_Language::English_US)));
  REQUIRE(report.getObservationContext().addDeviceObserver("1.2.826.0.1.3680043.8.498.20260926.9", "dicom-go test"));
  REQUIRE(report.addProcedureReported(DSRCodedEntryValue("363679005", "SCT", "Imaging procedure")));

  // Image Library: the CT image the measurements reference.
  DcmItem image;
  REQUIRE(image.putAndInsertString(DCM_SOPClassUID, UID_CTImageStorage));
  REQUIRE(image.putAndInsertString(DCM_SOPInstanceUID, CTImage));
  REQUIRE(report.getImageLibrary().createNewImageLibrary());
  REQUIRE(report.getImageLibrary().addImageGroup());
  REQUIRE(report.getImageLibrary().addImageEntry(image, TID1600_ImageLibrary::withoutDescriptors));

  // A volumetric ROI group (TID 1411) on segment 1 of a segmentation.
  TID1500_MeasurementReport::TID1411_Measurements &volume = report.getVolumetricROIMeasurements();
  REQUIRE(volume.setTrackingIdentifier("Lesion 1"));
  REQUIRE(volume.setTrackingUniqueIdentifier("1.2.826.0.1.3680043.8.498.20260926.3"));
  REQUIRE(volume.setFinding(DSRCodedEntryValue("52988006", "SCT", "Lesion")));
  REQUIRE(volume.addFindingSite(DSRCodedEntryValue("39607008", "SCT", "Lung")));
  DSRImageReferenceValue segment(UID_SegmentationStorage, Segmentation);
  segment.getSegmentList().addItem(1);
  REQUIRE(volume.setReferencedSegment(segment));
  REQUIRE(volume.setSourceSeriesForSegmentation("1.2.826.0.1.3680043.8.498.20260926.6"));
  REQUIRE(volume.addMeasurement(CMR_CID7469::Volume,
                                CMR_TID1411_in_TID1500::MeasurementValue("1234.5", CMR_CID7181::CubicMillimeter)));

  // A measurement group (TID 1501) with a diameter measured on the CT image.
  TID1500_MeasurementReport::TID1501_Measurements &individual = report.getIndividualMeasurements();
  REQUIRE(individual.setTrackingIdentifier("Lesion 2"));
  REQUIRE(individual.setTrackingUniqueIdentifier("1.2.826.0.1.3680043.8.498.20260926.4"));
  REQUIRE(individual.setFinding(DSRCodedEntryValue("52988006", "SCT", "Lesion")));
  REQUIRE(individual.addMeasurement(CMR_CID7469::Diameter,
                                    CMR_TID1501_in_TID1500::MeasurementValue("12.5", CMR_CID7181::Millimeter)));
  REQUIRE(individual.getMeasurement().addImage(CID7551_GenericPurposeOfReferenceToImagesAndCoordinatesInMeasurement(
                                                   CID7551_GenericPurposeOfReferenceToImagesAndCoordinatesInMeasurement::SourceOfMeasurement),
                                               DSRImageReferenceValue(UID_CTImageStorage, CTImage)));
  if (!report.isValid()) {
    std::fprintf(stderr, "report is not valid: context %d library %d volumetric %d individual %d\n",
                 report.getObservationContext().isValid(), report.getImageLibrary().isValid(),
                 volume.isValid(), individual.isValid());
    return 1;
  }

  DSRDocument document;
  REQUIRE(document.setTreeFromRootTemplate(report, OFTrue /*expandTree*/));
  REQUIRE(document.setPatientName("Synthetic^TID1500"));
  REQUIRE(document.setPatientID("TID1500"));
  REQUIRE(document.createNewSeriesInStudy("1.2.826.0.1.3680043.8.498.20260926.5"));
  REQUIRE(document.setSeriesNumber("1"));
  REQUIRE(document.setInstanceNumber("1"));
  REQUIRE(document.setContentDate("20260926"));
  REQUIRE(document.setContentTime("120000"));
  DcmFileFormat file;
  REQUIRE(document.write(*file.getDataset()));
  REQUIRE(file.saveFile(argv[1], EXS_LittleEndianExplicit));
  return 0;
}
