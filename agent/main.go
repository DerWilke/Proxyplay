// spiel-agent: streams the Windows desktop as H.264 (NVENC via ffmpeg) over plain HTTP
// and injects mouse/keyboard input via SendInput. No WebSockets.
// Endpoints (all under /api/, only reachable from the reverse proxy):
//
//	GET  /api/stream            chunked endless frame stream
//	GET  /api/frames?after=N&gen=G   long-poll fallback (for buffering proxies)
//	POST /api/input             JSON [[type,a,b,c],...]
//	GET/POST /api/settings      {w,h,fps,kbps,monitor}
//	GET  /api/status
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	version = "1.6.1"
	// Access: only the reverse proxy server (allowedIP, with header X-Spiel-Key = secret)
	// and localhost may use the API. Set them in spiel-config.json next to the exe,
	// or at build time: -ldflags "-X main.secret=... -X main.allowedIP=... -X main.defaultUpdateURL=..."
	secret    = ""
	allowedIP = "" // empty: localhost only
	listen    = envOr("SPIEL_LISTEN", ":8770")
	baseDir   string
)

// ---------------------------------------------------------------- frames hub

type Frame struct {
	Seq  uint32
	TS   uint32
	Gen  uint16
	Key  bool
	Data []byte
	At   time.Time // when the frame reached the hub (for measurements)
	Aud  bool      // audio packet (header byte 15 = 1) - audio can ride along in video polls
}

func (f *Frame) header() []byte {
	h := make([]byte, 16)
	binary.LittleEndian.PutUint32(h[0:], uint32(len(f.Data)))
	binary.LittleEndian.PutUint32(h[4:], f.Seq)
	binary.LittleEndian.PutUint32(h[8:], f.TS)
	binary.LittleEndian.PutUint16(h[12:], f.Gen)
	if f.Key {
		h[14] = 1
	}
	if f.Aud {
		h[15] = 1
	}
	return h
}

type Hub struct {
	mu       sync.Mutex
	frames   []*Frame // ring, oldest first
	seq      uint32
	notify   chan struct{}
	lastPoll time.Time
	bytes    int64
	count    int64
	cursors  map[string]hubCursor // per page: newest frame already handed out (overlapping polls)
}

type hubCursor struct {
	seq uint32
	gen uint16
	t   time.Time
}

func newHub() *Hub { return &Hub{notify: make(chan struct{}), cursors: map[string]hubCursor{}} }

// claim is framesAfter for overlapping long-polls of one page (client id c): frames that
// another poll of the same page already took are skipped, so parallel polls never
// return the same frame twice.
// peek reports the arrival time of the oldest frame a claim would return, without claiming.
func (h *Hub) peek(c string, after uint32, gen uint16, maxLag int) (time.Time, bool, chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.cursors[c]; ok && time.Since(cur.t) < 5*time.Second && (cur.gen != gen || cur.seq > after) {
		after, gen = cur.seq, cur.gen
	}
	frames, ch := h.framesAfterLocked(after, gen, maxLag)
	if len(frames) == 0 {
		return time.Time{}, false, ch
	}
	return frames[0].At, true, ch
}

// re=true (client lost an answer) ignores the cursor once and re-sends from `after`.
func (h *Hub) claim(c string, after uint32, gen uint16, maxLag int, re bool) ([]*Frame, chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if cur, ok := h.cursors[c]; ok && !re && now.Sub(cur.t) < 5*time.Second && (cur.gen != gen || cur.seq > after) {
		after, gen = cur.seq, cur.gen
	}
	frames, ch := h.framesAfterLocked(after, gen, maxLag)
	if len(frames) > 0 {
		l := frames[len(frames)-1]
		h.cursors[c] = hubCursor{l.Seq, l.Gen, now}
	} else if cur, ok := h.cursors[c]; ok {
		cur.t = now
		h.cursors[c] = cur
	}
	if len(h.cursors) > 16 {
		for k, v := range h.cursors {
			if now.Sub(v.t) > time.Minute {
				delete(h.cursors, k)
			}
		}
	}
	return frames, ch
}

func (h *Hub) push(f *Frame) {
	h.mu.Lock()
	h.seq++
	f.Seq = h.seq
	if f.At.IsZero() {
		f.At = time.Now()
	}
	h.frames = append(h.frames, f)
	// keyframes are rare with intra-refresh (every 5 s): keep enough to restart from one
	if len(h.frames) > 600 {
		h.frames = h.frames[len(h.frames)-600:]
	}
	h.bytes += int64(len(f.Data))
	h.count++
	close(h.notify)
	h.notify = make(chan struct{})
	h.mu.Unlock()
}

