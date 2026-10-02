#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdlib.h>
#include "_cgo_export.h"

struct watchStream {
	FSEventStreamRef stream;
	dispatch_queue_t queue;
};

static void watchCallback(ConstFSEventStreamRef stream, void *info, size_t count, void *paths,
                            const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
	watchEventsCallback((uintptr_t)info, count, (char **)paths, (uint32_t *)flags);
}

struct watchStream *watchStart(uintptr_t id, const char **paths, int count, double latency) {
	CFMutableArrayRef watched = CFArrayCreateMutable(NULL, count, &kCFTypeArrayCallBacks);
	for (int i = 0; i < count; i++) {
		CFStringRef path = CFStringCreateWithCString(NULL, paths[i], kCFStringEncodingUTF8);
		if (path == NULL) {
			continue;
		}
		CFArrayAppendValue(watched, path);
		CFRelease(path);
	}

	FSEventStreamContext context = {0, (void *)id, NULL, NULL, NULL};
	FSEventStreamRef stream = FSEventStreamCreate(NULL, watchCallback, &context, watched,
		kFSEventStreamEventIdSinceNow, latency,
		kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer);
	CFRelease(watched);
	if (stream == NULL) {
		return NULL;
	}

	dispatch_queue_t queue = dispatch_queue_create("app.repogo.projectwatch", DISPATCH_QUEUE_SERIAL);
	FSEventStreamSetDispatchQueue(stream, queue);
	if (!FSEventStreamStart(stream)) {
		FSEventStreamInvalidate(stream);
		FSEventStreamRelease(stream);
		dispatch_release(queue);
		return NULL;
	}

	struct watchStream *s = malloc(sizeof *s);
	s->stream = stream;
	s->queue = queue;
	return s;
}

void watchStop(struct watchStream *s) {
	FSEventStreamStop(s->stream);
	FSEventStreamInvalidate(s->stream);
	FSEventStreamRelease(s->stream);
	dispatch_release(s->queue);
	free(s);
}
