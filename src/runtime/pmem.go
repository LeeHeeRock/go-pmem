package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

const (
	isNotPersistent = 0
	isPersistent    = 1

	// maxMemTypes represents the memory types supported - persistent memory
	// and volatile memory.
	maxMemTypes = 2

	// The number of bytes needed to log a span allocation in the span bitmap.
	// To log allocation of a small span s, the value recorded is
	// ((s.spanclass) << 1 | s.needzero).
	// spanClass for a small allocation vary from 4 to 133. For a large
	// allocation that uses 'npages' pages and has spanClass 'spc', the value
	// recorded is: ((66+npages-4) << 2 | spc << 1 | s.needzero).
	// A large span uses 5 or more pages, and its spanClass is always 0 or 1.
	logBytesPerPage = 4

	// A magic constant that will be written to the first 8 bytes of the
	// persistent memory region. This constant will then help to differentiate
	// between a first run and subsequent runs
	pmemHdrMagic = 0xABCDCBA

	// Persistent memory region header size in bytes. This includes
	// pmemHdrMagic (8 bytes) and another 8 bytes to record the size of the
	// persistent memory region.
	pmemHdrSize = 16

	// Golang manages its heap in arenas of 64MB. Enforce persistent memory
	// initialization size to be a multiple of 64MB
	pmemInitSize = 64 * 1024 * 1024

	// The number of bytes required to log heap type bits for one page. Golang
	// runtime uses 1 byte of heap type bitmap to record type information of
	// 32 bytes of data.
	heapBytesPerPage = pageSize / 32
)

var (
	memTypes = []int{isPersistent, isNotPersistent}
)

// Constants representing possible persistent memory initialization states
const (
	initNotDone = iota // Persistent memory not initialiazed
	initOngoing        // Persistent memory initialization ongoing
	initDone           // Persistent memory initialization completed
)

// A volatile data-structure which stores all the necessary information about
// the persistent memory region.
var pmemInfo struct {
	// The persistent memory backing file name
	fname string

	// Persistent memory initialization state
	// This is used to prevent concurrent/multiple persistent memory initialization
	initState uint32

	// spanBitmap slice corresponds to the persistent memory region that stores
	// the span bitmap log. It uses logBytesPerPage bytes to store the information
	// about each page. See definition of logBytesPerPage for the layout of the
	// bits stored.
	spanBitmap []uint32

	// typeBitmap slice corresponds to the persistent memory region that stores
	// the heap type bitmap log. Heap type bits are used by the garbage collector
	// to identify what regions in the heap store pointer values.
	typeBitmap []byte

	// The start address of the persistent memory region which the runtime manages.
	// This is obtained by adding the offset value and header region size to the
	// address at which the persistent memory file is mapped.
	startAddr uintptr

	// The end address of the persistent memory region that the runtime manages.
	endAddr uintptr
}

// Persistent memory initialization function.
// 'fname' is the file on persistent memory device that should be used for
// persistent memory allocations. If the file does not exist on the persistent
// memory device, this implies a first-time initialization and the file is
// created on the device.
// 'size' is the size of the file to be used.
// 'offset' specifies the number of bytes in the beginning of the persistent
// memory region that should be left unmanaged by the runtime. The memory
// allocator and GC will not manage this space. This can be used by the
// application to store any application-specific data that need not be in the
// runtime-managed heap.
// This function returns the address at which the file was mapped.
// On error, a nil value is returned
func PmallocInit(fname string, size, offset int) unsafe.Pointer {
	if (size-offset) < pmemInitSize || size%pmemInitSize != 0 {
		println(`Persistent memory initialization requires a minimum of 64MB
			for initialization (size-offset) and size needs to be a
			multiple of 64MB`)
		return nil
	}

	if offset%pageSize != 0 {
		println(`Persistent memory initialization requires offset to be a
			multiple of page size`)
		return nil
	}

	// Change persistent memory initialization state from not-done to ongoing
	if !atomic.Cas(&pmemInfo.initState, initNotDone, initOngoing) {
		println(`Persistent memory is already initialized or initialization is
			ongoing`)
		return nil
	}

	// Set the persistent memory file name. This will be used to map the file
	// into memory in growPmemRegion().
	pmemInfo.fname = fname

	// Persistent memory size excluding the offset
	availSize := size - offset
	availPages := availSize >> pageShift

	// Compute the size of the header section. The header section includes the
	// span bitmap, the heap type bitmap, and 'pmemHdrSize' bytes to record the
	// magic constant and persistent memory size.
	heapTypeBitmapSize := availPages * heapBytesPerPage
	spanBitmapSize := availPages * logBytesPerPage
	headerSize := heapTypeBitmapSize + spanBitmapSize + pmemHdrSize

	reserveSize := uintptr(offset + headerSize)
	reservePages := round(reserveSize, pageSize) >> pageShift
	totalPages := uintptr(size) >> pageShift
	pmemMappedAddr := growPmemRegion(totalPages, reservePages)
	if pmemMappedAddr == nil {
		atomic.Store(&pmemInfo.initState, initNotDone)
		return nil
	}
	pmemInfo.startAddr = (uintptr)(pmemMappedAddr) + reservePages<<pageShift

	// hdrAddr is the address of the header section in persistent memory
	hdrAddr := unsafe.Pointer(uintptr(pmemMappedAddr) + uintptr(offset))
	// Cast hdrAddr as a pointer to a slice to easily do pointer manipulations
	addresses := (*[3]int)(hdrAddr)
	magicAddr := &addresses[0]
	sizeAddr := &addresses[1]

	firstTime := false
	// Read the first 8 bytes of header section to check for magic constant
	if *magicAddr == pmemHdrMagic {
		println("Not a first time initialization")

		if *sizeAddr != size {
			println("Initialization size does not match")
			// Unmap the mapped region
			sysFree(pmemMappedAddr, uintptr(size), &memstats.heap_sys)
			atomic.Store(&pmemInfo.initState, initNotDone)
			return nil
		}
	} else {
		println("First time initialization")
		firstTime = true
		// record the size of the persistent memory region
		*sizeAddr = size
		// todo persist size written to persistent memory

		// record a header magic to distinguish between first run and subsequent runs
		*magicAddr = pmemHdrMagic
		// todo persist the magic constant written to persistent memory

		// The first run of the application is distinguished from subsequent runs
		// by comparing the header magic value written. Hence if an application is
		// restarted before the header constant is written, then that run of the
		// application will be considered as a first-time initialization.
	}

	// usablePages is the actual number of pages usable by the allocator
	usablePages := totalPages - reservePages
	spanBitsAddr := unsafe.Pointer(&addresses[2])
	// pmemInfo.spanBitmap is a slice with 'usablePages' number of entries,
	// starting at address 'spanBitsAddr'
	pmemInfo.spanBitmap = (*(*[1 << 28]uint32)(spanBitsAddr))[:usablePages]

	// pmemInfo.typeBitmap is a slice with 'typeEntries' number of entries,
	// starting at address 'typeBitsAddr'
	typeEntries := (usablePages << pageShift) / 32
	typeBitsAddr := unsafe.Pointer(uintptr(spanBitsAddr) + uintptr(spanBitmapSize))
	pmemInfo.typeBitmap = (*(*[1 << 28]byte)(typeBitsAddr))[:typeEntries]

	// The end address of the persistent memory region
	pmemInfo.endAddr = pmemInfo.startAddr + (usablePages << pageShift) - 1

	if !firstTime {
		// TODO reconstruction
	}

	// Set persistent memory as initialized
	atomic.Store(&pmemInfo.initState, initDone)

	return pmemMappedAddr
}

