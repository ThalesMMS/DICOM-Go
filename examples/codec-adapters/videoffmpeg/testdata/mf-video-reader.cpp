#include <windows.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfreadwrite.h>
#include <wrl/client.h>
#include <cstdio>
using Microsoft::WRL::ComPtr;

// Independent Microsoft Media Foundation decoder for the optional Windows
// interoperability test. Reads a local MP4 and emits NV12 plus frame timing.
int wmain(int argc, wchar_t** argv) {
    if (argc != 3) return 2;
    HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    if (FAILED(hr)) return 3;
    hr = MFStartup(MF_VERSION);
    if (FAILED(hr)) return 4;
    ComPtr<IMFAttributes> attributes;
    MFCreateAttributes(&attributes, 2);
    attributes->SetUINT32(MF_READWRITE_ENABLE_HARDWARE_TRANSFORMS, FALSE);
    attributes->SetUINT32(MF_SOURCE_READER_ENABLE_VIDEO_PROCESSING, FALSE);
    ComPtr<IMFSourceReader> reader;
    hr = MFCreateSourceReaderFromURL(argv[1], attributes.Get(), &reader);
    if (FAILED(hr)) return 5;
    ComPtr<IMFMediaType> type;
    MFCreateMediaType(&type);
    type->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
    type->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_NV12);
    hr = reader->SetCurrentMediaType(MF_SOURCE_READER_FIRST_VIDEO_STREAM, nullptr, type.Get());
    if (FAILED(hr)) return 6;
    type.Reset();
    reader->GetCurrentMediaType(MF_SOURCE_READER_FIRST_VIDEO_STREAM, &type);
    UINT32 width = 0, height = 0, num = 0, den = 0;
    MFGetAttributeSize(type.Get(), MF_MT_FRAME_SIZE, &width, &height);
    MFGetAttributeRatio(type.Get(), MF_MT_FRAME_RATE, &num, &den);
    std::printf("width=%u height=%u fps=%u/%u\n", width, height, num, den);
    FILE* output = _wfopen(argv[2], L"wb");
    if (!output) return 7;
    unsigned frame = 0;
    for (;;) {
        DWORD stream, flags;
        LONGLONG timestamp;
        ComPtr<IMFSample> sample;
        hr = reader->ReadSample(MF_SOURCE_READER_FIRST_VIDEO_STREAM, 0, &stream, &flags, &timestamp, &sample);
        if (FAILED(hr)) return 8;
        if (flags & MF_SOURCE_READERF_ENDOFSTREAM) break;
        if (!sample) continue;
        ComPtr<IMFMediaBuffer> buffer;
        hr = sample->ConvertToContiguousBuffer(&buffer);
        if (FAILED(hr)) return 9;
        BYTE* pixels;
        DWORD maximum, length;
        hr = buffer->Lock(&pixels, &maximum, &length);
        if (FAILED(hr)) return 10;
        const size_t written = std::fwrite(pixels, 1, length, output);
        buffer->Unlock();
        if (written != length) return 11;
        std::printf("frame=%u timestamp100ns=%lld bytes=%lu\n", frame, timestamp, length);
        ++frame;
    }
    if (std::fclose(output) != 0) return 12;
    std::printf("frames=%u\n", frame);
    return frame == 6 ? 0 : 13;
}
