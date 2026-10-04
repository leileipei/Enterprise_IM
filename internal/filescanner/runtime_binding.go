package filescanner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type runtimeManifest struct {
	Kind             string            `json:"kind"`
	PID              int               `json:"pid"`
	Binary           string            `json:"clamd_binary_path"`
	BinarySHA        string            `json:"clamd_binary_sha256"`
	Config           string            `json:"config_path"`
	ConfigSHA        string            `json:"config_sha256"`
	Socket           string            `json:"clamd_socket_path"`
	QPDF             string            `json:"qpdf_path"`
	QPDFSHA          string            `json:"qpdf_sha256"`
	Definitions      string            `json:"definition_directory"`
	DefinitionHashes map[string]string `json:"definition_hashes"`
}

func privateReadonly(path string) error {
	st, e := os.Lstat(path)
	if e != nil || !filepath.IsAbs(path) || !st.Mode().IsRegular() || st.Mode().Perm()&0277 != 0 {
		return ErrRuntimeUnavailable
	}
	uid, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(uid.Uid) != os.Geteuid() {
		return ErrRuntimeUnavailable
	}
	return nil
}
func readManifest(c Config) (runtimeManifest, error) {
	var m runtimeManifest
	if privateReadonly(c.RuntimeManifestPath) != nil {
		return m, ErrRuntimeUnavailable
	}
	f, e := os.Open(c.RuntimeManifestPath)
	if e != nil {
		return m, ErrRuntimeUnavailable
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil || len(b) > 65536 {
		return m, ErrRuntimeUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF || m.Kind != "local-process" || m.PID < 1 || m.QPDF != c.QPDFPath || m.Socket != c.ClamdSocket || len(m.DefinitionHashes) != 3 {
		return m, ErrRuntimeUnavailable
	}
	for _, p := range []string{m.Binary, m.Config, m.Socket, m.QPDF, m.Definitions} {
		if !filepath.IsAbs(p) {
			return m, ErrRuntimeUnavailable
		}
	}
	for _, h := range []string{m.BinarySHA, m.ConfigSHA, m.QPDFSHA, m.DefinitionHashes["daily.cvd"], m.DefinitionHashes["main.cvd"], m.DefinitionHashes["bytecode.cvd"]} {
		b, e := hex.DecodeString(h)
		if e != nil || len(b) != 32 || h != strings.ToLower(h) {
			return m, ErrRuntimeUnavailable
		}
	}
	if privateReadonly(m.Config) != nil {
		return m, ErrRuntimeUnavailable
	}
	return m, nil
}

type cappedOutput struct {
	b     bytes.Buffer
	limit int
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.b.Len() {
		return 0, ErrRuntimeUnavailable
	}
	return w.b.Write(p)
}
func processOutput(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, path, args...)
	c.Env = append(os.Environ(), "LC_ALL=C")
	c.WaitDelay = time.Second
	b := &cappedOutput{limit: 65536}
	c.Stdout = b
	c.Stderr = io.Discard
	if c.Run() != nil {
		return "", ErrRuntimeUnavailable
	}
	return strings.TrimSpace(b.b.String()), nil
}
func processBinding(ctx context.Context, m runtimeManifest) (string, time.Time, error) {
	pid := strconv.Itoa(m.PID)
	start, e := processOutput(ctx, "ps", "-p", pid, "-o", "lstart=")
	if e != nil {
		return "", time.Time{}, e
	}
	began, e := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(start), " "), time.Local)
	if e != nil {
		return "", time.Time{}, ErrRuntimeUnavailable
	}
	if began.After(time.Now().Add(time.Second)) {
		return "", time.Time{}, ErrRuntimeUnavailable
	}
	switch runtime.GOOS {
	case "darwin":
		command, e := processOutput(ctx, "ps", "-ww", "-p", pid, "-o", "command=")
		if e != nil || command != m.Binary+" --config-file="+m.Config {
			return "", time.Time{}, ErrRuntimeUnavailable
		}
		sockets, e := processOutput(ctx, "/usr/sbin/lsof", "-a", "-p", pid, "-U", "-Fn")
		if e != nil {
			return "", time.Time{}, e
		}
		found := false
		for _, line := range strings.Split(sockets, "\n") {
			if line == "n"+m.Socket {
				found = true
			}
		}
		if !found {
			return "", time.Time{}, ErrRuntimeUnavailable
		}
	case "linux":
		b, e := os.ReadFile("/proc/" + pid + "/cmdline")
		if e != nil || string(b) != m.Binary+"\x00--config-file="+m.Config+"\x00" {
			return "", time.Time{}, ErrRuntimeUnavailable
		}
		exe, e := os.Readlink("/proc/" + pid + "/exe")
		real, e2 := filepath.EvalSymlinks(m.Binary)
		if e != nil || e2 != nil || exe != real {
			return "", time.Time{}, ErrRuntimeUnavailable
		}
		sockets, e := os.ReadFile("/proc/net/unix")
		if e != nil {
			return "", time.Time{}, ErrRuntimeUnavailable
		}
		inode := ""
		for _, line := range strings.Split(string(sockets), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 8 && fields[7] == m.Socket {
				inode = fields[6]
			}
		}
		found := false
		entries, e := os.ReadDir("/proc/" + pid + "/fd")
		if e != nil {
			return "", time.Time{}, ErrRuntimeUnavailable
		}
		for _, entry := range entries {
			link, _ := os.Readlink("/proc/" + pid + "/fd/" + entry.Name())
			if inode != "" && link == "socket:["+inode+"]" {
				found = true
			}
		}
		if !found {
			return "", time.Time{}, ErrRuntimeUnavailable
		}
	default:
		return "", time.Time{}, ErrRuntimeUnavailable
	}
	for _, p := range []string{m.Config, m.Binary} {
		st, e := os.Stat(p)
		if e != nil || st.ModTime().After(began.Add(time.Second)) {
			return "", time.Time{}, ErrRuntimeUnavailable
		}
	}
	st, e := os.Lstat(m.Socket)
	if e != nil || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm()&0077 != 0 {
		return "", time.Time{}, ErrRuntimeUnavailable
	}
	uid, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(uid.Uid) != os.Geteuid() {
		return "", time.Time{}, ErrRuntimeUnavailable
	}
	return fmt.Sprintf("%d/%s/%d", m.PID, start, uid.Ino), began, nil
}
func hashBoundFile(ctx context.Context, path, want string) error {
	f, e := os.Open(path)
	if e != nil {
		return ErrRuntimeUnavailable
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() < 1 || st.Size() > 256*1024*1024 {
		return ErrRuntimeUnavailable
	}
	h := sha256.New()
	buf := make([]byte, 32768)
	for {
		if ctx.Err() != nil {
			return ErrRuntimeUnavailable
		}
		n, e := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return ErrRuntimeUnavailable
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return ErrRuntimeUnavailable
	}
	return nil
}
func runtimeConfig(path string) (map[string]string, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, ErrRuntimeUnavailable
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil || len(b) > 65536 {
		return nil, ErrRuntimeUnavailable
	}
	allowed := map[string]bool{}
	for _, k := range strings.Fields("LocalSocket FixStaleSocket Foreground DatabaseDirectory StreamMaxLength MaxFileSize MaxScanSize MaxRecursion MaxFiles AlertExceedsMax AlertEncrypted AlertBrokenExecutables LocalSocketMode PidFile LogFile MaxScanTime") {
		allowed[k] = true
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, " ")
		v = strings.TrimSpace(v)
		if !ok || !allowed[k] || out[k] != "" || v == "" {
			return nil, ErrRuntimeUnavailable
		}
		out[k] = v
	}
	return out, nil
}
func attestRuntime(ctx context.Context, c Config) (RuntimeEvidence, [32]byte, error) {
	var e RuntimeEvidence
	var stamp [32]byte
	m, err := readManifest(c)
	if err != nil {
		return e, stamp, err
	}
	binding, began, err := processBinding(ctx, m)
	if err != nil {
		return e, stamp, err
	}
	cfg, err := runtimeConfig(m.Config)
	if err != nil {
		return e, stamp, err
	}
	expected := map[string]string{"LocalSocket": m.Socket, "DatabaseDirectory": m.Definitions, "StreamMaxLength": "26214400", "MaxFileSize": "26214400", "MaxScanSize": "262144000", "MaxRecursion": "16", "MaxFiles": "10000", "AlertExceedsMax": "yes", "AlertEncrypted": "yes", "AlertBrokenExecutables": "yes", "Foreground": "yes", "MaxScanTime": "90000", "LocalSocketMode": "600"}
	for k, v := range expected {
		if cfg[k] != v {
			return e, stamp, ErrRuntimeUnavailable
		}
	}
	for _, pair := range [][2]string{{m.Config, m.ConfigSHA}, {m.Binary, m.BinarySHA}, {m.QPDF, m.QPDFSHA}} {
		if err = hashBoundFile(ctx, pair[0], pair[1]); err != nil {
			return e, stamp, err
		}
	}
	if v, err := qpdfVersion(ctx, m.QPDF); err != nil || v != "qpdf-12.4.2/rules-v1" {
		return e, stamp, ErrRuntimeUnavailable
	}
	for _, name := range []string{"main.cvd", "daily.cvd", "bytecode.cvd"} {
		p := filepath.Join(m.Definitions, name)
		if err = hashBoundFile(ctx, p, m.DefinitionHashes[name]); err != nil {
			return e, stamp, err
		}
		st, err := os.Stat(p)
		if err != nil || st.ModTime().After(began.Add(time.Second)) {
			return e, stamp, ErrRuntimeUnavailable
		}
	}
	// The daemon must start after the pinned databases were installed. No inferred reload.
	f, err := os.Open(filepath.Join(m.Definitions, "daily.cvd"))
	if err != nil {
		return e, stamp, ErrRuntimeUnavailable
	}
	header := make([]byte, 512)
	_, err = io.ReadFull(f, header)
	f.Close()
	if err != nil {
		return e, stamp, ErrRuntimeUnavailable
	}
	fields := strings.Split(strings.TrimSpace(string(header)), ":")
	if len(fields) != 9 || fields[0] != "ClamAV-VDB" {
		return e, stamp, ErrRuntimeUnavailable
	}
	epoch, err := strconv.ParseInt(fields[8], 10, 64)
	if err != nil {
		return e, stamp, ErrRuntimeUnavailable
	}
	version, err := clamdCommand(ctx, c.ClamdSocket, "VERSION")
	if err != nil || !strings.HasPrefix(version, "ClamAV 1.5.4/"+fields[2]+"/") {
		return e, stamp, ErrRuntimeUnavailable
	}
	e.EngineVersion = "ClamAV 1.5.4"
	e.DefinitionVersion = fields[2]
	e.DefinitionsUpdatedAt = time.Unix(epoch, 0)
	e.StreamMaxLengthBytes = 26214400
	e.MaxFileSizeBytes = 26214400
	e.MaxScanSizeBytes = 262144000
	e.AlertExceedsMax = true
	e.AlertEncrypted = true
	e.AlertBroken = true
	b, _ := hex.DecodeString(m.ConfigSHA)
	copy(e.ConfigSHA256[:], b)
	body, _ := json.Marshal(m)
	stamp = sha256.Sum256(append(body, []byte(binding+version)...))
	if err = validateRuntimeEvidence(e, time.Now()); err != nil {
		return e, stamp, err
	}
	return e, stamp, nil
}
