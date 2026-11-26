// Own test harness using only public GDCM APIs. No codec implementation copied.
// Per-process driver refuses declared large workloads before this program runs.
#include <gdcmImageReader.h>
#include <gdcmImageChangePhotometricInterpretation.h>
#include <gdcmImageChangeTransferSyntax.h>
#include <gdcmVersion.h>
#include <gdcmAttribute.h>
#include <fstream>
#include <iostream>
#include <iomanip>
#include <vector>

template <typename T> void array(const T *values, unsigned count) {
  std::cout << '[';
  for (unsigned i = 0; i < count; ++i) {
    if (i) std::cout << ',';
    std::cout << values[i];
  }
  std::cout << ']';
}

int main(int argc, char **argv) {
  if (argc != 3) return 2;
  std::ifstream input(argv[1], std::ios::binary | std::ios::ate);
  if (!input || input.tellg() < 0 || input.tellg() > 65536) return 3;
  gdcm::ImageReader reader;
  reader.SetFileName(argv[1]);
  if (!reader.Read()) return 4;
  const gdcm::Image &source = reader.GetImage();
  const auto &format = source.GetPixelFormat();
  const auto *dimensions = source.GetDimensions();
  const unsigned frames = source.GetNumberOfDimensions() > 2 ? dimensions[2] : 1;
  if (frames > 4 || uint64_t(dimensions[0]) * dimensions[1] * frames > 4096 || source.GetBufferLength() > 65536) return 5;
  std::cout << std::setprecision(17);
  std::cout << "{\"Version\":\"" << gdcm::Version::GetVersion() << "\",\"Rows\":" << dimensions[1]
            << ",\"Columns\":" << dimensions[0] << ",\"Frames\":" << frames
            << ",\"Samples\":" << format.GetSamplesPerPixel()
            << ",\"BitsAllocated\":" << format.GetBitsAllocated()
            << ",\"BitsStored\":" << format.GetBitsStored()
            << ",\"HighBit\":" << format.GetHighBit()
            << ",\"PixelRepresentation\":" << format.GetPixelRepresentation()
            << ",\"SourcePhotometric\":\"" << source.GetPhotometricInterpretation().GetString()
            << "\",\"SpacingXYZ\":";
  array(source.GetSpacing(), 3);
  std::cout << ",\"Origin\":"; array(source.GetOrigin(), 3);
  std::cout << ",\"DirectionCosines\":"; array(source.GetDirectionCosines(), 6);
  // ImageReader applies SOP-dependent geometry defaults. Read the stored tags
  // separately so that those defaults cannot hide a metadata mismatch.
  gdcm::Attribute<0x0028, 0x0030> spacing;
  gdcm::Attribute<0x0020, 0x0037> orientation;
  gdcm::Attribute<0x0020, 0x0032> position;
  const auto &dataset = reader.GetFile().GetDataSet();
  spacing.SetFromDataSet(dataset); orientation.SetFromDataSet(dataset); position.SetFromDataSet(dataset);
  std::cout << ",\"DataSpacing\":"; array(spacing.GetValues(), 2);
  std::cout << ",\"DataOrientation\":"; array(orientation.GetValues(), 6);
  std::cout << ",\"DataPosition\":"; array(position.GetValues(), 3);
  gdcm::ImageChangeTransferSyntax uncompress;
  uncompress.SetInput(source);
  uncompress.SetTransferSyntax(gdcm::TransferSyntax::ExplicitVRLittleEndian);
  if (!uncompress.Change()) return 7;
  gdcm::ImageChangePhotometricInterpretation color;
  const gdcm::Image *decoded = &uncompress.GetOutput();
  if (format.GetSamplesPerPixel() == 3 && decoded->GetPhotometricInterpretation() != gdcm::PhotometricInterpretation::RGB) {
    color.SetInput(*decoded);
    color.SetPhotometricInterpretation(gdcm::PhotometricInterpretation::RGB);
    if (!color.Change()) return 6;
    decoded = &color.GetOutput();
  }
  if (decoded->GetBufferLength() > 65536) return 5;
  std::vector<char> pixels(decoded->GetBufferLength());
  if (!decoded->GetBuffer(pixels.data())) return 7;
  std::ofstream output(argv[2], std::ios::binary);
  output.write(pixels.data(), pixels.size());
  if (!output) return 8;
  std::cout << ",\"DecodedPhotometric\":\"" << decoded->GetPhotometricInterpretation().GetString()
            << "\",\"DecodedBytes\":" << pixels.size() << "}\n";
  return 0;
}
