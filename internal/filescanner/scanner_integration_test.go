package filescanner

import (
	"bytes"
	"context"
	"fmt"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestScannerDependenciesAndManifestRequired(t *testing.T) {
	if _, e := New(Config{}); e == nil {
		t.Fatal("missing dependencies accepted")
	}
	if _, e := New(Config{QPDFPath: "/missing", ClamdSocket: "/missing", RuntimeManifestPath: "/missing"}); e == nil {
		t.Fatal("unproven config accepted")
	}
}
func TestScannerRealStructurePDF(t *testing.T) {
	path, e := exec.LookPath("qpdf")
	if e != nil {
		t.Skip("real qpdf fixture required")
	}
	dir := t.TempDir()
	plain := dir + "/plain.pdf"
	encrypted := dir + "/encrypted.pdf"
	if e = os.WriteFile(plain, validPDF(), 0600); e != nil {
		t.Fatal(e)
	}
	if e = exec.Command(path, "--encrypt", "test-user", "test-owner", "256", "--", plain, encrypted).Run(); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		path   string
		reject bool
	}{{plain, false}, {encrypted, true}} {
		b, e := os.ReadFile(tc.path)
		if e != nil {
			t.Fatal(e)
		}
		f, m := scannerFile(t, b, "test.pdf", "application/pdf")
		d, e := CheckStructure(context.Background(), f, m)
		if e != nil || (d.State == files.StateRejected) != tc.reject {
			t.Fatal(d, e)
		}
		if tc.reject && !strings.Contains(d.DefinitionVersion, "12.4.2") {
			t.Fatal("actual qpdf version missing", d)
		}
	}
	b, _ := os.ReadFile(plain)
	f, m := scannerFile(t, b[:len(b)/2], "broken.pdf", "application/pdf")
	if _, e = CheckStructure(context.Background(), f, m); e == nil {
		t.Fatal("real malformed PDF accepted")
	}
}
func TestScannerRealCleanEICARProtocol(t *testing.T) {
	socket := os.Getenv("IM_TEST_CLAMD_SOCKET")
	if socket == "" {
		t.Skip("real dedicated ClamAV required")
	}
	for _, tc := range []struct {
		body  string
		state files.State
	}{{"valid UTF-8 text", files.StateReady}, {"X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*", files.StateRejected}} {
		d, e := scanClamd(context.Background(), socket, strings.NewReader(tc.body), int64(len(tc.body)))
		if e != nil || d.State != tc.state {
			t.Fatal(d, e)
		}
	}
}
func TestScannerRealNoIncompleteReady(t *testing.T) {
	manifest := os.Getenv("IM_TEST_SCANNER_MANIFEST")
	if manifest == "" {
		t.Skip("controlled scanner fixture required")
	}
	s, e := New(Config{QPDFPath: os.Getenv("IM_TEST_QPDF_PATH"), ClamdSocket: os.Getenv("IM_TEST_CLAMD_SOCKET"), RuntimeManifestPath: manifest})
	if e != nil {
		t.Fatal(e)
	}
	f, m := scannerFile(t, []byte("valid UTF-8 text"), "test.txt", "text/plain")
	d, e := s.Scan(context.Background(), f, m)
	if e == nil || d.State == files.StateReady {
		t.Fatal("unproven single-child limit allowed clean", d, e)
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		t.Fatal(e)
	}
}

func TestScannerRealAttestation(t *testing.T) {
	manifest := os.Getenv("IM_TEST_SCANNER_MANIFEST")
	if manifest == "" {
		t.Skip("controlled scanner fixture required")
	}
	c := Config{QPDFPath: os.Getenv("IM_TEST_QPDF_PATH"), ClamdSocket: os.Getenv("IM_TEST_CLAMD_SOCKET"), RuntimeManifestPath: manifest}
	e, stamp, err := attestRuntime(context.Background(), c)
	if err != nil || e.DefinitionVersion == "" || stamp == [32]byte{} {
		t.Fatal(e, err)
	}
	// Runtime binding alone is insufficient: the actual nested-tail probe fails.
	if err = runtimeProbes(context.Background(), c); err == nil {
		t.Fatal("partial scanning went undetected")
	}
}

func validPDF() []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Resources << >> /Contents 4 0 R >>", "<< /Length 0 >>\nstream\nendstream"}
	offsets := []int{0}
	for i, o := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	start := b.Len()
	fmt.Fprintf(&b, "xref\n0 5\n0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", start)
	return b.Bytes()
}

// This is the positive component acceptance gate. A negative fail-closed test
// cannot substitute for proving the controlled deployment can produce ready.
func TestScannerRealTrustedRuntime(t *testing.T) {
	manifest := os.Getenv("IM_TEST_SCANNER_MANIFEST")
	if manifest == "" {
		t.Skip("controlled scanner fixture required")
	}
	s, e := New(Config{QPDFPath: os.Getenv("IM_TEST_QPDF_PATH"), ClamdSocket: os.Getenv("IM_TEST_CLAMD_SOCKET"), RuntimeManifestPath: manifest})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ValidateRuntime(context.Background()); e != nil {
		t.Fatal("runtime acceptance failed", e)
	}
	f, m := scannerFile(t, []byte("valid UTF-8 text"), "valid.txt", "text/plain")
	d, e := s.Scan(context.Background(), f, m)
	if e != nil || d.State != files.StateReady || d.Engine == "" || d.DefinitionVersion == "" {
		t.Fatal(d, e)
	}
}
