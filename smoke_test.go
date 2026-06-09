package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// TestBuildAndBoot builds the binary and boots it against a temp data dir on a
// high loopback port, then checks the dashboard and API actually serve and that
// a clean shutdown exits zero. It is the end-to-end guard the unit tests can't
// be: wiring that compiles but wedges at startup (a bad handler mount, a store
// that won't open) shows up here and nowhere else. It NEVER binds :9000 (the
// port a real install owns). Skips gracefully when the Go toolchain or a free
// high port is unavailable.
func TestBuildAndBoot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build-and-boot smoke test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; skipping build-and-boot smoke test")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "pingularity")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	// Build the current package (the test runs in the repo root, where main lives).
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build failed: %v", err)
	}

	port := freeHighPort(t)
	addr := "127.0.0.1:" + port
	dbPath := filepath.Join(dir, "smoke.db")

	cmd := exec.Command(bin, "run", "-listen", addr, "-db", dbPath)
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	// Drain stderr so the child never blocks on a full pipe, and surface it if the
	// test fails.
	var errBuf []byte
	drained := make(chan struct{})
	go func() { errBuf, _ = io.ReadAll(stderr); close(drained) }()

	// Ensure the process is reaped even on an early failure.
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	base := "http://" + addr
	client := &http.Client{Timeout: 2 * time.Second}

	// Poll until the API answers or we give up.
	deadline := time.Now().Add(15 * time.Second)
	var ready bool
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/api/status")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		if cmd.ProcessState != nil {
			break // process already exited
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		<-drained
		t.Fatalf("daemon did not serve /api/status within the deadline; stderr:\n%s", errBuf)
	}

	// /api/status is JSON with the fields the dashboard reads.
	resp, err := client.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	var status map[string]any
	dec := json.NewDecoder(resp.Body)
	decErr := dec.Decode(&status)
	resp.Body.Close()
	if decErr != nil {
		t.Fatalf("decode /api/status: %v", decErr)
	}
	if _, ok := status["online"]; !ok {
		t.Errorf("/api/status missing \"online\" field: %v", status)
	}

	// The dashboard HTML is served at the root.
	rootResp, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	rootBody, _ := io.ReadAll(rootResp.Body)
	rootResp.Body.Close()
	if rootResp.StatusCode != http.StatusOK {
		t.Errorf("GET / status = %d, want 200", rootResp.StatusCode)
	}
	if len(rootBody) == 0 {
		t.Error("GET / returned an empty body")
	}

	// Shut down and assert a clean exit. On Unix the interactive service returns
	// zero on SIGINT/SIGTERM; on Windows there is no graceful signal, so just kill.
	if runtime.GOOS == "windows" {
		_ = cmd.Process.Kill()
		killed = true
		_ = cmd.Wait()
		return
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		killed = true
		if err != nil {
			<-drained
			t.Fatalf("expected a clean exit after SIGINT, got %v; stderr:\n%s", err, errBuf)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		killed = true
		<-waitErr
		t.Fatal("daemon did not exit within 10s of SIGINT")
	}
}

// freeHighPort returns a currently-free loopback port in the 19000-19999 range
// (never 9000, which a real install owns). Skips if none is free.
func freeHighPort(t *testing.T) string {
	t.Helper()
	for p := 19000; p < 20000; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			l.Close()
			return strconv.Itoa(p)
		}
	}
	t.Skip("no free port in 19000-19999")
	return ""
}
