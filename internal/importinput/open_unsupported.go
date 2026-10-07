//go:build !linux && !darwin

package importinput

import (
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"os"
)

func OpenRegular(string) (*os.File, error)     { return nil, p.Failure{Code: "INPUT_TYPE_UNSUPPORTED"} }
func VerifyOpened(os.FileInfo, *os.File) error { return p.Failure{Code: "INPUT_TYPE_UNSUPPORTED"} }