func (h *Hub) reset() {
	h.mu.Lock()
	h.frames = nil
	h.mu.Unlock()
}

func (h *Hub) touch() {
	h.mu.Lock()
	h.lastPoll = time.Now()
	h.mu.Unlock()
}

// framesAfter returns frames newer than `after`. If the client is too far behind,
// lost, or on another generation, it restarts at the newest keyframe.
func (h *Hub) framesAfter(after uint32, gen uint16, maxLag int) ([]*Frame, chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.framesAfterLocked(after, gen, maxLag)
}

func (h *Hub) framesAfterLocked(after uint32, gen uint16, maxLag int) ([]*Frame, chan struct{}) {
	n := len(h.frames)
	if n == 0 {
		return nil, h.notify
	}
	start := -1
	if after > 0 && h.frames[0].Gen == gen {
		for i, f := range h.frames {
			if f.Seq > after {
				if i > 0 && h.frames[i-1].Seq == after {
					start = i
				}
				break
			}
		}
		if start == -1 && h.frames[n-1].Seq == after {
			return nil, h.notify
		}
	}
	if start >= 0 && n-start > maxLag {
		// too far behind: jump to the newest keyframe, but only if that is AHEAD of the
		// client (keyframes are rare, jumping back would re-send seconds of video).
		// Otherwise send everything; the page decodes fast and skips drawing old frames.
		for i := n - 1; i > start; i-- {
			if h.frames[i].Key {
				start = i
				break
			}
		}
	}
	if start == -1 {
		for i := n - 1; i >= 0; i-- {
			if h.frames[i].Key {
				start = i
				break
			}
		}
		if start == -1 {
			return nil, h.notify
		}
	}
	out := make([]*Frame, n-start)
	copy(out, h.frames[start:])
	return out, h.notify
}

var hub = newHub()

// ---------------------------------------------------------------- encoder

type Settings struct {
	W       int    `json:"w"`
	H       int    `json:"h"`
	FPS     int    `json:"fps"`
	Kbps    int    `json:"kbps"`
	Monitor int    `json:"monitor"`
	KF      string `json:"kf"`    // "ir": intra-refresh, keyframe every 5 s (default); "gop": keyframe every second (old)
	Codec   string `json:"codec"` // "h264" (default) or "hevc" (H.265, NVENC only; falls back to h264)
}

type Encoder struct {
	mu         sync.Mutex
	set        Settings
	gen        uint16
	cmd        *exec.Cmd
	running    bool
	pipeline   string
	pipeIdx    int
	errs       []string
	started    time.Time
	stopCh     chan struct{}
	irFailed   bool // intra-refresh produced no picture on this PC -> classic keyframes
	irActive   bool
	hevcFailed bool // hevc_nvenc produced no picture -> h264
	codec      string
}

var enc = &Encoder{set: Settings{W: 1280, H: 720, FPS: 30, Kbps: 4000, KF: "ir", Codec: "h264"}}

func (e *Encoder) logErr(s string) {
	e.mu.Lock()
	e.errs = append(e.errs, time.Now().Format("15:04:05 ")+s)
	if len(e.errs) > 40 {
		e.errs = e.errs[len(e.errs)-40:]
	}
	e.mu.Unlock()
	log.Println("ffmpeg:", s)
}

