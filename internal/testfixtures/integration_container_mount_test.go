package testfixtures

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const mountTestOwner = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const mountTestSource = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const mountTestCID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
const mountTestVolume = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func containerMountFixture() map[string]any {
	return map[string]any{
		"Id":     mountTestCID,
		"Config": map[string]any{"Labels": map[string]string{"im.integration.owner": mountTestOwner, "im.integration.source": mountTestSource}},
		"Mounts": []any{
			map[string]any{"Type": "volume", "Name": mountTestVolume, "Destination": "/z-anonymous", "Source": "/volumes/anonymous", "RW": true},
			map[string]any{"Type": "volume", "Name": "preserved-named", "Destination": "/named", "Source": "/volumes/named", "RW": true},
			map[string]any{"Type": "bind", "Destination": "/a-bind", "Source": "/owned/product", "RW": false},
		},
		"HostConfig": map[string]any{"Binds": []string{"preserved-named:/named", "/owned/product:/a-bind:ro"}, "Mounts": []any{}, "VolumesFrom": []string{}},
	}
}

func mountTestRegistry(t *testing.T, actual map[string]any) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(root, "registry.jsonl")
	seed, _ := json.Marshal(map[string]any{"schema_version": 1, "owner": mountTestOwner, "source_commit": mountTestSource, "event": "reserve", "fingerprint": map[string]string{"private_root": filepath.Join(root, mountTestOwner)}})
	if err := os.WriteFile(ledger, append(seed, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, mountTestOwner), 0700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal([]any{actual})
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1\" = inspect ] && [ \"$2\" = " + mountTestCID + " ] && [ \"$#\" = 2 ] || exit 92\n/bin/cat <<'INSPECT_FIXTURE'\n" + string(body) + "\nINSPECT_FIXTURE\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("IM_TEST_INTEGRATION_REGISTRY", ledger)
	t.Setenv("IM_TEST_INTEGRATION_OWNER", mountTestOwner)
	t.Setenv("IM_TEST_INTEGRATION_SOURCE_SHA", mountTestSource)
	t.Setenv("IM_TEST_INTEGRATION_GATE", "full_repository")
	return ledger
}

func readMountTicket(t *testing.T, ledger string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(rows) != 2 {
		t.Fatalf("want reserve and one complete registration, got %d", len(rows))
	}
	var row map[string]any
	if err = json.Unmarshal([]byte(rows[1]), &row); err != nil {
		t.Fatal(err)
	}
	return row["fingerprint"].(map[string]any)
}

func TestIntegrationContainerPersistsAnonymousAndNamedMountTicket(t *testing.T) {
	actual := containerMountFixture()
	ledger := mountTestRegistry(t, actual)
	if _, err := RegisterIntegrationContainer(mountTestCID, "full_repository", "TestContainerParent"); err != nil {
		t.Fatal(err)
	}
	fp := readMountTicket(t, ledger)
	if !reflect.DeepEqual(fp["anonymous_volumes"], []any{mountTestVolume}) {
		t.Fatalf("missing exact anonymous-volume cleanup ticket: %#v", fp)
	}
	if _, ok := fp["mount_configuration"].(map[string]any); !ok {
		t.Fatal("mount configuration must be retained before container disposal")
	}
	mounts, ok := fp["mounts"].([]any)
	if !ok || len(mounts) != 3 || mounts[0].(map[string]any)["Destination"] != "/a-bind" {
		t.Fatal("complete canonical mount mapping missing")
	}
	input, _ := json.Marshal(actual)
	ticket, _ := json.Marshal(fp)
	t.Logf("MOUNT_INPUT=%s", input)
	t.Logf("MOUNT_TICKET=%s", ticket)
}

func TestIntegrationContainerEmptyMountTicketKeepsArrays(t *testing.T) {
	actual := containerMountFixture()
	actual["Mounts"] = []any{}
	actual["HostConfig"] = map[string]any{}
	ledger := mountTestRegistry(t, actual)
	if _, err := RegisterIntegrationContainer(mountTestCID, "full_repository", "TestEmptyContainer"); err != nil {
		t.Fatal(err)
	}
	fp := readMountTicket(t, ledger)
	for _, key := range []string{"mounts", "anonymous_volumes"} {
		if !reflect.DeepEqual(fp[key], []any{}) {
			t.Fatalf("%s must be an explicit empty array, got %#v", key, fp[key])
		}
	}
}

func TestIntegrationContainerRejectsUnprovenMountMapping(t *testing.T) {
	cases := map[string]func(map[string]any){
		"missing_mounts":         func(a map[string]any) { delete(a, "Mounts") },
		"null_mounts":            func(a map[string]any) { a["Mounts"] = nil },
		"missing_host_config":    func(a map[string]any) { delete(a, "HostConfig") },
		"volumes_from":           func(a map[string]any) { a["HostConfig"].(map[string]any)["VolumesFrom"] = []string{"foreign"} },
		"invalid_anonymous_name": func(a map[string]any) { a["Mounts"].([]any)[0].(map[string]any)["Name"] = "not-an-exact-volume-id" },
		"invalid_destination":    func(a map[string]any) { a["Mounts"].([]any)[0].(map[string]any)["Destination"] = 2 },
		"invalid_configured_target": func(a map[string]any) {
			a["HostConfig"].(map[string]any)["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/owned", "Target": 2}}
		},
		"invalid_bind": func(a map[string]any) { a["HostConfig"].(map[string]any)["Binds"] = []string{"relative:target"} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			actual := containerMountFixture()
			change(actual)
			ledger := mountTestRegistry(t, actual)
			if _, err := RegisterIntegrationContainer(mountTestCID, "full_repository", "TestRejectedContainer"); err == nil {
				t.Fatal("unproven mounts accepted")
			}
			data, _ := os.ReadFile(ledger)
			if len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 1 {
				t.Fatal("partial container registration must not be published")
			}
		})
	}
}

func TestIntegrationContainerUnicodeMountOrderingMatchesCleanupProtocol(t *testing.T) {
	for _, path := range []string{"/\u4e2d", "/\u007f"} {
		t.Run(path, func(t *testing.T) { checkContainerMountOrdering(t, path) })
	}
}

func checkContainerMountOrdering(t *testing.T, path string) {
	t.Helper()
	actual := containerMountFixture()
	actual["Mounts"] = []any{map[string]any{"Type": "bind", "Destination": "/a", "Source": "/ascii"}, map[string]any{"Type": "bind", "Destination": path, "Source": "/unicode"}}
	actual["HostConfig"] = map[string]any{}
	ledger := mountTestRegistry(t, actual)
	if _, err := RegisterIntegrationContainer(mountTestCID, "full_repository", "TestUnicodeMounts"); err != nil {
		t.Fatal(err)
	}
	fp := readMountTicket(t, ledger)
	mounts, ok := fp["mounts"].([]any)
	if !ok || len(mounts) != 2 || mounts[0].(map[string]any)["Destination"] != path {
		t.Fatal("wire ordering must match the Python cleanup consumer")
	}
	input, _ := json.Marshal(actual)
	ticket, _ := json.Marshal(fp)
	t.Logf("MOUNT_INPUT=%s", input)
	t.Logf("MOUNT_TICKET=%s", ticket)
}
