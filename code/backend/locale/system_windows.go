//go:build windows

package locale

import (
	"syscall"
	"unsafe"
)

var getUserPreferredUILanguages = syscall.NewLazyDLL("kernel32.dll").NewProc("GetUserPreferredUILanguages")

func systemPreferredLanguages() []string {
	const muiLanguageName = 0x8
	var count, size uint32
	r, _, _ := getUserPreferredUILanguages.Call(muiLanguageName, uintptr(unsafe.Pointer(&count)), 0, uintptr(unsafe.Pointer(&size)))
	if r == 0 || size == 0 || size > 4096 {
		return nil
	}
	buffer := make([]uint16, size)
	r, _, _ = getUserPreferredUILanguages.Call(muiLanguageName, uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return nil
	}
	var result []string
	start := 0
	for index, value := range buffer {
		if value != 0 {
			continue
		}
		if index > start {
			result = append(result, syscall.UTF16ToString(buffer[start:index]))
		}
		start = index + 1
	}
	return result
}
