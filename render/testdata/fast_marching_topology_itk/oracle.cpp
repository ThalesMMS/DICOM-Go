// Independent procedural fixtures and calls to public ITK APIs only.
#include <array>
#include <cmath>
#include <iostream>
#include <itkFastMarchingImageFilterBase.h>
#include <itkFastMarchingThresholdStoppingCriterion.h>
#include <itkImage.h>
#include <itkVersion.h>
#include <string>
#include <vector>

template <unsigned D> void run(const std::string &kind, int mode, bool &comma) {
  using Image = itk::Image<double, D>;
  using FM = itk::FastMarchingImageFilterBase<Image, Image>;
  using Stop = itk::FastMarchingThresholdStoppingCriterion<Image, Image>;
  typename Image::SizeType size;
  size.Fill(7);
  typename Image::IndexType origin;
  origin.Fill(0);
  typename Image::RegionType region;
  region.SetIndex(origin);
  region.SetSize(size);
  auto image = Image::New();
  image->SetRegions(region);
  image->Allocate();
  image->FillBuffer(0);
  typename Image::SpacingType spacing;
  spacing.Fill(1);
  image->SetSpacing(spacing);
  const int nz = D == 2 ? 1 : 7, n = 49 * nz;
  std::vector<bool> initial(n, false), domain(n, false);
  std::vector<typename Image::IndexType> coords(n);
  for (int i = 0; i < n; i++) {
    int x = i % 7, y = i / 7 % 7, z = i / 49;
    typename Image::IndexType p;
    p.Fill(0);
    p[0] = x;
    p[1] = y;
    if constexpr (D == 3)
      p[2] = z;
    coords[i] = p;
    bool inside = false, seed = false;
    if (kind == "bridge") {
      inside = x >= 1 && x <= 5 && y == 3 && (D == 2 || z == 3);
      seed = inside && (x == 1 || x == 5);
    } else if (kind == "ring" || kind == "tunnel") {
      inside = x >= 1 && x <= 5 && y >= 1 && y <= 5 &&
               (x == 1 || x == 5 || y == 1 || y == 5) &&
               (D == 2 || (z >= 2 && z <= 4));
      seed = inside && x == 1 && y == 1 && (D == 2 || z == 3);
    } else {
      inside = x >= 1 && x <= 5 && y >= 1 && y <= 5 && z >= 1 && z <= 5;
      seed =
          inside && (x == 1 || x == 5 || y == 1 || y == 5 || z == 1 || z == 5);
    }
    domain[i] = inside;
    initial[i] = seed;
    if (inside)
      image->SetPixel(p, 1);
  }
  auto alive = FM::NodePairContainerType::New(),
       trial = FM::NodePairContainerType::New(),
       forbidden = FM::NodePairContainerType::New();
  unsigned ai = 0, ti = 0, fi = 0;
  for (int i = 0; i < n; i++) {
    if (initial[i]) {
      alive->InsertElement(ai++, typename FM::NodePairType(coords[i], 0));
      continue;
    }
    if (!domain[i]) {
      forbidden->InsertElement(fi++, typename FM::NodePairType(coords[i], 0));
      continue;
    }
    bool touch = false;
    for (unsigned a = 0; a < D; a++)
      for (int d : {-1, 1}) {
        auto p = coords[i];
        p[a] += d;
        if (region.IsInside(p)) {
          int j = p[0] + 7 * p[1];
          if constexpr (D == 3)
            j += 49 * p[2];
          if (initial[j])
            touch = true;
        }
      }
    if (touch)
      trial->InsertElement(ti++, typename FM::NodePairType(coords[i], 1));
  }
  auto fm = FM::New();
  auto stop = Stop::New();
  stop->SetThreshold(1e9);
  fm->SetInput(image);
  fm->SetAlivePoints(alive);
  fm->SetTrialPoints(trial);
  fm->SetForbiddenPoints(forbidden);
  fm->SetStoppingCriterion(stop);
  fm->SetNumberOfWorkUnits(1);
  using Mode = itk::FastMarchingTraitsEnums::TopologyCheck;
  fm->SetTopologyCheck(mode == 0   ? Mode::Nothing
                       : mode == 1 ? Mode::Strict
                                   : Mode::NoHandles);
  fm->Update();
  auto labels = fm->GetLabelImage();
  if (comma)
    std::cout << ",";
  comma = true;
  std::cout << "{\"kind\":\"" << kind << "\",\"dimension\":" << D
            << ",\"mode\":" << mode << ",\"accepted\":[";
  bool first = true;
  for (int i = 0; i < n; i++) {
    if (labels->GetPixel(coords[i]) ==
        static_cast<unsigned char>(FM::Traits::Alive)) {
      if (!first)
        std::cout << ",";
      first = false;
      std::cout << i;
    }
  }
  std::cout << "]}";
}
int main() {
  try {
    std::cout << "{\"schema\":\"topology1501-itk-v1\",\"itk_version\":\""
              << itk::Version::GetITKVersion()
              << "\",\"spacing_mm\":[1,1,1],\"cases\":[";
    bool comma = false;
    for (int mode = 0; mode < 3; mode++) {
      run<2>("bridge", mode, comma);
      run<2>("ring", mode, comma);
      run<3>("bridge", mode, comma);
      run<3>("tunnel", mode, comma);
      run<3>("cavity", mode, comma);
    }
    std::cout << "]}\n";
    return 0;
  } catch (const std::exception &e) {
    std::cerr << e.what() << "\n";
    return 2;
  }
}
