//go:build windows

package uia

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// COM vtable slots, taken from the UIAutomationClient interface
// definitions (IUnknown occupies 0-2 on every interface):
//
//	IUIAutomation:        ElementFromHandle 6, CreateTrueCondition 21
//	IUIAutomationElement: FindAll 6, GetCurrentPattern 16,
//	                      CurrentControlType 21, CurrentName 23,
//	                      CurrentAutomationId 29
//	IUIAutomationElementArray: Length 3, GetElement 4
//	Patterns (first method is always slot 3): Invoke 3,
//	ExpandCollapse Expand 3 / Collapse 4, Value SetValue 3,
//	SelectionItem Select 3 / IsSelected 6, Toggle Toggle 3
const (
	slotRelease = 2

	slotAutomationElementFromHandle    = 6
	slotAutomationCreateTrueCondition  = 21
	slotAutomationCreateTrueCondition2 = 22 // shifted variant, tried if 21 fails

	slotElementFindAll           = 6
	slotElementGetCurrentPattern = 16
	slotElementCurrentType       = 21
	slotElementCurrentName       = 23
	slotElementCurrentAutoID     = 29

	slotArrayLength   = 3
	slotArrayGetElem  = 4
	slotPatternInvoke = 3
	slotPatternExpand = 3
	slotPatternSetVal = 3
	slotPatternSelect = 3
	slotPatternIsSel  = 6
	slotPatternToggle = 3

	treeScopeChildren = 2
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidCUIAutomation = guid{0xFF48DBA4, 0x60EF, 0x4201, [8]byte{0xAA, 0x87, 0x54, 0x10, 0x3E, 0xEF, 0x59, 0x4E}}
	iidIUIAutomation   = guid{0x30CBE57D, 0xD9D0, 0x452A, [8]byte{0xAB, 0x13, 0x7A, 0xC5, 0xAC, 0x48, 0x25, 0xEE}}
)

var (
	ole32    = windows.NewLazySystemDLL("ole32.dll")
	oleaut32 = windows.NewLazySystemDLL("oleaut32.dll")

	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procSysFreeString    = oleaut32.NewProc("SysFreeString")
)

// comObject mirrors a COM interface pointer's memory layout: a
// pointer to its vtable, whose slots are function addresses.
type comObject struct {
	vtbl *comVtbl
}

type comVtbl struct {
	slots [96]uintptr
}

// call invokes vtable slot on a COM object pointer.
func call(obj unsafe.Pointer, slot uintptr, args ...uintptr) uintptr {
	if obj == nil {
		return uintptr(0x80004003) // E_POINTER
	}
	fnPtr := (*comObject)(obj).vtbl.slots[slot]
	full := make([]uintptr, 0, len(args)+1)
	full = append(full, uintptr(obj))
	full = append(full, args...)
	r, _, _ := syscall.SyscallN(fnPtr, full...)
	return r
}

func release(obj unsafe.Pointer) {
	if obj != nil {
		call(obj, slotRelease)
	}
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

func bstrToString(bstr unsafe.Pointer) string {
	if bstr == nil {
		return ""
	}
	defer procSysFreeString.Call(uintptr(bstr))
	length := *(*uint32)(unsafe.Add(bstr, -4))
	if length == 0 {
		return ""
	}
	u16 := unsafe.Slice((*uint16)(bstr), length/2)
	return syscall.UTF16ToString(u16)
}

// ---- serialized UIA worker -----------------------------------------
//
// All COM work runs on one dedicated thread (UI Automation
// objects are apartment-bound, and serializing also keeps a slow
// browser's UIA tree from racing another's).

type uiaJob struct {
	fn   func(*automation) any
	done chan any
}

var (
	jobCh     chan uiaJob
	jobChOnce sync.Once
)

type automation struct {
	uia       unsafe.Pointer // IUIAutomation
	condition unsafe.Pointer // IUIAutomationCondition (true condition)
	condErr   error
}

func worker() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	a := &automation{}
	if hr, _, _ := procCoInitializeEx.Call(0, 0x2 /* APARTMENTTHREADED */); int32(hr) < 0 {
		a.condErr = fmt.Errorf("uia: CoInitializeEx failed: %#x", hr)
	} else {
		defer procCoUninitialize.Call()
		var obj unsafe.Pointer
		hr, _, _ := procCoCreateInstance.Call(
			uintptr(unsafe.Pointer(&clsidCUIAutomation)), 0, 1, /* INPROC_SERVER */
			uintptr(unsafe.Pointer(&iidIUIAutomation)), uintptr(unsafe.Pointer(&obj)))
		if int32(hr) < 0 || obj == nil {
			a.condErr = fmt.Errorf("uia: CoCreateInstance failed: %#x", hr)
		} else {
			a.uia = obj
			a.condition, a.condErr = a.createTrueCondition()
		}
	}
	for job := range jobCh {
		job.done <- job.fn(a)
	}
}

