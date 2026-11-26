// Authored public-API probe; no upstream solver implementation is copied.
// ITK5.4.5 public tag f51594ad88194a72cdc06a314dafff709fcdc2e9.
// G2 defaults are global per dimension; future build/Update requires root GO.
#include <itkImage.h>
#include <itkImageRegionConstIterator.h>
#include <itkMultiThreaderBase.h>
#include <itkShapeDetectionLevelSetImageFilter.h>
#include <itkVersion.h>
#include <array>
#include <cmath>
#include <cstdint>
#include <cstring>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <limits>
#include <stdexcept>
#include <string>

static_assert(sizeof(float) == 4 && std::numeric_limits<float>::is_iec559,
              "oracle input requires IEEE754 binary32");

static double number(const char *text) {
  std::size_t consumed = 0;
  const auto value = std::stod(text, &consumed);
  if (consumed != std::strlen(text) || !std::isfinite(value))
    throw std::runtime_error("invalid finite numeric argument");
  return value;
}

static unsigned integer(const char *text, unsigned maximum) {
  const auto value = number(text);
  if (value < 0 || value > maximum || std::floor(value) != value)
    throw std::runtime_error("invalid bounded integer argument");
  return static_cast<unsigned>(value);
}

static float read_float(std::istream &in) {
  std::array<unsigned char, 4> bytes{};
  if (!in.read(reinterpret_cast<char *>(bytes.data()), 4))
    throw std::runtime_error("short float input");
  const std::uint32_t bits = static_cast<std::uint32_t>(bytes[0]) |
      (static_cast<std::uint32_t>(bytes[1]) << 8) |
      (static_cast<std::uint32_t>(bytes[2]) << 16) |
      (static_cast<std::uint32_t>(bytes[3]) << 24);
  float value{};
  std::memcpy(&value, &bits, 4);
  if (!std::isfinite(value)) throw std::runtime_error("nonfinite float input");
  return value;
}

static void write_float(std::ostream &out, float value) {
  if (!std::isfinite(value)) throw std::runtime_error("nonfinite public oracle output");
  std::uint32_t bits{};
  std::memcpy(&bits, &value, 4);
  const std::array<unsigned char, 4> bytes{
      static_cast<unsigned char>(bits), static_cast<unsigned char>(bits >> 8),
      static_cast<unsigned char>(bits >> 16), static_cast<unsigned char>(bits >> 24)};
  if (!out.write(reinterpret_cast<const char *>(bytes.data()), 4))
    throw std::runtime_error("oracle output write error");
}

