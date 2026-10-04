package filescanner

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"os"
	"path/filepath"
)

const eicarTest = "X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*"

type repeatedZero struct{}

func (repeatedZero) Read(b []byte) (int, error) { clear(b); return len(b), nil }
func zipProbe(entries int, childSize int64) ([]byte, error) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for i := 0; i < entries; i++ {
		h := &zip.FileHeader{Name: filepath.Base("probe") + string(rune('a'+i)) + ".txt", Method: zip.Deflate}
		w, e := z.CreateHeader(h)
		if e != nil {
			return nil, e
		}
		if _, e = io.CopyN(w, repeatedZero{}, childSize); e != nil {
			return nil, e
		}
	}
	if e := z.Close(); e != nil {
		return nil, e
	}
	return b.Bytes(), nil
}
func nestedEICARProbe() ([]byte, error) {
	var inner bytes.Buffer
	z := zip.NewWriter(&inner)
	w, e := z.CreateHeader(&zip.FileHeader{Name: "padding.txt", Method: zip.Store})
	if e != nil {
		return nil, e
	}
	if _, e = io.CopyN(w, repeatedZero{}, 401*65536); e != nil {
		return nil, e
	}
	w, e = z.CreateHeader(&zip.FileHeader{Name: "eicar.txt", Method: zip.Store})
	if e != nil {
		return nil, e
	}
	if _, e = io.WriteString(w, eicarTest); e != nil {
		return nil, e
	}
	if e = z.Close(); e != nil {
		return nil, e
	}
	var outer bytes.Buffer
	z = zip.NewWriter(&outer)
	w, e = z.Create("nested.zip")
	if e != nil {
		return nil, e
	}
	if _, e = w.Write(inner.Bytes()); e != nil {
		return nil, e
	}
	if e = z.Close(); e != nil {
		return nil, e
	}
	return outer.Bytes(), nil
}
func requireProbe(ctx context.Context, c Config, b []byte, state files.State, limits bool) error {
	d, e := scanClamd(ctx, c.ClamdSocket, bytes.NewReader(b), int64(len(b)))
	if limits {
		if d.State != files.StateScanFailed || !errors.Is(e, ErrScannerLimits) {
			return ErrRuntimeUnavailable
		}
		return nil
	}
	if e != nil || d.State != state {
		return ErrRuntimeUnavailable
	}
	return nil
}
func runtimeProbes(ctx context.Context, c Config) error {
	if requireProbe(ctx, c, []byte("valid UTF-8 text"), files.StateReady, false) != nil {
		return ErrRuntimeUnavailable
	}
	if requireProbe(ctx, c, []byte(eicarTest), files.StateRejected, false) != nil {
		return ErrRuntimeUnavailable
	}
	// A standalone standard signature at the end of an oversized nested ZIP must
	// be detected. An OK response proves partial scanning and disables the runtime.
	b, e := nestedEICARProbe()
	if e != nil {
		return ErrRuntimeUnavailable
	}
	if e = requireProbe(ctx, c, b, files.StateRejected, false); e != nil {
		return e
	}
	b, e = zipProbe(1, 262144001)
	if e != nil {
		return ErrRuntimeUnavailable
	}
	if e = requireProbe(ctx, c, b, files.StateScanFailed, true); e != nil {
		return e
	}
	b, e = zipProbe(10001, 1)
	if e != nil {
		return ErrRuntimeUnavailable
	}
	if e = requireProbe(ctx, c, b, files.StateScanFailed, true); e != nil {
		return e
	}
	b = []byte("valid text")
	for i := 0; i < 20; i++ {
		var out bytes.Buffer
		z := zip.NewWriter(&out)
		w, e := z.Create("nested.zip")
		if e != nil {
			return ErrRuntimeUnavailable
		}
		w.Write(b)
		if e = z.Close(); e != nil {
			return ErrRuntimeUnavailable
		}
		b = out.Bytes()
	}
	if e = requireProbe(ctx, c, b, files.StateScanFailed, true); e != nil {
		return e
	}
	dir, e := os.MkdirTemp("", "im-file-scan-probe-")
	if e != nil {
		return ErrRuntimeUnavailable
	}
	defer os.RemoveAll(dir)
	plain, encrypted := filepath.Join(dir, "plain.pdf"), filepath.Join(dir, "encrypted.pdf")
	if code, e := commandExit(ctx, c.QPDFPath, "--empty", plain); e != nil || code != 0 {
		return ErrRuntimeUnavailable
	}
	if code, e := commandExit(ctx, c.QPDFPath, "--encrypt", "test-user", "test-owner", "256", "--", plain, encrypted); e != nil || code != 0 {
		return ErrRuntimeUnavailable
	}
	b, e = os.ReadFile(encrypted)
	if e != nil {
		return ErrRuntimeUnavailable
	}
	if e = requireProbe(ctx, c, b, files.StateRejected, false); e != nil {
		return e
	}
	b = make([]byte, 512)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x80)
	copy(b[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[0x84:], 0x14c)
	binary.LittleEndian.PutUint16(b[0x86:], 1)
	binary.LittleEndian.PutUint16(b[0x94:], 224)
	binary.LittleEndian.PutUint16(b[0x98:], 0x10b)
	binary.LittleEndian.PutUint32(b[0xb8:], 0x1000)
	binary.LittleEndian.PutUint32(b[0xbc:], 0x200)
	binary.LittleEndian.PutUint32(b[0x188:], 0x10000)
	binary.LittleEndian.PutUint32(b[0x18c:], 0x200)
	if e = requireProbe(ctx, c, b, files.StateRejected, false); e != nil {
		return e
	}
	return nil
}