func (e *Encoder) args(s Settings, idx int, ir, hevc bool) (string, []string) {
	src := fmt.Sprintf("ddagrab=output_idx=%d:framerate=%d:draw_mouse=1", s.Monitor, s.FPS)
	scaleGPU, scaleCPU := "", ""
	if s.W > 0 && s.H > 0 {
		scaleGPU = fmt.Sprintf(",scale_d3d11=width=%d:height=%d:format=nv12", s.W, s.H)
		scaleCPU = fmt.Sprintf(",scale=%d:%d:flags=bilinear", s.W, s.H)
	}
	g := strconv.Itoa(s.FPS)
	br := fmt.Sprintf("%dk", s.Kbps)
	buf := fmt.Sprintf("%dk", s.Kbps/2)
	if ir {
		// rate buffer of ~2 frames: no frame may be much bigger than average -> no size spikes
		buf = fmt.Sprintf("%dk", s.Kbps*2/s.FPS+1)
	}
	vcodec, outFmt := "h264_nvenc", "h264"
	if hevc {
		vcodec, outFmt = "hevc_nvenc", "hevc"
	}
	nvenc := []string{"-c:v", vcodec, "-preset", "p1", "-tune", "ull", "-zerolatency", "1", "-delay", "0",
		"-rc", "cbr", "-b:v", br, "-maxrate", br, "-bufsize", buf, "-g", g, "-bf", "0", "-profile:v", "main", "-aud", "1"}
	x264p := "aud=1:repeat-headers=1"
	var kfArgs []string
	if ir {
		// intra-refresh: a column of intra blocks wanders over the picture once per second
		// instead of one big IDR frame per second; a real IDR only every 5 s (for joining)
		kfArgs = []string{"-forced-idr", "1", "-force_key_frames", fmt.Sprintf("expr:gte(n,n_forced*%d)", 5*s.FPS)}
		nvenc = append(append(nvenc, "-intra-refresh", "1"), kfArgs...)
		x264p += ":intra-refresh=1"
	}
	x264 := []string{"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-b:v", br, "-maxrate", br,
		"-bufsize", buf, "-g", g, "-bf", "0", "-profile:v", "main", "-x264-params", x264p}
	x264 = append(x264, kfArgs...)
	head := []string{"-hide_banner", "-loglevel", "warning", "-fflags", "nobuffer", "-f", "lavfi", "-i"}
	tail := []string{"-bsf:v", "dump_extra=freq=keyframe", "-flush_packets", "1", "-an", "-f", outFmt, "pipe:1"}
	tailX := []string{"-bsf:v", "dump_extra=freq=keyframe", "-flush_packets", "1", "-an", "-f", "h264", "pipe:1"}
	var name string
	var a []string
	switch idx {
	case 0:
		name = "GPU-Aufnahme + GPU-Skalierung + NVENC"
		a = append(append(append(head, src+scaleGPU), nvenc...), tail...)
	case 1:
		name = "GPU-Aufnahme + CPU-Skalierung + NVENC"
		a = append(append(append(head, src+",hwdownload,format=bgra"+scaleCPU+",format=nv12"), nvenc...), tail...)
	default:
		name = "GPU-Aufnahme + CPU-Skalierung + x264 (ohne Grafikkarte)"
		a = append(append(append(head, src+",hwdownload,format=bgra"+scaleCPU+",format=yuv420p"), x264...), tailX...)
	}
	return name, a
}

func (e *Encoder) ensure() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return
	}
	e.running = true
	e.stopCh = make(chan struct{})
	go e.loop(e.stopCh)
}

func (e *Encoder) stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.running = false
	close(e.stopCh)
	cmd := e.cmd
	e.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

func (e *Encoder) restart(s Settings) {
	e.stop()
	e.mu.Lock()
	e.set = s
	e.pipeIdx = 0
	e.irFailed = false
	e.hevcFailed = false
	e.mu.Unlock()
	hub.reset()
	e.ensure()
}

func (e *Encoder) loop(stop chan struct{}) {
	fails := 0
	for {
		select {
		case <-stop:
			return
		default:
		}
		e.mu.Lock()
		e.gen++
		gen := e.gen
		s := e.set
		idx := e.pipeIdx
		ir := s.KF != "gop" && !e.irFailed
		hevc := s.Codec == "hevc" && !e.hevcFailed && idx < 2
		e.irActive = ir
		e.codec = "h264"
		if hevc {
			e.codec = "hevc"
		}
		e.mu.Unlock()
		name, args := e.args(s, idx, ir, hevc)
		if ir {
			name += " · Intra-Refresh"
		}
		if hevc {
			name += " · H.265"
		}
		bin := filepath.Join(baseDir, "ffmpeg.exe")
		if t := os.Getenv("SPIEL_TEST_CMD"); t != "" { // Linux test mode: synthetic stream
			f := strings.Fields(strings.NewReplacer("{W}", strconv.Itoa(s.W), "{H}", strconv.Itoa(s.H), "{FPS}", strconv.Itoa(s.FPS), "{KBPS}", strconv.Itoa(s.Kbps)).Replace(t))
			bin, args, name = f[0], f[1:], "TEST"
		}
		cmd := exec.Command(bin, args...)
		hideCmd(cmd)
		out, _ := cmd.StdoutPipe()
		errp, _ := cmd.StderrPipe()
		if err := cmd.Start(); err != nil {
			e.logErr("Start fehlgeschlagen: " + err.Error())
			time.Sleep(3 * time.Second)
			continue
		}
		e.mu.Lock()
		e.cmd = cmd
		e.pipeline = name
		e.started = time.Now()
		e.mu.Unlock()
		log.Println("encoder start:", name, strings.Join(args, " "))
		go func() {
			sc := bufio.NewScanner(errp)
			for sc.Scan() {
				e.logErr(sc.Text())
			}
		}()
		got := parseAnnexB(out, gen, time.Now(), hevc)
		cmd.Wait()
		select {
		case <-stop:
			return
		default:
		}
		if got < 5 {
			fails++
			e.logErr(fmt.Sprintf("Pipeline %q lieferte kein Bild", name))
			e.mu.Lock()
			if idx == 0 {
				e.pipeIdx++
			} else if hevc {
				e.hevcFailed = true // retry the same pipeline with H.264
			} else if ir {
				e.irFailed = true // then without intra-refresh
			} else if e.pipeIdx < 2 {
				e.pipeIdx++
			}
			e.mu.Unlock()
		} else {
			fails = 0
		}
		wait := time.Duration(1+fails) * time.Second
		if wait > 10*time.Second {
			wait = 10 * time.Second
		}
		time.Sleep(wait)
	}
}

