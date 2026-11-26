// dcmtk_segoracle: synthetic Segmentation objects written and read with DCMTK
// dcmseg, used as an oracle by dicom-go's seg tests (opt-in, see
// TestSegmentationsMatchDCMTK). This program is dicom-go test tooling that
// calls DCMTK's public API; it contains no DCMTK code.
//
// Build against an installed DCMTK (3.7 or later), for example:
//   g++ -O1 -o dcmtk_segoracle dcmtk_segoracle.cc \
//       -ldcmseg -ldcmfg -ldcmiod -ldcmdata -loflog -lofstd
// DCMTK 3.7.0 does not export OverlapUtil from its shared library; add
// <dcmtk source>/dcmseg/libsrc/overlaputil.cc to that command line (later
// DCMTK versions export it).
//
// Usage:
//   dcmtk_segoracle write <kind> <file>   kind: binary, fractional-probability,
//                                         fractional-occupancy, labelmap8, labelmap16
//   dcmtk_segoracle dump <file>           prints the header, segments, every
//                                         frame's pixel values and, for BINARY
//                                         and FRACTIONAL, DCMTK's overlap
//                                         matrix and disjoint segment groups
//
// dump loads with DcmSegmentation::loadFile, whose warnings (such as a Type 1
// or 2 attribute absent from a module) go to standard error; the Go test
// TestWrittenSegmentationsLoadInDCMTKWithoutWarnings requires none.
//
// The patterns match the Go test (seg/dcmtk_test.go): 5 rows by 7 columns.

#include "dcmtk/config/osconfig.h"
#include "dcmtk/dcmdata/dcdeftag.h"
#include "dcmtk/dcmfg/fgfracon.h"
#include "dcmtk/dcmfg/fgpixmsr.h"
#include "dcmtk/dcmfg/fgplanor.h"
#include "dcmtk/dcmfg/fgplanpo.h"
#include "dcmtk/dcmiod/iodmacro.h"
#include "dcmtk/dcmseg/overlaputil.h"
#include "dcmtk/dcmseg/segdoc.h"
#include "dcmtk/dcmseg/segment.h"
#include "dcmtk/dcmseg/segtypes.h"
#include "dcmtk/dcmseg/segutils.h"
#include "dcmtk/ofstd/ofstd.h"

#include <cstdio>
#include <cstring>
#include <iostream>
#include <sstream>
#include <vector>

static const Uint16 ROWS    = 5;
static const Uint16 COLUMNS = 7;
static const size_t PIXELS  = ROWS * COLUMNS;
static const char* DIMENSION_UID = "2.25.1000111122223333444455556666";

static int fail(const char* what, const OFCondition& cond)
{
    std::cerr << what << ": " << cond.text() << std::endl;
    return 1;
}

static OFString position(unsigned slice)
{
    char buf[32];
    std::snprintf(buf, sizeof(buf), "%u", slice * 2);
    return buf;
}

static DcmSegment* newSegment(const OFString& label)
{
    DcmSegment* segment = NULL;
    CodeSequenceMacro category("85756007", "SCT", "Tissue");
    CodeSequenceMacro type("51114001", "SCT", "Artery");
    DcmSegment::create(segment, label, category, type, DcmSegTypes::SAT_MANUAL);
    return segment;
}

