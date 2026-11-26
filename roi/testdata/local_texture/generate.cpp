// Authored synthetic-window driver of public ITK APIs. No filter code copied.
#include <itkImage.h>
#include <itkScalarImageToCooccurrenceMatrixFilter.h>
#include <itkHistogramToTextureFeaturesFilter.h>
#include <itkVersion.h>
#include <windows.h>
#include <psapi.h>
#include <array>
#include <chrono>
#include <cmath>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <vector>

int main(int argc,char **argv) {
  if(argc!=2) return 2;
  using Image=itk::Image<unsigned short,3>;
  using Co=itk::Statistics::ScalarImageToCooccurrenceMatrixFilter<Image>;
  using Features=itk::Statistics::HistogramToTextureFeaturesFilter<Co::HistogramType>;
  const std::array<std::array<int,3>,13> dirs{{{1,0,0},{0,1,0},{0,0,1},{1,1,0},{1,-1,0},{1,0,1},{1,0,-1},{0,1,1},{0,1,-1},{1,1,1},{1,1,-1},{1,-1,1},{1,-1,-1}}};
  const auto start=std::chrono::steady_clock::now();
  std::ofstream out(argv[1]);out<<std::setprecision(17);
  out<<"{\"itk\":\""<<itk::Version::GetITKVersion()<<"\",\"cases\":[";
  bool comma=false;
  for(int pattern=0;pattern<4;pattern++)for(int agg=0;agg<2;agg++) {
    if(comma)out<<",";comma=true;
    const char *names[]={"constant","periodic","heterogeneous","partial-mask"};
    std::vector<unsigned short> values(343),mask(343,1);
    for(int z=0;z<7;z++)for(int y=0;y<7;y++)for(int x=0;x<7;x++) {
      int i=(z*7+y)*7+x;
      values[i]=pattern==0?0:pattern==1?7*((x+y+z)%2):(3*x+2*y+z)%8;
      if(pattern==3)mask[i]=(i%5)!=0;
    }
    out<<"{\"name\":\""<<names[pattern]<<"\",\"aggregation\":"<<agg<<",\"values\":[";
    for(int i=0;i<343;i++){if(i)out<<",";out<<values[i];}
    out<<"],\"mask\":[";for(int i=0;i<343;i++){if(i)out<<",";out<<(mask[i]?"true":"false");}out<<"],\"features\":[";
    for(int z=0;z<7;z++)for(int y=0;y<7;y++)for(int x=0;x<7;x++) {
      int index=(z*7+y)*7+x;if(index)out<<",";
      std::array<double,4> sum{};int populated=0;
      if(mask[index]) {
        Image::SizeType size;Image::IndexType zero;zero.Fill(0);
        int lo[3]={std::max(0,x-1),std::max(0,y-1),std::max(0,z-1)};
        int hi[3]={std::min(7,x+2),std::min(7,y+2),std::min(7,z+2)};
        for(int a=0;a<3;a++)size[a]=hi[a]-lo[a];
        auto image=Image::New(),selection=Image::New();Image::RegionType region;region.SetIndex(zero);region.SetSize(size);
        image->SetRegions(region);selection->SetRegions(region);image->Allocate();selection->Allocate();
        for(int k=lo[2];k<hi[2];k++)for(int j=lo[1];j<hi[1];j++)for(int i=lo[0];i<hi[0];i++) {
          Image::IndexType p;p[0]=i-lo[0];p[1]=j-lo[1];p[2]=k-lo[2];
          image->SetPixel(p,values[(k*7+j)*7+i]);selection->SetPixel(p,mask[(k*7+j)*7+i]);
        }
        for(int d=0;d<(agg?1:13);d++) {
          auto co=Co::New();co->SetInput(image);co->SetMaskImage(selection);co->SetInsidePixelValue(1);
          co->SetNumberOfBinsPerAxis(8);co->SetPixelValueMinMax(0,7);
          if(agg) { auto offsets=Co::OffsetVector::New();for(unsigned int k=0;k<13;k++){Image::OffsetType o;for(int a=0;a<3;a++)o[a]=dirs[k][a];offsets->InsertElement(k,o);}co->SetOffsets(offsets); }
          else { Image::OffsetType o;for(int a=0;a<3;a++)o[a]=dirs[d][a];co->SetOffset(o); }
          co->Update();if(co->GetOutput()->GetTotalFrequency()==0)continue;
          auto f=Features::New();f->SetInput(co->GetOutput());f->Update();populated++;
          sum[0]+=f->GetInertia();sum[1]+=f->GetEntropy();sum[2]+=f->GetEnergy();sum[3]+=f->GetInverseDifferenceMoment();
        }
      }
      out<<"[";for(int f=0;f<4;f++){if(f)out<<",";if(populated)out<<sum[f]/populated;else out<<"null";}out<<"]";
    }
    out<<"]}";
  }
  PROCESS_MEMORY_COUNTERS memory{};GetProcessMemoryInfo(GetCurrentProcess(),&memory,sizeof(memory));
  const auto seconds=std::chrono::duration<double>(std::chrono::steady_clock::now()-start).count();
  out<<"],\"seconds\":"<<seconds<<",\"peak_rss_bytes\":"<<memory.PeakWorkingSetSize<<"}\n";
  std::cerr<<"ITK "<<itk::Version::GetITKVersion()<<" "<<seconds<<" seconds; peak RSS "<<memory.PeakWorkingSetSize<<"\n";
  return out?0:1;
}