// parseAnnexB splits the H.264 byte stream into access units (delimited by AUD NALs)
// and pushes them to the hub. A trailing slice is flushed after a short idle gap so
// a frame does not wait for the next one. Returns the number of frames produced.
func parseAnnexB(r io.Reader, gen uint16, t0 time.Time, hevc bool) int {
	nt := func(n []byte) int { return nalTypeC(n, hevc) }
	chunks := make(chan []byte, 64)
	go func() {
		defer close(chunks)
		for {
			b := make([]byte, 256*1024)
			n, err := r.Read(b)
			if n > 0 {
				chunks <- b[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	var buf []byte
	var au [][]byte
	count := 0
	emit := func() {
		if len(au) == 0 {
			return
		}
		key := false
		vcl := false
		size := 0
		for _, n := range au {
			t := nt(n)
			if isKey(t, hevc) {
				key = true
			}
			if isVCL(t, hevc) {
				vcl = true
			}
			size += len(n)
		}
		if vcl {
			data := make([]byte, 0, size)
			for _, n := range au {
				data = append(data, n...)
			}
			hub.push(&Frame{TS: uint32(time.Since(t0).Milliseconds()), Gen: gen, Key: key, Data: data})
			count++
		}
		au = nil
	}
	handle := func(n []byte) {
		if (!hevc && nt(n) == 9) || (hevc && nt(n) == 35) { // access unit delimiter
			emit()
		}
		c := make([]byte, len(n))
		copy(c, n)
		au = append(au, c)
	}
	idle := time.NewTimer(time.Hour)
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				if len(buf) > 0 {
					handle(buf)
				}
				emit()
				return count
			}
			buf = append(buf, c...)
			pos := startCodes(buf)
			if len(pos) > 0 {
				for k := 0; k < len(pos)-1; k++ {
					handle(buf[pos[k]:pos[k+1]])
				}
				buf = append([]byte(nil), buf[pos[len(pos)-1]:]...)
			}
			idle.Reset(3 * time.Millisecond)
		case <-idle.C:
			if len(buf) > 4 {
				if t := nt(buf); isVCL(t, hevc) {
					handle(buf)
					buf = nil
					emit()
				}
			}
		}
	}
}

// startCodes returns offsets where a start code (00 00 01 or 00 00 00 01) begins.
func startCodes(b []byte) []int {
	var pos []int
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			s := i
			if i > 0 && b[i-1] == 0 {
				s = i - 1
			}
			pos = append(pos, s)
			i += 2
		}
	}
	return pos
}

// H.264: type = byte & 0x1f (AUD 9, slices 1/5, IDR 5).
// H.265: type = (byte >> 1) & 0x3f (AUD 35, VCL 0..31, IDR/CRA 19..21).
func nalTypeC(n []byte, hevc bool) int {
	if !hevc {
		return nalType(n)
	}
	for i := 0; i+3 < len(n) && i < 4; i++ {
		if n[i] == 0 && n[i+1] == 0 && n[i+2] == 1 {
			return int(n[i+3]>>1) & 0x3f
		}
	}
	return -1
}

func isKey(t int, hevc bool) bool {
	if hevc {
		return t >= 19 && t <= 21
	}
	return t == 5
}

func isVCL(t int, hevc bool) bool {
	if hevc {
		return t >= 0 && t < 32
	}
	return t == 1 || t == 5
}

func nalType(n []byte) int {
	for i := 0; i+3 < len(n) && i < 4; i++ {
		if n[i] == 0 && n[i+1] == 0 && n[i+2] == 1 {
			return int(n[i+3] & 0x1f)
		}
	}
	return -1
}

var btnFlags = map[int][2]uint32{0: {0x2, 0x4}, 1: {0x20, 0x40}, 2: {0x8, 0x10}, 3: {0x80, 0x100}, 4: {0x80, 0x100}}

// event types: 1 rel move(dx,dy) 2 abs move(x,y in 0..65535) 3 btn down(b) 4 btn up(b)
// 5 wheel(delta) 6 key(scan, ext, down) 7 hwheel(delta)
func handleInput(evs [][]float64) { handleInputS(evs, false) }

