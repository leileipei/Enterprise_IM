package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestArgumentsExclusiveAndPrivate(t *testing.T) {
	for _, args := range [][]string{{"--input", "private-marker"}, {"--input=private-marker"}, {"--help"}, {"--version"}} {
		a, e := parseArguments(args)
		if e != nil || a.Input == "" && !a.Help && !a.Version {
			t.Fatal("valid args")
		}
	}
	for _, args := range [][]string{nil, {"--input"}, {"--input="}, {"--input", "private-marker", "--input", "other-marker"}, {"--input=a", "--help"}, {"--help", "--version"}, {"--help", "--help"}, {"--secret=private-marker"}, {"--input", "https://private-marker"}, {"--input", "-"}, {"--input", "--help"}} {
		var out, err bytes.Buffer
		if Run(context.Background(), args, &out, &err) != 2 || out.Len() != 0 || strings.Contains(err.String(), "private-marker") {
			t.Fatal("bad args accepted or leaked")
		}
	}
}
