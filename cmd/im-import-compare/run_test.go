package main

import (
	"bytes"
	"context"
	c "github.com/leileipei/Enterprise_IM/internal/importcompare"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"io"
	"testing"
	"time"
)

func TestCompareWorkerHelpAndPanic(t *testing.T) {
	reader := func(context.Context, string) ([]byte, *p.Issue, error) { panic("secret-reader") }
	env := func(string) string { panic("help read environment") }
	provider := func(c.Config) c.SnapshotReader { panic("help database") }
	for _, arg := range []string{"--help", "--version"} {
		var out, stderr bytes.Buffer
		if runWorker(context.Background(), []string{arg}, &out, &stderr, reader, env, provider) != 0 {
			t.Fatal("help called dependency")
		}
	}
	var out, stderr bytes.Buffer
	if runWorker(context.Background(), []string{"--input", "a", "--tenant-id", "00000000-0000-4000-8000-000000000001"}, &out, &stderr, reader, env, provider) != 2 || bytes.Contains(out.Bytes(), []byte("secret-reader")) || !bytes.Contains(out.Bytes(), []byte("DATABASE_READ_FAILED")) {
		t.Fatal("recoverable worker panic leaked")
	}
}

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("secret-output") }
func TestCompareOwnOutputPanic(t *testing.T) {
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if Run(ctx, []string{"--version"}, panicWriter{}, &stderr) != 2 || stderr.String() != "OUTPUT_WRITE_FAILED\n" {
		t.Fatal("output goroutine panic escaped")
	}
	_ = io.Discard
}