func handleInputS(evs [][]float64, smooth bool) {
	inputMu.Lock()
	defer inputMu.Unlock()
	var ins []rawInput
	for _, e := range evs {
		if len(e) == 0 {
			continue
		}
		g := func(i int) float64 {
			if i < len(e) {
				return e[i]
			}
			return 0
		}
		if int(e[0]) != 1 {
			ins = append(ins, pacer.takeAll()...) // movement before a click/key/wheel goes out first
		}
		switch int(e[0]) {
		case 1:
			if smooth {
				pacer.add(g(1), g(2))
				continue
			}
			ins = append(ins, mouseInput(int32(g(1)), int32(g(2)), 0, 0x0001))
		case 2:
			ins = append(ins, mouseInput(int32(g(1)), int32(g(2)), 0, 0x0001|0x8000))
		case 3, 4:
			b := int(g(1))
			f, ok := btnFlags[b]
			if !ok {
				continue
			}
			var data uint32
			if b == 3 {
				data = 1
			} else if b == 4 {
				data = 2
			}
			fl := f[0]
			if int(e[0]) == 4 {
				fl = f[1]
			}
			ins = append(ins, mouseInput(0, 0, data, fl))
		case 5:
			ins = append(ins, mouseInput(0, 0, uint32(int32(g(1))), 0x0800))
		case 7:
			ins = append(ins, mouseInput(0, 0, uint32(int32(g(1))), 0x1000))
		case 6:
			ins = append(ins, keyInput(uint16(g(1)), g(2) != 0, g(3) != 0))
		}
	}
	if len(ins) > 0 {
		sendFn(ins)
	}
}

// ---------------------------------------------------------------- http

func allowed(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if host == "127.0.0.1" || host == "::1" {
		return true
	}
	if allowedIP == "" || host != allowedIP {
		return false
	}
	if host == allowedIP && secret != "" && r.Header.Get("X-Spiel-Key") != secret {
		return false
	}
	return true
}

func guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowed(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		h(w, r)
	}
}

func maxLag() int {
	enc.mu.Lock()
	f := enc.set.FPS
	enc.mu.Unlock()
	if f < 10 {
		f = 10
	}
	return f / 2 // more than ~0.5 s behind: skip to the newest keyframe
}

