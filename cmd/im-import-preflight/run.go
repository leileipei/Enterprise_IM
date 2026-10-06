package main

import (
	"context"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"io"
	"time"
)

const usage = "usage: im-import-preflight --input FILE | --help | --version\n"

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWithReader(ctx, args, stdout, stderr, readInput)
}

// The buffered result avoids retaining a blocked output operation in the command's control path.
func writeComplete(ctx context.Context, w io.Writer, b []byte) error {
	done := make(chan error, 1)
	go func() {
		n, e := w.Write(b)
		if e == nil && n != len(b) {
			e = io.ErrShortWrite
		}
		done <- e
	}()
	select {
	case <-ctx.Done():
		return p.ContextFailure(ctx)
	case e := <-done:
		return e
	}
}
func runWithReader(parent context.Context, args []string, stdout, stderr io.Writer, reader inputReader) int {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	a, e := parseArguments(args)
	if e != nil {
		_ = writeComplete(ctx, stderr, []byte(usage))
		return 2
	}
	if a.Help || a.Version {
		text := usage
		if a.Version {
			text = "im-import-preflight 0.1.0\n"
		}
		if writeComplete(ctx, stdout, []byte(text)) != nil {
			diagCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			_ = writeComplete(diagCtx, stderr, []byte("OUTPUT_WRITE_FAILED\n"))
			return 2
		}
		return 0
	}
	b, issue, e := reader(ctx, a.Input)
	var r p.Report
	incomplete := func(code p.Code) p.Report {
		c := p.NewCollector()
		c.Add(p.Issue{Entity: "document", Field: "_document", Code: code})
		return p.BuildReport(p.Outcome{Status: "incomplete"}, c)
	}
	if e != nil {
		code := p.Code("INPUT_READ_FAILED")
		if f, ok := e.(p.Failure); ok {
			code = f.Code
		}
		r = incomplete(code)
	} else if issue != nil {
		c := p.NewCollector()
		c.Add(*issue)
		r = p.BuildReport(p.Outcome{Status: "invalid"}, c)
	} else {
		r = p.Evaluate(ctx, b)
	}
	// A canceled context must still permit a bounded final incomplete report.
	outCtx := ctx
	if e := p.ContextFailure(ctx); e != nil {
		r = incomplete(e.(p.Failure).Code)
		var stop context.CancelFunc
		outCtx, stop = context.WithTimeout(context.Background(), time.Second)
		defer stop()
	}
	encoded, e := p.EncodeReport(r)
	if e != nil || writeComplete(outCtx, stdout, encoded) != nil {
		diagCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = writeComplete(diagCtx, stderr, []byte("OUTPUT_WRITE_FAILED\n"))
		return 2
	}
	if e := p.ContextFailure(ctx); e != nil {
		return 2
	}
	return r.ExitCode()
}
