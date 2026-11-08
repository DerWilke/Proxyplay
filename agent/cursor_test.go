package main

import (
	"strings"
	"testing"
)

func TestCursorSetting(t *testing.T) {
	s := Settings{W: 1280, H: 720, FPS: 30, Kbps: 4000, Cursor: "quatsch"}
	sanitize(&s)
	if s.Cursor != "video" {
		t.Fatalf("unknown value must fall back to video, got %q", s.Cursor)
	}
	e := &Encoder{}
	_, a := e.args(s, 0, true, false)
	if !strings.Contains(strings.Join(a, " "), "draw_mouse=1") {
		t.Fatal("video: pointer must be drawn")
	}
	s.Cursor = "local"
	sanitize(&s)
	_, a = e.args(s, 0, true, false)
	if !strings.Contains(strings.Join(a, " "), "draw_mouse=0") {
		t.Fatal("local: pointer must not be drawn")
	}
}
