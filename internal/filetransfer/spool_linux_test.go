//go:build linux

package filetransfer

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"os"
	"testing"
)

func TestFileTransferRealDiskFull(t *testing.T) {
	dir := os.Getenv("IM_TEST_SPOOL_FULL_DIR")
	if dir == "" {
		t.Skip("dedicated bounded tmpfs fixture required")
	}
	if _, _, e := Spool(context.Background(), dir, files.MaxFileSizeBytes, io.LimitReader(transferRepeat{}, files.MaxFileSizeBytes)); !errors.Is(e, files.ErrDependencyUnavailable) {
		t.Fatal("real ENOSPC not rejected", e)
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 0 {
		t.Fatal("failed spool file retained", entries, e)
	}
}
