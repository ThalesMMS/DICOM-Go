// Independent synthetic oracle generator; project-owned, no vendor code.
// Build against GDCM's public CharLS API; no codec is linked into dicom-go.
#define CHARLS_DLL
#include <gdcmcharls/charls.h>
#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <fstream>
#include <stdexcept>
#include <string>
#include <vector>

static void write(const std::filesystem::path& path, const std::vector<unsigned char>& bytes) {
    std::ofstream file(path, std::ios::binary);
    file.write(reinterpret_cast<const char*>(bytes.data()), bytes.size());
    if (!file) throw std::runtime_error("write failed: " + path.string());
}

int main(int argc, char** argv) {
    if (argc != 2) return 2;
    const std::filesystem::path root(argv[1]);
    std::filesystem::create_directories(root);
    for (int bits : {8, 16}) {
        const unsigned int maximum = (1u << bits) - 1;
        std::vector<unsigned char> source;
        uint32_t random = 1459;
        for (int pixel = 0; pixel < 33 * 17; ++pixel) {
            for (int component = 0; component < 3; ++component) {
                random = random * 1664525u + 1013904223u;
                unsigned int value = (random >> 8) & maximum;
                if (pixel < 8) value = (pixel & (1 << component)) ? maximum : 0;
                if (pixel == 8) value = maximum / 2 + component;
                source.push_back(static_cast<unsigned char>(value));
                if (bits == 16) source.push_back(static_cast<unsigned char>(value >> 8));
            }
        }
        const std::string prefix = "rgb" + std::to_string(bits) + "-33x17";
        write(root / (prefix + ".raw"), source);
        for (int hp : {1, 2, 3}) for (int ilv : {1, 2}) {
            JlsParameters params{};
            params.width = 33;
            params.height = 17;
            params.bitsPerSample = bits;
            params.components = 3;
            params.interleaveMode = static_cast<CharlsInterleaveModeType>(ilv);
            params.colorTransformation = static_cast<CharlsColorTransformationType>(hp);
            std::vector<unsigned char> encoded(source.size() * 4 + 1024);
            size_t length = 0;
            char message[256]{};
            auto result = JpegLsEncode(encoded.data(), encoded.size(), &length, source.data(), source.size(), &params, message);
            if (static_cast<int>(result) != 0) throw std::runtime_error("encode: " + std::string(message));
            encoded.resize(length);
            std::vector<unsigned char> decoded(source.size());
            result = JpegLsDecode(decoded.data(), decoded.size(), encoded.data(), encoded.size(), nullptr, message);
            if (static_cast<int>(result) != 0 || decoded != source) throw std::runtime_error("independent RGB decode mismatch");
            const std::string name = prefix + "-hp" + std::to_string(hp) + "-ilv" + std::to_string(ilv) + ".jls";
            write(root / name, encoded);
            std::printf("%s bytes=%zu exact=%zu samples\n", name.c_str(), length, source.size() / (bits / 8));
        }
    }
}