template<unsigned D>
int execute(int argc, char **argv) {
  using Image = itk::Image<float, D>;
  using Filter = itk::ShapeDetectionLevelSetImageFilter<Image, Image, float>;
  itk::MultiThreaderBase::SetGlobalDefaultNumberOfThreads(1);
  auto filter = Filter::New();
  std::cout << std::setprecision(17);
  const auto defaults = [&]() {
    std::cout << "{\"itk_version\":\"" << itk::Version::GetITKVersion()
      << "\",\"dimension\":" << D
      << ",\"number_of_layers\":" << filter->GetNumberOfLayers()
      << ",\"interpolate_surface_location\":" << (filter->GetInterpolateSurfaceLocation() ? "true" : "false")
      << ",\"use_image_spacing\":" << (filter->GetUseImageSpacing() ? "true" : "false")
      << ",\"use_minimal_curvature\":" << (filter->GetUseMinimalCurvature() ? "true" : "false")
      << ",\"reverse_expansion_direction\":" << (filter->GetReverseExpansionDirection() ? "true" : "false")
      << ",\"maximum_curvature_time_step\":" << filter->GetMaximumCurvatureTimeStep()
      << ",\"maximum_propagation_time_step\":" << filter->GetMaximumPropagationTimeStep()
      << ",\"iso_surface_value\":" << filter->GetIsoSurfaceValue()
      << "}";
  };
  const auto check_declared_defaults = [&]() {
    if (filter->GetNumberOfLayers() != D || filter->GetInterpolateSurfaceLocation() || !filter->GetUseImageSpacing() ||
        filter->GetUseMinimalCurvature() || filter->GetReverseExpansionDirection() || filter->GetIsoSurfaceValue() != 0 ||
        std::string(itk::Version::GetITKVersion()) != "5.4.5" ||
        filter->GetMaximumCurvatureTimeStep() != 1.0 / (2 * D) ||
        filter->GetMaximumPropagationTimeStep() != 1.0 / (2 * D))
      throw std::runtime_error("public defaults disagree with sealed declaration; stop for review");
  };
  if (std::string(argv[1]) == "defaults") {
    if (argc != 3) throw std::runtime_error("defaults takes only dimension");
    defaults();
    std::cout << '\n';
    check_declared_defaults();
    return 0;
  }
  if (argc != 21 || std::string(argv[1]) != "run")
    throw std::runtime_error("run: dimension nx ny nz sx sy sz ox oy oz P C iterations rms interpolate layers feature-f32 initial-f32 output-f32");
  std::array<unsigned, 3> dims{integer(argv[3], 4096), integer(argv[4], 4096), integer(argv[5], 4096)};
  if (dims[0] < 3 || dims[1] < 3 || (D == 2 ? dims[2] != 1 : dims[2] < 3))
    throw std::runtime_error("invalid oracle dimensions before allocation");
  std::uint64_t count = 1;
  for (const auto n : dims) {
    if (n == 0 || count > (16u << 20) / n) throw std::runtime_error("oracle voxel ceiling before allocation");
    count *= n;
  }
  typename Image::SpacingType spacing;
  typename Image::PointType origin;
  typename Image::SizeType size;
  for (unsigned a = 0; a < D; ++a) {
    spacing[a] = number(argv[6 + a]);
    origin[a] = number(argv[9 + a]);
    size[a] = dims[a];
    if (!(spacing[a] > 0) || spacing[a] > 16)
      throw std::runtime_error("oracle spacing outside sealed authoring range");
  }
  const auto propagation = number(argv[12]);
  const auto curvature = number(argv[13]);
  const auto iterations = integer(argv[14], 300);
  const auto rms = number(argv[15]);
  const auto interpolate = integer(argv[16], 1);
  const auto layers = integer(argv[17], 3);
  if (std::abs(propagation) > 1 || curvature < 0 || curvature > .2 ||
      iterations == 0 || rms != .0005 || layers < 2)
    throw std::runtime_error("oracle settings outside frozen matrix");
  auto read_image = [&](const char *path, bool potential) {
    std::ifstream input(path, std::ios::binary | std::ios::ate);
    if (!input || input.tellg() != static_cast<std::streamoff>(count * 4))
      throw std::runtime_error("input byte count mismatch before image allocation");
    input.seekg(0);
    auto image = Image::New();
    typename Image::IndexType start;
    start.Fill(0);
    typename Image::RegionType region;
    region.SetIndex(start);
    region.SetSize(size);
    image->SetRegions(region);
    image->SetSpacing(spacing);
    image->SetOrigin(origin);
    typename Image::DirectionType direction;
    direction.SetIdentity();
    image->SetDirection(direction);
    image->Allocate();
    auto *buffer = image->GetBufferPointer();
    for (std::uint64_t i = 0; i < count; ++i) {
      const auto value = read_float(input);
      if (potential && !(value >= 0 && value <= 1))
        throw std::runtime_error("potential outside [0,1]");
      buffer[i] = value;
    }
    return image;
  };
  std::cout << "{\"defaults_before_setters\":";
  defaults();
  check_declared_defaults();
  auto feature = read_image(argv[18], true);
  auto initial = read_image(argv[19], false);
  filter->SetInput(initial);
  filter->SetFeatureImage(feature);
  filter->SetNumberOfWorkUnits(1);
  filter->SetUseImageSpacing(true);
  filter->SetUseMinimalCurvature(false);
  filter->SetReverseExpansionDirection(false);
  filter->SetIsoSurfaceValue(0);
  filter->SetInterpolateSurfaceLocation(interpolate != 0);
  filter->SetNumberOfLayers(layers);
  filter->SetPropagationScaling(propagation);
  filter->SetCurvatureScaling(curvature);
  filter->SetAdvectionScaling(0);
  filter->SetNumberOfIterations(iterations);
  filter->SetMaximumRMSError(rms);
  // Static time-step constraints are intentionally never set/tuned by this harness.
  filter->Update();
  std::ofstream output(argv[20], std::ios::binary | std::ios::trunc);
  if (!output) throw std::runtime_error("cannot open output");
  const auto *buffer = filter->GetOutput()->GetBufferPointer();
  std::uint64_t foreground = 0;
  for (std::uint64_t i = 0; i < count; ++i) {
    write_float(output, buffer[i]);
    foreground += buffer[i] < 0;
  }
  output.close();
  if (!output) throw std::runtime_error("cannot close oracle output");
  std::cout << ",\"effective\":{\"propagation\":" << filter->GetPropagationScaling()
    << ",\"curvature\":" << filter->GetCurvatureScaling()
    << ",\"advection\":" << filter->GetAdvectionScaling()
    << ",\"iso_surface_value\":" << filter->GetIsoSurfaceValue()
    << ",\"number_of_layers\":" << filter->GetNumberOfLayers()
    << ",\"interpolate_surface_location\":" << (filter->GetInterpolateSurfaceLocation() ? "true" : "false")
    << ",\"use_image_spacing\":" << (filter->GetUseImageSpacing() ? "true" : "false")
    << ",\"use_minimal_curvature\":" << (filter->GetUseMinimalCurvature() ? "true" : "false")
    << ",\"reverse_expansion_direction\":" << (filter->GetReverseExpansionDirection() ? "true" : "false")
    << ",\"requested_iterations\":" << iterations
    << ",\"maximum_rms\":" << rms
    << ",\"elapsed_iterations\":" << filter->GetElapsedIterations()
    << ",\"rms\":" << filter->GetRMSChange()
    << ",\"foreground_voxels\":" << foreground << "}}\n";
  return 0;
}

int main(int argc, char **argv) {
  try {
    if (argc < 3) throw std::runtime_error("defaults|run, dimension2|3 required");
    const auto dimension = integer(argv[2], 3);
    if (dimension == 2) return execute<2>(argc, argv);
    if (dimension == 3) return execute<3>(argc, argv);
    throw std::runtime_error("unsupported dimension");
  } catch (const std::exception &e) {
    std::cerr << e.what() << '\n';
    return 2;
  }
}
