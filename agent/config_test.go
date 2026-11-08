package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func req(remote, key string) bool {
	r := httptest.NewRequest("GET", "/api/ping", nil)
	r.RemoteAddr = remote + ":5555"
	if key != "" {
		r.Header.Set("X-Spiel-Key", key)
	}
	return allowed(r)
}

func TestAccessAndConfig(t *testing.T) {
	oldIP, oldKey, oldURL := allowedIP, secret, updateURL
	defer func() { allowedIP, secret, updateURL, baseDir = oldIP, oldKey, oldURL, "" }()

	// no config: only localhost
	allowedIP, secret = "", ""
	if !req("127.0.0.1", "") || req("10.0.0.5", "") || req("10.0.0.5", "x") {
		t.Fatal("without config only localhost may connect")
	}

	baseDir = t.TempDir()
	cfg := `{"server_ip":"10.0.0.5","key":"k123","update_url":"http://10.0.0.5:8104/dl/spiel-agent.exe"}`
	if err := os.WriteFile(filepath.Join(baseDir, "spiel-config.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	loadConfig()
	if allowedIP != "10.0.0.5" || secret != "k123" || updateURL != "http://10.0.0.5:8104/dl/spiel-agent.exe" {
		t.Fatalf("config not applied: %q %q %q", allowedIP, secret, updateURL)
	}
	if !req("10.0.0.5", "k123") {
		t.Fatal("server with right key must be allowed")
	}
	if req("10.0.0.5", "") || req("10.0.0.5", "wrong") || req("10.0.0.6", "k123") {
		t.Fatal("wrong key, missing key or other host must be refused")
	}
	if !req("127.0.0.1", "") {
		t.Fatal("localhost must stay allowed")
	}
}
