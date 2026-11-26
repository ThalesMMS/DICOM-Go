// Own API harness, not an implementation of ITK's filter. Inputs/workunits
// and criteria must be sealed before execution by a finite external runner.
#include <itkImage.h>
#include <itkFlatStructuringElement.h>
#include <itkMaskedRankImageFilter.h>
#include <itkVersion.h>
#include <cstdint>
#include <cstring>
#include <fstream>
#include <iostream>
#include <limits>
#include <stdexcept>
#include <string>
#include <type_traits>

static uint32_t bits(float v) { uint32_t n; std::memcpy(&n,&v,4); return n; }
static float value(uint32_t n) { float v; std::memcpy(&v,&n,4); return v; }

template<unsigned D> int run(std::istream* input) {
 using Image=itk::Image<float,D>; using Mask=itk::Image<unsigned char,D>;
 using Kernel=itk::FlatStructuringElement<D>;
 using Filter=itk::MaskedRankImageFilter<Image,Mask,Image,Kernel>;
 auto filter=Filter::New();
 static_assert(std::is_same<decltype(filter->GetRank()),float>::value,"rank public type");
 static_assert(std::is_same<decltype(filter->GetFillValue()),float>::value,"fill public type");
 static_assert(std::is_same<decltype(filter->GetGenerateOutputMask()),bool>::value,"mask public type");
 std::cout << "{\"itk_version\":\"" << itk::Version::GetITKVersion() << "\",\"dimension\":" << D
 << ",\"defaults\":{\"rank\":" << filter->GetRank()
 << ",\"fill_bits\":" << bits(filter->GetFillValue())
 << ",\"mask_value\":" << unsigned(filter->GetMaskValue())
 << ",\"background_mask_value\":" << unsigned(filter->GetBackgroundMaskValue())
 << ",\"generate_output_mask\":" << (filter->GetGenerateOutputMask()?"true":"false")
 << ",\"work_units\":" << filter->GetNumberOfWorkUnits() << "}";
 if (!input) { std::cout << "}\n";return 0; }
 auto& in=*input;
 typename Image::SizeType size,cropSize;
 typename Image::IndexType zero,crop;zero.Fill(0);
 typename Image::SpacingType spacing;typename Kernel::RadiusType radius;
 uint64_t n=1,k=1;
 for(unsigned a=0;a<D;++a) { uint64_t v;in>>v;if(v==0 || v>256 || n>65536/v) throw std::runtime_error("input dimensions");size[a]=v;n*=v; }
 for(unsigned a=0;a<D;++a) { in>>spacing[a];if(!(spacing[a]>0 && spacing[a]<=1e6)) throw std::runtime_error("input spacing"); }
 for(unsigned a=0;a<D;++a) { uint64_t v;in>>v;if(v>16 || k>65536/(2*v+1)) throw std::runtime_error("kernel dimensions");radius[a]=v;k*=2*v+1; }
 for(unsigned a=0;a<D;++a) { long v;in>>v;if(v<0 || uint64_t(v)>=size[a]) throw std::runtime_error("crop origin");crop[a]=v; }
 uint64_t outputs=1;
 for(unsigned a=0;a<D;++a) { uint64_t v;in>>v;if(v==0 || v>size[a]-crop[a]) throw std::runtime_error("crop dimensions");cropSize[a]=v;outputs*=v; }
 float q;uint32_t fill;in>>q>>fill;if(!(q>=0 && q<=1)) throw std::runtime_error("rank");
 uint64_t count;in>>count;if(count!=k) throw std::runtime_error("kernel count");
 Kernel kernel;kernel.SetRadius(radius);
 bool center=false;
 for(uint64_t i=0;i<k;++i) {unsigned v;in>>v;if(v>1) throw std::runtime_error("kernel bit");kernel[i]=v!=0;if(i==k/2)center=v!=0;}
 if(!center) throw std::runtime_error("missing kernel center");
 in>>count;if(count!=n) throw std::runtime_error("sample count");
 typename Image::RegionType region;region.SetIndex(zero);region.SetSize(size);
 auto image=Image::New();image->SetRegions(region);image->SetSpacing(spacing);image->Allocate();
 auto mask=Mask::New();mask->SetRegions(region);mask->SetSpacing(spacing);mask->Allocate();
 for(uint64_t i=0;i<n;++i) {uint32_t v;in>>v;float f=value(v);if(!(f>=-std::numeric_limits<float>::max() && f<=std::numeric_limits<float>::max())) throw std::runtime_error("nonfinite input");image->GetBufferPointer()[i]=f;}
 for(uint64_t i=0;i<n;++i) {unsigned v;in>>v;if(v>1)throw std::runtime_error("mask bit");mask->GetBufferPointer()[i]=v;}
 if(!in || (in>>std::ws,!in.eof()))throw std::runtime_error("input stream/trailing data");
 filter->SetInput(image);filter->SetMaskImage(mask);filter->SetKernel(kernel);
 filter->SetRank(q);filter->SetMaskValue(1);filter->SetBackgroundMaskValue(0);
 filter->SetFillValue(value(fill));filter->SetGenerateOutputMask(true);filter->SetNumberOfWorkUnits(1);
 std::cout << ",\"settings\":{\"rank\":" << filter->GetRank() << ",\"fill_bits\":" << bits(filter->GetFillValue())
 << ",\"mask_value\":" << unsigned(filter->GetMaskValue()) << ",\"background_mask_value\":" << unsigned(filter->GetBackgroundMaskValue())
 << ",\"generate_output_mask\":true,\"work_units\":" << filter->GetNumberOfWorkUnits() << "}";
 filter->Update();
 auto* output=filter->GetOutput();auto* valid=filter->GetOutputMask();
 std::cout << ",\"value_bits\":[";
 for(uint64_t i=0;i<outputs;++i) {auto idx=crop;uint64_t j=i;for(unsigned a=0;a<D;++a){idx[a]+=j%cropSize[a];j/=cropSize[a];}if(i)std::cout<<',';std::cout<<bits(output->GetPixel(idx));}
 std::cout << "],\"valid\":[";
 for(uint64_t i=0;i<outputs;++i) {auto idx=crop;uint64_t j=i;for(unsigned a=0;a<D;++a){idx[a]+=j%cropSize[a];j/=cropSize[a];}if(i)std::cout<<',';std::cout<<(valid->GetPixel(idx)==1?"true":"false");}
 std::cout << "]}\n";return 0;
}
int main(int argc,char** argv) {
 try {
  if(argc!=2)throw std::runtime_error("one defaults2/defaults3 or workunit argument required");
  std::string arg=argv[1];if(arg=="defaults2")return run<2>(nullptr);if(arg=="defaults3")return run<3>(nullptr);
  std::ifstream in(arg);std::string magic;unsigned d;in>>magic>>d;if(!in || magic!="MR1")throw std::runtime_error("workunit header");
  if(d==2)return run<2>(&in);if(d==3)return run<3>(&in);throw std::runtime_error("dimension");
 } catch(const std::exception& e) {std::cerr << e.what() << '\n';return 2;}
}
