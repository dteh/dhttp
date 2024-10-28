package godebug

import (
	"strconv"
	"sync/atomic"
)

var godebugDefault string
var godebugUpdate atomic.Pointer[func(string, string)]
var godebugEnv atomic.Pointer[string] // set by parsedebugvars
var MemProfileRate int
var debug struct {
	cgocheck                 int32
	clobberfree              int32
	disablethp               int32
	dontfreezetheworld       int32
	efence                   int32
	gccheckmark              int32
	gcpacertrace             int32
	gcshrinkstackoff         int32
	gcstoptheworld           int32
	gctrace                  int32
	invalidptr               int32
	madvdontneed             int32 // for Linux; issue 28466
	runtimeContentionStacks  atomic.Int32
	scavtrace                int32
	scheddetail              int32
	schedtrace               int32
	tracebackancestors       int32
	asyncpreemptoff          int32
	harddecommit             int32
	adaptivestackstart       int32
	tracefpunwindoff         int32
	traceadvanceperiod       int32
	traceCheckStackOwnership int32
	profstackdepth           int32

	// debug.malloc is used as a combined debug check
	// in the malloc function and should be set
	// if any of the below debug options is != 0.
	malloc    bool
	inittrace int32
	sbrk      int32
	// traceallocfree controls whether execution traces contain
	// detailed trace data about memory allocation. This value
	// affects debug.malloc only if it is != 0 and the execution
	// tracer is enabled, in which case debug.malloc will be
	// set to "true" if it isn't already while tracing is enabled.
	// It will be set while the world is stopped, so it's safe.
	// The value of traceallocfree can be changed any time in response
	// to os.Setenv("GODEBUG").
	traceallocfree atomic.Int32

	panicnil atomic.Int32

	// asynctimerchan controls whether timer channels
	// behave asynchronously (as in Go 1.22 and earlier)
	// instead of their Go 1.23+ synchronous behavior.
	// The value can change at any time (in response to os.Setenv("GODEBUG"))
	// and affects all extant timer channels immediately.
	// Programs wouldn't normally change over an execution,
	// but allowing it is convenient for testing and for programs
	// that do an os.Setenv in main.init or main.main.
	asynctimerchan atomic.Int32
}

type dbgVar struct {
	name   string
	value  *int32        // for variables that can only be set at startup
	atomic *atomic.Int32 // for variables that can be changed during execution
	def    int32         // default value (ideally zero)
}

var dbgvars = []*dbgVar{
	{name: "adaptivestackstart", value: &debug.adaptivestackstart},
	{name: "asyncpreemptoff", value: &debug.asyncpreemptoff},
	{name: "asynctimerchan", atomic: &debug.asynctimerchan},
	{name: "cgocheck", value: &debug.cgocheck},
	{name: "clobberfree", value: &debug.clobberfree},
	{name: "disablethp", value: &debug.disablethp},
	{name: "dontfreezetheworld", value: &debug.dontfreezetheworld},
	{name: "efence", value: &debug.efence},
	{name: "gccheckmark", value: &debug.gccheckmark},
	{name: "gcpacertrace", value: &debug.gcpacertrace},
	{name: "gcshrinkstackoff", value: &debug.gcshrinkstackoff},
	{name: "gcstoptheworld", value: &debug.gcstoptheworld},
	{name: "gctrace", value: &debug.gctrace},
	{name: "harddecommit", value: &debug.harddecommit},
	{name: "inittrace", value: &debug.inittrace},
	{name: "invalidptr", value: &debug.invalidptr},
	{name: "madvdontneed", value: &debug.madvdontneed},
	{name: "panicnil", atomic: &debug.panicnil},
	{name: "profstackdepth", value: &debug.profstackdepth, def: 128},
	{name: "runtimecontentionstacks", atomic: &debug.runtimeContentionStacks},
	{name: "sbrk", value: &debug.sbrk},
	{name: "scavtrace", value: &debug.scavtrace},
	{name: "scheddetail", value: &debug.scheddetail},
	{name: "schedtrace", value: &debug.schedtrace},
	{name: "traceadvanceperiod", value: &debug.traceadvanceperiod},
	{name: "traceallocfree", atomic: &debug.traceallocfree},
	{name: "tracecheckstackownership", value: &debug.traceCheckStackOwnership},
	{name: "tracebackancestors", value: &debug.tracebackancestors},
	{name: "tracefpunwindoff", value: &debug.tracefpunwindoff},
}

func godebugNotify(envChanged bool) {
	update := godebugUpdate.Load()
	var env string
	if p := godebugEnv.Load(); p != nil {
		env = *p
	}
	if envChanged {
		reparsedebugvars(env)
	}
	if update != nil {
		(*update)(godebugDefault, env)
	}
}
func setUpdate(update func(string, string)) {
	p := new(func(string, string))
	*p = update
	godebugUpdate.Store(p)
	godebugNotify(false)
}

func reparsedebugvars(env string) {
	seen := make(map[string]bool)
	// apply environment settings
	parsegodebug(env, seen)
	// apply compile-time GODEBUG settings for as-yet-unseen variables
	parsegodebug(godebugDefault, seen)
	// apply defaults for as-yet-unseen variables
	for _, v := range dbgvars {
		if v.atomic != nil && !seen[v.name] {
			v.atomic.Store(0)
		}
	}
}

func parsegodebug(godebug string, seen map[string]bool) {
	for p := godebug; p != ""; {
		var field string
		if seen == nil {
			// startup: process left to right, overwriting older settings with newer
			i := IndexByteString(p, ',')
			if i < 0 {
				field, p = p, ""
			} else {
				field, p = p[:i], p[i+1:]
			}
		} else {
			// incremental update: process right to left, updating and skipping seen
			i := len(p) - 1
			for i >= 0 && p[i] != ',' {
				i--
			}
			if i < 0 {
				p, field = "", p
			} else {
				p, field = p[:i], p[i+1:]
			}
		}
		i := IndexByteString(field, '=')
		if i < 0 {
			continue
		}
		key, value := field[:i], field[i+1:]
		if seen[key] {
			continue
		}
		if seen != nil {
			seen[key] = true
		}

		// Update MemProfileRate directly here since it
		// is int, not int32, and should only be updated
		// if specified in GODEBUG.
		if seen == nil && key == "memprofilerate" {
			if n, ok := strconv.Atoi(value); ok == nil {
				MemProfileRate = n
			}
		} else {
			for _, v := range dbgvars {
				if v.name == key {
					if n, ok := strconv.Atoi(value); ok == nil {
						if seen == nil && v.value != nil {
							*v.value = int32(n)
						} else if v.atomic != nil {
							v.atomic.Store(int32(n))
						}
					}
				}
			}
		}
	}

	if debug.cgocheck > 1 {
		panic("cgocheck > 1 mode is no longer supported at runtime. Use GOEXPERIMENT=cgocheck2 at build time instead.")
	}
}

func IndexByteString(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
