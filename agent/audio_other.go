//go:build !windows

package main

import (
	"encoding/binary"
	"io"
	"math"
	"time"
)

// Linux test stand-in: a 440 Hz tone as f32le 48 kHz stereo in real time.
func captureLoopback(stop chan struct{}, formatCh chan<- PCMFormat, w io.Writer) error {
	formatCh <- PCMFormat{Rate: 48000, Channels: 2, Sample: "f32le"}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	ph := 0.0
	buf := make([]byte, 480*2*4)
	for {
		select {
		case <-stop:
			return nil
		case <-tick.C:
		}
		for i := 0; i < 480; i++ {
			v := float32(0.2 * math.Sin(ph))
			ph += 2 * math.Pi * 440 / 48000
			b := math.Float32bits(v)
			binary.LittleEndian.PutUint32(buf[i*8:], b)
			binary.LittleEndian.PutUint32(buf[i*8+4:], b)
		}
		if _, err := w.Write(buf); err != nil {
			return nil
		}
	}
}
