package selfupdate

import (
	"syscall"
	"unsafe"
)

func hideFile(path string) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	setFileAttributes := kernel32.NewProc("SetFileAttributesW")
	// 分别获取指针和错误
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err // 如果有错误，直接返回错误
	}
	r1, _, err := setFileAttributes.Call(uintptr(unsafe.Pointer(ptr)), 2)

	if r1 == 0 {
		return err
	} else {
		return nil
	}
}
