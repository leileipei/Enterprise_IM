package filescanner

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxImagePixels int64 = 40000000

var ErrStructure = errors.New("incomplete file structure")
var ErrRuntimeUnavailable = errors.New("scanner runtime unavailable")
var ErrScannerProtocol = errors.New("invalid scanner response")
var ErrScannerLimits = errors.New("scanner limits exceeded")
var ErrDefinitionsStale = errors.New("scanner definitions stale")

func rejectedStructure(m files.Metadata, engine, version, reason string) files.ScanDecision {
	d := files.ScanDecision{State: files.StateRejected, Engine: engine, DefinitionVersion: version, ReasonCode: reason}
	copy(d.SHA256[:], m.SHA256)
	return d
}
func checkText(ctx context.Context, f *os.File) error {
	if _, e := f.Seek(0, 0); e != nil {
		return ErrStructure
	}
	buf := make([]byte, 32768+utf8.UTFMax)
	pending := 0
	for {
		if ctx.Err() != nil {
			return ErrStructure
		}
		n, e := f.Read(buf[pending:32768])
		b := buf[:pending+n]
		i := 0
		for i < len(b) {
			if !utf8.FullRune(b[i:]) {
				break
			}
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 || r == 0 {
				return ErrStructure
			}
			i += size
		}
		pending = copy(buf, b[i:])
		if e == io.EOF {
			if pending != 0 {
				return ErrStructure
			}
			return nil
		}
		if e != nil {
			return ErrStructure
		}
	}
}
func commandExit(ctx context.Context, path string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	e := cmd.Run()
	if ctx.Err() != nil {
		return -1, ErrStructure
	}
	if e == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(e, &exit) {
		return exit.ExitCode(), nil
	}
	return -1, ErrRuntimeUnavailable
}
func qpdfVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	value, e := processOutput(ctx, path, "--version")
	if e != nil || len(value) > 4096 {
		return "", ErrRuntimeUnavailable
	}
	first := strings.SplitN(value, "\n", 2)[0]
	if !strings.HasPrefix(first, "qpdf version ") {
		return "", ErrRuntimeUnavailable
	}
	v := strings.TrimPrefix(first, "qpdf version ")
	if len(v) == 0 || len(v) > 64 || strings.ContainsAny(v, "\r\n ") {
		return "", ErrRuntimeUnavailable
	}
	return "qpdf-" + v + "/rules-v1", nil
}
func CheckStructure(ctx context.Context, f *os.File, m files.Metadata) (files.ScanDecision, error) {
	qpdf, e := exec.LookPath("qpdf")
	if e != nil && m.DeclaredMediaType == "application/pdf" {
		return files.ScanDecision{}, ErrRuntimeUnavailable
	}
	return checkStructure(ctx, f, m, qpdf)
}
func checkStructure(parent context.Context, f *os.File, m files.Metadata, qpdf string) (files.ScanDecision, error) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	var zero files.ScanDecision
	if f == nil {
		return zero, ErrStructure
	}
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() != m.DeclaredSizeBytes || st.Size() < 1 || st.Size() > files.MaxFileSizeBytes {
		return zero, ErrStructure
	}
	if m.DeclaredMediaType == "text/plain" {
		if e = checkText(ctx, f); e != nil {
			return zero, e
		}
	}
	ext := strings.ToLower(filepath.Ext(m.OriginalFilename))
	allowed := false
	engine := "structure/utf8"
	version := runtime.Version() + "/rules-v1"
	switch m.DeclaredMediaType {
	case "text/plain":
		allowed = ext == ".txt"
	case "image/png":
		allowed = ext == ".png"
		engine = "structure/image"
	case "image/jpeg":
		allowed = ext == ".jpg" || ext == ".jpeg"
		engine = "structure/image"
	case "application/pdf":
		allowed = ext == ".pdf"
		engine = "structure/qpdf"
		version, e = qpdfVersion(ctx, qpdf)
		if e != nil {
			return zero, e
		}
	}
	if !allowed || m.DetectedMediaType != m.DeclaredMediaType {
		return rejectedStructure(m, engine, version, "type_not_allowed"), nil
	}
	prefix := make([]byte, 512)
	n, e := f.ReadAt(prefix, 0)
	if e != nil && e != io.EOF {
		return zero, ErrStructure
	}
	actual := sniffStructure(prefix[:n])
	if actual != m.DeclaredMediaType {
		return rejectedStructure(m, engine, version, "type_not_allowed"), nil
	}
	if _, e = f.Seek(0, 0); e != nil {
		return zero, ErrStructure
	}
	switch m.DeclaredMediaType {
	case "image/png", "image/jpeg":
		cfg, format, e := image.DecodeConfig(f)
		if e != nil || format != "png" && format != "jpeg" {
			return zero, ErrStructure
		}
		if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width) > MaxImagePixels/int64(cfg.Height) {
			return rejectedStructure(m, engine, version, "structure_invalid"), nil
		}
		f.Seek(0, 0)
		decoded, actualFormat, e := image.Decode(f)
		if e != nil || actualFormat != format || decoded.Bounds().Dx() != cfg.Width || decoded.Bounds().Dy() != cfg.Height {
			return zero, ErrStructure
		}
		if ctx.Err() != nil {
			return zero, ErrStructure
		}
	case "application/pdf":
		encrypted, e := commandExit(ctx, qpdf, "--is-encrypted", f.Name())
		if e != nil {
			return zero, e
		}
		if encrypted == 0 {
			return rejectedStructure(m, engine, version, "encrypted_file"), nil
		}
		if encrypted != 2 {
			return zero, ErrStructure
		}
		checked, e := commandExit(ctx, qpdf, "--check", f.Name())
		if e != nil {
			return zero, e
		}
		if checked != 0 {
			return zero, ErrStructure
		}
	}
	if _, e = f.Seek(0, 0); e != nil {
		return zero, ErrStructure
	}
	return zero, nil
}
