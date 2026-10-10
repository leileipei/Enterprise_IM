#!/bin/bash
# Local release build only. Signing/provisioning and installed-app acceptance
# must be recorded separately; this script never accepts Apple terms.
set -eu
: "${IM_FLUTTER_SDK:?Set IM_FLUTTER_SDK to the verified Flutter checkout}"
if [ "$(uname -s)" != Darwin ] || [ "$(uname -m)" != arm64 ]; then
  echo 'This gate requires native macOS ARM64.' >&2
  exit 2
fi
task_sdk_head="$(git -C "$IM_FLUTTER_SDK" rev-parse HEAD)"
task_sdk_changes="$(git -C "$IM_FLUTTER_SDK" status --porcelain --untracked-files=no)"
if [ "$task_sdk_head" != e8113bf45620cbeb8aff64947ee4c93e16adb4cf ] || [ -n "$task_sdk_changes" ]; then
  echo 'Fixed Flutter SDK source verification failed.' >&2
  exit 2
fi
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
/usr/bin/xcodebuild -checkFirstLaunchStatus
/usr/bin/xcrun --sdk macosx --show-sdk-version
client_root="$(cd "$(dirname "$0")/../clients/enterprise_im" && pwd)"
cd "$client_root"
export CI=true FLUTTER_SUPPRESS_ANALYTICS=true
exec "$IM_FLUTTER_SDK/bin/flutter" build macos --release --no-pub
