package main

import (
	"errors"
	"strings"
)

type cleanerArguments struct{ Execute, Once, RepairOnly bool }

func parseCleanerArguments(args []string) (cleanerArguments, error) {
	var result cleanerArguments
	seen := map[string]bool{}
	fail := errors.New("invalid cleaner arguments")
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return cleanerArguments{}, fail
		}
		raw := strings.TrimPrefix(arg, "-")
		raw = strings.TrimPrefix(raw, "-")
		key, value, hasValue := strings.Cut(raw, "=")
		if seen[key] {
			return cleanerArguments{}, fail
		}
		seen[key] = true
		enabled := true
		if hasValue {
			switch value {
			case "true":
			case "false":
				enabled = false
			default:
				return cleanerArguments{}, fail
			}
		}
		switch key {
		case "execute":
			result.Execute = enabled
		case "once":
			result.Once = enabled
		case "repair-only":
			result.RepairOnly = enabled
		default:
			return cleanerArguments{}, fail
		}
	}
	return result, nil
}
