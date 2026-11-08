package main

// Minimal live Matroska writer for raw PCM. ffmpeg's raw PCM demuxer always reads ~93 ms
// blocks (PCM_DEMUX_TARGET_FPS = 10), which made the audio arrive in 100 ms bursts. Wrapped
// in Matroska, every Write() of the capture (~10 ms) becomes its own packet.

import (
	"encoding/binary"
	"io"
	"math"
)

type mkvPCM struct {
	w          io.Writer
	rate       int
	block      int   // bytes per sample frame
	samples    int64 // samples written so far
	clusterMs  int64
	inCluster  bool
	headerDone bool
	f          PCMFormat
}

func newMkvPCM(w io.Writer, f PCMFormat) *mkvPCM {
	bps := 4
	switch f.Sample {
	case "s16le":
		bps = 2
	case "s24le":
		bps = 3
	}
	return &mkvPCM{w: w, rate: f.Rate, block: bps * f.Channels, f: f}
}

func ebmlID(id uint32) []byte {
	switch {
	case id >= 0x1000000:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	case id >= 0x10000:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	case id >= 0x100:
		return []byte{byte(id >> 8), byte(id)}
	}
	return []byte{byte(id)}
}

func ebmlSize(n int) []byte { // 8-byte size field (simple, always valid)
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(n)|0x01<<56)
	return b
}

func el(id uint32, data []byte) []byte {
	out := append(ebmlID(id), ebmlSize(len(data))...)
	return append(out, data...)
}

func elUint(id uint32, v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return el(id, b)
}

func elFloat(id uint32, v float64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, math.Float64bits(v))
	return el(id, b)
}

var unknownSize = []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

func (m *mkvPCM) header() error {
	codec, depth := "A_PCM/FLOAT/IEEE", 32
	switch m.f.Sample {
	case "s16le":
		codec, depth = "A_PCM/INT/LIT", 16
	case "s24le":
		codec, depth = "A_PCM/INT/LIT", 24
	case "s32le":
		codec, depth = "A_PCM/INT/LIT", 32
	}
	var h []byte
	h = append(h, el(0x1A45DFA3, concat(elUint(0x4286, 1), elUint(0x42F7, 1), elUint(0x42F2, 4), elUint(0x42F3, 8),
		el(0x4282, []byte("matroska")), elUint(0x4287, 4), elUint(0x4285, 2)))...)
	h = append(h, ebmlID(0x18538067)...) // Segment, unknown size (live)
	h = append(h, unknownSize...)
	h = append(h, el(0x1549A966, concat(elUint(0x2AD7B1, 1000000), el(0x4D80, []byte("spiel")), el(0x5741, []byte("spiel"))))...)
	audio := concat(elFloat(0xB5, float64(m.rate)), elUint(0x9F, uint64(m.f.Channels)), elUint(0x6264, uint64(depth)))
	track := concat(elUint(0xD7, 1), elUint(0x73C5, 1), elUint(0x83, 2), el(0x86, []byte(codec)), el(0xE1, audio))
	h = append(h, el(0x1654AE6B, el(0xAE, track))...)
	_, err := m.w.Write(h)
	return err
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func (m *mkvPCM) Write(p []byte) (int, error) {
	if !m.headerDone {
		if err := m.header(); err != nil {
			return 0, err
		}
		m.headerDone = true
	}
	n := len(p) - len(p)%m.block
	if n <= 0 {
		return len(p), nil
	}
	ms := m.samples * 1000 / int64(m.rate)
	var out []byte
	if !m.inCluster || ms-m.clusterMs > 30000 { // new cluster every 30 s (block timecodes are int16)
		m.clusterMs, m.inCluster = ms, true
		out = append(out, ebmlID(0x1F43B675)...)
		out = append(out, unknownSize...)
		out = append(out, elUint(0xE7, uint64(ms))...)
	}
	blk := make([]byte, 4, 4+n)
	blk[0] = 0x81 // track 1
	binary.BigEndian.PutUint16(blk[1:], uint16(int16(ms-m.clusterMs)))
	blk[3] = 0x80 // keyframe
	blk = append(blk, p[:n]...)
	out = append(out, el(0xA3, blk)...)
	m.samples += int64(n / m.block)
	if _, err := m.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