func serveStream(w http.ResponseWriter, r *http.Request, hub *Hub, ensure func(), lag func() int) {
	ensure()
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(200)
	var last uint32
	var gen uint16
	ctx := r.Context()
	keep := time.NewTicker(2 * time.Second)
	defer keep.Stop()
	for {
		hub.touch()
		frames, ch := hub.framesAfter(last, gen, lag())
		for _, f := range frames {
			if _, err := w.Write(f.header()); err != nil {
				return
			}
			if _, err := w.Write(f.Data); err != nil {
				return
			}
			last, gen = f.Seq, f.Gen
		}
		if len(frames) > 0 && fl != nil {
			fl.Flush()
		}
		select {
		case <-ctx.Done():
			return
		case <-ch:
		case <-keep.C:
			// empty heartbeat frame (len 0) keeps proxies from timing out
			if _, err := w.Write(make([]byte, 16)); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func serveFrames(w http.ResponseWriter, r *http.Request, hub *Hub, ensure func(), lag func() int) {
	ensure()
	hub.touch()
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 32)
	g, _ := strconv.ParseUint(r.URL.Query().Get("gen"), 10, 16)
	c := r.URL.Query().Get("c")
	re := r.URL.Query().Get("re") == "1"
	if len(c) > 64 {
		c = c[:64]
	}
	// v1.6: the page puts pending input into the poll itself (POST body), so mouse movement
	// needs no extra requests (restrictive proxies are limited by request rate). Applied at once.
	if r.Method == http.MethodPost {
		if body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil && len(body) > 0 && body[0] == '{' {
			applyInputPacket(body)
		}
	}
	// a=1: audio rides along in video polls (no separate audio request). Audio is added to
	// every video answer; if no video comes, the poll answers once audio is 60 ms old.
	wantA := r.URL.Query().Get("a") == "1" && c != "" && hub != ahub
	aa, _ := strconv.ParseUint(r.URL.Query().Get("aa"), 10, 32)
	ag, _ := strconv.ParseUint(r.URL.Query().Get("ag"), 10, 16)
	const audioLagFold = 25
	if wantA {
		aud.ensure()
		ahub.touch()
	}
	tReq := time.Now()
	deadline := time.After(2 * time.Second)
	var frames []*Frame
	for {
		var ch chan struct{}
		if c != "" {
			ml := lag()
			if re && ml < 90 {
				ml = 90 // re-send after a lost answer: fill the gap instead of jumping to a keyframe
			}
			frames, ch = hub.claim(c, uint32(after), uint16(g), ml, re)
		} else {
			frames, ch = hub.framesAfter(uint32(after), uint16(g), lag())
		}
		if len(frames) > 0 {
			break
		}
		var ach chan struct{}
		var await <-chan time.Time
		if wantA {
			oldest, ok, ac := ahub.peek(c, uint32(aa), uint16(ag), audioLagFold)
			if ok {
				age := time.Since(oldest)
				if age >= 60*time.Millisecond {
					break
				}
				await = time.After(60*time.Millisecond - age)
			} else {
				ach = ac
			}
		}
		select {
		case <-ch:
			continue
		case <-ach:
			continue
		case <-await:
			continue
		case <-deadline:
		case <-r.Context().Done():
			return
		}
		break
	}
	var afr []*Frame
	if wantA {
		afr, _ = ahub.claim(c, uint32(aa), uint16(ag), audioLagFold, false)
	}
	var b bytes.Buffer
	for _, f := range frames {
		b.Write(f.header())
		b.Write(f.Data)
		if b.Len() > 4<<20 {
			break
		}
	}
	for _, f := range afr {
		b.Write(f.header())
		b.Write(f.Data)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	// measurements: how long the poll was held, and how old the frames were when sent
	now := time.Now()
	w.Header().Set("X-Hold", strconv.FormatInt(now.Sub(tReq).Milliseconds(), 10))
	if len(frames) > 0 {
		w.Header().Set("X-Age", fmt.Sprintf("%d,%d", now.Sub(frames[0].At).Milliseconds(), now.Sub(frames[len(frames)-1].At).Milliseconds()))
	}
	w.Write(b.Bytes())
}

func inputHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read", 400)
		return
	}
	// v1.2: {"c":client,"s":seq,"e":[[...]]} - several POSTs may be in flight,
	// batches are applied in seq order. A plain array (old pages) is applied directly.
	if len(body) > 0 && body[0] == '{' {
		if err := applyInputPacket(body); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
	} else {
		var evs [][]float64
		if err := json.Unmarshal(body, &evs); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		handleInput(evs)
	}
	w.WriteHeader(204)
}

// applyInputPacket handles {"c":client,"s":seq,"e":[[...]],"m":1} - from POST /api/input or
// riding along in a video poll (POST /api/frames). m=1: mouse smoothing on.
func applyInputPacket(body []byte) error {
	var pkt struct {
		C string      `json:"c"`
		S uint32      `json:"s"`
		E [][]float64 `json:"e"`
		M int         `json:"m"`
	}
	if err := json.Unmarshal(body, &pkt); err != nil {
		return err
	}
	inOrder.mu.Lock()
	inOrder.smooth = pkt.M == 1
	inOrder.mu.Unlock()
	inOrder.add(pkt.C, pkt.S, pkt.E)
	return nil
}

// inputOrder applies input batches of one client in sequence order. A missing batch
// is waited for at most 150 ms; batches that arrive after being skipped are still
// applied (a late key-up is better than a stuck key).
type inputOrder struct {
	mu      sync.Mutex
	client  string
	next    uint32
	pending map[uint32][][]float64
	timer   *time.Timer
	done    [1024]uint32 // recently applied seqs (+1, 0 = empty): retried POSTs are ignored
	smooth  bool
}

func (o *inputOrder) apply(s uint32, evs [][]float64) {
	o.done[s%1024] = s + 1
	handleInputS(evs, o.smooth)
}

var inOrder = &inputOrder{pending: map[uint32][][]float64{}}

func (o *inputOrder) add(c string, s uint32, evs [][]float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c != o.client {
		o.client, o.next = c, s
		o.pending = map[uint32][][]float64{}
		o.done = [1024]uint32{}
	}
	if o.done[s%1024] == s+1 {
		return // duplicate (client retried after a proxy error)
	}
	if _, dup := o.pending[s]; dup {
		return
	}
	if s < o.next {
		o.apply(s, evs)
		return
	}
	o.pending[s] = evs
	o.flushLocked()
}

func (o *inputOrder) flushLocked() {
	for {
		evs, ok := o.pending[o.next]
		if !ok {
			break
		}
		delete(o.pending, o.next)
		o.apply(o.next, evs)
		o.next++
	}
	if len(o.pending) > 0 && o.timer == nil {
		o.timer = time.AfterFunc(150*time.Millisecond, func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.timer = nil
			if len(o.pending) == 0 {
				return
			}
			min := uint32(0)
			first := true
			for k := range o.pending {
				if first || k < min {
					min, first = k, false
				}
			}
			o.next = min
			o.flushLocked()
		})
	}
}

