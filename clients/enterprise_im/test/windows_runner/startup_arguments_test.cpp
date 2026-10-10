// Execute the production wWinMain on this host with OS/Flutter SDK boundaries
// substituted. This verifies its branch and pre-engine rejection, not Windows
// quoting, COM, HWND, DLL or installed-app behavior. Those require the native
// executable gate on Windows. All input values below are owned fake fixtures.
#include <cstdlib>
#include <iostream>
#include <string>
#include <vector>
#include "windows.h"

int fixture_project_count = 0, fixture_forwarded_arguments = 0;
int fixture_com_count = 0, fixture_console_count = 0, fixture_freed = 0, fixture_allocations = 0;
int fixture_parse_count = 1;
bool fixture_parse_fails = false;
std::vector<std::wstring> fixture_arguments;
std::vector<wchar_t*> fixture_argv;

int AttachConsole(unsigned) { ++fixture_console_count; return 1; }
int IsDebuggerPresent() { return 0; }
long CoInitializeEx(void*, unsigned) { ++fixture_com_count; return 0; }
void CoUninitialize() {}
int GetMessage(MSG*, void*, unsigned, unsigned) { return 0; }
int TranslateMessage(const MSG*) { return 0; }
long DispatchMessage(const MSG*) { return 0; }
wchar_t* GetCommandLineW() { static wchar_t value[] = L"owned-fixture"; return value; }
wchar_t** CommandLineToArgvW(const wchar_t*, int* count) {
  *count = fixture_parse_count;
  if (fixture_parse_fails) return nullptr;
  ++fixture_allocations;
  return fixture_argv.data();
}
void* LocalFree(void*) { ++fixture_freed; return nullptr; }

// Replace only unavailable OS/Flutter window classes, keep the entire production
// entry point and its argument handling. No product test hook is compiled.
#define RUNNER_FLUTTER_WINDOW_H_
#define RUNNER_UTILS_H_
#include <flutter/dart_project.h>
class Win32Window {
 public:
  struct Point { Point(int, int) {} };
  struct Size { Size(int, int) {} };
};
class FlutterWindow {
 public:
  explicit FlutterWindow(const flutter::DartProject&) {}
  bool Create(const wchar_t*, Win32Window::Point, Win32Window::Size) { return true; }
  void SetQuitOnClose(bool) {}
};
void CreateAndAttachConsole() {}
std::vector<std::string> GetCommandLineArguments() {
  // Preserve the old utility's empty-on-parse-failure contract for the RED run.
  if (fixture_parse_fails) return {};
  return std::vector<std::string>(fixture_arguments.size(), "owned-input");
}
#include "../../windows/runner/main.cpp"

struct Case {
  const char* name;
  std::vector<std::wstring> args;
  bool parse_fails;
  int count_override;  // -1 means normal executable + argument count.
  int wanted_exit;
};
int main() {
  const std::vector<Case> cases = {
    {"plain launch", {}, false, -1, EXIT_SUCCESS},
    {"token flag", {L"--access-token=owned-fixture"}, false, -1, EXIT_FAILURE},
    {"split refresh token", {L"--refresh-token", L"owned-fixture"}, false, -1, EXIT_FAILURE},
    {"external HTTPS business URL", {L"https://owned.invalid/chat/fixture"}, false, -1, EXIT_FAILURE},
    {"loopback callback in argv", {L"http://127.0.0.1:12345/oauth/callback?code=owned"}, false, -1, EXIT_FAILURE},
    {"custom URI", {L"com.enterpriseim.client:/oauth/callback?code=owned"}, false, -1, EXIT_FAILURE},
    {"file path", {L"C:\\owned\\private.txt"}, false, -1, EXIT_FAILURE},
    {"quoted empty argument", {L""}, false, -1, EXIT_FAILURE},
    {"unsupported cold COM activation", {L"-Embedding"}, false, -1, EXIT_FAILURE},
    {"parser failure", {}, true, -1, EXIT_FAILURE},
    {"malformed zero count", {}, false, 0, EXIT_FAILURE},
  };
  int failed = 0;
  for (const auto& c : cases) {
    fixture_project_count = fixture_forwarded_arguments = fixture_com_count = 0;
    fixture_console_count = fixture_freed = fixture_allocations = 0;
    fixture_arguments = c.args;
    fixture_argv.clear();
    std::wstring executable = L"owned-client.exe";
    fixture_argv.push_back(executable.data());
    for (auto& arg : fixture_arguments) fixture_argv.push_back(arg.data());
    fixture_parse_fails = c.parse_fails;
    fixture_parse_count = c.count_override < 0 ? static_cast<int>(fixture_argv.size()) : c.count_override;
    const int result = wWinMain(nullptr, nullptr, nullptr, 0);
    const bool normal = c.wanted_exit == EXIT_SUCCESS;
    const bool safe = result == c.wanted_exit &&
        fixture_project_count == (normal ? 1 : 0) &&
        fixture_com_count == (normal ? 1 : 0) &&
        fixture_console_count == (normal ? 1 : 0) &&
        fixture_forwarded_arguments == 0 &&
        fixture_freed == fixture_allocations;
    std::cout << (safe ? "PASS " : "FAIL ") << c.name << "\n";
    if (!safe) ++failed;
  }
  std::cout << "cases=" << cases.size() << " failed=" << failed << "\n";
  return failed == 0 ? EXIT_SUCCESS : EXIT_FAILURE;
}
