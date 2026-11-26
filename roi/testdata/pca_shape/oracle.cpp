// Project-authored public SDK probe: no ITK implementation is copied.
#include <itkAffineTransform.h>
#include <itkImage.h>
#include <itkPCAShapeSignedDistanceFunction.h>
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
#include <vector>

static std::uint32_t u32(std::istream &in) {
  std::array<unsigned char,4> b{};
  if (!in.read(reinterpret_cast<char*>(b.data()),4))
    throw std::runtime_error("short uint32");
  return std::uint32_t(b[0])|(std::uint32_t(b[1])<<8)|
         (std::uint32_t(b[2])<<16)|(std::uint32_t(b[3])<<24);
}
static double f64(std::istream &in) {
  static_assert(sizeof(double)==8 && std::numeric_limits<double>::is_iec559,
                "IEEE754 binary64 required");
  const auto low=u32(in);
  const auto high=u32(in);
  const std::uint64_t bits = std::uint64_t(low)|(std::uint64_t(high)<<32);
  double v; std::memcpy(&v,&bits,8);
  if (!std::isfinite(v)||std::abs(v)>1e9) throw std::runtime_error("finite input required");
  return v;
}
int main(int argc,char**argv) {
  try {
    if (argc!=2) throw std::runtime_error("input binary path required");
    std::ifstream in(argv[1],std::ios::binary);
    if (u32(in)!=0x31414350) throw std::runtime_error("PCA1 envelope required");
    using Image=itk::Image<double,3>;
    using Shape=itk::PCAShapeSignedDistanceFunction<double,3,Image>;
    Image::SizeType size;std::size_t n=1;
    for (int a=0;a<3;a++) {
      size[a]=u32(in);
      if (!size[a]||size[a]>(1u<<20)/n) throw std::runtime_error("voxel bound");
      n*=size[a];
    }
    const auto k=u32(in),count=u32(in);
    if (k>16||count>(1u<<20)) throw std::runtime_error("component/point bound");
    Image::SpacingType spacing;Image::PointType origin;Image::DirectionType direction;
    for (int a=0;a<3;a++) spacing[a]=f64(in);
    for (int a=0;a<3;a++) origin[a]=f64(in);
    for (int a=0;a<3;a++) for (int b=0;b<3;b++) direction[a][b]=f64(in);
    using Transform=itk::AffineTransform<double,3>;
    auto transform=Transform::New();Transform::MatrixType rotation;Transform::OutputVectorType translation;
    for (int a=0;a<3;a++) for (int b=0;b<3;b++) rotation[a][b]=f64(in);
    for (int a=0;a<3;a++) translation[a]=f64(in);
    transform->SetMatrix(rotation);transform->SetTranslation(translation);
    Shape::ParametersType sigma(k),coefficients(k);
    for (unsigned a=0;a<k;a++) sigma[a]=f64(in);
    for (unsigned a=0;a<k;a++) coefficients[a]=f64(in);
    auto image=[&]() {
      auto out=Image::New();Image::RegionType region;Image::IndexType start;start.Fill(0);
      region.SetIndex(start);region.SetSize(size);out->SetRegions(region);
      out->SetSpacing(spacing);out->SetOrigin(origin);out->SetDirection(direction);out->Allocate();
      for (std::size_t i=0;i<n;i++) out->GetBufferPointer()[i]=f64(in);
      return out;
    };
    auto mean=image();Shape::ImagePointerVector components;
    for (unsigned a=0;a<k;a++) components.push_back(image());
    auto shape=Shape::New();shape->SetNumberOfPrincipalComponents(k);
    shape->SetMeanImage(mean);shape->SetPrincipalComponentImages(components);
    shape->SetPrincipalComponentStandardDeviations(sigma);shape->SetTransform(transform);
    Shape::ParametersType parameters(k+transform->GetNumberOfParameters());
    for (unsigned a=0;a<k;a++) parameters[a]=coefficients[a];
    for (unsigned a=0;a<transform->GetNumberOfParameters();a++) parameters[k+a]=transform->GetParameters()[a];
    shape->SetParameters(parameters);shape->Initialize();
    std::vector<double> output;output.reserve(count);
    for (unsigned i=0;i<count;i++) {
      Shape::PointType point;for (int a=0;a<3;a++) point[a]=f64(in);
      output.push_back(shape->Evaluate(point));
    }
    if (in.peek()!=std::char_traits<char>::eof()) throw std::runtime_error("trailing input");
    std::cout<<std::setprecision(17)<<"{\"engine\":\"ITK\",\"version\":\""<<ITK_VERSION<<"\",\"values\":[";
    for (std::size_t i=0;i<output.size();i++) {if (i) std::cout<<",";std::cout<<output[i];}
    std::cout<<"]}\n";return 0;
  } catch (const std::exception &e) {std::cerr<<e.what()<<"\n";return 1;}
}
