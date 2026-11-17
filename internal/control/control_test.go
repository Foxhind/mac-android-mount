package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Foxhind/macdroidfs/internal/manager"
)

type fakeDaemon struct {
	devices []manager.DeviceStatus
	calls   []string
}

func (f *fakeDaemon) Status() []manager.DeviceStatus { return f.devices }

func (f *fakeDaemon) Eject(id string) error { return f.do("eject", id) }

func (f *fakeDaemon) Mount(id string) error { return f.do("mount", id) }

func (f *fakeDaemon) do(cmd, id string) error {
	for _, d := range f.devices {
		if d.ID == id {
			f.calls = append(f.calls, cmd+" "+id)
			return nil
		}
	}
	return fmt.Errorf("%w: %s", manager.ErrUnknownDevice, id)
}

func TestHandler_Devices(t *testing.T) {
	d := &fakeDaemon{devices: []manager.DeviceStatus{{
		ID: "adb:X1", Transport: "adb", Model: "Pixel 8", Serial: "X1",
		Point: "/Users/u/MacDroidFS/Pixel 8", State: manager.StateMounted,
	}}}
	rec := httptest.NewRecorder()
	Handler(d).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/devices", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got []Device
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := Device{ID: "adb:X1", Transport: "adb", Model: "Pixel 8", Serial: "X1",
		Point: "/Users/u/MacDroidFS/Pixel 8", State: "mounted"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want [%+v]", got, want)
	}
}

func TestHandler_EmptyListIsArray(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(&fakeDaemon{}).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/devices", nil))
	if body := rec.Body.String(); body != "[]" {
		t.Fatalf("body %q, want []", body)
	}
}

func TestHandler_Commands(t *testing.T) {
	tests := []struct {
		name, method, path string
		wantCode           int
		wantCall           string
	}{
		{"eject", "POST", "/v1/devices/adb:X1/eject", http.StatusNoContent, "eject adb:X1"},
		{"mount", "POST", "/v1/devices/adb:X1/mount", http.StatusNoContent, "mount adb:X1"},
		{"escaped id", "POST", "/v1/devices/mtp%3AAB%2FC/mount", http.StatusNoContent, "mount mtp:AB/C"},
		{"unknown id", "POST", "/v1/devices/adb:nope/eject", http.StatusNotFound, ""},
		{"wrong method", "GET", "/v1/devices/adb:X1/eject", http.StatusMethodNotAllowed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDaemon{devices: []manager.DeviceStatus{{ID: "adb:X1"}, {ID: "mtp:AB/C"}}}
			rec := httptest.NewRecorder()
			Handler(d).ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.wantCode {
				t.Errorf("status %d, want %d", rec.Code, tt.wantCode)
			}
			var call string
			if len(d.calls) > 0 {
				call = d.calls[0]
			}
			if call != tt.wantCall {
				t.Errorf("call %q, want %q", call, tt.wantCall)
			}
		})
	}
}

func TestServe_SocketLifecycle(t *testing.T) {
	// Socket paths are limited to ~104 bytes; t.TempDir can be longer.
	dir, err := os.MkdirTemp("/tmp", "macdroidfs-ctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "c.sock")

	// A stale socket from an earlier run is replaced.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()

	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode %o, want 600", perm)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, &fakeDaemon{}) }()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
	resp, err := client.Get("http://macdroidfs/v1/devices")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
}

func TestListen_RefusesNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ln, err := Listen(path); err == nil {
		ln.Close()
		t.Fatal("Listen accepted a regular file at the socket path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("regular file was removed: %v", err)
	}
}
