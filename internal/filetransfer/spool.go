package filetransfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
)

func privateSpoolDir(dir string) error {
	if dir == "" {
		return files.ErrDependencyUnavailable
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return files.ErrDependencyUnavailable
	}
	st, e := os.Lstat(dir)
	if e != nil || !st.IsDir() || st.Mode().Perm() != 0700 || st.Mode()&os.ModeSymlink != 0 {
		return files.ErrDependencyUnavailable
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return files.ErrDependencyUnavailable
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	n, e := r.r.Read(b)
	if err := r.ctx.Err(); err != nil {
		return n, err
	}
	return n, e
}

// Spool uses a fixed copy buffer; the caller owns the returned file and its removal.
// A plain non-cancelable Reader remains synchronous; no detached reader goroutine is started.
func Spool(ctx context.Context, dir string, declared int64, body io.Reader) (file *os.File, m files.Measurement, err error) {
	if body == nil || declared < 1 || declared > files.MaxFileSizeBytes {
		return nil, m, files.ErrInvalidFileSize
	}
	if e := privateSpoolDir(dir); e != nil {
		return nil, m, e
	}
	if closer, ok := body.(io.ReadCloser); ok {
		stop := context.AfterFunc(ctx, func() { closer.Close() })
		defer stop()
	}
	f, e := os.CreateTemp(dir, "im-upload-*")
	if e != nil {
		return nil, m, files.ErrDependencyUnavailable
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	n, e := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, body}, declared+1), make([]byte, 32768))
	if e != nil {
		if ctx.Err() != nil {
			return nil, m, ctx.Err()
		}
		if errors.Is(e, io.ErrUnexpectedEOF) {
			return nil, m, files.ErrInvalidFileSize
		}
		return nil, m, files.ErrDependencyUnavailable
	}
	if n > declared {
		return nil, m, files.ErrFileTooLarge
	}
	if n != declared {
		return nil, m, files.ErrInvalidFileSize
	}
	if e = f.Sync(); e != nil {
		return nil, m, files.ErrDependencyUnavailable
	}
	sniff := make([]byte, 512)
	nSniff, e := f.ReadAt(sniff, 0)
	if e != nil && e != io.EOF {
		return nil, m, files.ErrDependencyUnavailable
	}
	typ := http.DetectContentType(sniff[:nSniff])
	if strings.HasPrefix(typ, "text/plain;") {
		typ = "text/plain"
	}
	switch typ {
	case "application/pdf", "image/png", "image/jpeg", "text/plain":
	default:
		typ = "application/octet-stream"
	}
	m.SizeBytes = n
	copy(m.SHA256[:], h.Sum(nil))
	m.DetectedMediaType = typ
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return nil, m, files.ErrDependencyUnavailable
	}
	ok = true
	return f, m, nil
}
