package projectwatch

/*
#cgo LDFLAGS: -framework CoreServices
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>

typedef struct watchStream watchStream;
watchStream *watchStart(uintptr_t id, const char **paths, int count, double latency);
void watchStop(watchStream *s);
*/
import "C"

import (
	"sync"
	"unsafe"
)

// latency is how long FSEvents batches before calling back.
const latency = 0.1

// created marks an event for a file made or renamed into place
// (kFSEventStreamEventFlagItemCreated, …ItemRenamed).
const created = 0x00000100 | 0x00000800

// Streams are found by id rather than a Go pointer handed to C, so a callback
// that races a stop finds nothing instead of freed state.
var (
	streamsMu  sync.Mutex
	streams    = map[uintptr]func(string, bool){}
	nextStream uintptr
)

// watchEvents streams every change under paths through one FSEvents stream:
// one descriptor for a whole tree, where kqueue needs one per file.
func watchEvents(paths []string, onPath func(string, bool)) (func(), bool) {
	streamsMu.Lock()
	nextStream++
	id := nextStream
	streams[id] = onPath
	streamsMu.Unlock()
	forget := func() {
		streamsMu.Lock()
		delete(streams, id)
		streamsMu.Unlock()
	}

	cpaths := make([]*C.char, len(paths))
	for i, path := range paths {
		cpaths[i] = C.CString(path)
	}
	stream := C.watchStart(C.uintptr_t(id), &cpaths[0], C.int(len(cpaths)), C.double(latency))
	for _, p := range cpaths {
		C.free(unsafe.Pointer(p))
	}
	if stream == nil {
		forget()
		return nil, false
	}
	return func() {
		C.watchStop(stream)
		forget()
	}, true
}

//export watchEventsCallback
func watchEventsCallback(id C.uintptr_t, count C.size_t, paths **C.char, flags *C.uint32_t) {
	streamsMu.Lock()
	onPath := streams[uintptr(id)]
	streamsMu.Unlock()
	if onPath == nil {
		return
	}
	n := int(count)
	events := unsafe.Slice(flags, n)
	for i, path := range unsafe.Slice(paths, n) {
		onPath(C.GoString(path), uint32(events[i])&created != 0)
	}
}
