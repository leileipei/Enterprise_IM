package policystore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Faults modify only an already successful response from the official API.
// They cannot turn any failure into success or supply business DTOs.
type processCutBody struct {
	io.ReadCloser
	remaining int64
}

func (b *processCutBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, e := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, e
}

type processSlowBody struct {
	io.ReadCloser
	ctx   context.Context
	first bool
}

func (b *processSlowBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		return b.ReadCloser.Read(p)
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-time.After(66 * time.Second):
		return b.ReadCloser.Read(p)
	}
}
func (f *fileBusinessProcessFixture) webTransport(res *http.Response) error {
	if res.StatusCode != 200 || res.Request.Method != "GET" || !strings.HasSuffix(res.Request.URL.Path, "/content") {
		return nil
	}
	fault := res.Request.Header.Get("X-P426-Transport-Fault")
	for i, name := range []string{"cut", "length", "redirect", "oversize", "compressed", "slow-read"} {
		if fault == name {
			f.webFaultCounts[i].Add(1)
		}
	}
	switch fault {
	case "cut":
		res.Body = &processCutBody{res.Body, max(1, res.ContentLength/2)}
	case "length":
		res.ContentLength++
		res.Header.Set("Content-Length", fmt.Sprint(res.ContentLength))
	case "redirect":
		res.Body.Close()
		res.Body = io.NopCloser(bytes.NewReader(nil))
		res.ContentLength = 0
		res.StatusCode = 307
		res.Header.Set("Location", "https://127.0.0.1:1/transport-negative")
		res.Header.Set("Content-Length", "0")
	case "oversize":
		res.ContentLength = 26214401
		res.Header.Set("Content-Length", "26214401")
	case "compressed":
		res.Header.Set("Content-Encoding", "gzip")
	case "slow-read":
		res.Body = &processSlowBody{ReadCloser: res.Body, ctx: res.Request.Context()}
	}
	return nil
}
func (f *fileBusinessProcessFixture) browser(t *testing.T, scenario string, options map[string]any) {
	t.Helper()
	dir, e := os.MkdirTemp(f.privateRoot, "chrome-")
	if e != nil {
		t.Fatal(e)
	}
	options["baseURL"] = f.webURL
	options["evidenceDir"] = dir
	options["scenario"] = scenario
	observation := filepath.Join(dir, "observation.json")
	options["observationFile"] = observation
	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			check, cancel := context.WithTimeout(monitorCtx, time.Second)
			var pending int
			e := f.pool.QueryRow(check, "SELECT count(*) FROM file_download_sessions WHERE NOT audit_acked").Scan(&pending)
			cancel()
			faults := map[string]int64{}
			for i, name := range []string{"cut", "length", "redirect", "oversize", "compressed", "slow-read"} {
				faults[name] = f.webFaultCounts[i].Load()
			}
			record, _ := json.Marshal(map[string]any{"idle": e == nil && pending == 0, "pending": pending, "faults": faults})
			tmp := observation + ".tmp"
			if os.WriteFile(tmp, record, 0600) == nil {
				os.Rename(tmp, observation)
			}
			select {
			case <-monitorCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { stopMonitor(); <-monitorDone }()
	b, e := json.Marshal(options)
	if e != nil {
		t.Fatal(e)
	}
	script, e := filepath.Abs("../webclient/e2e/file_business_runtime.cjs")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("IM_TEST_BROWSER_NODE"), script)
	cmd.Stdin = bytes.NewReader(b)
	cmd.Env = processChildEnv(map[string]string{"NODE_PATH": os.Getenv("NODE_PATH"), "CHROMIUM_EXECUTABLE": os.Getenv("CHROMIUM_EXECUTABLE")})
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	out, e := cmd.CombinedOutput()
	processPrivateFile(t, filepath.Join(dir, "browser.log"), out)
	if e != nil {
		t.Fatal("official Chrome scenario failed", scenario, "private browser.log retained")
	}
	var facts map[string]bool
	if json.Unmarshal(bytes.TrimSpace(out), &facts) != nil || len(facts) == 0 {
		t.Fatal("official Chrome facts invalid")
	}
	for name, ok := range facts {
		if !ok {
			t.Fatal("official Chrome false fact", name)
		}
	}
	record, _ := json.MarshalIndent(facts, "", "  ")
	processPrivateFile(t, filepath.Join(dir, "facts.json"), record)
	t.Log("official Chrome scenario", scenario, "assertions", len(facts))
}
func TestFileBusinessProcessRP14(t *testing.T) {
	for _, scenario := range []string{"unknown", "ack_pull", "group_unknown", "context", "settings", "faults", "long", "extra_transport"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFileBusinessProcessFixture(t)
			f.startWorkers(t)
			f.startAPI(t, true, true, "api-a")
			f.restartRepair(t, false)
			samples := (&webFileFixture{}).samples(t, false)
			samples = samples[:1]
			samples[0].Name = "上下文选择.txt"
			options := map[string]any{"conversation": directA, "kind": "direct", "samples": samples}
			mode := "unknown"
			switch scenario {
			case "unknown":
				options["unknown"] = true
			case "ack_pull":
				options["ackPullFault"] = true
				mode = "lifecycle"
			case "group_unknown":
				options["conversation"] = f.group(t)
				options["kind"] = "group"
				options["unknown"] = true
				options["groupComposer"] = true
			case "context", "faults", "long", "extra_transport":
				body := bytes.Repeat([]byte("context bytes"), 8192)
				name := "上下文现有.txt"
				id := f.upload(t, "api-a", directA, name, "text/plain", body, true)
				f.sendFile(t, "api-a", directA, "direct", id, businessClientID())
				options["name"] = name
				options["fileID"] = id
				options["sample"] = samples[0]
				mode = scenario
				if scenario == "long" {
					mode = "existing_download"
					options["expectedPath"] = filepath.Join(f.privateRoot, "long-source.txt")
					processPrivateFile(t, options["expectedPath"].(string), body)
					g := f.gate(t, id)
					ctx, stop := context.WithCancel(context.Background())
					defer stop()
					go func() {
						select {
						case <-g.entered:
						case <-ctx.Done():
							return
						}
						timer := time.NewTimer(12 * time.Second)
						defer timer.Stop()
						select {
						case <-timer.C:
							close(g.release)
						case <-ctx.Done():
							return
						}
					}()
				}
				if scenario == "extra_transport" {
					mode = "existing_faults"
				}
			case "settings":
				run(t, f.conn, `UPDATE tenant_file_upload_policy SET version=9007199254740993 WHERE tenant_id=$1`, tenantA)
				mode = "settings"
				options["conflict"] = true
			}
			f.browser(t, mode, options)
			if scenario == "faults" {
				for i := 0; i < 4; i++ {
					if f.webFaultCounts[i].Load() != 1 {
						t.Fatal("mandatory actual successful response fault missing or repeated", i)
					}
				}
			}
			if scenario == "extra_transport" {
				for i := 4; i < 6; i++ {
					if f.webFaultCounts[i].Load() != 1 {
						t.Fatal("mandatory extra transport fault missing or repeated", i)
					}
				}
			}
			f.assertEvidence(t)
		})
	}
}
