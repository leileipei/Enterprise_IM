// Package testfixtures supplies optional evidence bridges to integration tests.
package testfixtures

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

type IntegrationProcess struct {
	path, owner, source, gate, test, identity string
	fingerprint                               map[string]any
	completedExit                             string
	kind                                      string
	privateRoot                               string
}

func integrationRecord(gate, test string) (*IntegrationProcess, error) {
	p := &IntegrationProcess{path: os.Getenv("IM_TEST_INTEGRATION_REGISTRY"), kind: "process"}
	if p.path == "" {
		return p, nil
	}
	p.owner = os.Getenv("IM_TEST_INTEGRATION_OWNER")
	p.source = os.Getenv("IM_TEST_INTEGRATION_SOURCE_SHA")
	p.gate = os.Getenv("IM_TEST_INTEGRATION_GATE")
	p.test = test
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(p.owner) ||
		!regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(p.source) || p.gate == "" || p.gate != gate || test == "" {
		return nil, errors.New("invalid integration registration")
	}
	info, err := os.Lstat(p.path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("registry must be private regular file")
	}
	root, err := os.Stat(filepath.Dir(p.path))
	if err != nil {
		return nil, err
	}
	if root.Mode().Perm()&0077 != 0 {
		return nil, errors.New("registry root must be private")
	}
	file, err := os.Open(p.path)
	if err != nil {
		return nil, err
	}
	reserved := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			file.Close()
			return nil, errors.New("corrupt registry")
		}
		if row["source_commit"] != p.source {
			file.Close()
			return nil, errors.New("foreign registry source")
		}
		if row["event"] == "reserve" && row["owner"] == p.owner {
			reserved = true
			fp, _ := row["fingerprint"].(map[string]any)
			p.privateRoot, _ = fp["private_root"].(string)
		}
	}
	scanErr := scanner.Err()
	file.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if !reserved {
		return nil, errors.New("owner not reserved")
	}
	return p, nil
}

func RegisterIntegrationProcess(cmd *exec.Cmd, gate, test string) (*IntegrationProcess, error) {
	return registerIntegrationProcess(cmd, gate, test, 0)
}

// verifiedParent is set only after independent browser parent/executable checks.
// Write the complete fingerprint at registration; later lifecycle events must
// preserve it exactly.
func registerIntegrationProcess(cmd *exec.Cmd, gate, test string, verifiedParent int) (*IntegrationProcess, error) {
	p, err := integrationRecord(gate, test)
	if err != nil {
		return nil, err
	}
	if p.path == "" {
		return p, nil
	}
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("invalid integration process")
	}
	pid := cmd.Process.Pid
	p.identity = strconv.Itoa(pid)
	executable, err := filepath.EvalSymlinks(cmd.Path)
	if err != nil {
		return nil, err
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return nil, err
	}
	directory := cmd.Dir
	if directory == "" {
		directory, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	registryRoot, err := filepath.EvalSymlinks(filepath.Dir(p.path))
	if err != nil {
		return nil, err
	}
	owned := func(path string) bool {
		rel, err := filepath.Rel(registryRoot, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if !owned(executable) && !owned(directory) {
		return nil, errors.New("process outside private root")
	}
	if state := cmd.ProcessState; state != nil {
		if state.Pid() != pid || !state.Exited() || cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil {
			return nil, errors.New("unproven completed process")
		}
		info, err := os.Stat(executable)
		if err != nil || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return nil, errors.New("unsafe completed executable")
		}
		p.completedExit = fmt.Sprintf("exit:%d", state.ExitCode())
		p.fingerprint = map[string]any{"pid": pid, "executable_path": executable, "executable_sha256": fmt.Sprintf("%x", sha256.Sum256(binary)), "workdir": directory}
		if err := p.write("completed", map[string]any{"actual_exit": p.completedExit, "actual_wait": true}); err != nil {
			return nil, err
		}
		return p, nil
	}
	ps := exec.Command("/bin/ps", "-p", p.identity, "-o", "uid=,lstart=,comm=")
	ps.Env = []string{"PATH=/usr/bin:/bin", "TZ=UTC"}
	raw, err := ps.Output()
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 7 {
		return nil, errors.New("unproven process identity")
	}
	uid, err := strconv.Atoi(fields[0])
	if err != nil || uid != os.Getuid() {
		return nil, errors.New("foreign process user")
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return nil, err
	}
	p.fingerprint = map[string]any{"pid": pid, "uid": uid, "start_time": strings.Join(fields[1:6], " "),
		"executable_path": executable, "executable_sha256": fmt.Sprintf("%x", sha256.Sum256(binary)), "workdir": directory, "pgid": pgid}
	if verifiedParent > 0 {
		p.fingerprint["parent_pid"] = verifiedParent
	}
	if err := p.write("registered", nil); err != nil {
		return nil, err
	}
	return p, nil
}

// Validate the reservation before creating a container. These labels allow
// recovery even if cancellation lands between Docker create and registration.
func IntegrationContainerLabels(gate, test string) ([]string, error) {
	p, err := integrationRecord(gate, test)
	if err != nil {
		return nil, err
	}
	if p.path == "" {
		return nil, nil
	}
	return []string{"--label", "im.integration.owner=" + p.owner, "--label", "im.integration.source=" + p.source}, nil
}

type integrationContainerInspect struct {
	ID         string `json:"Id"`
	Config     struct{ Labels map[string]string }
	Mounts     []map[string]any
	HostConfig *struct {
		Binds       []string
		Mounts      []map[string]any
		VolumesFrom []string
	}
}

// The Python cleanup ledger sorts by JSON with ASCII escaping. Preserve the same
// ordering for Unicode paths while retaining the decoded mount objects verbatim.
func sortContainerMounts(mounts []map[string]any) error {
	type keyed struct {
		key   string
		mount map[string]any
	}
	items := make([]keyed, len(mounts))
	for i, mount := range mounts {
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(mount); err != nil {
			return err
		}
		var key strings.Builder
		for _, r := range strings.TrimSpace(encoded.String()) {
			if r < 127 {
				key.WriteRune(r)
			} else if r <= 0xffff {
				fmt.Fprintf(&key, "\\u%04x", r)
			} else {
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(&key, "\\u%04x\\u%04x", hi, lo)
			}
		}
		items[i] = keyed{key.String(), mount}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })
	for i := range items {
		mounts[i] = items[i].mount
	}
	return nil
}

