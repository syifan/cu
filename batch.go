package cu

// #cgo CFLAGS: -g -O3 -std=c99
// #include <cuda.h>
// #include "batch.h"
import "C"
import (
	"bytes"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/pkg/errors"
)

const workBufLen = 15

type call struct {
	fnargs   *fnargs
	blocking bool
}

// fnargs is a representation of function and arguments to the function
// it's a super huge struct because it has to contain all the possible things that can be passed into a function
type fnargs struct {
	fn C.batchFn

	ctx C.CUcontext

	devptr0 C.CUdeviceptr
	devptr1 C.CUdeviceptr

	ptr0 unsafe.Pointer
	ptr1 unsafe.Pointer

	f C.CUfunction

	gridDimX, gridDimY, gridDimZ    C.uint
	blockDimX, blockDimY, blockDimZ C.uint
	sharedMemBytes                  C.uint

	kernelParams *unsafe.Pointer // void* stuff
	extra        *unsafe.Pointer

	size   C.size_t
	stream C.CUstream // for async
}

func (fn *fnargs) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s. ", batchFnString[fn.fn])
	switch fn.fn {
	case C.fn_setCurrent:
		fmt.Fprintf(&buf, "Current Context %d", fn.ctx)
	case C.fn_mallocD:
		fmt.Fprintf(&buf, "Size %d", fn.size)
	case C.fn_mallocH:
		fmt.Fprintf(&buf, "Size %d", fn.size)
	case C.fn_mallocManaged:
		fmt.Fprintf(&buf, "Size %d", fn.size)
	case C.fn_memfreeD:
		fmt.Fprintf(&buf, "mem: 0x%x", fn.devptr0)
	case C.fn_memfreeH:
		fmt.Fprintf(&buf, "mem: 0x%x", fn.devptr0)
	case C.fn_memcpy:
		fmt.Fprintf(&buf, "dest: 0x%x, src: 0x%x", fn.devptr0, fn.devptr1)
	case C.fn_memcpyHtoD:
		fmt.Fprintf(&buf, "dest: 0x%x, src: 0x%x", fn.devptr0, fn.ptr0)
	case C.fn_memcpyDtoH:
		fmt.Fprintf(&buf, "dest: 0x%x, src: 0x%x", fn.ptr0, fn.devptr0)
	case C.fn_memcpyDtoD:

	case C.fn_memcpyHtoDAsync:

	case C.fn_memcpyDtoHAsync:

	case C.fn_memcpyDtoDAsync:

	case C.fn_launchKernel:
		fmt.Fprintf(&buf, "fn: 0x%v, KernelParams: %v", fn.f, fn.kernelParams)
	case C.fn_sync:
		fmt.Fprintf(&buf, "Current Context %d", fn.ctx)
	case C.fn_lauchAndSync:

	case C.fn_allocAndCopy:
		fmt.Fprintf(&buf, "Size: %v, src: %v", fn.size, fn.ptr0)
	}
	return buf.String()
}

func (fn *fnargs) c() C.uintptr_t {
	return C.uintptr_t(uintptr(unsafe.Pointer(fn)))
}

// BatchedContext is a CUDA context where the cgo calls are batched up.
type BatchedContext struct {
	Context
	Device

	workAvailable chan struct{}
	work          chan call // queue of calls to exec

	queue []call
	// fns     []*C.fnargs_t
	fns     []C.uintptr_t
	results []C.CUresult
	frees   []unsafe.Pointer
	retVal  interface{}

	// sync.Mutex
}

func NewBatchedContext(c Context, d Device) *BatchedContext {
	return &BatchedContext{
		Context: c,
		Device:  d,

		workAvailable: make(chan struct{}, 1),
		work:          make(chan call, workBufLen),
		queue:         make([]call, 0, workBufLen),
		fns:           make([]C.uintptr_t, 0, workBufLen),
		results:       make([]C.CUresult, workBufLen),
		// fns:           make([]*C.fnargs_t, workBufLen),
	}
}

func (ctx *BatchedContext) enqueue(c call) {
	ctx.work <- c
	select {
	case ctx.workAvailable <- struct{}{}:
	default:
	}

	if c.blocking {
		// do something
		ctx.DoWork()
	}
}

func (ctx *BatchedContext) WorkAvailable() <-chan struct{} { return ctx.workAvailable }

