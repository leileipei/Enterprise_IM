package main

import (
	"testing"
)

func TestCleanerArgumentsStrict(t *testing.T) {
	for _, args := range [][]string{nil, {"--execute", "--repair-only", "--once"}, {"-execute=true", "-repair-only=false", "-once"}, {"--execute=false"}} {
		if _, e := parseCleanerArguments(args); e != nil {
			t.Fatal("valid arguments rejected", args)
		}
	}
	for _, args := range [][]string{{"--execute", "--execute"}, {"--execute", "--execute=false"}, {"--repair-only", "--repair-only=false"}, {"--once", "-once=true"}, {"--unknown"}, {"--execute=TRUE"}, {"--once=1"}, {"--execute", "positional"}, {"--execute", "--"}, {"--execute", "true"}} {
		if _, e := parseCleanerArguments(args); e == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
	got, e := parseCleanerArguments([]string{"--execute", "--repair-only", "--once"})
	if e != nil || !got.Execute || !got.RepairOnly || !got.Once {
		t.Fatal("mode not decoded")
	}
}
