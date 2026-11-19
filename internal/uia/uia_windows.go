//go:build windows

package uia

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// Minimal COM UI Automation client: just enough to ask which
// element has focus. All COM work runs on one dedicated OS
// thread (MTA); callers get a timeout instead of a hang.

var (
	ole32 = syscall.NewLazyDLL("ole32.dll")

	procCoInitializeEx = ole32.NewProc("CoInitializeEx")
	procCoUninitialize = ole32.NewProc("CoUninitialize")
	procCoCreateInst   = ole32.NewProc("CoCreateInstance")
	procSysFreeString  = ole32.NewProc("SysFreeString")
	procCoTaskMemFree  = ole32.NewProc("CoTaskMemFree")
)

var (
	clsidCUIAutomation = guid{0xff48dba4, 0x60ef, 0x4201, [8]byte{0xaa, 0x87, 0x54, 0x10, 0x3e, 0xef, 0x59, 0x4e}}
	iidIUIAutomation   = guid{0x30cbe57d, 0xd9d0, 0x452a, [8]byte{0xab, 0x13, 0x7a, 0xc5, 0xac, 0x48, 0x2b, 0xd4}}
)

type guid struct {
	D1 uint32
	D2 uint16
	D3 uint16
	D4 [8]byte
}

const (
	clsctxInprocServer = 0x1
	coinitMultithread  = 0x0
	sOK                = 0x0
)

// vtbl indexes (IUIAutomation / IUIAutomationElement).
const (
	vtGetFocusedElement = 9
	vtElemRelease       = 2
	vtElemControlType   = 16
	vtElemName          = 18
)

func call(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtable := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtable, uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	ret, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return ret
}

type focusJob struct {
	hwnd  uintptr
	reply chan focusReply
}

type focusReply struct {
	info FocusInfo
	err  error
}

var jobs = make(chan focusJob)

func init() { go comThread() }

func comThread() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithread)
	if hr != sOK && hr != 0x80010106 { // S_FALSE / already initialized are fine
		// Without COM nothing here works; keep the thread alive
		// to answer with errors instead of deadlocking callers.
		for job := range jobs {
			job.reply <- focusReply{err: ErrUnsupported}
		}
		return
	}
	defer procCoUninitialize.Call()
	var automation unsafe.Pointer
	hr, _, _ = procCoCreateInst.Call(
		uintptr(unsafe.Pointer(&clsidCUIAutomation)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIUIAutomation)), uintptr(unsafe.Pointer(&automation)))
	if hr != sOK || automation == nil {
		for job := range jobs {
			job.reply <- focusReply{err: ErrUnsupported}
		}
		return
	}
	for job := range jobs {
		job.reply <- doFocused(automation)
	}
}

func doFocused(automation unsafe.Pointer) focusReply {
	var elem unsafe.Pointer
	hr := call(automation, vtGetFocusedElement, uintptr(unsafe.Pointer(&elem)))
	if hr != sOK || elem == nil {
		return focusReply{err: ErrUnsupported}
	}
	defer call(elem, vtElemRelease)
	var ct int32
	call(elem, vtElemControlType, uintptr(unsafe.Pointer(&ct)))
	var bstr unsafe.Pointer
	call(elem, vtElemName, uintptr(unsafe.Pointer(&bstr)))
	name := ""
	if bstr != nil {
		name = bstrToString(bstr)
		_, _, _ = procSysFreeString.Call(uintptr(bstr))
	}
	_ = procCoTaskMemFree
	return focusReply{info: FocusInfo{Name: name, ControlType: ct}}
}

func bstrToString(bstr unsafe.Pointer) string {
	if bstr == nil {
		return ""
	}
	length := *(*uint32)(unsafe.Add(bstr, -4))
	if length == 0 {
		return ""
	}
	u16 := unsafe.Slice((*uint16)(bstr), int(length)/2)
	return syscall.UTF16ToString(u16)
}

func focusedElement(hwnd uintptr) (FocusInfo, error) {
	reply := make(chan focusReply, 1)
	select {
	case jobs <- focusJob{hwnd: hwnd, reply: reply}:
	case <-time.After(2 * time.Second):
		return FocusInfo{}, ErrUnsupported
	}
	select {
	case r := <-reply:
		return r.info, r.err
	case <-time.After(3 * time.Second):
		return FocusInfo{}, ErrUnsupported
	}
}
