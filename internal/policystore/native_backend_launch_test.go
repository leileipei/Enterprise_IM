//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNativeBackendLaunchMode(t *testing.T) {
	cases := []struct {
		name, target, os, isolated, team, identity string
		native, denied                             bool
	}{
		{"legacy host", "", "darwin", "", "", "", false, false},
		{"explicit host", "host", "linux", "", "", "", false, false},
		{"native mac", "macos", "darwin", "true", "owned-team", "owned-identity", true, false},
		{"unknown target", "windows", "darwin", "true", "owned-team", "owned-identity", false, true},
		{"wrong host", "macos", "linux", "true", "owned-team", "owned-identity", false, true},
		{"profile unconfirmed", "macos", "darwin", "", "owned-team", "owned-identity", false, true},
		{"missing team", "macos", "darwin", "true", "", "owned-identity", false, true},
		{"missing identity", "macos", "darwin", "true", "owned-team", "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mode, err := nativeBackendMode(c.target, c.os, c.isolated, c.team, c.identity)
			if (err != nil) != c.denied || mode.native != c.native {
				t.Fatalf("native=%v denied=%v; want native=%v denied=%v", mode.native, err != nil, c.native, c.denied)
			}
		})
	}
}
func TestNativeBackendCommandUsesCompiledEntry(t *testing.T) {
	mode := nativeBackendLaunch{native: true}
	cmd := mode.command(context.Background(), "/owned/dart", "/owned/flutter.snapshot", "/owned/input.json", "messaging")
	want := []string{"/owned/dart", "/owned/flutter.snapshot", "test", "--no-pub", "--reporter", "expanded", "-d", "macos", "--dart-define=IM_NATIVE_ISOLATED_PROFILE=true", "--dart-define=IM_BACKEND_SCENARIO=messaging", "--dart-define=IM_NATIVE_BUNDLED_FIXTURE=true", "integration_test/real_backend_test.dart"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("launch command differs: %v", cmd.Args)
	}
	if got := mode.runtimePath("/owned/client", "/owned/tester"); got != "/owned/client/build/macos/Build/Products/Debug/enterprise_im.app/Contents/MacOS/enterprise_im" {
		t.Fatalf("native runtime=%s", got)
	}
}
func TestNativeBackendHostCommandRetainsExplicitProbe(t *testing.T) {
	mode := nativeBackendLaunch{}
	cmd := mode.command(context.Background(), "/owned/dart", "/owned/flutter.snapshot", "/owned/input.json", "files")
	want := []string{"/owned/dart", "/owned/flutter.snapshot", "test", "--no-pub", "--reporter", "expanded", "--dart-define=IM_FILES_FIXTURE=/owned/input.json", "test/native_files_backend_probe.dart"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("host command differs: %v", cmd.Args)
	}
	if got := mode.runtimePath("/owned/client", "/owned/tester"); got != "/owned/tester" {
		t.Fatalf("host runtime=%s", got)
	}
}

func TestNativeBackendStagesOnlyOwnedBundle(t *testing.T) {
	client := t.TempDir()
	pubspec := []byte("name: enterprise_im\nflutter:\n  uses-material-design: true\n")
	if err := os.WriteFile(filepath.Join(client, "pubspec.yaml"), pubspec, 0600); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"refresh": "owned-marker", "tenant": "owned-tenant"}
	nativeBackendLaunch{native: true}.stageBundledFixture(t, client, input, []byte("owned-ca"))
	asset := filepath.Join(client, "assets/owned_backend.json")
	b, err := os.ReadFile(asset)
	if err != nil {
		t.Fatal("owned native asset missing")
	}
	var got map[string]any
	if err = json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"refresh": "owned-marker", "tenant": "owned-tenant", "ca_pem": "owned-ca"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("owned asset payload changed")
	}
	if _, ok := input["ca_pem"]; ok {
		t.Fatal("shared host input mutated")
	}
	info, err := os.Stat(asset)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("asset is not owner-only")
	}
}
func TestNativeBackendHostDoesNotPackageCredentials(t *testing.T) {
	client := t.TempDir()
	input := map[string]any{"refresh": "owned-marker"}
	nativeBackendLaunch{}.stageBundledFixture(t, client, input, []byte("owned-ca"))
	if _, err := os.Stat(filepath.Join(client, "assets/owned_backend.json")); !os.IsNotExist(err) {
		t.Fatal("host packaged credentials")
	}
}