// growPmemRegion maps the persistent memory file into the process address space
// and returns the address at which the file was mapped.
// npages is the total number of pages to be mapped into memory
// reservePages is the number of pages in the beginning of the mapped region that
// should be left unmanaged by the runtime.
// On error, a nil value is returned.
func growPmemRegion(npages, reservePages uintptr) unsafe.Pointer {
	// code skeleton taken from grow() in mheap.go
	h := &mheap_
	ask := npages << pageShift
	lock(&h.lock)
	v, size := h.sysAlloc(ask, isPersistent)
	if v == nil {
		unlock(&h.lock)
		println("Unable to reserve persistent memory heap")
		return nil
	}
	if size != ask {
		unlock(&h.lock)
		println("Unable to reserve requested size")
		sysFree(v, size, &memstats.heap_sys)
		return nil
	}

	// The persistent memory region address from which allocator can allocate from
	spanBase := uintptr(v) + (reservePages << pageShift)

	// Create a fake span and free it, so that the right coalescing happens.
	s := (*mspan)(h.spanalloc.alloc())
	s.init(spanBase, npages-reservePages)
	s.persistent = isPersistent
	h.setSpan(s.base(), s)
	h.setSpan(s.base()+s.npages*pageSize-1, s)
	s.state = mSpanManual
	h.freeSpanLocked(s, false, true, 0)
	unlock(&h.lock)
	return v
}

// Function to log a span allocation.
func logSpanAlloc(s *mspan) {
	if s.persistent == isNotPersistent {
		throw("Invalid span passed to logSpanAlloc")
	}

	// Index of the first page of this span within the persistent memory region
	index := (s.base() - pmemInfo.startAddr) >> pageShift

	// The value that should be logged
	logVal := spanLogValue(s)

	// The address at which the span information should be logged
	logAddr := &pmemInfo.spanBitmap[index]

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
	// todo persist the changes
}

// Function to log that a span has been completely freed. This is done by
// writing 0 to the bitmap entry corresponding to this span.
func logSpanFree(s *mspan) {
	if s.persistent == isNotPersistent {
		throw("Invalid span passed to logSpanFree")
	}

	index := (s.base() - pmemInfo.startAddr) >> pageShift
	logAddr := &pmemInfo.spanBitmap[index]

	atomic.Store(logAddr, 0)
	// todo persist the changes
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

// logHeapBits is used to log the heap type bits set by the memory allocator during
// a persistent memory allocation request.
// 'addr' is the start address of the allocated region.
// The heap type bits to be copied from are between addresses 'startByte' and 'endByte.
// This type bitmap will be restored during subsequent run of the program
// and will help GC identify which addresses in the reconstructed persistent memory
// region has pointers.
func logHeapBits(addr uintptr, startByte, endByte *byte) {
	if uintptr(unsafe.Pointer(endByte)) < uintptr(unsafe.Pointer(startByte)) {
		throw("Invalid addresses passed to logHeapBits")
	}

	if !inPmem(addr) {
		throw("Invalid heap type bits logging request")
	}

	offset := (addr - pmemInfo.startAddr) / 32
	bitAddr := &pmemInfo.typeBitmap[offset]
	sourceAddr := startByte

	// From heapBitsSetType():
	// There can only be one allocation from a given span active at a time,
	// and the bitmap for a span always falls on byte boundaries,
	// so there are no write-write races for access to the heap bitmap.
	// Hence, heapBitsSetType can access the bitmap without atomics.
	for {
		*bitAddr = *sourceAddr
		if sourceAddr == endByte {
			break
		}
		bitAddr = add1(bitAddr)
		sourceAddr = add1(sourceAddr)
	}

	// Todo persist the changes
}

// Function to check that 'addr' is an address in the persistent memory range
func inPmem(addr uintptr) bool {
	return addr >= pmemInfo.startAddr && addr <= pmemInfo.endAddr
}