static int writeObject(const std::string& kind, const char* path)
{
    IODGeneralEquipmentModule::EquipmentInfo equipment("dicom-go", "segoracle", "1", "1");
    ContentIdentificationMacro content("1", "ORACLE", "DCMTK segmentation oracle", "Oracle^Seg");
    DcmSegmentation* seg = NULL;
    OFCondition cond;
    unsigned segments = 0, slices = 2, maxValue = 0, maxLabel = 0;
    const bool labelmap = kind == "labelmap8" || kind == "labelmap16";
    if (kind == "binary")
    {
        segments = 3;
        cond = DcmSegmentation::createBinarySegmentation(seg, ROWS, COLUMNS, equipment, content);
    }
    else if (kind == "fractional-probability" || kind == "fractional-occupancy")
    {
        segments = 2;
        maxValue = kind == "fractional-probability" ? 200 : 255;
        cond = DcmSegmentation::createFractionalSegmentation(seg, ROWS, COLUMNS,
            kind == "fractional-probability" ? DcmSegTypes::SFT_PROBABILITY : DcmSegTypes::SFT_OCCUPANCY,
            OFstatic_cast(Uint16, maxValue), equipment, content);
    }
    else if (labelmap)
    {
        slices = 3;
        maxLabel = kind == "labelmap8" ? 255 : 300;
        cond = DcmSegmentation::createLabelmapSegmentation(seg, ROWS, COLUMNS, equipment, content, kind == "labelmap16");
    }
    else
    {
        std::cerr << "unknown kind " << kind << std::endl;
        return 2;
    }
    if (cond.bad() || seg == NULL)
        return fail("create", cond);

    seg->getPatient().setPatientName("TEST^SEG");
    seg->getPatient().setPatientID("SEG1000");
    seg->getStudy().setStudyInstanceUID("1.2.826.0.1.3680043.10.543.1000.20");
    seg->getStudy().setStudyDate("20260926");
    seg->getStudy().setStudyTime("120000");
    seg->getSeries().setSeriesInstanceUID("1.2.826.0.1.3680043.10.543.1000.21");
    seg->getSeries().setSeriesNumber("1");
    seg->getSOPCommon().setSOPInstanceUID("1.2.826.0.1.3680043.10.543.1000.22");
    seg->getFrameOfReference().setFrameOfReferenceUID("1.2.826.0.1.3680043.10.543.1000.23");
    seg->getGeneralImage().setContentDate("20260926");
    seg->getGeneralImage().setContentTime("120000");

    FGPixelMeasures measures;
    measures.setPixelSpacing("0.5\\0.5");
    measures.setSliceThickness("2");
    FGPlaneOrientationPatient orientation;
    orientation.setImageOrientationPatient("1", "0", "0", "0", "1", "0");
    if ((cond = seg->addForAllFrames(measures)).bad() || (cond = seg->addForAllFrames(orientation)).bad())
        return fail("shared groups", cond);

    IODMultiframeDimensionModule& dims = seg->getDimensions();
    if (!labelmap)
        dims.addDimensionIndex(DCM_ReferencedSegmentNumber, DIMENSION_UID, DCM_SegmentIdentificationSequence, "SEGMENT");
    dims.addDimensionIndex(DCM_ImagePositionPatient, DIMENSION_UID, DCM_PlanePositionSequence, "POSITION");

    if (labelmap)
    {
        for (unsigned number = 0; number <= maxLabel; number++)
        {
            std::ostringstream label;
            label << "L" << number;
            Uint16 segmentNumber = OFstatic_cast(Uint16, number);
            if ((cond = seg->addSegment(newSegment(label.str().c_str()), segmentNumber)).bad())
                return fail("segment", cond);
        }
    }
    else
    {
        for (unsigned number = 1; number <= segments; number++)
        {
            std::ostringstream label;
            label << "S" << number;
            Uint16 segmentNumber = 0;
            if ((cond = seg->addSegment(newSegment(label.str().c_str()), segmentNumber)).bad())
                return fail("segment", cond);
        }
    }

    for (unsigned segment = 1; segment <= (labelmap ? 1 : segments); segment++)
    {
        for (unsigned slice = 0; slice < slices; slice++)
        {
            FGPlanePosPatient plane;
            plane.setImagePositionPatient("0", "0", position(slice));
            FGFrameContent frameContent;
            if (labelmap)
                frameContent.setDimensionIndexValues(slice + 1, 0);
            else
            {
                frameContent.setDimensionIndexValues(segment, 0);
                frameContent.setDimensionIndexValues(slice + 1, 1);
            }
            OFVector<FGBase*> groups;
            groups.push_back(&plane);
            groups.push_back(&frameContent);
            if (kind == "labelmap16")
            {
                std::vector<Uint16> data(PIXELS);
                for (size_t i = 0; i < PIXELS; i++)
                    data[i] = OFstatic_cast(Uint16, (i * 97 + slice) % (maxLabel + 1));
                cond = seg->addFrame<Uint16>(&data[0], 0, groups);
            }
            else
            {
                std::vector<Uint8> data(PIXELS);
                for (size_t i = 0; i < PIXELS; i++)
                {
                    const unsigned x = OFstatic_cast(unsigned, i % COLUMNS), y = OFstatic_cast(unsigned, i / COLUMNS);
                    if (labelmap)
                        data[i] = OFstatic_cast(Uint8, (i * 97 + slice) % (maxLabel + 1));
                    else if (maxValue > 0)
                        data[i] = OFstatic_cast(Uint8, (i * 13 + slice * 5 + segment) % (maxValue + 1));
                    else if (segment == 1)
                        data[i] = (x + y + slice) % 3 == 0;
                    else if (segment == 2)
                        data[i] = (x + y + slice) % 3 == 1;
                    else
                        data[i] = x < 3;
                }
                cond = seg->addFrame<Uint8>(&data[0], OFstatic_cast(Uint16, segment), groups);
            }
            if (cond.bad())
                return fail("frame", cond);
        }
    }
    if ((cond = seg->saveFile(path, EXS_LittleEndianExplicit)).bad())
        return fail("save", cond);
    delete seg;
    return 0;
}

