package testfixtures

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type IntegrationBrowser struct {
	cmd            *exec.Cmd
	node           *IntegrationProcess
	events         *os.File
	ack            *os.File
	done           chan error
	BrowsersReady  chan struct{}
	requireBrowser bool
}

func StartIntegrationBrowser(cmd *exec.Cmd, test string, requireBrowser bool) (*IntegrationBrowser, error) {
	gate := os.Getenv("IM_TEST_INTEGRATION_GATE")
	reservation, err := integrationRecord(gate, test)
	if err != nil {
		return nil, err
	}
	h := &IntegrationBrowser{cmd: cmd, requireBrowser: requireBrowser, BrowsersReady: make(chan struct{})}
	if reservation.path == "" {
		return h, cmd.Start()
	}
	root := reservation.privateRoot
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("browser private reservation invalid")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return nil, errors.New("browser private user invalid")
	}
	registryRoot, _ := filepath.Abs(filepath.Dir(reservation.path))
	rel, err := filepath.Rel(registryRoot, root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("browser private root outside reservation")
	}
	for current := root; current != filepath.Dir(current); current = filepath.Dir(current) {
		if st, e := os.Lstat(current); e != nil || st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("browser private ancestor invalid")
		}
	}
	hook, err := filepath.Abs("../testfixtures/integration_browser.cjs")
	if err != nil {
		return nil, err
	}
	if len(cmd.Args) < 2 || !strings.HasSuffix(cmd.Args[1], ".cjs") || len(cmd.ExtraFiles) != 0 {
		return nil, errors.New("browser command shape invalid")
	}
	script, err := filepath.Abs(cmd.Args[1])
	if err != nil {
		return nil, err
	}
	private, err := os.MkdirTemp(root, "browser-")
	if err != nil {
		return nil, err
	}
	if err = os.Mkdir(filepath.Join(private, "home"), 0700); err != nil {
		return nil, err
	}
	if err = os.Mkdir(filepath.Join(private, "tmp"), 0700); err != nil {
		return nil, err
	}
	events, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ack, receiver, err := os.Pipe()
	if err != nil {
		events.Close()
		writer.Close()
		return nil, err
	}
	h.events = events
	h.ack = receiver
	h.done = make(chan error, 1)
	cmd.ExtraFiles = []*os.File{writer, ack}
	cmd.Args = append([]string{cmd.Args[0], "--require", hook, script}, cmd.Args[2:]...)
	cmd.Dir = private
	// Replace only controlled internal values, never append ambient secrets.
	filtered := make([]string, 0, len(cmd.Env)+4)
	for _, value := range cmd.Env {
		key, _, _ := strings.Cut(value, "=")
		if key != "HOME" && key != "TMPDIR" && key != "IM_BROWSER_LIFECYCLE_WRITE_FD" && key != "IM_BROWSER_LIFECYCLE_ACK_FD" {
			filtered = append(filtered, value)
		}
	}
	cmd.Env = append(filtered, "HOME="+filepath.Join(private, "home"), "TMPDIR="+filepath.Join(private, "tmp"), "IM_BROWSER_LIFECYCLE_WRITE_FD=3", "IM_BROWSER_LIFECYCLE_ACK_FD=4")
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 5 * time.Second
	}
	if err = cmd.Start(); err != nil {
		events.Close()
		writer.Close()
		ack.Close()
		receiver.Close()
		return nil, err
	}
	writer.Close()
	ack.Close()
	h.node, err = RegisterIntegrationProcess(cmd, gate, test)
	if err != nil {
		receiver.Close()
		events.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	if _, err = io.WriteString(receiver, "node_registered\n"); err != nil {
		receiver.Close()
		events.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	go h.observe(gate, test)
	return h, nil
}

func (h *IntegrationBrowser) observe(gate, test string) {
	defer h.events.Close()
	defer h.ack.Close()
	type message struct {
		Event string `json:"event"`
		PID   int    `json:"pid"`
		Exit  string `json:"actual_exit"`
	}
	type child struct {
		proof         *IntegrationProcess
		ready, exited bool
	}
	children := map[int]*child{}
	nodeReady := false
	var once sync.Once
	var result error
	scanner := bufio.NewScanner(h.events)
	scanner.Buffer(make([]byte, 4096), 16384)
	for scanner.Scan() {
		var value message
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		var trailing any
		if decoder.Decode(&value) != nil || value.PID <= 0 || decoder.Decode(&trailing) != io.EOF {
			result = errors.New("browser lifecycle message invalid")
			break
		}
		switch value.Event {
		case "node_ready":
			if value.PID != h.cmd.Process.Pid || nodeReady || value.Exit != "" {
				result = errors.New("browser Node readiness invalid")
				break
			}
			nodeReady = true
			result = h.node.Ready()
		case "chrome_started":
			if children[value.PID] != nil || value.Exit != "" {
				result = errors.New("browser child repeated")
				break
			}
			path := os.Getenv("CHROMIUM_EXECUTABLE")
			proof, err := RegisterIntegrationBrowserChild(h.cmd, value.PID, path, gate, test)
			if err != nil {
				result = err
				break
			}
			children[value.PID] = &child{proof: proof}
			_, result = fmt.Fprintf(h.ack, "chrome_registered:%d\n", value.PID)
		case "chrome_ready":
			p := children[value.PID]
			if p == nil || p.ready || p.exited || value.Exit != "" {
				result = errors.New("browser child readiness invalid")
				break
			}
			p.ready = true
			result = p.proof.Ready()
			once.Do(func() { close(h.BrowsersReady) })
		case "chrome_exited":
			p := children[value.PID]
			if p == nil || p.exited || !(strings.HasPrefix(value.Exit, "exit:") || strings.HasPrefix(value.Exit, "signal:")) {
				result = errors.New("browser child exit invalid")
				break
			}
			p.exited = true
			result = p.proof.Exited(value.Exit, value.Exit)
		default:
			result = errors.New("browser lifecycle event invalid")
		}
		if result != nil {
			break
		}
	}
	if result == nil {
		result = scanner.Err()
	}
	if result == nil && !nodeReady {
		result = errors.New("actual Node readiness missing")
	}
	if result == nil && h.requireBrowser && len(children) == 0 {
		result = errors.New("actual Chrome lifecycle missing")
	}
	for _, p := range children {
		if result == nil && (!p.ready || !p.exited) {
			result = errors.New("actual Chrome lifecycle incomplete")
		}
	}
	h.done <- result
}

// The browser PID comes from a dedicated pipe owned by the actual Node child.
// Independently inspect its parent and executable before registering it.
func RegisterIntegrationBrowserChild(parent *exec.Cmd, pid int, path, gate, test string) (*IntegrationProcess, error) {
	if parent == nil || parent.Process == nil || pid <= 0 {
		return nil, errors.New("browser parent invalid")
	}
	ps := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "ppid=,comm=")
	ps.Env = []string{"PATH=/usr/bin:/bin", "TZ=UTC"}
	raw, err := ps.Output()
	if err != nil {
		return nil, errors.New("browser child identity unavailable")
	}
	fields := strings.SplitN(strings.TrimSpace(string(raw)), " ", 2)
	if len(fields) != 2 {
		return nil, errors.New("browser child identity invalid")
	}
	ppid, err := strconv.Atoi(fields[0])
	if err != nil || ppid != parent.Process.Pid {
		return nil, errors.New("browser child parent differs")
	}
	expected, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	actual, err := filepath.EvalSymlinks(strings.TrimSpace(fields[1]))
	if err != nil || actual != expected {
		return nil, errors.New("browser child executable differs")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Path: expected, Dir: parent.Dir, Process: process}
	p, err := RegisterIntegrationProcess(cmd, gate, test)
	if err == nil {
		p.fingerprint["parent_pid"] = parent.Process.Pid
	}
	return p, err
}

func integrationExit(state *os.ProcessState) string {
	if state == nil {
		return "wait:unavailable"
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return "signal:" + status.Signal().String()
	}
	return fmt.Sprintf("exit:%d", state.ExitCode())
}

func (h *IntegrationBrowser) Wait() error {
	err := h.cmd.Wait()
	if h.node == nil {
		return err
	}
	observed := <-h.done
	actual := integrationExit(h.cmd.ProcessState)
	proofErr := h.node.Exited(actual, actual)
	if err != nil {
		return err
	}
	if observed != nil {
		return observed
	}
	return proofErr
}

func IntegrationBrowserCombinedOutput(cmd *exec.Cmd, test string, requireBrowser bool) ([]byte, error) {
	if os.Getenv("IM_TEST_INTEGRATION_REGISTRY") == "" {
		return cmd.CombinedOutput()
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	h, err := StartIntegrationBrowser(cmd, test, requireBrowser)
	if err != nil {
		return output.Bytes(), err
	}
	err = h.Wait()
	return output.Bytes(), err
}
