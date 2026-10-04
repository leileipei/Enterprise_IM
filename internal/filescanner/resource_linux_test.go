//go:build linux

package filescanner

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"os"
	"testing"
)

func TestScannerRealResourceBoundary(t *testing.T) {
	dir := os.Getenv("IM_TEST_STRUCTURE_SAMPLES")
	if dir == "" {
		t.Skip("requires controlled resource fixtures; explicit resource gate supplies them")
	}
	for _, name := range []string{"40m.png", "40m-plus-one.png"} {
		b, e := os.ReadFile(dir + "/" + name)
		if e != nil {
			t.Fatal(e)
		}
		f, m := scannerFile(t, b, name, "image/png")
		d, e := CheckStructure(context.Background(), f, m)
		if name == "40m.png" && (e != nil || d.State != "") {
			t.Fatal(d, e)
		}
		if name == "40m-plus-one.png" && (e != nil || d.State != files.StateRejected) {
			t.Fatal("pixel budget bypass", d, e)
		}
	}
}
