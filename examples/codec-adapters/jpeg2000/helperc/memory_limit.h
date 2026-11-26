// Bound native working memory before accepting any patient data over IPC.
#pragma once

#include <charconv>
#include <cstddef>
#include <cstring>
#ifdef _WIN32
#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <windows.h>
#else
#include <sys/resource.h>
#endif

inline bool apply_memory_limit(const char* text) {
    size_t bytes = 0;
    const char* end = text + std::strlen(text);
    const auto parsed = std::from_chars(text, end, bytes);
    if (parsed.ec != std::errc{} || parsed.ptr != end || bytes == 0) {
        return false;
    }
#ifdef _WIN32
    const HANDLE process = GetCurrentProcess();
    SIZE_T minimum = 0, maximum = 0;
    DWORD flags = 0;
    if (!GetProcessWorkingSetSizeEx(process, &minimum, &maximum, &flags) ||
        !SetProcessWorkingSetSizeEx(process, minimum, bytes, QUOTA_LIMITS_HARDWS_MAX_ENABLE)) {
        return false;
    }

    HANDLE job = CreateJobObjectW(nullptr, nullptr);
    if (job == nullptr) {
        return false;
    }
    JOBOBJECT_EXTENDED_LIMIT_INFORMATION limits{};
    limits.BasicLimitInformation.LimitFlags =
        JOB_OBJECT_LIMIT_PROCESS_MEMORY | JOB_OBJECT_LIMIT_ACTIVE_PROCESS;
    limits.BasicLimitInformation.ActiveProcessLimit = 1;
    limits.ProcessMemoryLimit = bytes;
    const bool installed =
        SetInformationJobObject(job, JobObjectExtendedLimitInformation, &limits, sizeof(limits)) &&
        AssignProcessToJobObject(job, process);
    CloseHandle(job);
    return installed;
#else
    const auto ceiling = static_cast<rlim_t>(bytes);
    if (static_cast<size_t>(ceiling) != bytes) {
        return false;
    }
    const rlimit limit{ceiling, ceiling};
    return setrlimit(RLIMIT_AS, &limit) == 0;
#endif
}
