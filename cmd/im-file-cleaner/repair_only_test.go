package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type auditRepairStub struct {
	calls, closes int
	callback      func(context.Context) (int, error)
}

func (s *auditRepairStub) Repair(ctx context.Context) (int, error) {
	s.calls++
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second {
		panic("repair batch budget missing")
	}
	if s.callback != nil {
		return s.callback(ctx)
	}
	return 1, nil
}
func (s *auditRepairStub) Close() { s.closes++ }
func TestAuditRepairOnlyNoObjectFactory(t *testing.T) {
	ops := &auditRepairStub{}
	factories := cleanerFactories{cleanup: func(context.Context, func(string) string) (cleanerOperations, error) {
		t.Fatal("repair constructed deleter")
		return nil, nil
	}, repair: func(context.Context, func(string) string) (repairOperations, error) { return ops, nil }}
	var out bytes.Buffer
	get := func(string) string { t.Fatal("repair loop accessed object configuration"); return "" }
	if e := runCleaner(context.Background(), []string{"--execute", "--repair-only", "--once"}, get, &out, factories); e != nil || ops.calls != 1 || ops.closes != 1 {
		t.Fatal(e, ops.calls, ops.closes)
	}
	factories.repair = func(context.Context, func(string) string) (repairOperations, error) {
		t.Fatal("disabled mode constructed repair")
		return nil, nil
	}
	for _, args := range [][]string{nil, {"--repair-only"}, {"--execute=false", "--repair-only"}} {
		out.Reset()
		if e := runCleaner(context.Background(), args, get, &out, factories); e != nil || !bytes.Contains(out.Bytes(), []byte("file_cleaner_disabled")) {
			t.Fatal("default mode not closed")
		}
	}
}
func TestAuditRepairBudgetBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delays := []time.Duration{}
	ops := &auditRepairStub{callback: func(context.Context) (int, error) { return 0, errors.New("private database failure") }}
	wait := func(context.Context, time.Duration) error { return nil }
	wait = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		if len(delays) == 8 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	var out bytes.Buffer
	e := runAuditRepairWithWait(ctx, false, &out, ops, wait)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if !errors.Is(e, context.Canceled) || !reflect.DeepEqual(delays, want) || ops.closes != 1 || bytes.Contains(out.Bytes(), []byte("private database failure")) {
		t.Fatal("backoff or fixed output differs", delays, e)
	}
	ctx, cancel = context.WithCancel(context.Background())
	delays = nil
	ops = &auditRepairStub{}
	ops.callback = func(context.Context) (int, error) {
		if ops.calls == 2 {
			return 1, nil
		}
		return 0, errors.New("failure")
	}
	e = runAuditRepairWithWait(ctx, false, &out, ops, func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		if len(delays) == 4 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	if !errors.Is(e, context.Canceled) || !reflect.DeepEqual(delays, []time.Duration{time.Second, time.Second, time.Second, 2 * time.Second}) {
		t.Fatal("success did not reset backoff", delays)
	}
}
func TestAuditRepairCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ops := &auditRepairStub{callback: func(context.Context) (int, error) { cancel(); return 0, errors.New("failure") }}
	started := time.Now()
	var out bytes.Buffer
	if e := runAuditRepair(ctx, false, &out, ops); !errors.Is(e, context.Canceled) || time.Since(started) > 100*time.Millisecond || ops.closes != 1 {
		t.Fatal("cancellation not immediate")
	}
	ops = &auditRepairStub{callback: func(context.Context) (int, error) { return 0, errors.New("failure") }}
	if e := runAuditRepair(context.Background(), true, &out, ops); e == nil || ops.closes != 1 {
		t.Fatal("once failure accepted")
	}
}