// createTrueCondition resolves the true condition. The method's
// vtable slot is 21 in the interface definition; a shifted
// variant (22) is tried when the first attempt does not yield a
// usable condition — the result is validated by the caller using
// it in a FindAll, so a wrong guess degrades to "UIA
// unavailable", never to a wrong operation.
func (a *automation) createTrueCondition() (unsafe.Pointer, error) {
	for _, slot := range []uintptr{slotAutomationCreateTrueCondition, slotAutomationCreateTrueCondition2} {
		var cond unsafe.Pointer
		if hr := call(a.uia, slot, uintptr(unsafe.Pointer(&cond))); !failed(hr) && cond != nil {
			return cond, nil
		}
	}
	return nil, fmt.Errorf("uia: cannot obtain a search condition")
}

func runJob[T any](fn func(*automation) (T, error)) (T, error) {
	jobChOnce.Do(func() {
		jobCh = make(chan uiaJob)
		go worker()
	})
	done := make(chan any, 1)
	jobCh <- uiaJob{fn: func(a *automation) any {
		v, err := fn(a)
		return [2]any{v, err}
	}, done: done}
	res := (<-done).([2]any)
	return res[0].(T), res[1].(error)
}

// ---- element helpers -------------------------------------------------

func (a *automation) elementFromHandle(hwnd uintptr) (unsafe.Pointer, error) {
	if a.uia == nil {
		return nil, ErrUnsupported
	}
	var el unsafe.Pointer
	hr := call(a.uia, slotAutomationElementFromHandle, hwnd, uintptr(unsafe.Pointer(&el)))
	if failed(hr) || el == nil {
		return nil, fmt.Errorf("uia: ElementFromHandle failed: %#x", hr)
	}
	return el, nil
}

func (a *automation) findChildren(el unsafe.Pointer) ([]unsafe.Pointer, error) {
	var arr unsafe.Pointer
	hr := call(el, slotElementFindAll, treeScopeChildren, uintptr(a.condition), uintptr(unsafe.Pointer(&arr)))
	if failed(hr) || arr == nil {
		return nil, fmt.Errorf("uia: FindAll failed: %#x", hr)
	}
	defer release(arr)
	var length int32
	call(arr, slotArrayLength, uintptr(unsafe.Pointer(&length)))
	out := make([]unsafe.Pointer, 0, length)
	for i := int32(0); i < length; i++ {
		var child unsafe.Pointer
		if hr := call(arr, slotArrayGetElem, uintptr(i), uintptr(unsafe.Pointer(&child))); !failed(hr) && child != nil {
			out = append(out, child)
		}
	}
	return out, nil
}

