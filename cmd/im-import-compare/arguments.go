package main

import (
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"regexp"
	"strings"
)

type arguments struct {
	Input, Tenant string
	Help, Version bool
}

var uuidPattern = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func parseArguments(args []string) (arguments, error) {
	a := arguments{}
	seen := map[string]bool{}
	bad := func() (arguments, error) { return arguments{}, p.Failure{Code: "TYPE_INVALID"} }
	for i := 0; i < len(args); i++ {
		flag, value, equals := strings.Cut(args[i], "=")
		if seen[flag] {
			return bad()
		}
		seen[flag] = true
		switch flag {
		case "--help", "--version":
			if equals {
				return bad()
			}
			if flag == "--help" {
				a.Help = true
			} else {
				a.Version = true
			}
		case "--input", "--tenant-id":
			if !equals {
				i++
				if i >= len(args) {
					return bad()
				}
				value = args[i]
			}
			if value == "" || strings.HasPrefix(value, "--") || strings.ContainsRune(value, 0) {
				return bad()
			}
			if flag == "--input" {
				if value == "-" || strings.Contains(value, "://") {
					return bad()
				}
				a.Input = value
			} else {
				if !uuidPattern.MatchString(value) {
					return bad()
				}
				a.Tenant = strings.ToLower(value)
			}
		default:
			return bad()
		}
	}
	if a.Help || a.Version {
		if len(seen) != 1 {
			return bad()
		}
	} else if a.Input == "" || a.Tenant == "" || len(seen) != 2 {
		return bad()
	}
	return a, nil
}