func containerMountFingerprint(actual integrationContainerInspect) (map[string]any, error) {
	if actual.Mounts == nil || actual.HostConfig == nil {
		return nil, errors.New("integration container mounts unproven")
	}
	host := actual.HostConfig
	if len(host.VolumesFrom) != 0 {
		return nil, errors.New("integration container mount ownership unproven")
	}
	binds := append([]string{}, host.Binds...)
	configured := append([]map[string]any{}, host.Mounts...)
	explicit := make(map[string]bool)
	for _, bind := range binds {
		parts := strings.Split(bind, ":")
		if (len(parts) == 2 || len(parts) == 3) && strings.HasPrefix(parts[1], "/") {
			explicit[parts[1]] = true
		} else if !((len(parts) == 1 || len(parts) == 2) && strings.HasPrefix(parts[0], "/")) {
			return nil, errors.New("integration container mounts unproven")
		}
	}
	for _, mount := range configured {
		target, ok := mount["Target"].(string)
		if !ok {
			return nil, errors.New("integration container mounts unproven")
		}
		if source, exists := mount["Source"]; exists && source != nil {
			value, ok := source.(string)
			if !ok {
				return nil, errors.New("integration container mounts unproven")
			}
			if value != "" {
				explicit[target] = true
			}
		}
	}
	volumes := make(map[string]bool)
	for _, mount := range actual.Mounts {
		destination, ok := mount["Destination"].(string)
		if !ok {
			return nil, errors.New("integration container mounts unproven")
		}
		if mount["Type"] == "volume" && !explicit[destination] {
			name, ok := mount["Name"].(string)
			if !ok || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(name) {
				return nil, errors.New("integration anonymous volume identity unproven")
			}
			volumes[name] = true
		}
	}
	anonymous := make([]string, 0, len(volumes))
	for name := range volumes {
		anonymous = append(anonymous, name)
	}
	sort.Strings(anonymous)
	sort.Strings(binds)
	if err := sortContainerMounts(actual.Mounts); err != nil {
		return nil, err
	}
	if err := sortContainerMounts(configured); err != nil {
		return nil, err
	}
	return map[string]any{"container_id": actual.ID, "mounts": actual.Mounts,
		"mount_configuration": map[string]any{"Binds": binds, "Mounts": configured},
		"anonymous_volumes":   anonymous}, nil
}

func RegisterIntegrationContainer(identity, gate, test string) (*IntegrationProcess, error) {
	p, err := integrationRecord(gate, test)
	if err != nil {
		return nil, err
	}
	if p.path == "" {
		return p, nil
	}
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(identity) {
		return nil, errors.New("invalid integration container identity")
	}
	raw, err := exec.Command("docker", "inspect", identity).Output()
	if err != nil {
		return nil, errors.New("integration container inspect failed")
	}
	var actual []integrationContainerInspect
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var trailing any
	if decoder.Decode(&actual) != nil || decoder.Decode(&trailing) != io.EOF || len(actual) != 1 || actual[0].ID != identity || actual[0].Config.Labels["im.integration.owner"] != p.owner || actual[0].Config.Labels["im.integration.source"] != p.source {
		return nil, errors.New("foreign integration container")
	}
	fingerprint, err := containerMountFingerprint(actual[0])
	if err != nil {
		return nil, err
	}
	p.kind = "container"
	p.identity = identity
	p.fingerprint = fingerprint
	if err = p.write("registered", nil); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *IntegrationProcess) write(event string, detail map[string]any) error {
	if p.path == "" {
		return nil
	}
	row := map[string]any{"schema_version": 1, "owner": p.owner, "source_commit": p.source,
		"event": event, "identity": p.identity, "kind": p.kind, "fingerprint": p.fingerprint,
		"detail": map[string]any{"gate": p.gate, "test": p.test}, "time": float64(time.Now().UnixNano()) / 1e9}
	for k, v := range detail {
		row["detail"].(map[string]any)[k] = v
	}
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	fd, err := syscall.Open(p.path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), p.path)
	defer file.Close()
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func (p *IntegrationProcess) Ready() error {
	if p.completedExit != "" {
		return errors.New("completed process cannot become ready")
	}
	return p.write("ready", nil)
}
func (p *IntegrationProcess) Exited(expected, actual string) error {
	if p.completedExit != "" && (actual != p.completedExit || expected != actual) {
		return errors.New("completed exit proof differs from actual Wait")
	}
	return p.write("exited", map[string]any{"expected_exit": expected, "actual_exit": actual})
}
