// Own qualification adapter using installed public GDCM API declarations.
// No vendor implementation is copied or linked into the Go encoder.
#include <gdcmJPEGCodec.h>
#include <gdcmPixelFormat.h>
#include <gdcmPhotometricInterpretation.h>
#include <gdcmTransferSyntax.h>
#include <gdcmVersion.h>
#include <cstdlib>
#include <fstream>
#include <iostream>
#include <string>
class JPEGOracle : public gdcm::JPEGCodec {
 public: using gdcm::JPEGCodec::DecodeByStreams;
};
int main(int argc, char **argv) {
 if (argc == 2 && std::string(argv[1]) == "--version") {
  std::cout << "public gdcm::JPEGCodec " << gdcm::Version::GetVersion() << std::endl;
  return 0;
 }
 if (argc != 9) return 2;
 const unsigned bits=std::strtoul(argv[4],nullptr,10);
 const unsigned components=std::strtoul(argv[5],nullptr,10);
 const unsigned rows=std::strtoul(argv[7],nullptr,10);
 const unsigned cols=std::strtoul(argv[8],nullptr,10);
 if ((bits!=8 && bits!=12) || (components!=1 && components!=3) || !rows || !cols) return 2;
 JPEGOracle codec;
 codec.SetPixelFormat(gdcm::PixelFormat(components,bits==8?8:16,bits,bits-1,0));
 codec.SetPhotometricInterpretation(gdcm::PhotometricInterpretation::GetPIType(argv[6]));
 codec.SetPlanarConfiguration(0);
 // Leave dimensions unset: GetHeaderInfo must discover them from JPEG.
 codec.SetNumberOfDimensions(2);
 std::ifstream input(argv[1],std::ios::binary);
 std::ofstream output(argv[2],std::ios::binary);
 std::ofstream header(argv[3]);
 if (!input || !output || !header) return 3;
 gdcm::TransferSyntax ts;
 if (!codec.GetHeaderInfo(input,ts)) return 4;
 const auto &pf=codec.GetPixelFormat(); const auto *dims=codec.GetDimensions();
 if (dims[0]!=cols || dims[1]!=rows || pf.GetBitsStored()!=bits || pf.GetSamplesPerPixel()!=components) return 8;
 header << "{\"Columns\":" << dims[0] << ",\"Rows\":" << dims[1]
  << ",\"BitsStored\":" << pf.GetBitsStored()
  << ",\"SamplesPerPixel\":" << pf.GetSamplesPerPixel() << "}\n";
 if (!header) return 5;
 input.clear(); input.seekg(0);
 if (!codec.DecodeByStreams(input,output)) return 6;
 output.flush(); if (!output) return 7;
 return 0;
}
