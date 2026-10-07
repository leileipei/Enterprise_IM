package importinput

import (
	"context"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"io"
	"os"
)

type Reader func(context.Context, string) ([]byte, *p.Issue, error)
type inputResult struct {
	data  []byte
	issue *p.Issue
	err   error
}

func Read(ctx context.Context, path string) ([]byte, *p.Issue, error) {
	if e := p.ContextFailure(ctx); e != nil {
		return nil, nil, e
	}
	done := make(chan inputResult, 1)
	go func() { b, i, e := readFile(ctx, path); done <- inputResult{b, i, e} }()
	select {
	case <-ctx.Done():
		return nil, nil, p.ContextFailure(ctx)
	case r := <-done:
		if e := p.ContextFailure(ctx); e != nil {
			return nil, nil, e
		}
		return r.data, r.issue, r.err
	}
}
func readFile(ctx context.Context, path string) ([]byte, *p.Issue, error) {
	f, e := OpenRegular(path)
	if e != nil {
		return nil, nil, e
	}
	defer f.Close()
	initial, e := f.Stat()
	if e != nil {
		return nil, nil, p.Failure{Code: "INPUT_READ_FAILED"}
	}
	data := make([]byte, 0, 65536)
	buf := make([]byte, 65536)
	for {
		if e := p.ContextFailure(ctx); e != nil {
			return nil, nil, e
		}
		remaining := p.MaxInput + 1 - len(data)
		want := len(buf)
		if remaining < want {
			want = remaining
		}
		n, e := f.Read(buf[:want])
		data = append(data, buf[:n]...)
		if len(data) > p.MaxInput {
			return nil, &p.Issue{Entity: "document", Field: "_document", Code: "INPUT_TOO_LARGE"}, nil
		}
		if e == io.EOF {
			break
		}
		if e != nil || n == 0 {
			return nil, nil, p.Failure{Code: "INPUT_READ_FAILED"}
		}
	}
	if e := VerifyOpened(initial, f); e != nil {
		return nil, nil, e
	}
	end, e := os.Lstat(path)
	if e != nil || !end.Mode().IsRegular() || !os.SameFile(initial, end) || initial.Size() != end.Size() || !initial.ModTime().Equal(end.ModTime()) {
		return nil, nil, p.Failure{Code: "INPUT_TYPE_UNSUPPORTED"}
	}
	if e := p.ContextFailure(ctx); e != nil {
		return nil, nil, e
	}
	return data, nil, nil
}
