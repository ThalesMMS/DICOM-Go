// dcmtk_concat: splits enhanced multi-frame objects into Concatenations and
// merges Concatenations back with DCMTK dcmfg, as an oracle for dicom-go's
// multiframe.Concatenation tests (opt-in, see TestConcatenationsMatchDCMTK).
// This program is dicom-go test tooling that calls DCMTK's public API; it
// contains no DCMTK code.
//
// Build against an installed DCMTK (3.7 or later), for example:
//   g++ -O1 -o dcmtk_concat dcmtk_concat.cc -ldcmfg -ldcmiod -ldcmdata -loflog -lofstd
//
// Usage:
//   dcmtk_concat split <in.dcm> <frames per instance> <out prefix>
//       writes <out prefix>-1.dcm, <out prefix>-2.dcm, ... and prints their count
//   dcmtk_concat merge <out.dcm> <instance.dcm>...
//       loads the Concatenation of the instances and writes the merged
//       multi-frame object (with its frames as native Pixel Data)

#include "dcmtk/config/osconfig.h"
#include "dcmtk/dcmdata/dcdeftag.h"
#include "dcmtk/dcmdata/dcfilefo.h"
#include "dcmtk/dcmdata/dcpixel.h"
#include "dcmtk/dcmfg/concatenationcreator.h"
#include "dcmtk/dcmfg/concatenationloader.h"
#include "dcmtk/dcmiod/iodtypes.h"

#include <cstring>
#include <iostream>
#include <sstream>
#include <vector>

static int fail(const char* what, const OFCondition& cond)
{
    std::cerr << what << ": " << cond.text() << std::endl;
    return 1;
}

static int split(const char* in, unsigned framesPerInstance, const std::string& prefix)
{
    DcmFileFormat source;
    OFCondition cond = source.loadFile(in);
    if (cond.bad())
        return fail("load", cond);
    ConcatenationCreator creator;
    if ((cond = creator.setCfgFramesPerInstance(framesPerInstance)).bad())
        return fail("frames per instance", cond);
    if ((cond = creator.setCfgInput(source.getDataset(), OFFalse)).bad())
        return fail("input", cond);
    const size_t instances = creator.getNumInstances();
    for (size_t i = 0; i < instances; i++)
    {
        DcmFileFormat out;
        if ((cond = creator.writeNextInstance(*out.getDataset())).bad())
            return fail("instance", cond);
        std::ostringstream name;
        name << prefix << "-" << (i + 1) << ".dcm";
        if ((cond = out.saveFile(name.str().c_str(), EXS_LittleEndianExplicit)).bad())
            return fail("save", cond);
    }
    std::cout << instances << std::endl;
    return 0;
}

static int merge(const char* out, int count, char* files[])
{
    OFList<OFFilename> names;
    for (int i = 0; i < count; i++)
        names.push_back(files[i]);
    ConcatenationLoader loader;
    OFCondition cond = loader.scan(names);
    if (cond.bad())
        return fail("scan", cond);
    const ConcatenationLoader::TScanResult& found = loader.getInfo();
    if (found.size() != 1)
    {
        std::cerr << "found " << found.size() << " concatenations" << std::endl;
        return 1;
    }
    DcmFileFormat merged;
    OFVector<DcmIODTypes::FrameBase*> frames;
    if ((cond = loader.load(found.begin()->first, merged.getDataset(), frames)).bad())
        return fail("load", cond);
    std::vector<Uint8> pixels;
    for (size_t f = 0; f < frames.size(); f++)
    {
        const Uint8* data = OFstatic_cast(const Uint8*, frames[f]->getPixelData());
        pixels.insert(pixels.end(), data, data + frames[f]->getLengthInBytes());
        delete frames[f];
    }
    if (pixels.size() % 2)
        pixels.push_back(0);
    Uint16 bitsAllocated = 16;
    merged.getDataset()->findAndGetUint16(DCM_BitsAllocated, bitsAllocated);
    if (bitsAllocated > 8)
        cond = merged.getDataset()->putAndInsertUint16Array(DCM_PixelData, OFreinterpret_cast(Uint16*, &pixels[0]), OFstatic_cast(unsigned long, pixels.size() / 2));
    else
        cond = merged.getDataset()->putAndInsertUint8Array(DCM_PixelData, &pixels[0], OFstatic_cast(unsigned long, pixels.size()));
    if (cond.bad())
        return fail("pixel data", cond);
    if ((cond = merged.saveFile(out, EXS_LittleEndianExplicit)).bad())
        return fail("save", cond);
    std::cout << frames.size() << std::endl;
    return 0;
}

int main(int argc, char* argv[])
{
    if (argc == 5 && std::strcmp(argv[1], "split") == 0)
        return split(argv[2], OFstatic_cast(unsigned, std::atoi(argv[3])), argv[4]);
    if (argc >= 4 && std::strcmp(argv[1], "merge") == 0)
        return merge(argv[2], argc - 3, argv + 3);
    std::cerr << "usage: dcmtk_concat split <in> <frames per instance> <prefix> | merge <out> <files>..." << std::endl;
    return 2;
}
