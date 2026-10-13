//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Test orchestration only: no production route or repository is substituted.
type nativeBackendLaunch struct{ native bool }

func nativeBackendMode(target, hostOS, isolated, team, identity string) (nativeBackendLaunch, error) {
	if target == "" || target == "host" {
		return nativeBackendLaunch{}, nil
	}
	if target != "macos" || hostOS != "darwin" || isolated != "true" || team == "" || identity == "" {
		return nativeBackendLaunch{}, errors.New("native backend requires macOS, isolated OS account and explicit existing signing identity/team")
	}
	return nativeBackendLaunch{native: true}, nil
}
func loadNativeBackendLaunch(t *testing.T) nativeBackendLaunch {
	t.Helper()
	mode, err := nativeBackendMode(os.Getenv("IM_TEST_NATIVE_TARGET"), runtime.GOOS, os.Getenv("IM_TEST_NATIVE_ISOLATED_PROFILE"), os.Getenv("FLUTTER_XCODE_DEVELOPMENT_TEAM"), os.Getenv("FLUTTER_XCODE_CODE_SIGN_IDENTITY"))
	if err != nil {
		t.Fatal(err)
	}
	return mode
}
func (m nativeBackendLaunch) command(ctx context.Context, dart, snapshot, config, scenario string) *exec.Cmd {
	args := []string{snapshot, "test", "--no-pub", "--reporter", "expanded"}
	if m.native {
		args = append(args, "-d", "macos", "--dart-define=IM_NATIVE_ISOLATED_PROFILE=true", "--dart-define=IM_BACKEND_SCENARIO="+scenario)
	}
	if m.native {
		args = append(args, "--dart-define=IM_NATIVE_BUNDLED_FIXTURE=true")
	} else {
		args = append(args, "--dart-define=IM_"+strings.ToUpper(scenario)+"_FIXTURE="+config)
	}
	if m.native {
		args = append(args, "integration_test/real_backend_test.dart")
	} else {
		args = append(args, "test/native_"+scenario+"_backend_probe.dart")
	}
	return exec.CommandContext(ctx, dart, args...)
}
func (m nativeBackendLaunch) runtimePath(client, tester string) string {
	if !m.native {
		return tester
	}
	return filepath.Join(client, "build/macos/Build/Products/Debug/enterprise_im.app/Contents/MacOS/enterprise_im")
}
func (m nativeBackendLaunch) environment() []string {
	values := map[string]string{"HOME": os.Getenv("HOME"), "CI": "true", "FLUTTER_SUPPRESS_ANALYTICS": "true", "PUB_CACHE": os.Getenv("IM_TEST_NATIVE_PUB_CACHE")}
	if m.native {
		values["DEVELOPER_DIR"] = "/Applications/Xcode.app/Contents/Developer"
		for _, k := range []string{"FLUTTER_XCODE_DEVELOPMENT_TEAM", "FLUTTER_XCODE_CODE_SIGN_IDENTITY"} {
			values[k] = os.Getenv(k)
		}
	}
	return processChildEnv(values)
}
func (m nativeBackendLaunch) prepare(t *testing.T, client string) {
	t.Helper()
	if !m.native {
		return
	}
	// Only tracked platform files; never copy generated build products or secrets.
	files, err := exec.Command("git", "ls-files", "-z", "--", "../../clients/enterprise_im/macos", "../../clients/enterprise_im/.metadata").Output()
	if err != nil {
		t.Fatal("tracked native project inventory unavailable")
	}
	// git paths are relative to this package directory.
	copied := 0
	for _, part := range strings.Split(string(files), "\x00") {
		if part == "" {
			continue
		}
		rel, err := filepath.Rel("../../clients/enterprise_im", part)
		if err != nil || !filepath.IsLocal(rel) {
			t.Fatal("native project path outside client")
		}
		info, err := os.Lstat(part)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatal("native project file is not regular")
		}
		b, err := os.ReadFile(part)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(client, rel)
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(target, b, 0600); err != nil {
			t.Fatal(err)
		}
		copied++
	}
	if copied == 0 {
		t.Fatal("native project inventory empty")
	}
	for _, part := range []string{"real_backend_test.dart", "messaging_test.dart", "files_and_devices_test.dart", "owned_backend_fixture.dart"} {
		b, err := os.ReadFile("../../clients/enterprise_im/integration_test/" + part)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(client, "integration_test", part)
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(target, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("IM_TEST_NATIVE_DART"), os.Getenv("IM_TEST_NATIVE_FLUTTER_SNAPSHOT"), "pub", "get", "--offline", "--enforce-lockfile")
	cmd.Dir = client
	cmd.Env = m.environment()
	log, err := os.OpenFile(filepath.Join(client, "native-pub.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proof := trackFileProductProcess(t, cmd)
	if err = proof.Ready(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal("native package preparation failed; owned log retained until cleanup")
	}
}

func (m nativeBackendLaunch) stageBundledFixture(t *testing.T, client string, input map[string]any, ca []byte) {
	t.Helper()
	if !m.native {
		return
	}
	pubspec := filepath.Join(client, "pubspec.yaml")
	source, err := os.ReadFile(pubspec)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if strings.Count(text, "flutter:\n") != 1 || strings.Contains(text, "  assets:") {
		t.Fatal("native test asset declaration is ambiguous")
	}
	payload := map[string]any{}
	for k, v := range input {
		payload[k] = v
	}
	payload["ca_pem"] = string(ca)
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	assets := filepath.Join(client, "assets")
	if err = os.Mkdir(assets, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(assets, "owned_backend.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	text = strings.Replace(text, "flutter:\n", "flutter:\n  assets:\n    - assets/owned_backend.json\n", 1)
	if err = os.WriteFile(pubspec, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
