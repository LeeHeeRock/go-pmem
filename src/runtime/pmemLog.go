package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// logEntry is the structure used to store one log entry.
type logEntry struct {
	ptr uintptr
	val uintptr
}

// logHeapBits is used to log the heap type bits set by the memory allocator during
// a persistent memory allocation request.
// 'addr' is the start address of the allocated region.
// The heap type bits to be copied from are between addresses 'startByte' and 'endByte'.
// This type bitmap will be restored during subsequent run of the program
// and will help GC identify which addresses in the reconstructed persistent memory
// region has pointers.
func logHeapBits(addr uintptr, startByte, endByte *byte) {
	span := spanOf(addr)
	if span.memtype != isPersistent {
		throw("Invalid heap type bits logging request")
	}

	pArena := (*pArena)(unsafe.Pointer(span.pArena))
	numHeapBytes := uintptr(unsafe.Pointer(endByte)) - uintptr(unsafe.Pointer(startByte)) + 1
	dstAddr := pmemHeapBitsAddr(addr, pArena)

	// From heapBitsSetType():
	// There can only be one allocation from a given span active at a time,
	// and the bitmap for a span always falls on byte boundaries,
	// so there are no write-write races for access to the heap bitmap.
	// Hence, heapBitsSetType can access the bitmap without atomics.
	memmove(dstAddr, unsafe.Pointer(startByte), numHeapBytes)
	PersistRange(dstAddr, numHeapBytes)
}

// clearHeapBits clears the logged heap type bits for the object allocated at
// address 'addr' and occupying 'size' bytes.
// The allocator tries to reuse memory regions if possible to satisfy allocation
// requests. If the reused regions do not contain pointers, then the heap type
// bits need to be cleared. This is because for swizzling pointers, the runtime
// need to be exactly sure what regions are static data and what regions contain
// pointers.
// This function expects size to be a multiple of bytesPerBitmapByte.
func clearHeapBits(addr uintptr, size uintptr) {
	span := spanOf(addr)
	if span.memtype != isPersistent {
		throw("Invalid heap type bits logging request")
	}

	pArena := (*pArena)(unsafe.Pointer(span.pArena))
	heapBitsAddr := pmemHeapBitsAddr(addr, pArena)
	numTypeBytes := size / bytesPerBitmapByte
	memclrNoHeapPointers(heapBitsAddr, numTypeBytes)
	PersistRange(heapBitsAddr, numTypeBytes)
}

// pmemHeapBitsAddr returns the address in persistent memory where heap type
// bitmap will be logged corresponding to virtual address 'x'
func pmemHeapBitsAddr(x uintptr, pa *pArena) unsafe.Pointer {
	arenaOffset := pa.offset
	typeBitsAddr := pa.mapAddr + arenaOffset + pArenaHeaderSize

	mdSize, _ := pa.layout()
	arenaStart := pa.mapAddr + mdSize

	allocOffset := (x - arenaStart) / 32
	return unsafe.Pointer(typeBitsAddr + allocOffset)
}

// Function to log a span allocation.
func logSpanAlloc(s *mspan) {
	if s.memtype == isNotPersistent {
		throw("Invalid span passed to logSpanAlloc")
	}

	// The address at which the span value has to be logged
	logAddr := spanLogAddr(s)

	// The value that should be logged
	logVal := spanLogValue(s)

	bitmapVal := *logAddr
	if bitmapVal != 0 {
		// The span bitmap already has an entry corresponding to this span.
		// We clear the span bitmap when a span is freed. Since the entry still
		// exists, this means that the span is getting reused. Hence, the first
		// 31 bits of the entry should match with the corresponding value to be
		// logged. The last bit need not be the same as needzero bit can change
		// as spans get reused.
		// compare the first 31 bits
		if bitmapVal>>1 != logVal>>1 {
			throw("Logged span information mismatch")
		}
		// compare the last bit
		if bitmapVal&1 == logVal&1 {
			// all bits are equal, need not store the value again
			return
		}
	}

	atomic.Store(logAddr, logVal)
	PersistRange(unsafe.Pointer(logAddr), unsafe.Sizeof(*logAddr))
}

// Function to log that a span has been completely freed. This is done by
// writing 0 to the bitmap entry corresponding to this span.
func logSpanFree(s *mspan) {
	if s.memtype == isNotPersistent {
		throw("Invalid span passed to logSpanAlloc")
	}

	logAddr := spanLogAddr(s)
	atomic.Store(logAddr, 0)
	PersistRange(unsafe.Pointer(logAddr), unsafe.Sizeof(*logAddr))
}

// A helper function to compute the value that should be logged to record the
// allocation of span s.
// For a small span, the value logged is -
// ((s.spc) << 1 | s.needzero) and for a large span the value logged is -
// ((66+s.npages-4) << 2 | s.spc << 1 | s.needzero)
// See definition of logBytesPerPage for more details.
func spanLogValue(s *mspan) uint32 {
	var logVal uintptr
	if s.elemsize > maxSmallSize { // large allocation
		npages := s.elemsize >> pageShift
		logVal = (66+npages-4)<<2 | uintptr(s.spanclass)<<1 | uintptr(s.needzero)
	} else {
		logVal = uintptr(s.spanclass)<<1 | uintptr(s.needzero)
	}
	return uint32(logVal)
}

// A helper function to compute the address at which the span log has to be
// written.
func spanLogAddr(s *mspan) *uint32 {
	pArena := (*pArena)(unsafe.Pointer(s.pArena))
	mdSize, allocSize := pArena.layout()
	arenaStart := pArena.mapAddr + mdSize

	// Add offset, arena header, and heap typebitmap size to get the address of span bitmap
	spanBitmap := pArena.mapAddr + pArena.offset + pArenaHeaderSize + allocSize/bytesPerBitmapByte

	// Index of the first page of this span within the persistent memory arena
	pageOffset := (s.base() - arenaStart) >> pageShift

	logAddr := spanBitmap + (pageOffset * spanBytesPerPage)
	return (*uint32)(unsafe.Pointer(logAddr))
}