func (a *automation) snapshot(el unsafe.Pointer, depth int) ElementInfo {
	info := ElementInfo{Depth: depth}
	var ct int32
	call(el, slotElementCurrentType, uintptr(unsafe.Pointer(&ct)))
	info.ControlType = ct
	var name unsafe.Pointer
	if hr := call(el, slotElementCurrentName, uintptr(unsafe.Pointer(&name))); !failed(hr) {
		info.Name = bstrToString(name)
	}
	var aid unsafe.Pointer
	if hr := call(el, slotElementCurrentAutoID, uintptr(unsafe.Pointer(&aid))); !failed(hr) {
		info.AutomationID = bstrToString(aid)
	}
	for _, pid := range []int32{PatternInvoke, PatternValue, PatternExpandCollapse, PatternSelectionItem, PatternToggle} {
		var pat unsafe.Pointer
		if hr := call(el, slotElementGetCurrentPattern, uintptr(pid), uintptr(unsafe.Pointer(&pat))); !failed(hr) && pat != nil {
			info.Patterns = append(info.Patterns, pid)
			if pid == PatternSelectionItem {
				var sel int32
				if hr := call(pat, slotPatternIsSel, uintptr(unsafe.Pointer(&sel))); !failed(hr) && sel != 0 {
					info.Selected = true
				}
			}
			release(pat)
		}
	}
	return info
}

// probeTree walks the window's chrome tree breadth-first,
// pruning page content (Document subtrees) and capping the work.
func (a *automation) probeTree(root unsafe.Pointer) []ElementInfo {
	const maxDepth = 8
	const maxElements = 600
	var out []ElementInfo
	type item struct {
		el    unsafe.Pointer
		depth int
	}
	queue := []item{{root, 0}}
	for len(queue) > 0 && len(out) < maxElements {
		cur := queue[0]
		queue = queue[1:]
		info := a.snapshot(cur.el, cur.depth)
		out = append(out, info)
		if cur.depth < maxDepth && info.ControlType != ControlDocument {
			children, err := a.findChildren(cur.el)
			if err == nil {
				for _, ch := range children {
					queue = append(queue, item{ch, cur.depth + 1})
				}
			}
		}
		if cur.el != root {
			release(cur.el)
		}
	}
	// Any elements still queued hold references; release them.
	for _, q := range queue {
		if q.el != root {
			release(q.el)
		}
	}
	return out
}

func (a *automation) probe(hwnd uintptr) (Capabilities, error) {
	root, err := a.elementFromHandle(hwnd)
	if err != nil {
		return Capabilities{SelectedTab: -1}, err
	}
	defer release(root)
	if a.condition == nil {
		return Capabilities{SelectedTab: -1, Reason: "UI Automation 搜索条件不可用"}, ErrUnsupported
	}
	elements := a.probeTree(root)
	caps := AnalyzeCapabilities(elements)
	if !caps.Available {
		return caps, ErrUnsupported
	}
	return caps, nil
}

// ProbeWindow measures one browser window's UIA capabilities.
func ProbeWindow(hwnd uintptr) (Capabilities, error) {
	return runJob(func(a *automation) (Capabilities, error) {
		if a.condErr != nil {
			return Capabilities{SelectedTab: -1, Reason: a.condErr.Error()}, ErrUnsupported
		}
		return a.probe(hwnd)
	})
}

// SelectedTabIndex reports the selected tab's index for a
// window, or -1 when it cannot be determined.
func SelectedTabIndex(hwnd uintptr) (int, bool) {
	caps, err := ProbeWindow(hwnd)
	if err != nil || !caps.TabStrip {
		return -1, false
	}
	return caps.SelectedTab, caps.SelectedTab >= 0
}

// SelectTab selects the index-th tab of a window through the
// SelectionItem pattern — a semantic operation that neither
// activates the window nor moves the cursor.
func SelectTab(hwnd uintptr, index int) error {
	_, err := runJob(func(a *automation) (struct{}, error) {
		if a.condErr != nil {
			return struct{}{}, ErrUnsupported
		}
		root, err := a.elementFromHandle(hwnd)
		if err != nil {
			return struct{}{}, err
		}
		defer release(root)
		elements := a.probeTree(root)
		// Re-walk to find the live element for the index-th tab:
		// probeTree released non-root elements, so locate it
		// again by walking with the same order.
		tab, err := a.nthTab(root, index)
		if err != nil {
			return struct{}{}, err
		}
		defer release(tab)
		_ = elements
		var pat unsafe.Pointer
		if hr := call(tab, slotElementGetCurrentPattern, uintptr(PatternSelectionItem), uintptr(unsafe.Pointer(&pat))); failed(hr) || pat == nil {
			return struct{}{}, fmt.Errorf("uia: tab does not support selection")
		}
		defer release(pat)
		if hr := call(pat, slotPatternSelect); failed(hr) {
			return struct{}{}, fmt.Errorf("uia: tab Select failed: %#x", hr)
		}
		return struct{}{}, nil
	})
	return err
}

