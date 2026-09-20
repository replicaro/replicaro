//go:build !windows

package main

import (
	"io/fs"
	"os"
)

func pathIsLinkLike(_ string, info fs.FileInfo) (bool, error) {
	return info.Mode()&os.ModeSymlink != 0, nil
}
