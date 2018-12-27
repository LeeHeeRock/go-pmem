// +build !linux !amd64

package runtime

import "unsafe"

const (
	fileCreate = 0
)

func PersistRange(addr unsafe.Pointer, len uintptr) {
	throw("Not implemented")
}

func FlushRange(addr unsafe.Pointer, len uintptr) {
	throw("Not implemented")
}

func Fence() {
	throw("Not implemented")
}

func mapFile(path string, len, flags, mode, off int,
	mapAddr unsafe.Pointer) (addr unsafe.Pointer, isPmem bool, err int) {
	throw("Not implemented")
	return
}

func getFileSize(fname string) (size int) {
	throw("Not implemented")
	return
}
