//go:build !windows

package main

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

func TestPacerSmoothsBursts(t *testing.T) {
	var mu sync.Mutex
	type ev struct {
		t  time.Time
		dx int32
	}
	var got []ev
	sendFn = func(ins []rawInput) {
		mu.Lock()
		for _, in := range ins {
			got = append(got, ev{time.Now(), int32(binary.LittleEndian.Uint32(in.u[0:]))})
		}
		mu.Unlock()
	}
	defer func() { sendFn = sendInputs }()
	// jittery arrival: bursts of 2 batches, then a 70 ms pause; 30 counts each
	start := time.Now()
	total := 0
	for i := 0; i < 20; i++ {
		handleInputS([][]float64{{1, 30, 0}}, true)
		total += 30
		if i%2 == 0 {
			time.Sleep(5 * time.Millisecond)
		} else {
			time.Sleep(70 * time.Millisecond)
		}
	}
	time.Sleep(200 * time.Millisecond)
	// a click flushes pending movement first
	handleInputS([][]float64{{1, 7, 0}, {3, 0}}, true)
	total += 7
	mu.Lock()
	defer mu.Unlock()
	sum := int32(0)
	for _, e := range got {
		sum += e.dx
	}
	if int(sum) != total {
		t.Fatalf("lost movement: sent %d, got %d", total, sum)
	}
	// count output gaps > 40 ms while moving (the input had 70 ms gaps)
	gaps := 0
	for i := 1; i < len(got); i++ {
		if got[i].t.Sub(start) > 1500*time.Millisecond {
			break
		}
		if got[i].t.Sub(got[i-1].t) > 40*time.Millisecond {
			gaps++
		}
	}
	t.Logf("events out: %d, gaps>40ms: %d (input had 10 gaps of 70 ms)", len(got), gaps)
	if gaps > 4 {
		t.Fatalf("not smoothed enough: %d gaps", gaps)
	}
}
