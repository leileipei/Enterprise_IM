package main

import "testing"

func TestCompareArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"--worker"}, {"--help", "--version"}, {"--input", "a"}, {"--tenant-id", "00000000-0000-4000-8000-000000000001"}, {"--input", "a", "--tenant-id", "secret-marker"}, {"--input", "a", "--tenant-id", "00000000-0000-4000-8000-000000000001", "--tenant-id", "00000000-0000-4000-8000-000000000001"}, {"--input=-", "--tenant-id=00000000-0000-4000-8000-000000000001"}} {
		if _, err := parseArguments(args); err == nil {
			t.Fatal("bad public arguments accepted")
		}
	}
	a, err := parseArguments([]string{"--input=a", "--tenant-id=ABCDEF00-0000-4000-8000-000000000001"})
	if err != nil || a.Tenant != "abcdef00-0000-4000-8000-000000000001" {
		t.Fatal("uuid value normalization")
	}
}
