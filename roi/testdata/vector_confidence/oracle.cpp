// Synthetic opt-in oracle using public ITK API only. No algorithm body copied.
#include "itkImage.h"
#include "itkVector.h"
#include "itkVectorConfidenceConnectedImageFilter.h"
#include "itkVersion.h"
#include <cstdlib>
#include <iomanip>
#include <iostream>
#include <stdexcept>
#include <string>

template<unsigned int D, unsigned int C> void run(unsigned int iterations) {
  using Pixel = itk::Vector<double, C>;
  using Image = itk::Image<Pixel, D>;
  using Mask = itk::Image<unsigned char, D>;
  using Filter = itk::VectorConfidenceConnectedImageFilter<Image, Mask>;
  typename Image::SizeType size;
  size[0] = D == 2 ? 9 : 7;
  size[1] = 7;
  if constexpr(D == 3) size[2] = 5;
  auto image = Image::New();
  typename Image::RegionType region;
  region.SetSize(size);
  image->SetRegions(region);
  image->Allocate();
  const int split = D == 2 ? 4 : 3;
  const int depth = D == 2 ? 1 : 5;
  for(int z=0; z<depth; ++z) for(int y=0; y<7; ++y) for(int x=0; x<int(size[0]); ++x) {
    typename Image::IndexType index;
    index[0]=x; index[1]=y;
    if constexpr(D == 3) index[2]=z;
    Pixel value;
    value[0]=(x+y)%3-1;
    if constexpr(C == 2) value[1]=(x+2*y+z)%3-1+(x>split?8:0);
    image->SetPixel(index,value);
  }
  auto filter = Filter::New();
  filter->SetInput(image);
  filter->SetMultiplier(2.5);
  filter->SetInitialNeighborhoodRadius(1);
  filter->SetNumberOfIterations(iterations);
  filter->SetReplaceValue(1);
  typename Image::IndexType seed;
  seed[0]=2; seed[1]=3;
  if constexpr(D == 3) seed[2]=2;
  filter->AddSeed(seed);
  filter->Update();
  const auto *data=filter->GetOutput()->GetBufferPointer();
  std::cout<<std::setprecision(17)<<"{\"version\":\""<<itk::Version::GetITKVersion()<<"\",\"indices\":[";
  bool comma=false;
  for(size_t i=0;i<size[0]*size[1]*depth;++i) if(data[i]) { if(comma)std::cout<<','; std::cout<<i; comma=true; }
  std::cout<<"],\"mean\":[";
  for(unsigned int i=0;i<C;++i){if(i)std::cout<<',';std::cout<<filter->GetMean()[i];}
  std::cout<<"],\"covariance\":[";
  for(unsigned int i=0;i<C;++i)for(unsigned int j=0;j<C;++j){if(i||j)std::cout<<',';std::cout<<filter->GetCovariance()(i,j);}
  std::cout<<"]}\n";
}

int main(int argc,char **argv) {
  try {
    if(argc!=4)throw std::runtime_error("expected dimension channels iterations");
    const std::string d=argv[1],c=argv[2],i=argv[3];
    if((d!="2"&&d!="3")||(c!="1"&&c!="2")||(i!="0"&&i!="2"))throw std::runtime_error("outside sealed synthetic profile");
    const unsigned int iterations=i=="0"?0:2;
    if(d=="2"&&c=="1")run<2,1>(iterations);
    else if(d=="2")run<2,2>(iterations);
    else if(c=="1")run<3,1>(iterations);
    else run<3,2>(iterations);
    return 0;
  }catch(const std::exception &e){std::cerr<<e.what()<<'\n';return 1;}
}
