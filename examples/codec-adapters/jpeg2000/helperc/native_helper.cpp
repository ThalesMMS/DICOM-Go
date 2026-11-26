// Project-authored IPC wrapper around public OpenJPEG/OpenJPH APIs.
// No codec implementation is copied or embedded. Link qualified BSD runtimes.
#include <openjpeg.h>
#include <openjph/ojph_codestream.h>
#include <openjph/ojph_file.h>
#include <openjph/ojph_mem.h>
#include <openjph/ojph_message.h>
#include <openjph/ojph_params.h>
#include <openjph/ojph_version.h>
#include "memory_limit.h"
#include <algorithm>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <exception>
#include <memory>
#include <stdexcept>
#include <string>
#include <vector>
#ifdef _WIN32
#include <fcntl.h>
#include <io.h>
#endif

constexpr uint32_t max_bytes=64u<<20;
uint32_t u32(const unsigned char*p){return uint32_t(p[0])|(uint32_t(p[1])<<8)|(uint32_t(p[2])<<16)|(uint32_t(p[3])<<24);}
uint64_t u64(const unsigned char*p){return u32(p)|(uint64_t(u32(p+4))<<32);}
void put32(unsigned char*p,uint32_t v){for(int i=0;i<4;i++)p[i]=static_cast<unsigned char>(v>>(i*8));}
void put64(unsigned char*p,uint64_t v){put32(p,uint32_t(v));put32(p+4,uint32_t(v>>32));}
struct Request{uint64_t id;uint32_t opcode,rows,cols,samples,bits,stored;std::vector<unsigned char> payload;};
struct failure{uint32_t status;};
bool exact_read(void*p,size_t n){return n==0||std::fread(p,1,n,stdin)==n;}
bool exact_write(const void*p,size_t n){return n==0||std::fwrite(p,1,n,stdout)==n;}
bool reply(const Request&r,uint32_t status,uint32_t samples,const std::vector<unsigned char>&b){
 unsigned char h[36]={};std::memcpy(h,"J2KH",4);put32(h+4,1);put64(h+8,r.id);put32(h+16,status);put32(h+20,r.rows);put32(h+24,r.cols);put32(h+28,samples);put32(h+32,uint32_t(b.size()));
 return exact_write(h,sizeof(h))&&exact_write(b.data(),b.size())&&std::fflush(stdout)==0;
}
void sample(std::vector<unsigned char>&out,size_t i,int64_t value,const Request&r){
 int64_t maximum=(int64_t(1)<<r.stored)-1;
 value=std::max<int64_t>(0,std::min(value,maximum));
 size_t at=i*(r.bits/8);out[at]=static_cast<unsigned char>(value);if(r.bits==16)out[at+1]=static_cast<unsigned char>(value>>8);
}

struct Memory{const unsigned char*p;size_t n,at=0;};
OPJ_SIZE_T memory_read(void*p,OPJ_SIZE_T n,void*user){auto&m=*static_cast<Memory*>(user);size_t amount=std::min<size_t>(n,m.n-m.at);if(!amount)return static_cast<OPJ_SIZE_T>(-1);std::memcpy(p,m.p+m.at,amount);m.at+=amount;return amount;}
OPJ_OFF_T memory_skip(OPJ_OFF_T n,void*user){auto&m=*static_cast<Memory*>(user);if(n<0||uint64_t(n)>m.n-m.at)return -1;m.at+=size_t(n);return n;}
OPJ_BOOL memory_seek(OPJ_OFF_T n,void*user){auto&m=*static_cast<Memory*>(user);if(n<0||uint64_t(n)>m.n)return OPJ_FALSE;m.at=size_t(n);return OPJ_TRUE;}
void ignore_message(const char*,void*){}
std::vector<unsigned char> openjpeg(const Request&r){
 using Codec=std::unique_ptr<opj_codec_t,decltype(&opj_destroy_codec)>;
 using Stream=std::unique_ptr<opj_stream_t,decltype(&opj_stream_destroy)>;
 using Image=std::unique_ptr<opj_image_t,decltype(&opj_image_destroy)>;
 Codec codec(opj_create_decompress(OPJ_CODEC_J2K),opj_destroy_codec);if(!codec)throw failure{4};
 opj_set_error_handler(codec.get(),ignore_message,nullptr);opj_set_warning_handler(codec.get(),ignore_message,nullptr);opj_set_info_handler(codec.get(),ignore_message,nullptr);
 opj_dparameters_t params;opj_set_default_decoder_parameters(&params);
 if(!opj_setup_decoder(codec.get(),&params)||!opj_codec_set_threads(codec.get(),1))throw failure{1};
 Memory memory{r.payload.data(),r.payload.size()};Stream stream(opj_stream_create(65536,OPJ_TRUE),opj_stream_destroy);if(!stream)throw failure{4};
 opj_stream_set_user_data(stream.get(),&memory,nullptr);opj_stream_set_user_data_length(stream.get(),memory.n);opj_stream_set_read_function(stream.get(),memory_read);opj_stream_set_skip_function(stream.get(),memory_skip);opj_stream_set_seek_function(stream.get(),memory_seek);
 opj_image_t* raw=nullptr;bool read=opj_read_header(stream.get(),codec.get(),&raw)!=0;Image image(raw,opj_image_destroy);if(!read||!image)throw failure{1};
 if(image->numcomps!=r.samples||image->x1-image->x0!=r.cols||image->y1-image->y0!=r.rows)throw failure{3};
 for(uint32_t c=0;c<r.samples;c++){const auto&comp=image->comps[c];if(comp.w!=r.cols||comp.h!=r.rows||comp.dx!=1||comp.dy!=1||comp.sgnd||comp.prec!=r.stored)throw failure{2};}
 if(!opj_decode(codec.get(),stream.get(),image.get())||!opj_end_decompress(codec.get(),stream.get()))throw failure{1};
 std::vector<unsigned char> out(size_t(r.rows)*r.cols*r.samples*(r.bits/8));
 for(uint32_t c=0;c<r.samples;c++){const auto&comp=image->comps[c];if(!comp.data)throw failure{1};for(size_t i=0;i<size_t(r.rows)*r.cols;i++)sample(out,i*r.samples+c,comp.data[i],r);}
 return out;
}

