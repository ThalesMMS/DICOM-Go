#include <dcmtk/config/osconfig.h>
#include <dcmtk/dcmdata/dctk.h>
#include <dcmtk/dcmdata/dcpixseq.h>
#include <dcmtk/dcmdata/dcpxitem.h>
#include <dcmtk/dcmdata/dcrledrg.h>
#include <dcmtk/dcmjpls/djdecode.h>
#include <openssl/evp.h>

#include <array>
#include <chrono>
#include <cstdlib>
#include <iomanip>
#include <iostream>
#include <stdexcept>
#include <sstream>
#include <string>
#include <vector>

using Clock = std::chrono::steady_clock;

static void require(OFCondition result) {
    if (result.bad()) throw std::runtime_error(result.text());
}

static std::string sha256(const void *data, size_t size) {
    std::array<unsigned char, 32> hash{};
    unsigned int length = 0;
    if (EVP_Digest(data, size, hash.data(), &length, EVP_sha256(), nullptr) != 1 ||
        length != hash.size())
        throw std::runtime_error("SHA-256 failed");
    std::ostringstream hex;
    hex << std::hex << std::setfill('0');
    for (unsigned char byte : hash) hex << std::setw(2) << static_cast<unsigned int>(byte);
    return hex.str();
}

static void run(const char *path, const std::string &storage, long iterations) {
    const bool deferred = storage == "deferred";
    if (!deferred && storage != "resident") throw std::runtime_error("unknown storage");
    if (gLocalByteOrder != EBO_LittleEndian)
        throw std::runtime_error("adapter currently verifies a little-endian 16-bit host");

    auto start = Clock::now();
    DcmFileFormat file;
    require(file.loadFile(path, EXS_Unknown, EGL_noChange, deferred ? 4096 : DCM_UndefinedLength));
    DcmDataset *dataset = file.getDataset();
    DcmElement *element = nullptr;
    require(dataset->findAndGetElement(DCM_PixelData, element));
    auto *pixel = dynamic_cast<DcmPixelData *>(element);
    if (!pixel) throw std::runtime_error("missing encapsulated Pixel Data");
    E_TransferSyntax syntax = EXS_Unknown;
    const DcmRepresentationParameter *parameter = nullptr;
    pixel->getOriginalRepresentationKey(syntax, parameter);
    DcmPixelSequence *sequence = nullptr;
    require(pixel->getEncapsulatedRepresentation(syntax, parameter, sequence));
    std::vector<DcmPixelItem *> fragments;
    for (unsigned long item = 1; item < sequence->card(); ++item) {
        DcmPixelItem *fragment = nullptr;
        require(sequence->getItem(fragment, item));
        if (deferred == bool(fragment->valueLoaded()))
            throw std::runtime_error("fixture fragment violates requested storage precondition");
        fragments.push_back(fragment);
    }
    const auto setup = std::chrono::duration_cast<std::chrono::nanoseconds>(Clock::now() - start).count();

    Sint32 frames = 0;
    Uint16 rows = 0, columns = 0, samples = 0, allocated = 0, stored = 0, high = 0, representation = 0;
    OFString photo;
    require(dataset->findAndGetSint32(DCM_NumberOfFrames, frames));
    require(dataset->findAndGetUint16(DCM_Rows, rows));
    require(dataset->findAndGetUint16(DCM_Columns, columns));
    require(dataset->findAndGetUint16(DCM_SamplesPerPixel, samples));
    require(dataset->findAndGetUint16(DCM_BitsAllocated, allocated));
    require(dataset->findAndGetUint16(DCM_BitsStored, stored));
    require(dataset->findAndGetUint16(DCM_HighBit, high));
    require(dataset->findAndGetUint16(DCM_PixelRepresentation, representation));
    require(dataset->findAndGetOFString(DCM_PhotometricInterpretation, photo));
    if (frames != 64 || rows != 256 || columns != 256 || samples != 1 || allocated != 16 ||
        stored != 16 || high != 15 || representation != 1 || photo != "MONOCHROME2")
        throw std::runtime_error("unexpected fixture metadata");
    const auto fragmentsPerFrame = fragments.size() / frames;
    if (fragments.size() % frames != 0 || fragmentsPerFrame < 1 || fragmentsPerFrame > 2)
        throw std::runtime_error("unexpected fixture fragment counts");
    DcmPixelItem *bot = nullptr;
    require(sequence->getItem(bot, 0));
    const bool splitEmptyBOT = fragmentsPerFrame == 2 && bot->getLength() == 0;
    std::vector<Uint16> output(size_t(rows) * columns * samples);
    const auto bytes = static_cast<Uint32>(output.size() * sizeof(output[0]));
    const std::array<Uint32, 4> indices{0, 1, Uint32(frames - 1), 0};
    OFString model;
    std::array<double, 4> phaseNS{};
    double totalNS = 0;
    double loadedBytes = 0;
    for (long turn = 0; turn <= iterations; ++turn) {
        Uint32 next = 0;
        for (size_t op = 0; op < indices.size(); ++op) {
            const Uint32 frame = indices[op];
            const Uint32 hint = op == 1 ? next : 0;
            next = hint;
            start = Clock::now();
            const auto result = pixel->getUncompressedFrame(dataset, frame, next, output.data(), bytes, model);
            const auto elapsed = std::chrono::duration_cast<std::chrono::nanoseconds>(Clock::now() - start).count();
            if (turn != 0) {
                totalNS += elapsed;
                phaseNS[op] += elapsed;
            }
            if (result.good()) {
                if (model != photo) throw std::runtime_error("decoded color model mismatch");
                if (next != 1 + (frame + 1) * fragmentsPerFrame)
                    throw std::runtime_error("unexpected next fragment");
            }
            double touched = 0;
            unsigned loaded = 0;
            unsigned foreign = 0;
            if (deferred) {
                for (size_t i = 0; i < fragments.size(); ++i) {
                    auto *item = fragments[i];
                    if (item->valueLoaded()) {
                        touched += item->getLength();
                        ++loaded;
                        // This benchmark either has one Item per frame or
                        // exactly two, both indexed by their corpus frame.
                        if (i / fragmentsPerFrame != frame) ++foreign;
                        item->compact();
                        if (item->valueLoaded()) throw std::runtime_error("cannot restore deferred fragment");
                    }
                }
            }
            if (result.good() && deferred && syntax == EXS_JPEGLSLossless) {
                const unsigned lookahead = splitEmptyBOT && frame + 1 < Uint32(frames) ? 1 : 0;
                if (foreign != lookahead || loaded != fragmentsPerFrame + lookahead)
                    throw std::runtime_error("opened unexpected deferred fragments");
            }
            if (turn != 0) loadedBytes += touched;
            if (iterations == 0) {
                std::cerr << "frame=" << frame << " ns=" << elapsed << " startFragment=" << hint
                          << " nextFragment=" << next << " loaded=" << loaded << " foreign=" << foreign
                          << " loaded_bytes=" << touched << '\n';
                if (result.bad()) {
                    std::cout << frame << ",error," << result.text() << '\n';
                    continue;
                }
                std::cout << frame << ',' << sha256(output.data(), bytes) << ',' << rows << ',' << columns
                          << ',' << samples << ',' << stored << ',' << high << ',' << representation
                          << ',' << model << '\n';
            } else {
                require(result);
            }
        }
    }
    if (iterations != 0) {
        const double count = double(iterations) * indices.size();
        std::cout << std::fixed << std::setprecision(3) << totalNS / count
                  << ",,,,";
        // RLE releases the just-read fragment inside the decoder. Its
        // post-call residency cannot measure payload bytes read.
        if (deferred && syntax != EXS_RLELossless) std::cout << loadedBytes / count;
        std::cout << ',' << setup << ",,";
        for (const auto ns : phaseNS) std::cout << ',' << ns / iterations;
        std::cout << '\n';
    }
}

int main(int argc, char **argv) {
    if (argc != 4) {
        std::cerr << "usage: dcmtk-frame-access FILE {resident|deferred} ITERATIONS\n";
        return 1;
    }
    DcmRLEDecoderRegistration::registerCodecs();
    DJLSDecoderRegistration::registerCodecs();
    int exitCode = 0;
    try {
        const std::string repetitions(argv[3]);
        size_t consumed = 0;
        long iterations = std::stol(repetitions, &consumed);
        if (iterations < 0 || consumed != repetitions.size()) throw std::runtime_error("invalid iterations");
        run(argv[1], argv[2], iterations);
    } catch (const std::exception &err) {
        std::cerr << err.what() << '\n';
        exitCode = 1;
    }
    DJLSDecoderRegistration::cleanup();
    DcmRLEDecoderRegistration::cleanup();
    return exitCode;
}
