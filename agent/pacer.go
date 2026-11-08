package main

// Mouse smoothing ("Maus glätten"): relative mouse movement arrives in jerky batches when
// the network jitters, so the game camera turned in jumps. With smoothing on, movement is
// collected and handed to Windows in small steps every 4 ms, spread over roughly the
// interval between batches (90th percentile, 10..60 ms). Clicks, keys and wheel flush pending movement
// first, so a click still lands exactly where the mouse was aimed.

import (
	"math"
	"sync"
	"time"
)

var inputMu sync.Mutex // serialises SendInput from handlers and the pacer

var sendFn = sendInputs // replaced in tests

type mousePacer struct {
	mu       sync.Mutex
	px, py   float64
	deadline time.Time
	lastAdd  time.Time
	ivs      []float64 // recent intervals between batches (ms)
	running  bool
}

var pacer = &mousePacer{}

// horizon: spread movement over the 90th percentile of recent batch intervals (10..60 ms),
// so even the longer gaps of a jittery line are bridged.
func (p *mousePacer) horizon() float64 {
	if len(p.ivs) == 0 {
		return 33
	}
	a := append([]float64(nil), p.ivs...)
	for i := 1; i < len(a); i++ { // tiny insertion sort (<= 16 values)
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
	return math.Max(10, math.Min(60, a[len(a)*9/10]))
}

func (p *mousePacer) add(dx, dy float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if !p.lastAdd.IsZero() {
		if iv := float64(now.Sub(p.lastAdd).Microseconds()) / 1000; iv < 200 {
			p.ivs = append(p.ivs, iv)
			if len(p.ivs) > 16 {
				p.ivs = p.ivs[1:]
			}
		}
	}
	p.lastAdd = now
	p.px += dx
	p.py += dy
	d := p.horizon()
	p.deadline = now.Add(time.Duration(d * float64(time.Millisecond)))
	if !p.running {
		p.running = true
		go p.loop()
	}
}

// takeAll returns all pending movement as one input (caller holds inputMu).
func (p *mousePacer) takeAll() []rawInput {
	p.mu.Lock()
	defer p.mu.Unlock()
	ix, iy := int32(math.Round(p.px)), int32(math.Round(p.py))
	p.px, p.py = 0, 0
	if ix == 0 && iy == 0 {
		return nil
	}
	return []rawInput{mouseInput(ix, iy, 0, 0x0001)}
}

func step(v, frac float64) int32 {
	s := v * frac
	i := int32(s) // towards zero, the remainder stays pending
	if i == 0 && math.Abs(v) >= 1 {
		if v > 0 {
			i = 1
		} else {
			i = -1
		}
	}
	return i
}

func (p *mousePacer) loop() {
	t := time.NewTicker(4 * time.Millisecond)
	defer t.Stop()
	idle := 0
	for range t.C {
		inputMu.Lock()
		p.mu.Lock()
		var ins []rawInput
		if math.Abs(p.px) >= 1 || math.Abs(p.py) >= 1 {
			idle = 0
			rem := time.Until(p.deadline)
			frac := 1.0
			if rem > 4*time.Millisecond {
				frac = float64(4*time.Millisecond) / float64(rem)
			}
			ix, iy := step(p.px, frac), step(p.py, frac)
			if frac >= 1 {
				ix, iy = int32(math.Round(p.px)), int32(math.Round(p.py))
			}
			p.px -= float64(ix)
			p.py -= float64(iy)
			if ix != 0 || iy != 0 {
				ins = append(ins, mouseInput(ix, iy, 0, 0x0001))
			}
		} else {
			idle++
		}
		stop := idle > 250 // ~1 s without movement
		if stop {
			p.running = false
		}
		p.mu.Unlock()
		if len(ins) > 0 {
			sendFn(ins)
		}
		inputMu.Unlock()
		if stop {
			return
		}
	}
}
