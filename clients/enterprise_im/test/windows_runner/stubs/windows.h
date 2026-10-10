#pragma once
#include <cstddef>
#define APIENTRY
#define _In_
#define _In_opt_
using HINSTANCE = void*;
struct MSG {};
constexpr unsigned ATTACH_PARENT_PROCESS = static_cast<unsigned>(-1);
constexpr unsigned COINIT_APARTMENTTHREADED = 2;
int AttachConsole(unsigned);
int IsDebuggerPresent();
long CoInitializeEx(void*, unsigned);
void CoUninitialize();
int GetMessage(MSG*, void*, unsigned, unsigned);
int TranslateMessage(const MSG*);
long DispatchMessage(const MSG*);
wchar_t* GetCommandLineW();
void* LocalFree(void*);