func (ctx *BatchedContext) DoWork() {
	// ctx.Lock()
	// defer ctx.Unlock()
	for {
		select {
		case w := <-ctx.work:
			ctx.queue = append(ctx.queue, w)
		default:
			return
		}

		blocking := ctx.queue[len(ctx.queue)-1].blocking
	enqueue:
		for len(ctx.queue) < cap(ctx.queue) && !blocking {
			select {
			case w := <-ctx.work:
				ctx.queue = append(ctx.queue, w)
				blocking = ctx.queue[len(ctx.queue)-1].blocking
			default:
				break enqueue
			}
		}

		for _, c := range ctx.queue {
			ctx.fns = append(ctx.fns, c.fnargs.c())
		}
		logf("GOING TO PROCESS")
		logf(ctx.introspect())

		cctx := C.CUcontext(unsafe.Pointer(uintptr(ctx.Context)))
		ctx.results = ctx.results[:cap(ctx.results)]                         // make sure of the maximum availability for ctx.results
		C.process(cctx, &ctx.fns[0], &ctx.results[0], C.int(len(ctx.queue))) // process the queue
		ctx.results = ctx.results[:len(ctx.queue)]                           // then  truncate it to the len of queue for reporting purposes
		// log.Printf("ERRORS %v", ctx.results)

		for _, f := range ctx.frees {
			C.free(f)
		}

		if blocking {
			b := ctx.queue[len(ctx.queue)-1]
			switch b.fnargs.fn {
			case C.fn_mallocD:
				retVal := (*fnargs)(unsafe.Pointer(uintptr(ctx.fns[len(ctx.fns)-1])))
				ctx.retVal = DevicePtr(retVal.devptr0)
			case C.fn_mallocH:
			case C.fn_mallocManaged:
			case C.fn_allocAndCopy:
				retVal := (*fnargs)(unsafe.Pointer(uintptr(ctx.fns[len(ctx.fns)-1])))
				ctx.retVal = DevicePtr(retVal.devptr0)
			}
			logf("\t[RET] %v", ctx.retVal)
		}

		// clear queue
		ctx.queue = ctx.queue[:0]
		ctx.frees = ctx.frees[:0]
		ctx.fns = ctx.fns[:0]
	}
}

// Retval is used to acquire any buffered return value from the calls
func (ctx *BatchedContext) Retval() interface{} { retVal := ctx.retVal; ctx.retVal = nil; return retVal }

// Errors returns any errors that may have occured during a batch processing
func (ctx *BatchedContext) Errors() error { return ctx.errors() }

// FirstError returns the first error if there was any
func (ctx *BatchedContext) FirstError() error {
	for i, v := range ctx.results {
		if cuResult(v) != Success {
			return result(v)
		}
		ctx.results[i] = C.CUDA_SUCCESS
	}
	return nil
}

func (ctx *BatchedContext) SetCurrent() {
	fn := &fnargs{
		fn:  C.fn_setCurrent,
		ctx: C.CUcontext(unsafe.Pointer(uintptr(ctx.Context))),
	}
	c := call{fn, false}
	ctx.enqueue(c)
}

func (ctx *BatchedContext) MemAlloc(bytesize int64) (retVal DevicePtr, err error) {
	fn := &fnargs{
		fn:   C.fn_mallocD,
		size: C.size_t(bytesize),
	}
	c := call{fn, true}
	ctx.enqueue(c)

	if err = ctx.errors(); err != nil {
		return
	}

	var ok bool
	ret := ctx.Retval()
	if retVal, ok = ret.(DevicePtr); !ok {
		err = errors.Errorf("Expected retVal to be DevicePtr. Got %T instead", ret)
	}
	return
}

func (ctx *BatchedContext) Memcpy(dst, src DevicePtr, byteCount int64) {
	fn := &fnargs{
		fn:      C.fn_memcpy,
		devptr0: C.CUdeviceptr(dst),
		devptr1: C.CUdeviceptr(src),
		size:    C.size_t(byteCount),
	}
	c := call{fn, false}
	ctx.enqueue(c)
}

func (ctx *BatchedContext) MemcpyHtoD(dst DevicePtr, src unsafe.Pointer, byteCount int64) {
	fn := &fnargs{
		fn:      C.fn_memcpyHtoD,
		devptr0: C.CUdeviceptr(dst),
		ptr0:    src,
		size:    C.size_t(byteCount),
	}
	c := call{fn, false}
	ctx.enqueue(c)
}

func (ctx *BatchedContext) MemcpyDtoH(dst unsafe.Pointer, src DevicePtr, byteCount int64) {
	fn := &fnargs{
		fn:      C.fn_memcpyDtoH,
		devptr0: C.CUdeviceptr(src),
		ptr0:    dst,
		size:    C.size_t(byteCount),
	}
	c := call{fn, false}
	ctx.enqueue(c)
}

func (ctx *BatchedContext) MemFree(mem DevicePtr) {
	pc, _, _, _ := runtime.Caller(1)
	logf("MEMFREE  %v CALLED BY %v", mem, runtime.FuncForPC(pc).Name())
	fn := &fnargs{
		fn:      C.fn_memfreeD,
		devptr0: C.CUdeviceptr(mem),
	}
	c := call{fn, false}
	ctx.enqueue(c)
}

