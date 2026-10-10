#pragma once
#include <string>
#include <vector>
extern int fixture_project_count, fixture_forwarded_arguments;
namespace flutter {
class DartProject {
 public:
  explicit DartProject(const wchar_t*) { ++fixture_project_count; }
  void set_dart_entrypoint_arguments(std::vector<std::string> args) {
    fixture_forwarded_arguments += static_cast<int>(args.size());
  }
};
}
