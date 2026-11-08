package main

// Audio: system sound (WASAPI loopback on Windows) -> raw PCM -> ffmpeg libopus -> Ogg
// -> Opus packets -> ahub. Runs only while someone listens.

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ahub = newHub()

type PCMFormat struct {
	Rate     int    `json:"rate"`
	Channels int    `json:"channels"`
	Sample   string `json:"sample"` // ffmpeg raw format: f32le or s16le
}

type Audio struct {
	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
	format  *PCMFormat
	errs    []string
	gen     uint16
}

var aud = &Audio{}

func (a *Audio) logErr(s string) {
	a.mu.Lock()
	a.errs = append(a.errs, time.Now().Format("15:04:05 ")+s)
	if len(a.errs) > 20 {
		a.errs = a.errs[len(a.errs)-20:]
	}
	a.mu.Unlock()
	log.Println("audio:", s)
}

func (a *Audio) ensure() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return
	}
	a.running = true
	a.stopCh = make(chan struct{})
	go a.loop(a.stopCh)
}

func (a *Audio) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		return
	}
	a.running = false
	close(a.stopCh)
}

func (a *Audio) loop(stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		if err := a.runOnce(stop); err != nil {
			a.logErr(err.Error())
		}
		select {
		case <-stop:
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *Audio) runOnce(stop chan struct{}) error {
	pr, pw := io.Pipe()
	formatCh := make(chan PCMFormat, 1)
	capErr := make(chan error, 1)
	// the capture writes ~10 ms chunks; each becomes one Matroska block (see mkvpcm.go).
	// The writer needs the PCM format, which the capture announces before its first write.
	fmtIn := make(chan PCMFormat)
	ready := make(chan struct{})
	var mw *mkvPCM
	gate := writerFunc(func(b []byte) (int, error) { <-ready; return mw.Write(b) })
	go func() {
		select {
		case f := <-fmtIn:
			mw = newMkvPCM(pw, f)
			close(ready)
			formatCh <- f
		case <-stop:
		}
	}()
	go func() { capErr <- captureLoopback(stop, fmtIn, gate); pw.Close() }()
	var f PCMFormat
	select {
	case f = <-formatCh:
	case err := <-capErr:
		return fmt.Errorf("Tonaufnahme: %v", err)
	case <-stop:
		return nil
	}
	a.mu.Lock()
	a.format = &f
	a.gen++
	gen := a.gen
	a.mu.Unlock()
	bin := filepath.Join(baseDir, "ffmpeg.exe")
	if t := os.Getenv("SPIEL_TEST_FFMPEG"); t != "" {
		bin = t
	}
	args := []string{"-hide_banner", "-loglevel", "warning",
		"-probesize", "4096", "-analyzeduration", "0", "-fflags", "nobuffer",
		"-f", "matroska", "-i", "pipe:0",
		"-ac", "2", "-ar", "48000", "-c:a", "libopus", "-b:a", "96k", "-application", "lowdelay",
		"-frame_duration", "20", "-page_duration", "20000", "-flush_packets", "1", "-f", "ogg", "pipe:1"}
	cmd := exec.Command(bin, args...)
	hideCmd(cmd)
	cmd.Stdin = pr
	out, _ := cmd.StdoutPipe()
	errp, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		pr.Close()
		return fmt.Errorf("ffmpeg (Ton): %v", err)
	}
	log.Println("audio start:", f, strings.Join(args, " "))
	go func() {
		sc := bufio.NewScanner(errp)
		for sc.Scan() {
			a.logErr(sc.Text())
		}
	}()
	go func() { <-stop; cmd.Process.Kill(); pr.Close() }()
	t0 := time.Now()
	n := parseOgg(out, func(pkt []byte) {
		ahub.push(&Frame{TS: uint32(time.Since(t0).Milliseconds()), Gen: gen, Key: true, Aud: true, Data: pkt})
	})
	cmd.Wait()
	pr.Close()
	if n == 0 {
		return fmt.Errorf("kein Ton-Paket erzeugt")
	}
	return nil
}

// parseOgg extracts packets from an Ogg stream and skips the OpusHead/OpusTags headers.
func parseOgg(r io.Reader, cb func([]byte)) int {
	br := bufio.NewReaderSize(r, 1<<16)
	var pkt []byte
	count := 0
	hdr := make([]byte, 27)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return count
		}
		if string(hdr[0:4]) != "OggS" {
			// resync: drop one byte at a time until "OggS"
			for {
				b, err := br.ReadByte()
				if err != nil {
					return count
				}
				if b == 'O' {
					pk, _ := br.Peek(3)
					if string(pk) == "ggS" {
						copy(hdr, "O")
						if _, err := io.ReadFull(br, hdr[1:]); err != nil {
							return count
						}
						break
					}
				}
			}
		}
		nseg := int(hdr[26])
		seg := make([]byte, nseg)
		if _, err := io.ReadFull(br, seg); err != nil {
			return count
		}
		if hdr[5]&1 == 0 {
			pkt = pkt[:0]
		}
		for _, l := range seg {
			b := make([]byte, int(l))
			if _, err := io.ReadFull(br, b); err != nil {
				return count
			}
			pkt = append(pkt, b...)
			if l < 255 {
				if len(pkt) > 0 && !(len(pkt) >= 8 && (string(pkt[:8]) == "OpusHead" || string(pkt[:8]) == "OpusTags")) {
					c := make([]byte, len(pkt))
					copy(c, pkt)
					cb(c)
					count++
				}
				pkt = pkt[:0]
			}
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
