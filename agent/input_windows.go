//go:build windows

package main

import (
	"encoding/binary"
	"os/exec"
	"syscall"
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
	procSendInput.Call(uintptr(len(ins)), uintptr(unsafe.Pointer(&ins[0])), unsafe.Sizeof(ins[0]))
}

func hideCmd(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