func sanitize(s *Settings) {
	if s.FPS < 5 || s.FPS > 60 {
		s.FPS = 30
	}
	if s.Kbps < 300 || s.Kbps > 50000 {
		s.Kbps = 4000
	}
	if s.W < 0 || s.W > 7680 || s.H < 0 || s.H > 4320 {
		s.W, s.H = 1280, 720
	}
	if s.W%2 == 1 {
		s.W++
	}
	if s.H%2 == 1 {
		s.H++
	}
	if s.Monitor < 0 || s.Monitor > 7 {
		s.Monitor = 0
	}
	if s.KF != "gop" {
		s.KF = "ir"
	}
	if s.Codec != "hevc" {
		s.Codec = "h264"
	}
}

// settings survive restarts and self-updates: spiel-settings.json next to the exe
func settingsPath() string { return filepath.Join(baseDir, "spiel-settings.json") }

func loadSavedSettings() {
	b, err := os.ReadFile(settingsPath())
	if err != nil {
		return
	}
	var s Settings
	if json.Unmarshal(b, &s) != nil || s.FPS == 0 {
		return
	}
	sanitize(&s)
	enc.mu.Lock()
	enc.set = s
	enc.mu.Unlock()
	log.Println("settings loaded:", s)
}

func saveSettings(s Settings) {
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		log.Println("settings save:", err)
		return
	}
	if err := os.Rename(tmp, settingsPath()); err != nil {
		log.Println("settings save:", err)
	}
}

func settingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var s Settings
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&s); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		sanitize(&s)
		saveSettings(s)
		enc.restart(s)
	}
	enc.mu.Lock()
	s := enc.set
	enc.mu.Unlock()
	json.NewEncoder(w).Encode(s)
}

// measurement reports posted by the page (Messmodus), readable via GET /api/reports
var reports struct {
	mu   sync.Mutex
	list []json.RawMessage
}

func reportsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil || !json.Valid(b) {
			http.Error(w, "bad json", 400)
			return
		}
		reports.mu.Lock()
		reports.list = append(reports.list, json.RawMessage(b))
		if len(reports.list) > 120 {
			reports.list = reports.list[len(reports.list)-120:]
		}
		reports.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	reports.mu.Lock()
	out := append([]json.RawMessage(nil), reports.list...)
	reports.mu.Unlock()
	if out == nil {
		out = []json.RawMessage{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	enc.mu.Lock()
	st := map[string]any{
		"version": version, "running": enc.running, "pipeline": enc.pipeline, "gen": enc.gen,
		"settings": enc.set, "errors": append([]string(nil), enc.errs...),
		"uptime_s": int(time.Since(enc.started).Seconds()), "intra_refresh": enc.irActive, "codec": enc.codec,
	}
	enc.mu.Unlock()
	aud.mu.Lock()
	st["audio"] = map[string]any{"running": aud.running, "format": aud.format, "errors": append([]string(nil), aud.errs...)}
	aud.mu.Unlock()
	ahub.mu.Lock()
	st["audio_packets"] = ahub.count
	ahub.mu.Unlock()
	hub.mu.Lock()
	st["frames_total"] = hub.count
	st["bytes_total"] = hub.bytes
	st["idle_s"] = int(time.Since(hub.lastPoll).Seconds())
	hub.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

// stop the encoder when nobody watches (saves GPU while playing locally)
func idleWatcher() {
	for range time.Tick(5 * time.Second) {
		hub.mu.Lock()
		idle := time.Since(hub.lastPoll)
		hub.mu.Unlock()
		if idle > 30*time.Second {
			enc.stop()
		}
		ahub.mu.Lock()
		aidle := time.Since(ahub.lastPoll)
		ahub.mu.Unlock()
		if aidle > 30*time.Second {
			aud.stop()
		}
	}
}

func registerAutostart(exe string) {
	cmd := exec.Command("reg", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "SpielAgent",
		"/t", "REG_SZ", "/d", `"`+exe+`"`, "/f")
	hideCmd(cmd)
	if err := cmd.Run(); err != nil {
		log.Println("autostart:", err)
	}
}

func main() {
	exe, _ := os.Executable()
	baseDir = filepath.Dir(exe)
	lf, err := os.OpenFile(filepath.Join(baseDir, "spiel-agent.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err == nil {
		log.SetOutput(lf)
	}
	log.Println("spiel-agent", version, "start in", baseDir)
	os.Remove(exe + ".old") // leftover from a self-update
	loadSavedSettings()
	loadConfig()
	var ln net.Listener
	for i := 0; i < 20; i++ { // after a self-update the old process may still hold the port briefly
		if ln, err = net.Listen("tcp", listen); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		log.Println("port belegt, läuft schon?", err)
		return
	}
	registerAutostart(exe)
	go idleWatcher()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stream", guard(func(w http.ResponseWriter, r *http.Request) { serveStream(w, r, hub, enc.ensure, maxLag) }))
	mux.HandleFunc("/api/frames", guard(func(w http.ResponseWriter, r *http.Request) { serveFrames(w, r, hub, enc.ensure, maxLag) }))
	audioLag := func() int { return 25 }
	mux.HandleFunc("/api/audio", guard(func(w http.ResponseWriter, r *http.Request) { serveStream(w, r, ahub, aud.ensure, audioLag) }))
	mux.HandleFunc("/api/audioframes", guard(func(w http.ResponseWriter, r *http.Request) { serveFrames(w, r, ahub, aud.ensure, audioLag) }))
	mux.HandleFunc("/api/input", guard(inputHandler))
	mux.HandleFunc("/api/settings", guard(settingsHandler))
	mux.HandleFunc("/api/status", guard(statusHandler))
	mux.HandleFunc("/api/reports", guard(reportsHandler))
	mux.HandleFunc("/api/ping", guard(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	mux.HandleFunc("/api/update", guard(func(w http.ResponseWriter, r *http.Request) { updateHandler(w, r, exe, srv) }))
	err = srv.Serve(ln)
	log.Println(err)
	if err == http.ErrServerClosed {
		select {} // self-update in progress: the update goroutine restarts and exits
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// updateHandler downloads the new agent from the server inside the LAN, swaps the
// running exe (Windows allows renaming a running file) and restarts.
var defaultUpdateURL = "" // e.g. http://<server>:8104/dl/spiel-agent.exe
var updateURL = envOr("SPIEL_UPDATE_URL", defaultUpdateURL)

// spiel-config.json next to the exe (all fields optional):
// {"server_ip": "192.168.1.10", "key": "long-random-string", "update_url": "http://192.168.1.10:8104/dl/spiel-agent.exe", "listen": ":8770"}
type agentConfig struct {
	ServerIP  string `json:"server_ip"`
	Key       string `json:"key"`
	UpdateURL string `json:"update_url"`
	Listen    string `json:"listen"`
}

func loadConfig() {
	b, err := os.ReadFile(filepath.Join(baseDir, "spiel-config.json"))
	if err != nil {
		if allowedIP == "" {
			log.Println("no spiel-config.json and no built-in server_ip: only localhost may connect")
		}
		return
	}
	var c agentConfig
	if err := json.Unmarshal(b, &c); err != nil {
		log.Println("spiel-config.json:", err)
		return
	}
	if c.ServerIP != "" {
		allowedIP = c.ServerIP
	}
	if c.Key != "" {
		secret = c.Key
	}
	if c.UpdateURL != "" {
		updateURL = c.UpdateURL
	}
	if c.Listen != "" {
		listen = c.Listen
	}
	log.Println("config loaded: server_ip", allowedIP, "key set:", secret != "", "update_url", updateURL)
}

func updateHandler(w http.ResponseWriter, r *http.Request, exe string, srv *http.Server) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if updateURL == "" {
		http.Error(w, "no update_url configured (spiel-config.json)", 400)
		return
	}
	resp, err := http.Get(updateURL)
	if err != nil {
		http.Error(w, "download: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil || resp.StatusCode != 200 || len(data) < 1<<20 || (runtime.GOOS == "windows" && string(data[:2]) != "MZ") {
		http.Error(w, fmt.Sprintf("ungültige Datei (status %d, %d bytes)", resp.StatusCode, len(data)), 502)
		return
	}
	os.Remove(exe + ".old")
	if err := os.Rename(exe, exe+".old"); err != nil {
		http.Error(w, "rename: "+err.Error(), 500)
		return
	}
	if err := os.WriteFile(exe, data, 0755); err != nil {
		os.Rename(exe+".old", exe)
		http.Error(w, "write: "+err.Error(), 500)
		return
	}
	w.WriteHeader(202)
	w.Write([]byte("update ok, restart"))
	log.Println("self-update: new exe written, restarting")
	go func() {
		time.Sleep(300 * time.Millisecond)
		enc.stop()
		aud.stop()
		srv.Close()
		cmd := exec.Command(exe)
		hideCmd(cmd)
		if err := cmd.Start(); err != nil {
			log.Println("restart failed, rolling back:", err)
			os.Rename(exe+".old", exe)
			exec.Command(exe).Start()
		}
		os.Exit(0)
	}()
}