static const char* typeName(DcmSegTypes::E_SegmentationType type)
{
    switch (type)
    {
        case DcmSegTypes::ST_BINARY: return "BINARY";
        case DcmSegTypes::ST_FRACTIONAL: return "FRACTIONAL";
        case DcmSegTypes::ST_LABELMAP: return "LABELMAP";
        default: return "UNKNOWN";
    }
}

static int dumpObject(const char* path)
{
    DcmSegmentation* seg = NULL;
    OFCondition cond = DcmSegmentation::loadFile(path, seg);
    if (cond.bad() || seg == NULL)
        return fail("load", cond);
    const DcmSegTypes::E_SegmentationType type = seg->getSegmentationType();
    std::cout << "type " << typeName(type) << " rows " << seg->getRows() << " columns " << seg->getColumns()
              << " frames " << seg->getNumberOfFrames() << " bits16 " << (seg->has16BitPixelData() ? 1 : 0);
    if (type == DcmSegTypes::ST_FRACTIONAL)
        std::cout << " fractional " << (seg->getSegmentationFractionalType() == DcmSegTypes::SFT_PROBABILITY ? "PROBABILITY" : "OCCUPANCY");
    std::cout << std::endl << "segments";
    const OFMap<Uint16, DcmSegment*>& segments = seg->getSegments();
    for (OFMap<Uint16, DcmSegment*>::const_iterator it = segments.begin(); it != segments.end(); ++it)
        std::cout << " " << it->first;
    std::cout << std::endl;
    const Uint16 rows = seg->getRows(), columns = seg->getColumns();
    for (size_t f = 0; f < seg->getNumberOfFrames(); f++)
    {
        const DcmIODTypes::FrameBase* frame = seg->getFrame(f);
        if (frame == NULL)
            return fail("frame", EC_IllegalCall);
        std::cout << "frame " << f << ":";
        if (type == DcmSegTypes::ST_BINARY)
        {
            DcmIODTypes::Frame<Uint8>* unpacked = DcmSegUtils::unpackBinaryFrame(
                OFstatic_cast(const DcmIODTypes::Frame<Uint8>*, frame), rows, columns);
            if (unpacked == NULL)
                return fail("unpack", EC_IllegalCall);
            for (size_t i = 0; i < OFstatic_cast(size_t, rows) * columns; i++)
            {
                Uint8 value = 0;
                unpacked->getUint8AtIndex(value, i);
                std::cout << " " << (value ? 1 : 0);
            }
            delete unpacked;
        }
        else
        {
            for (size_t i = 0; i < OFstatic_cast(size_t, rows) * columns; i++)
            {
                if (seg->has16BitPixelData())
                {
                    Uint16 value = 0;
                    frame->getUint16AtIndex(value, i);
                    std::cout << " " << value;
                }
                else
                {
                    Uint8 value = 0;
                    frame->getUint8AtIndex(value, i);
                    std::cout << " " << OFstatic_cast(unsigned, value);
                }
            }
        }
        std::cout << std::endl;
    }
    if (type != DcmSegTypes::ST_LABELMAP)
    {
        OverlapUtil overlap;
        overlap.setSegmentationObject(seg);
        OverlapUtil::OverlapMatrix matrix;
        if ((cond = overlap.getOverlapMatrix(matrix)).bad())
            return fail("overlap", cond);
        for (size_t a = 0; a < matrix.size(); a++)
            for (size_t b = a + 1; b < matrix[a].size(); b++)
                if (matrix[a][b] == 1)
                    std::cout << "overlap " << a + 1 << " " << b + 1 << std::endl;
        OverlapUtil::SegmentGroups groups;
        if ((cond = overlap.getNonOverlappingSegments(groups)).bad())
            return fail("groups", cond);
        for (size_t g = 0; g < groups.size(); g++)
        {
            std::cout << "group";
            for (size_t s = 0; s < groups[g].size(); s++)
                std::cout << " " << groups[g][s];
            std::cout << std::endl;
        }
    }
    delete seg;
    return 0;
}

int main(int argc, char* argv[])
{
    if (argc == 4 && std::strcmp(argv[1], "write") == 0)
        return writeObject(argv[2], argv[3]);
    if (argc == 3 && std::strcmp(argv[1], "dump") == 0)
        return dumpObject(argv[2]);
    std::cerr << "usage: dcmtk_segoracle write <kind> <file> | dump <file>" << std::endl;
    return 2;
}
