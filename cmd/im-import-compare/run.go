package main

import (
	"context"
	"errors"
	c "github.com/leileipei/Enterprise_IM/internal/importcompare"
	"github.com/leileipei/Enterprise_IM/internal/importinput"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"io"
	"time"
)

const usage = "usage: im-import-compare --input FILE --tenant-id UUID | --help | --version\n"
const version = "im-import-compare 0.1.0\n"

func writeComplete(ctx context.Context, w io.Writer, b []byte) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- p.Failure{Code: "OUTPUT_WRITE_FAILED"}
			}
		}()
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

type cleanupBudget struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (b *cleanupBudget) get() context.Context {
	if b.ctx == nil {
		b.ctx, b.cancel = context.WithTimeout(context.Background(), time.Second)
	}
	return b.ctx
}
func (b *cleanupBudget) close() {
	if b.cancel != nil {
		b.cancel()
	}
}
func emit(ctx context.Context, b *cleanupBudget, report c.Report, stdout, stderr io.Writer) int {
	outputCtx := ctx
	if ctx.Err() != nil {
		outputCtx = b.get()
	}
	raw, err := c.EncodeReport(report)
	if err != nil || writeComplete(outputCtx, stdout, raw) != nil {
		_ = writeComplete(b.get(), stderr, []byte("OUTPUT_WRITE_FAILED\n"))
		return 2
	}
	return report.ExitCode()
}
func reason(err error, fallback p.Code) p.Code {
	var f p.Failure
	if errors.As(err, &f) {
		return f.Code
	}
	return fallback
}
func Run(parent context.Context, args []string, stdout, stderr io.Writer) (exit int) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	cleanup := &cleanupBudget{}
	defer cleanup.close()
	defer func() {
		if recover() != nil {
			exit = emit(ctx, cleanup, c.Incomplete(p.Report{}, "database", "DATABASE_READ_FAILED"), stdout, stderr)
		}
	}()
	a, err := parseArguments(args)
	if err != nil {
		_ = writeComplete(ctx, stderr, []byte(usage))
		return 2
	}
	if a.Help || a.Version {
		raw := usage
		if a.Version {
			raw = version
		}
		if writeComplete(ctx, stdout, []byte(raw)) != nil {
			_ = writeComplete(cleanup.get(), stderr, []byte("OUTPUT_WRITE_FAILED\n"))
			return 2
		}
		return 0
	}
	if err = p.ContextFailure(ctx); err != nil {
		return emit(ctx, cleanup, c.Incomplete(p.Report{}, "file", reason(err, "CANCELED")), stdout, stderr)
	}
	report, err := runProcess(ctx, args, cleanup)
	if err != nil {
		report = c.Incomplete(p.Report{}, "database", reason(err, "DATABASE_READ_FAILED"))
	}
	if err = p.ContextFailure(ctx); err != nil {
		report = c.Incomplete(p.Report{}, "database", reason(err, "CANCELED"))
	}
	return emit(ctx, cleanup, report, stdout, stderr)
}
func runWorker(parent context.Context, args []string, stdout, stderr io.Writer, reader importinput.Reader, getenv func(string) string, provider func(c.Config) c.SnapshotReader) (exit int) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	cleanup := &cleanupBudget{}
	defer cleanup.close()
	file := p.Report{}
	stage := c.Stage("file")
	defer func() {
		if recover() != nil {
			exit = emit(ctx, cleanup, c.Incomplete(file, stage, "DATABASE_READ_FAILED"), stdout, stderr)
		}
	}()
	a, err := parseArguments(args)
	if err != nil {
		_ = writeComplete(ctx, stderr, []byte(usage))
		return 2
	}
	if a.Help || a.Version {
		raw := usage
		if a.Version {
			raw = version
		}
		if writeComplete(ctx, stdout, []byte(raw)) != nil {
			_ = writeComplete(cleanup.get(), stderr, []byte("OUTPUT_WRITE_FAILED\n"))
			return 2
		}
		return 0
	}
	raw, issue, err := reader(ctx, a.Input)
	if err != nil {
		return emit(ctx, cleanup, c.Incomplete(file, stage, reason(err, "INPUT_READ_FAILED")), stdout, stderr)
	}
	if issue != nil {
		col := p.NewCollector()
		col.Add(*issue)
		file = p.BuildReport(p.Outcome{Status: "invalid"}, col)
		return emit(ctx, cleanup, c.BuildReport(file, "invalid", false, nil, c.FileIssues(file)), stdout, stderr)
	}
	var doc *p.Document
	file, doc = p.EvaluateDocument(ctx, raw)
	if doc == nil {
		return emit(ctx, cleanup, c.BuildReport(file, file.Status, false, nil, c.FileIssues(file)), stdout, stderr)
	}
	col := c.FileIssues(file)
	matched := len(doc.Tables["tenants"]) == 1 && doc.Tables["tenants"][0].Values["id"].Text == a.Tenant
	if matched {
		for entity, rows := range doc.Tables {
			if entity == "tenants" {
				continue
			}
			for _, row := range rows {
				if row.Values["tenant_id"].Text != a.Tenant {
					matched = false
				}
			}
		}
	}
	if !matched {
		col.Add(c.Issue{Stage: "file", Issue: p.Issue{Entity: "document", Field: "tables", Code: "TENANT_SELECTION_MISMATCH"}})
		return emit(ctx, cleanup, c.BuildReport(file, "invalid", false, nil, col), stdout, stderr)
	}
	stage = "database"
	cfg, err := c.LoadConfig(getenv)
	if err != nil {
		return emit(ctx, cleanup, c.Incomplete(file, stage, reason(err, "DATABASE_CONFIG_INVALID")), stdout, stderr)
	}
	snapshot, err := provider(cfg).Read(ctx, a.Tenant, *doc)
	if err != nil {
		return emit(ctx, cleanup, c.Incomplete(file, stage, reason(err, "DATABASE_READ_FAILED")), stdout, stderr)
	}
	if !snapshot.TenantFound {
		col.Add(c.Issue{Stage: stage, Issue: p.Issue{Entity: "document", Field: "tables", Code: "TARGET_TENANT_NOT_FOUND"}})
		return emit(ctx, cleanup, c.BuildReport(file, "invalid", false, nil, col), stdout, stderr)
	}
	classes, col, err := c.Compare(ctx, *doc, snapshot)
	if err != nil {
		return emit(ctx, cleanup, c.Incomplete(file, stage, reason(err, "DATABASE_READ_FAILED")), stdout, stderr)
	}
	if err = p.ContextFailure(ctx); err != nil {
		return emit(ctx, cleanup, c.Incomplete(file, stage, reason(err, "CANCELED")), stdout, stderr)
	}
	return emit(ctx, cleanup, c.BuildReport(file, "valid", true, classes, col), stdout, stderr)
}
