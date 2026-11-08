//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32        = syscall.NewLazyDLL("user32.dll")
	procSendInput = user32.NewProc("SendInput")
)

const (
	inputMouse    = 0
	inputKeyboard = 1
)

type rawInput struct {
	typ uint32
	_   uint32
	u   [32]byte
}

func mouseInput(dx, dy int32, data uint32, flags uint32) rawInput {
	var in rawInput
	in.typ = inputMouse
	binary.LittleEndian.PutUint32(in.u[0:], uint32(dx))
	binary.LittleEndian.PutUint32(in.u[4:], uint32(dy))
	binary.LittleEndian.PutUint32(in.u[8:], data)
	binary.LittleEndian.PutUint32(in.u[12:], flags)
	return in
}

func keyInput(scan uint16, ext bool, down bool) rawInput {
	var in rawInput
	in.typ = inputKeyboard
	flags := uint32(0x0008) // KEYEVENTF_SCANCODE
	if ext {
		flags |= 0x0001
	}
	if !down {
		flags |= 0x0002
	}
	binary.LittleEndian.PutUint16(in.u[2:], scan)
	binary.LittleEndian.PutUint32(in.u[4:], flags)
	return in
}

func sendInputs(ins []rawInput) {
	if len(ins) == 0 {
		return
	}
	n, _, e := procSendInput.Call(uintptr(len(ins)), uintptr(unsafe.Pointer(&ins[0])), unsafe.Sizeof(ins[0]))
	if int(n) == len(ins) {
		atomic.AddInt64(&inOK, int64(n))
		return
	}
	atomic.AddInt64(&inOK, int64(n))
	atomic.AddInt64(&inFail, int64(len(ins)-int(n)))
	diagMu.Lock()
	diagLastErr = fmt.Sprintf("SendInput %d von %d angenommen: %v", n, len(ins), e)
	diagMu.Unlock()
}

// ---- input diagnostics (shown in /api/status under "input") ----
// SendInput returns how many events Windows accepted. It silently accepts nothing when the input desktop
// is the lock screen / a UAC prompt, or when the focused window runs with higher rights (UIPI).
var (
	inOK, inFail int64
	diagMu       sync.Mutex
	diagLastErr  string

	kernel32                         = syscall.NewLazyDLL("kernel32.dll")
	advapi32                         = syscall.NewLazyDLL("advapi32.dll")
	procOpenInputDesktop             = user32.NewProc("OpenInputDesktop")
	procCloseDesktop                 = user32.NewProc("CloseDesktop")
	procGetUserObjectInformationW    = user32.NewProc("GetUserObjectInformationW")
	procGetForegroundWindow          = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId     = user32.NewProc("GetWindowThreadProcessId")
	procGetCursorInfo                = user32.NewProc("GetCursorInfo")
	procOpenProcess                  = kernel32.NewProc("OpenProcess")
	procCloseHandle                  = kernel32.NewProc("CloseHandle")
	procQueryFullProcessImageNameW   = kernel32.NewProc("QueryFullProcessImageNameW")
	procProcessIdToSessionId         = kernel32.NewProc("ProcessIdToSessionId")
	procWTSGetActiveConsoleSessionId = kernel32.NewProc("WTSGetActiveConsoleSessionId")
	procGetCurrentProcess            = kernel32.NewProc("GetCurrentProcess")
	procOpenProcessToken             = advapi32.NewProc("OpenProcessToken")
	procGetTokenInformation          = advapi32.NewProc("GetTokenInformation")
)

func tokenElevated(proc uintptr) (elevated bool, err string) {
	var tok uintptr
	r, _, e := procOpenProcessToken.Call(proc, 0x0008, uintptr(unsafe.Pointer(&tok)))
	if r == 0 {
		return false, fmt.Sprint(e)
	}
	defer procCloseHandle.Call(tok)
	var el, ret uint32
	r, _, e = procGetTokenInformation.Call(tok, 20, uintptr(unsafe.Pointer(&el)), 4, uintptr(unsafe.Pointer(&ret)))
	if r == 0 {
		return false, fmt.Sprint(e)
	}
	return el != 0, ""
}

// inputWarning returns the exe name of the focused window when it runs with higher rights than the agent
// (Windows then drops our injected input). Empty when all is fine. Cached for 2 s; sent to the page as the
// X-Input-Warn header of the frame polls, so it costs no extra requests.
var (
	warnMu  sync.Mutex
	warnAt  time.Time
	warnVal string
)

func inputWarning() string {
	warnMu.Lock()
	defer warnMu.Unlock()
	if time.Since(warnAt) < 2*time.Second {
		return warnVal
	}
	warnAt = time.Now()
	warnVal = computeInputWarning()
	return warnVal
}

