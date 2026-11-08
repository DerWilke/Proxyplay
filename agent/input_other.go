//go:build !windows

package main

import (
	"encoding/binary"
	"log"
	"os/exec"
)

type rawInput struct {
	typ uint32
	_   uint32
	u   [32]byte
}

func mouseInput(dx, dy int32, data uint32, flags uint32) rawInput {
	var in rawInput
	binary.LittleEndian.PutUint32(in.u[0:], uint32(dx))
	binary.LittleEndian.PutUint32(in.u[12:], flags)
	return in
}
func keyInput(scan uint16, ext bool, down bool) rawInput { return rawInput{typ: 1} }
func sendInputs(ins []rawInput)                          { log.Printf("sendInputs %d", len(ins)); lastInputs = ins }
func hideCmd(c *exec.Cmd)                                {}

var lastInputs []rawInput
