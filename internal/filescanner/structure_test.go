package filescanner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func scannerFile(t *testing.T, b []byte, name, typ string) (*os.File, files.Metadata) {
	t.Helper()
	f, e := os.CreateTemp(t.TempDir(), "scan-*")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write(b); e != nil {
		t.Fatal(e)
	}
	f.Seek(0, 0)
	t.Cleanup(func() { f.Close() })
	hash := sha256.Sum256(b)
	size := int64(len(b))
	return f, files.Metadata{CreateParams: files.CreateParams{OriginalFilename: name, DeclaredMediaType: typ, DeclaredSizeBytes: size}, ActualSizeBytes: &size, DetectedMediaType: typ, SHA256: hash[:]}
}
func TestFileStructureTextAndMime(t *testing.T) {
	for _, tc := range []struct {
		b              []byte
		name, typ      string
		reject, failed bool
	}{{[]byte("有效 UTF-8\n"), "说明.txt", "text/plain", false, false}, {[]byte{0xff}, "bad.txt", "text/plain", false, true}, {[]byte{'a', 0, 'b'}, "bad.txt", "text/plain", false, true}, {[]byte("<html>bad</html>"), "bad.txt", "text/plain", true, false}, {[]byte("plain"), "wrong.pdf", "text/plain", true, false}, {[]byte("PK\x03\x04"), "zip.txt", "text/plain", true, false}} {
		f, m := scannerFile(t, tc.b, tc.name, tc.typ)
		d, e := CheckStructure(context.Background(), f, m)
		if (e != nil) != tc.failed || (d.State == files.StateRejected) != tc.reject {
			t.Fatal(tc.name, d, e)
		}
		if tc.reject && (d.Engine == "" || d.DefinitionVersion == "") {
			t.Fatal("unattributed structural rejection")
		}
	}
}
func TestFileStructureImageBound(t *testing.T) {
	for _, w := range []int{8000, 8001} {
		var b bytes.Buffer
		if e := png.Encode(&b, image.NewGray(image.Rect(0, 0, w, 5000))); e != nil {
			t.Fatal(e)
		}
		f, m := scannerFile(t, b.Bytes(), "picture.png", "image/png")
		d, e := CheckStructure(context.Background(), f, m)
		if w == 8000 && (e != nil || d.State != "") {
			t.Fatal(d, e)
		}
		if w == 8001 && d.State != files.StateRejected {
			t.Fatal("pixel cap bypassed", d, e)
		}
	}
	var b bytes.Buffer
	png.Encode(&b, image.NewGray(image.Rect(0, 0, 3, 3)))
	f, m := scannerFile(t, b.Bytes()[:len(b.Bytes())-8], "broken.png", "image/png")
	if _, e := CheckStructure(context.Background(), f, m); e == nil {
		t.Fatal("truncated image accepted")
	}
}
func TestFileStructurePDFExitCodes(t *testing.T) {
	for _, tc := range []struct {
		encrypted, check int
		reject, failed   bool
	}{{0, 0, true, false}, {2, 0, false, false}, {2, 2, false, true}, {2, 3, false, true}, {1, 0, false, true}} {
		dir := t.TempDir()
		script := filepath.Join(dir, "qpdf")
		s := "#!/bin/sh\ncase \"$1\" in --version) echo 'qpdf version test'; exit 0;; --is-encrypted) exit " + string(rune('0'+tc.encrypted)) + ";; --check) exit " + string(rune('0'+tc.check)) + ";; esac\n"
		if e := os.WriteFile(script, []byte(s), 0700); e != nil {
			t.Fatal(e)
		}
		f, m := scannerFile(t, []byte("%PDF-1.7\n"), "document.pdf", "application/pdf")
		d, e := checkStructure(context.Background(), f, m, script)
		if (e != nil) != tc.failed || (d.State == files.StateRejected) != tc.reject {
			t.Fatal(tc, d, e)
		}
	}
}

func TestFileStructureJPEGAndProcessFailure(t *testing.T) {
	var b bytes.Buffer
	jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 4, 4)), nil)
	f, m := scannerFile(t, b.Bytes(), "test.jpeg", "image/jpeg")
	if d, e := CheckStructure(context.Background(), f, m); e != nil || d.State != "" {
		t.Fatal(d, e)
	}
	f, m = scannerFile(t, b.Bytes()[:len(b.Bytes())/2], "broken.jpg", "image/jpeg")
	if _, e := CheckStructure(context.Background(), f, m); e == nil {
		t.Fatal("truncated jpeg accepted")
	}
	for _, body := range []string{"kill -KILL $$", "sleep 10"} {
		script := filepath.Join(t.TempDir(), "qpdf")
		os.WriteFile(script, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'qpdf version test'; exit 0; fi\n"+body+"\n"), 0700)
		f, m = scannerFile(t, validPDF(), "test.pdf", "application/pdf")
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		d, e := checkStructure(ctx, f, m, script)
		cancel()
		if e == nil || d.State == files.StateReady {
			t.Fatal("process failure accepted", d, e)
		}
	}
}
