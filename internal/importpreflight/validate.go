package importpreflight

import (
	"context"
	"crypto/sha256"
	"fmt"
)

func Evaluate(ctx context.Context, raw []byte) Report {
	r, _ := EvaluateDocument(ctx, raw)
	return r
}

func EvaluateDocument(ctx context.Context, raw []byte) (Report, *Document) {
	c := NewCollector()
	o := Outcome{Status: "invalid"}
	runtimeFailure := func(e error) (Report, *Document) {
		code := Code("CANCELED")
		if f, ok := e.(Failure); ok {
			code = f.Code
		}
		c.Add(Issue{Entity: "document", Field: "_document", Code: code})
		o.Status = "incomplete"
		o.ChecksComplete = false
		return BuildReport(o, c), nil
	}
	if e := ContextFailure(ctx); e != nil {
		return runtimeFailure(e)
	}
	if len(raw) > MaxInput {
		c.Add(Issue{Entity: "document", Field: "_document", Code: "INPUT_TOO_LARGE"})
		return BuildReport(o, c), nil
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
		return BuildReport(o, c), nil
	}
	doc, counts, ok, e := Normalize(ctx, rd, c)
	o.Counts = counts
	if e != nil {
		return runtimeFailure(e)
	}
	if !ok {
		return BuildReport(o, c), nil
	}
	if e = ValidateModel(ctx, doc, c); e != nil {
		return runtimeFailure(e)
	}
	if e = ContextFailure(ctx); e != nil {
		return runtimeFailure(e)
	}
	o.Status = "valid"
	o.ChecksComplete = true
	r := BuildReport(o, c)
	if r.Status != "valid" {
		return r, nil
	}
	return r, &doc
}
