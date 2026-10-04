package filescanner

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func protocolSocket(t *testing.T, response string) string {
	t.Helper()
	dir, e := os.MkdirTemp("/tmp", "im22-protocol-")
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "clamd.sock")
	listener, e := net.Listen("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { listener.Close(); os.RemoveAll(dir) })
	go func() {
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		command := make([]byte, 10)
		io.ReadFull(c, command)
		if string(command) != "zINSTREAM\x00" {
			t.Error("invalid framing")
		}
		for {
			var size uint32
			if e = binary.Read(c, binary.BigEndian, &size); e != nil {
				return
			}
			if size == 0 {
				break
			}
			if size > 32768 {
				t.Error("unbounded frame")
			}
			if _, e = io.CopyN(io.Discard, c, int64(size)); e != nil {
				return
			}
		}
		c.Write([]byte(response))
	}()
	return path
}
func TestClamdStrictFrames(t *testing.T) {
	for _, tc := range []struct {
		reply  string
		state  files.State
		reason string
	}{{"stream: OK\x00", files.StateReady, "scan_clean"}, {"stream: Eicar-Test FOUND\x00", files.StateRejected, "scan_rejected"}, {"stream: Heuristics.Limits.Exceeded.MaxFileSize FOUND\x00", files.StateScanFailed, "scanner_limits"}, {"INSTREAM size limit exceeded. ERROR\x00", files.StateScanFailed, "scanner_limits"}, {"stream: broken ERROR\x00", files.StateScanFailed, "scanner_unavailable"}, {"stream: OK", files.StateScanFailed, "scanner_protocol_error"}, {"stream: OK\x00stream: OK\x00", files.StateScanFailed, "scanner_protocol_error"}, {"UNKNOWN\x00", files.StateScanFailed, "scanner_protocol_error"}, {strings.Repeat("x", 4097), files.StateScanFailed, "scanner_protocol_error"}} {
		path := protocolSocket(t, tc.reply)
		d, e := scanClamd(context.Background(), path, bytes.NewReader([]byte("x")), 1)
		if d.State != tc.state || d.ReasonCode != tc.reason {
			t.Fatal(tc, d, e)
		}
	}
}
func TestClamdFreshRuntimeAndLimits(t *testing.T) {
	e := RuntimeEvidence{EngineVersion: "1.5.4", DefinitionVersion: "28142", DefinitionsUpdatedAt: time.Now().Add(-23 * time.Hour), StreamMaxLengthBytes: 26214400, MaxFileSizeBytes: 26214400, MaxScanSizeBytes: 262144000, AlertExceedsMax: true, AlertEncrypted: true, AlertBroken: true}
	if err := validateRuntimeEvidence(e, time.Now()); err != nil {
		t.Fatal(err)
	}
	old := e
	old.DefinitionsUpdatedAt = time.Now().Add(-24*time.Hour - time.Second)
	if err := validateRuntimeEvidence(old, time.Now()); err == nil {
		t.Fatal("stale definitions accepted")
	}
	old = e
	old.AlertExceedsMax = false
	if err := validateRuntimeEvidence(old, time.Now()); err == nil {
		t.Fatal("missing limits accepted")
	}
	old = e
	old.MaxScanSizeBytes = 0
	if err := validateRuntimeEvidence(old, time.Now()); err == nil {
		t.Fatal("unknown scan budget accepted")
	}
}