func (ctx *BatchedContext) MemFreeHost(p unsafe.Pointer) {
	fn := &fnargs{
		fn:   C.fn_memfreeH,
		ptr0: p,
	}
	c := call{fn, false}
	ctx.enqueue(c)
}

func (ctx *BatchedContext) LaunchKernel(function Function, gridDimX, gridDimY, gridDimZ int, blockDimX, blockDimY, blockDimZ int, sharedMemBytes int, stream Stream, kernelParams []unsafe.Pointer) {
	argv := C.malloc(C.size_t(len(kernelParams) * pointerSize))
	argp := C.malloc(C.size_t(len(kernelParams) * pointerSize))
	ctx.frees = append(ctx.frees, argv)
	ctx.frees = append(ctx.frees, argp)

	for i := range kernelParams {
		*((*unsafe.Pointer)(offset(argp, i))) = offset(argv, i)       // argp[i] = &argv[i]
		*((*uint64)(offset(argv, i))) = *((*uint64)(kernelParams[i])) // argv[i] = *kernelParams[i]
	}
	f := C.CUfunction(unsafe.Pointer(uintptr(function)))
	fn := &fnargs{
		fn:             C.fn_launchKernel,
		f:              f,
		gridDimX:       C.uint(gridDimX),
		gridDimY:       C.uint(gridDimY),
		gridDimZ:       C.uint(gridDimZ),
		blockDimX:      C.uint(blockDimX),
		blockDimY:      C.uint(blockDimY),
		blockDimZ:      C.uint(blockDimZ),
		sharedMemBytes: C.uint(sharedMemBytes),
		stream:         C.CUstream(unsafe.Pointer(uintptr(stream))),
		kernelParams:   (*unsafe.Pointer)(argp),
		extra:          (*unsafe.Pointer)(unsafe.Pointer(uintptr(0))),
	}
	c := call{fn, false}
	ctx.enqueue(c)
}

func (ctx *BatchedContext) Synchronize() {
	fn := &fnargs{
		fn: C.fn_sync,
	}
	c := call{fn, false}
	ctx.enqueue(c)
}

func (ctx *BatchedContext) LaunchAndSync(function Function, gridDimX, gridDimY, gridDimZ int, blockDimX, blockDimY, blockDimZ int, sharedMemBytes int, stream Stream, kernelParams []unsafe.Pointer) {
	ctx.LaunchKernel(function, gridDimX, gridDimY, gridDimZ, blockDimX, blockDimY, blockDimZ, sharedMemBytes, stream, kernelParams)
	ctx.Synchronize()
}

func (ctx *BatchedContext) AllocAndCopy(p unsafe.Pointer, bytesize int64) (retVal DevicePtr, err error) {
	fn := &fnargs{
		fn:   C.fn_allocAndCopy,
		size: C.size_t(bytesize),
		ptr0: p,
	}
	c := call{fn, true}
	ctx.enqueue(c)

	if err = ctx.errors(); err != nil {
		return
	}

	var ok bool
	ret := ctx.Retval()
	if retVal, ok = ret.(DevicePtr); !ok {
		err = errors.Errorf("Expected retVal to be DevicePtr. Got %T instead", ret)
	}
	return
}

/* PRIVATE METHODS */

// checkResults returns true if an error has occured while processing the queue
func (ctx *BatchedContext) checkResults() bool {
	for _, v := range ctx.results {
		if v != C.CUDA_SUCCESS {
			return true
		}
	}
	return false
}

// errors convert ctx.results into errors
func (ctx *BatchedContext) errors() error {
	if !ctx.checkResults() {
		return nil
	}
	err := make(errorSlice, len(ctx.results))
	for i, res := range ctx.results {
		err[i] = result(res)
	}
	return err
}

var batchFnString = map[C.batchFn]string{
	C.fn_setCurrent:      "setCurrent",
	C.fn_mallocD:         "mallocD",
	C.fn_mallocH:         "mallocH",
	C.fn_mallocManaged:   "mallocManaged",
	C.fn_memfreeD:        "memfreeD",
	C.fn_memfreeH:        "memfreeH",
	C.fn_memcpy:          "memcpy",
	C.fn_memcpyHtoD:      "memcpyHtoD",
	C.fn_memcpyDtoH:      "memcpyDtoH",
	C.fn_memcpyDtoD:      "memcpyDtoD",
	C.fn_memcpyHtoDAsync: "memcpyHtoDAsync",
	C.fn_memcpyDtoHAsync: "memcpyDtoHAsync",
	C.fn_memcpyDtoDAsync: "memcpyDtoDAsync",
	C.fn_launchKernel:    "launchKernel",
	C.fn_sync:            "sync",
	C.fn_lauchAndSync:    "lauchAndSync",

	C.fn_allocAndCopy: "allocAndCopy",
}
