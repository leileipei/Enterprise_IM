package importpreflight

import (
	"context"
	"crypto/sha256"
	"fmt"
)

func Evaluate(ctx context.Context, raw []byte) Report {
	c := NewCollector()
	o := Outcome{Status: "invalid"}
	runtimeFailure := func(e error) Report {
		code := Code("CANCELED")
		if f, ok := e.(Failure); ok {
			code = f.Code
		}
		c.Add(Issue{Entity: "document", Field: "_document", Code: code})
		o.Status = "incomplete"
		o.ChecksComplete = false
		return BuildReport(o, c)
	}
	if e := ContextFailure(ctx); e != nil {
		return runtimeFailure(e)
	}
	if len(raw) > MaxInput {
		c.Add(Issue{Entity: "document", Field: "_document", Code: "INPUT_TOO_LARGE"})
		return BuildReport(o, c)
	}
	h := sha256.New()
	for i := 0; i < len(raw); i += 65536 {
		if e := ContextFailure(ctx); e != nil {
			return runtimeFailure(e)
		}
		end := i + 65536
		if end > len(raw) {
			end = len(raw)
		}
		_, _ = h.Write(raw[i:end])
	}
	sum := fmt.Sprintf("%x", h.Sum(nil))
	o.InputSHA256 = &sum
	rd, ok, e := DecodeRaw(ctx, raw, c)
	if e != nil {
		return runtimeFailure(e)
	}
	if !ok {
		return BuildReport(o, c)
	}
	doc, counts, ok, e := Normalize(ctx, rd, c)
	o.Counts = counts
	if e != nil {
		return runtimeFailure(e)
	}
	if !ok {
		return BuildReport(o, c)
	}
	idx, e := BuildIndex(ctx, doc, c)
	if e != nil {
		return runtimeFailure(e)
	}
	rels, e := CheckReferences(ctx, doc, idx, c)
	if e != nil {
		return runtimeFailure(e)
	}
	if e = CheckGraphs(ctx, doc, idx, rels, c); e != nil {
		return runtimeFailure(e)
	}
	if e = CheckIntervals(ctx, doc, idx, rels, c); e != nil {
		return runtimeFailure(e)
	}
	if e = ContextFailure(ctx); e != nil {
		return runtimeFailure(e)
	}
	o.Status = "valid"
	o.ChecksComplete = true
	return BuildReport(o, c)
}
