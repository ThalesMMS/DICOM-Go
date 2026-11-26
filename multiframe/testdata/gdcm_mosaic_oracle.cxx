// Prints what GDCM's SplitMosaicFilter computes for a Siemens mosaic: slice
// dimensions, origin, spacing, direction cosines and the FNV-1a 64 hash of
// each slice buffer. Used as an oracle for multiframe.SplitMosaic.
//
// Build (MSYS2 UCRT64, GDCM 3.2):
//   g++ -O1 -I/ucrt64/include/gdcm-3.2 gdcm_mosaic_oracle.cxx -o gdcm_mosaic_oracle \
//       -lgdcmMSFF -lgdcmDSED -lgdcmCommon -lgdcmDICT -lgdcmIOD
#include "gdcmImageReader.h"
#include "gdcmSplitMosaicFilter.h"
#include <cstdio>
#include <vector>

int main(int argc, char *argv[]) {
  if (argc < 2) return 2;
  gdcm::ImageReader reader;
  reader.SetFileName(argv[1]);
  if (!reader.Read()) { std::fprintf(stderr, "read failed\n"); return 1; }
  gdcm::SplitMosaicFilter filter;
  filter.SetImage(reader.GetImage());
  filter.SetFile(reader.GetFile());
  if (!filter.Split()) { std::fprintf(stderr, "split failed\n"); return 1; }
  const gdcm::Image &image = filter.GetImage();
  const unsigned int *d = image.GetDimensions();
  const double *o = image.GetOrigin();
  const double *s = image.GetSpacing();
  const double *c = image.GetDirectionCosines();
  std::printf("dims %u %u %u\n", d[0], d[1], d[2]);
  std::printf("origin %.10f %.10f %.10f\n", o[0], o[1], o[2]);
  std::printf("spacing %.10f %.10f %.10f\n", s[0], s[1], s[2]);
  std::printf("cosines %.10f %.10f %.10f %.10f %.10f %.10f\n", c[0], c[1], c[2], c[3], c[4], c[5]);
  std::vector<char> buf(image.GetBufferLength());
  image.GetBuffer(buf.data());
  size_t slice = buf.size() / d[2];
  for (unsigned int z = 0; z < d[2]; ++z) {
    unsigned long long h = 14695981039346656037ULL;
    for (size_t i = 0; i < slice; ++i) { h = (h ^ (unsigned char)buf[z * slice + i]) * 1099511628211ULL; }
    std::printf("slice %u fnv %08x%08x\n", z, (unsigned)(h >> 32), (unsigned)h);
  }
  if (argc > 2) {
    // Optionally write the split volume buffer to argv[2].
    FILE *out = std::fopen(argv[2], "wb");
    if (!out) return 1;
    std::fwrite(buf.data(), 1, buf.size(), out);
    std::fclose(out);
  }
  return 0;
}
