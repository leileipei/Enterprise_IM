//go:build !linux && !darwin

package main

import (
	"context"
	c "github.com/leileipei/Enterprise_IM/internal/importcompare"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"time"
)

func controlDeadline(context.Context) (time.Time, error) {
	return time.Time{}, p.Failure{Code: "INPUT_TYPE_UNSUPPORTED"}
}
func runProcess(context.Context, []string, *cleanupBudget) (c.Report, error) {
	return c.Incomplete(p.Report{}, "file", "INPUT_TYPE_UNSUPPORTED"), nil
}
