package filescanner

import (
	"context"
	"encoding/binary"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

func sniffStructure(b []byte) string {
	s := http.DetectContentType(b)
	if strings.HasPrefix(s, "text/plain;") {
		s = "text/plain"
	}
	return s
}
func clamdConnection(ctx context.Context, path string) (net.Conn, func(), error) {
	d := net.Dialer{}
	c, e := d.DialContext(ctx, "unix", path)
	if e != nil {
		return nil, nil, ErrRuntimeUnavailable
	}
	deadline := time.Now().Add(90 * time.Second)
	if at, ok := ctx.Deadline(); ok && at.Before(deadline) {
		deadline = at
	}
	c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	return c, func() { stop(); c.Close() }, nil
}
func clamdReply(c net.Conn) (string, error) {
	b, e := io.ReadAll(io.LimitReader(c, 4097))
	if e != nil || len(b) < 1 || len(b) > 4096 || b[len(b)-1] != 0 || strings.Count(string(b), "\x00") != 1 {
		return "", ErrScannerProtocol
	}
	s := string(b[:len(b)-1])
	if strings.ContainsAny(s, "\r\n") {
		return "", ErrScannerProtocol
	}
	return s, nil
}
func clamdCommand(ctx context.Context, path, command string) (string, error) {
	c, close, e := clamdConnection(ctx, path)
	if e != nil {
		return "", e
	}
	defer close()
	if _, e = io.WriteString(c, "z"+command+"\x00"); e != nil {
		return "", ErrRuntimeUnavailable
	}
	return clamdReply(c)
}
func failedScan(reason string) files.ScanDecision {
	return files.ScanDecision{State: files.StateScanFailed, ReasonCode: reason}
}
func scanClamd(ctx context.Context, path string, r io.Reader, size int64) (files.ScanDecision, error) {
	c, close, e := clamdConnection(ctx, path)
	if e != nil {
		return failedScan("scanner_unavailable"), e
	}
	defer close()
	if _, e = io.WriteString(c, "zINSTREAM\x00"); e != nil {
		return failedScan("scanner_unavailable"), ErrRuntimeUnavailable
	}
	buf := make([]byte, 32768)
	header := make([]byte, 4)
	total := int64(0)
	for {
		if ctx.Err() != nil {
			return failedScan("scanner_unavailable"), ErrRuntimeUnavailable
		}
		n, e := r.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > size {
				return failedScan("object_mismatch"), ErrStructure
			}
			binary.BigEndian.PutUint32(header, uint32(n))
			if _, e2 := c.Write(header); e2 != nil {
				return failedScan("scanner_unavailable"), ErrRuntimeUnavailable
			}
			if _, e2 := c.Write(buf[:n]); e2 != nil {
				return failedScan("scanner_unavailable"), ErrRuntimeUnavailable
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return failedScan("object_read_failed"), ErrStructure
		}
	}
	if total != size {
		return failedScan("object_mismatch"), ErrStructure
	}
	if _, e = c.Write([]byte{0, 0, 0, 0}); e != nil {
		return failedScan("scanner_unavailable"), ErrRuntimeUnavailable
	}
	reply, e := clamdReply(c)
	if e != nil {
		return failedScan("scanner_protocol_error"), e
	}
	if strings.Contains(reply, "Heuristics.Limits.Exceeded.") || strings.Contains(reply, "INSTREAM size limit exceeded") {
		return failedScan("scanner_limits"), ErrScannerLimits
	}
	if reply == "stream: OK" {
		return files.ScanDecision{State: files.StateReady, ReasonCode: "scan_clean"}, nil
	}
	if strings.HasPrefix(reply, "stream: ") && strings.HasSuffix(reply, " FOUND") && len(reply) > len("stream:  FOUND") {
		return files.ScanDecision{State: files.StateRejected, ReasonCode: "scan_rejected"}, nil
	}
	if strings.HasSuffix(reply, " ERROR") {
		return failedScan("scanner_unavailable"), ErrRuntimeUnavailable
	}
	return failedScan("scanner_protocol_error"), ErrScannerProtocol
}