std::vector<unsigned char> openjph(const Request&r){
 ojph::mem_infile input;input.open(r.payload.data(),r.payload.size());ojph::codestream stream;stream.read_headers(&input);
 auto size=stream.access_siz();if(size.get_num_components()!=r.samples)throw failure{3};
 for(uint32_t c=0;c<r.samples;c++)if(size.get_recon_width(c)!=r.cols||size.get_recon_height(c)!=r.rows||size.get_bit_depth(c)!=r.stored||size.is_signed(c))throw failure{2};
 stream.set_planar(false);stream.create();
 std::vector<unsigned char> out(size_t(r.rows)*r.cols*r.samples*(r.bits/8));
 std::vector<uint32_t> rows(r.samples,0);
 for(size_t i=0;i<size_t(r.rows)*r.samples;i++){
  ojph::ui32 c=0;ojph::line_buf*line=stream.pull(c);
  if(!line||c>=r.samples||rows[c]>=r.rows||line->size<r.cols)throw failure{1};
  if((line->flags&(ojph::line_buf::LFT_SIZE_MASK|ojph::line_buf::LFT_INTEGER))!=(ojph::line_buf::LFT_32BIT|ojph::line_buf::LFT_INTEGER))throw failure{2};
  size_t row=rows[c]++;for(uint32_t x=0;x<r.cols;x++)sample(out,(row*r.cols+x)*r.samples+c,line->i32[x],r);
 }
 stream.close();return out;
}

int main(int argc,char**argv){
#ifdef _WIN32
 _setmode(_fileno(stdin),_O_BINARY);_setmode(_fileno(stdout),_O_BINARY);
#endif
 if (argc != 2 && argc != 3) return 2;
 if (argc == 3 && !apply_memory_limit(argv[2])) return 5;
 bool jph=std::strcmp(argv[1],"openjph")==0;if(!jph&&std::strcmp(argv[1],"openjpeg")!=0)return 2;
 // OpenJPH defaults info/warnings to stdout. Keep the binary IPC channel
 // exclusive even for a corrupt stream which emits diagnostics.
 ojph::set_info_stream(stderr);ojph::set_warning_stream(stderr);ojph::set_error_stream(stderr);
 std::string version=jph?std::to_string(OPENJPH_VERSION_MAJOR)+"."+std::to_string(OPENJPH_VERSION_MINOR)+"."+std::to_string(OPENJPH_VERSION_PATCH):opj_version();
 while(true){
  unsigned char h[44];size_t first=std::fread(h,1,sizeof(h),stdin);if(first==0&&std::feof(stdin))return 0;if(first!=sizeof(h))return 3;
  if(std::memcmp(h,"J2KH",4)||u32(h+4)!=1)return 3;
  Request r{u64(h+12),u32(h+8),u32(h+20),u32(h+24),u32(h+28),u32(h+32),u32(h+36),{}};uint32_t count=u32(h+40);
  if(count>max_bytes)return 4;
  try{
   r.payload.resize(count);if(!exact_read(r.payload.data(),count))return 3;
   if(r.opcode==1){if(count||r.rows||r.cols||r.samples||r.bits||r.stored)return 3;std::vector<unsigned char>b(version.begin(),version.end());if(!reply(r,0,jph?2:1,b))return 3;continue;}
   if(r.opcode!=2)return 3;
   // Validate factors before multiplying untrusted protocol fields. DICOM
   // Rows/Columns are unsigned 16-bit, independent of the caller's checks.
   if(!count||!r.rows||!r.cols||r.rows>65535||r.cols>65535||(r.samples!=1&&r.samples!=3)||(r.bits!=8&&r.bits!=16)||!r.stored||r.stored>r.bits)throw failure{4};
   uint64_t n=uint64_t(r.rows)*r.cols*r.samples*(r.bits/8);
   if(n>max_bytes)throw failure{4};
   auto out=jph?openjph(r):openjpeg(r);if(!reply(r,0,r.samples,out))return 3;
  }catch(const failure&e){if(!reply(r,e.status,r.samples,{}))return 3;}
  catch(const std::bad_alloc&){if(!reply(r,4,r.samples,{}))return 3;}
  catch(const std::exception&){if(!reply(r,1,r.samples,{}))return 3;}
 }
}
