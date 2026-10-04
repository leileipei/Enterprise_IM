package main

import (
	"bytes"
	"context"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"testing"
)

func TestFileCleanerDefaultClosed(t *testing.T) {
	for _, args := range [][]string{nil, {"-execute=false"}} {
		var out bytes.Buffer
		getenv := func(string) string { t.Fatal("default cleaner read dependencies"); return "" }
		factory := func(context.Context, func(string) string) (cleanerOperations, error) {
			t.Fatal("default cleaner connected dependencies")
			return nil, nil
		}
		if e := runCleaner(context.Background(), args, getenv, &out, factory); e != nil {
			t.Fatal(e)
		}
		if !bytes.Contains(out.Bytes(), []byte("file_cleaner_disabled")) {
			t.Fatal(out.String())
		}
	}
}

type cleanerOpsStub struct{ calls []string }

func (s *cleanerOpsStub) Repair(context.Context) (int, error) {
	s.calls = append(s.calls, "repair")
	return 1, nil
}
func (s *cleanerOpsStub) Step(context.Context) (bool, error) {
	s.calls = append(s.calls, "step")
	return false, nil
}
func (s *cleanerOpsStub) Close() { s.calls = append(s.calls, "close") }
func TestFileCleanerPausedStillRepairsAudit(t *testing.T) {
	ops := &cleanerOpsStub{}
	var out bytes.Buffer
	e := runCleaner(context.Background(), []string{"-execute", "-once"}, func(string) string { return "" }, &out, func(context.Context, func(string) string) (cleanerOperations, error) { return ops, nil })
	if e != nil || len(ops.calls) != 3 || ops.calls[0] != "repair" || ops.calls[1] != "step" || ops.calls[2] != "close" {
		t.Fatal(e, ops.calls)
	}
}

type blockedCleanerOps struct {
	cleanerOpsStub
	steps int
}

func (o *blockedCleanerOps) Step(context.Context) (bool, error) {
	o.steps++
	if o.steps == 1 {
		return true, filecleanup.ErrBlocked
	}
	return o.steps == 2, nil
}
func TestFileCleanerContinuesAfterQuarantine(t *testing.T) {
	ops := &blockedCleanerOps{}
	var out bytes.Buffer
	e := runCleaner(context.Background(), []string{"-execute", "-once"}, func(string) string { return "" }, &out, func(context.Context, func(string) string) (cleanerOperations, error) { return ops, nil })
	if e != nil || ops.steps != 3 || !bytes.Contains(out.Bytes(), []byte(`"blocked":1`)) {
		t.Fatal("quarantine stopped sweep", e, ops.steps, out.String())
	}
}
