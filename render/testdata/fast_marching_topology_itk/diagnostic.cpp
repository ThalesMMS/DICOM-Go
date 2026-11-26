// Read-only instrumentation of public ITK APIs and a protected state accessor.
// No SDK mutation, algorithm override, or production implementation changes.
#include <iostream>
#include <itkFastMarchingImageFilterBase.h>
#include <itkFastMarchingThresholdStoppingCriterion.h>
#include <itkImage.h>
#include <itkVersion.h>
#include <string>

template <unsigned D>
class Observer final
    : public itk::FastMarchingImageFilterBase<itk::Image<double, D>,
                                              itk::Image<double, D>> {
public:
  using Self = Observer;
  using Superclass = itk::FastMarchingImageFilterBase<itk::Image<double, D>,
                                                      itk::Image<double, D>>;
  using Pointer = itk::SmartPointer<Self>;
  using NodeType = typename Superclass::NodeType;
  itkNewMacro(Self);
  bool HasComponentImage() const {
    return static_cast<bool>(this->m_ConnectedComponentImage);
  }
  unsigned ComponentAt(const NodeType &index) const {
    return this->m_ConnectedComponentImage->GetPixel(index);
  }

protected:
  Observer() = default;
  ~Observer() override = default;
};

template <unsigned D> void run(int length, int mode, bool &comma) {
  using Image = itk::Image<double, D>;
  using FM = Observer<D>;
  using Stop = itk::FastMarchingThresholdStoppingCriterion<Image, Image>;
  typename Image::SizeType size;
  size.Fill(7);
  typename Image::IndexType origin;
  origin.Fill(0);
  typename Image::RegionType region;
  region.SetIndex(origin);
  region.SetSize(size);
  auto speed = Image::New();
  speed->SetRegions(region);
  speed->Allocate();
  speed->FillBuffer(0);
  typename Image::SpacingType spacing;
  spacing.Fill(1);
  speed->SetSpacing(spacing);
  const int first = length == 3 ? 2 : 1, last = length == 3 ? 4 : 5;
  auto alive = FM::NodePairContainerType::New(),
       trial = FM::NodePairContainerType::New(),
       forbidden = FM::NodePairContainerType::New();
  unsigned ai = 0, ti = 0, fi = 0;
  const int count = D == 2 ? 49 : 343;
  for (int i = 0; i < count; ++i) {
    typename Image::IndexType p;
    p.Fill(0);
    p[0] = i % 7;
    p[1] = i / 7 % 7;
    if constexpr (D == 3)
      p[2] = i / 49;
    const bool onLine = p[1] == 3 && (D == 2 || p[2] == 3);
    const bool inside = onLine && p[0] >= first && p[0] <= last;
    const bool seed = inside && (p[0] == first || p[0] == last);
    if (inside)
      speed->SetPixel(p, 1);
    if (seed)
      alive->InsertElement(ai++, typename FM::NodePairType(p, 0));
    else if (!inside)
      forbidden->InsertElement(fi++, typename FM::NodePairType(p, 0));
    else if (p[0] == first + 1 || p[0] == last - 1)
      trial->InsertElement(ti++, typename FM::NodePairType(p, 1));
  }
  auto fm = FM::New();
  auto stop = Stop::New();
  stop->SetThreshold(1e9);
  fm->SetInput(speed);
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
  if (comma)
    std::cout << ",";
  comma = true;
  std::cout << "{\"dimension\":" << D << ",\"length\":" << length
            << ",\"mode\":" << mode << ",\"has_component_image\":"
            << (fm->HasComponentImage() ? "true" : "false") << ",\"voxels\":[";
  for (int x = first; x <= last; ++x) {
    typename Image::IndexType p;
    p.Fill(3);
    p[0] = x;
    if (x != first)
      std::cout << ",";
    const auto label = fm->GetLabelImage()->GetPixel(p);
    std::cout << "{\"x\":" << x
              << ",\"seed\":" << ((x == first || x == last) ? "true" : "false")
              << ",\"alive\":"
              << (label == static_cast<unsigned char>(FM::Traits::Alive)
                      ? "true"
                      : "false")
              << ",\"label\":" << static_cast<unsigned>(label)
              << ",\"component_id\":";
    if (fm->HasComponentImage())
      std::cout << fm->ComponentAt(p);
    else
      std::cout << "null";
    std::cout << "}";
  }
  std::cout << "]}";
}
int main() {
  try {
    std::cout << "{\"schema\":\"topology1501-diagnostic-v1\",\"itk_version\":\""
              << itk::Version::GetITKVersion()
              << "\",\"spacing_mm\":[1,1,1],\"cases\":[";
    bool comma = false;
    for (int mode = 0; mode < 3; ++mode)
      for (int length : {3, 5}) {
        run<2>(length, mode, comma);
        run<3>(length, mode, comma);
      }
    std::cout << "]}\n";
    return 0;
  } catch (const std::exception &e) {
    std::cerr << e.what() << "\n";
    return 2;
  }
}