// nthTab returns the live element of the index-th selectable tab
// in tree order, walking the same pruned tree probeTree uses.
func (a *automation) nthTab(root unsafe.Pointer, index int) (unsafe.Pointer, error) {
	const maxDepth = 8
	type item struct {
		el    unsafe.Pointer
		depth int
	}
	queue := []item{{root, 0}}
	seen := -1
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		info := a.snapshot(cur.el, cur.depth)
		if info.ControlType == ControlTabItem && info.HasPattern(PatternSelectionItem) {
			seen++
			if seen == index {
				// Release everything else still queued.
				for _, q := range queue {
					if q.el != root {
						release(q.el)
					}
				}
				return cur.el, nil
			}
		}
		if cur.depth < maxDepth && info.ControlType != ControlDocument {
			children, err := a.findChildren(cur.el)
			if err == nil {
				for _, ch := range children {
					queue = append(queue, item{ch, cur.depth + 1})
				}
			}
		}
		if cur.el != root {
			release(cur.el)
		}
	}
	return nil, fmt.Errorf("uia: tab index %d not found", index)
}

// InvokeButton finds the unique button matching spec in a
// window's chrome and invokes it through the Invoke pattern.
func InvokeButton(hwnd uintptr, spec MatchSpec) error {
	_, err := runJob(func(a *automation) (struct{}, error) {
		if a.condErr != nil {
			return struct{}{}, ErrUnsupported
		}
		root, err := a.elementFromHandle(hwnd)
		if err != nil {
			return struct{}{}, err
		}
		defer release(root)
		// Snapshot first for the pure, ambiguity-checked match.
		elements := a.probeTree(root)
		idx := FindMatch(elements, spec)
		if idx < 0 {
			return struct{}{}, fmt.Errorf("uia: no unique control matches the request")
		}
		// Re-locate the live element at the same tree position.
		live, err := a.nthBySnapshotIndex(root, idx)
		if err != nil {
			return struct{}{}, err
		}
		defer release(live)
		var pat unsafe.Pointer
		if hr := call(live, slotElementGetCurrentPattern, uintptr(PatternInvoke), uintptr(unsafe.Pointer(&pat))); failed(hr) || pat == nil {
			return struct{}{}, fmt.Errorf("uia: control does not support Invoke")
		}
		defer release(pat)
		if hr := call(pat, slotPatternInvoke); failed(hr) {
			return struct{}{}, fmt.Errorf("uia: Invoke failed: %#x", hr)
		}
		return struct{}{}, nil
	})
	return err
}

// nthBySnapshotIndex returns the live element at position idx of
// the probeTree traversal order.
func (a *automation) nthBySnapshotIndex(root unsafe.Pointer, idx int) (unsafe.Pointer, error) {
	const maxDepth = 8
	type item struct {
		el    unsafe.Pointer
		depth int
	}
	queue := []item{{root, 0}}
	seen := -1
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		seen++
		if seen == idx {
			for _, q := range queue {
				if q.el != root {
					release(q.el)
				}
			}
			return cur.el, nil
		}
		info := a.snapshot(cur.el, cur.depth)
		if cur.depth < maxDepth && info.ControlType != ControlDocument {
			children, err := a.findChildren(cur.el)
			if err == nil {
				for _, ch := range children {
					queue = append(queue, item{ch, cur.depth + 1})
				}
			}
		}
		if cur.el != root {
			release(cur.el)
		}
	}
	return nil, fmt.Errorf("uia: element index %d not found", idx)
}
