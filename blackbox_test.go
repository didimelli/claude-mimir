package main

import (
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "it-router")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func TestBinaryFailsFastWithoutKey(t *testing.T) {
	cmd := exec.Command(buildBinary(t), "--mimir-url", "https://m.example/anthropic", "--port", "0")
	cmd.Env = []string{"PATH=/usr/bin:/bin"} // no MIMIR_API_KEY
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "MIMIR_API_KEY") {
		t.Fatalf("want exit 2 naming MIMIR_API_KEY, got %v: %s", err, out)
	}
}

func TestBinaryShutsDownOnSIGTERM(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	cmd := exec.Command(buildBinary(t), "--port", strconv.Itoa(port), "--log", filepath.Join(t.TempDir(), "l.jsonl"))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "MIMIR_API_KEY=test-key"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	up := false
	for i := 0; i < 50 && !up; i++ {
		if r, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/health/liveliness"); err == nil {
			_ = r.Body.Close()
			up = true
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !up {
		t.Fatal("did not start")
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("did not exit within 4s of SIGTERM")
	}
}
