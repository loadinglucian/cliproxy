// Command cliproxy-log is a CLIProxyAPI dynamic-library plugin that records every
// model request and response into ClickHouse. Build it with -buildmode=c-shared;
// see README.md for configuration, columns, and failure behavior.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func main() {}

// cliproxy_plugin_init is the ABI v1 entry point: it stores the host API for host.log
// and hands the host this plugin's call, free, and shutdown functions.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) (rc C.int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			rc = 1
		}
	}()
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	hostLogger = callHostLog
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

// cliproxyPluginCall handles one RPC call from the host. The response buffer is
// allocated with C malloc and released by the host through cliproxyPluginFree.
//
//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			stats.panics.Add(1)
			rc = 0
			writeResponse(response, okEmptyEnvelope)
		}
	}()
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		// GoBytes copies, so the event can outlive this call safely.
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, ok := handleMethod(C.GoString(method), requestBytes)
	writeResponse(response, raw)
	if !ok {
		return 1
	}
	return 0
}

// cliproxyPluginFree releases a response buffer returned by cliproxyPluginCall.
//
//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	defer func() { _ = recover() }()
	if ptr != nil {
		C.free(ptr)
	}
}

// cliproxyPluginShutdown flushes pending rows when the host unloads the plugin.
//
//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() {
		if recovered := recover(); recovered != nil {
			stats.panics.Add(1)
		}
	}()
	stopRuntime()
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// callHostLog forwards one log line to the host's logrus logger via host.log.
func callHostLog(payload []byte) {
	cMethod := C.CString(pluginabi.MethodHostLog)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}
	if C.call_host_api(cMethod, req, C.size_t(len(payload)), &response) == 0 && response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
}