func computeInputWarning() (v string) {
	defer func() {
		if r := recover(); r != nil {
			v = ""
		}
	}()
	self, _, _ := procGetCurrentProcess.Call()
	if el, _ := tokenElevated(self); el {
		return ""
	}
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == 0 {
		return ""
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(fg, uintptr(unsafe.Pointer(&pid)))
	if int(pid) == os.Getpid() {
		return ""
	}
	ph, _, _ := procOpenProcess.Call(0x1000, 0, uintptr(pid))
	if ph == 0 {
		return "ein Programm" // cannot even be opened: higher rights
	}
	defer procCloseHandle.Call(ph)
	fe, ferr := tokenElevated(ph)
	if !fe && ferr == "" {
		return ""
	}
	name := "ein Programm"
	nb := make([]uint16, 260)
	sz := uint32(len(nb))
	if r, _, _ := procQueryFullProcessImageNameW.Call(ph, 0, uintptr(unsafe.Pointer(&nb[0])), uintptr(unsafe.Pointer(&sz))); r != 0 {
		name = filepath.Base(syscall.UTF16ToString(nb[:sz]))
	}
	clean := make([]rune, 0, len(name))
	for _, c := range name {
		if c < 127 && c >= 32 && c != '"' && c != '<' && c != '>' {
			clean = append(clean, c)
		}
	}
	if len(clean) == 0 {
		return "ein Programm"
	}
	return string(clean)
}

func inputDiag() (out map[string]any) {
	out = map[string]any{"ok": atomic.LoadInt64(&inOK), "failed": atomic.LoadInt64(&inFail)}
	diagMu.Lock()
	out["last_error"] = diagLastErr
	diagMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			out["diag_panic"] = fmt.Sprint(r)
		}
	}()
	// which desktop receives input? (the lock screen / UAC prompt is a different, protected desktop)
	h, _, e := procOpenInputDesktop.Call(0, 0, 0x0001)
	if h == 0 {
		out["input_desktop"] = "nicht lesbar (Sperrbildschirm oder UAC-Abfrage?) " + fmt.Sprint(e)
	} else {
		buf := make([]uint16, 128)
		var need uint32
		r, _, _ := procGetUserObjectInformationW.Call(h, 2, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&need)))
		if r != 0 {
			out["input_desktop"] = syscall.UTF16ToString(buf)
		}
		procCloseDesktop.Call(h)
	}
	// session of the agent vs session on the console (another logged-in user would be the console)
	var sess uint32
	procProcessIdToSessionId.Call(uintptr(os.Getpid()), uintptr(unsafe.Pointer(&sess)))
	con, _, _ := procWTSGetActiveConsoleSessionId.Call()
	out["agent_session"], out["console_session"] = sess, uint32(con)
	// rights of the agent and of the focused window
	self, _, _ := procGetCurrentProcess.Call()
	el, er := tokenElevated(self)
	out["agent_elevated"] = el
	if er != "" {
		out["agent_elevated_err"] = er
	}
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == 0 {
		out["foreground"] = "keins"
	} else {
		var pid uint32
		procGetWindowThreadProcessId.Call(fg, uintptr(unsafe.Pointer(&pid)))
		ph, _, e := procOpenProcess.Call(0x1000, 0, uintptr(pid))
		if ph == 0 {
			out["foreground"] = fmt.Sprintf("pid %d, nicht lesbar (höhere Rechte?): %v", pid, e)
		} else {
			defer procCloseHandle.Call(ph)
			name := ""
			nb := make([]uint16, 260)
			sz := uint32(len(nb))
			if r, _, _ := procQueryFullProcessImageNameW.Call(ph, 0, uintptr(unsafe.Pointer(&nb[0])), uintptr(unsafe.Pointer(&sz))); r != 0 {
				name = filepath.Base(syscall.UTF16ToString(nb[:sz]))
			}
			fe, ferr := tokenElevated(ph)
			out["foreground"] = name
			out["foreground_elevated"] = fe
			if ferr != "" {
				out["foreground_elevated_err"] = ferr
			}
		}
	}
	// the real cursor: visible? where? (position changes when injected mouse input works)
	var ci struct {
		size  uint32
		flags uint32
		cur   uintptr
		x, y  int32
	}
	ci.size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r != 0 {
		out["cursor_showing"] = ci.flags&1 != 0
		out["cursor_x"], out["cursor_y"] = ci.x, ci.y
	}
	return out
}

var procGetSystemMetrics = user32.NewProc("GetSystemMetrics")

// cursorState: "x,y,visible,screenW,screenH" of the real pointer (primary screen), cached 80 ms
var curCache struct {
	mu sync.Mutex
	t  time.Time
	v  string
}

func cursorState() string {
	curCache.mu.Lock()
	defer curCache.mu.Unlock()
	if time.Since(curCache.t) < 80*time.Millisecond {
		return curCache.v
	}
	curCache.t = time.Now()
	curCache.v = ""
	defer func() { recover() }()
	var ci struct {
		size  uint32
		flags uint32
		cur   uintptr
		x, y  int32
	}
	ci.size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r != 0 {
		w, _, _ := procGetSystemMetrics.Call(0)
		h, _, _ := procGetSystemMetrics.Call(1)
		vis := 0
		if ci.flags&1 != 0 {
			vis = 1
		}
		curCache.v = fmt.Sprintf("%d,%d,%d,%d,%d", ci.x, ci.y, vis, w, h)
	}
	return curCache.v
}

func hideCmd(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
