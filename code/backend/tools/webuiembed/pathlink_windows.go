//go:build windows

package main

import (
	"fmt"
	"io/fs"

	"golang.org/x/sys/windows"
)

func pathIsLinkLike(path string, _ fs.FileInfo) (bool, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, fmt.Errorf("encode Windows path %s: %w", path, err)
	}
	attributes, err := windows.GetFileAttributes(pointer)
	if err != nil {
		return false, fmt.Errorf("inspect Windows path attributes %s: %w", path, err)
	}
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}
