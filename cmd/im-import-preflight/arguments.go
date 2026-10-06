package main

import (
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"strings"
)

type arguments struct {
	Input   string
	Help    bool
	Version bool
}

func parseArguments(args []string) (arguments, error) {
	a := arguments{}
	bad := func() (arguments, error) { return arguments{}, p.Failure{Code: "TYPE_INVALID"} }
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--help":
			if a.Help {
				return bad()
			}
			a.Help = true
		case arg == "--version":
			if a.Version {
				return bad()
			}
			a.Version = true
		case arg == "--input" || strings.HasPrefix(arg, "--input="):
			if a.Input != "" {
				return bad()
			}
			v := strings.TrimPrefix(arg, "--input=")
			if arg == "--input" {
				i++
				if i >= len(args) {
					return bad()
				}
				v = args[i]
			}
			if v == "" || v == "-" || strings.HasPrefix(v, "--") || strings.Contains(v, "://") || strings.ContainsRune(v, 0) {
				return bad()
			}
			a.Input = v
		default:
			return bad()
		}
	}
	n := 0
	if a.Input != "" {
		n++
	}
	if a.Help {
		n++
	}
	if a.Version {
		n++
	}
	if n != 1 {
		return bad()
	}
	return a, nil
}
