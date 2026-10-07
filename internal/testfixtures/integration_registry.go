// Package testfixtures supplies optional evidence bridges to integration tests.
package testfixtures

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type IntegrationProcess struct {
	path, owner, source, gate, test, identity string
	fingerprint                               map[string]any
}

func RegisterIntegrationProcess(cmd *exec.Cmd, gate, test string) (*IntegrationProcess, error) {
	p := &IntegrationProcess{path: os.Getenv("IM_TEST_INTEGRATION_REGISTRY")}
	if p.path == "" {
		return p, nil
	}
	p.owner = os.Getenv("IM_TEST_INTEGRATION_OWNER")
	p.source = os.Getenv("IM_TEST_INTEGRATION_SOURCE_SHA")
	p.gate = os.Getenv("IM_TEST_INTEGRATION_GATE")
	p.test = test
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(p.owner) ||
		!regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(p.source) || p.gate == "" || p.gate != gate || test == "" || cmd == nil || cmd.Process == nil {
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
	pid := cmd.Process.Pid
	p.identity = strconv.Itoa(pid)
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
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return nil, err
	}
	p.fingerprint = map[string]any{"pid": pid, "uid": uid, "start_time": strings.Join(fields[1:6], " "),
		"executable_path": executable, "executable_sha256": fmt.Sprintf("%x", sha256.Sum256(binary)), "workdir": directory, "pgid": pgid}
	if err := p.write("registered", nil); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *IntegrationProcess) write(event string, detail map[string]any) error {
	if p.path == "" {
		return nil
	}
	row := map[string]any{"schema_version": 1, "owner": p.owner, "source_commit": p.source,
		"event": event, "identity": p.identity, "kind": "process", "fingerprint": p.fingerprint,
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

func (p *IntegrationProcess) Ready() error { return p.write("ready", nil) }
func (p *IntegrationProcess) Exited(expected, actual string) error {
	return p.write("exited", map[string]any{"expected_exit": expected, "actual_exit": actual})
}
