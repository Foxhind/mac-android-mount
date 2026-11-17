// Package control serves the daemon's status and commands over a Unix
// socket, for the MacDroidFS app and other local tools.
//
// The API is plain HTTP/JSON:
//
//	GET  /v1/devices             -> [Device, ...]
//	POST /v1/devices/{id}/eject  -> 204, or 404 for an unknown id
//	POST /v1/devices/{id}/mount  -> 204, or 404 for an unknown id
//
// The socket is created with mode 0600, so only the user running the daemon
// can reach it.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Foxhind/macdroidfs/internal/manager"
)

// Daemon is what the control API needs from the daemon.
type Daemon interface {
	Status() []manager.DeviceStatus
	Eject(id string) error
	Mount(id string) error
}

// Device is one entry of GET /v1/devices.
type Device struct {
	ID        string `json:"id"`
	Transport string `json:"transport"`
	Model     string `json:"model"`
	Serial    string `json:"serial"`
	Point     string `json:"point"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

// Handler returns the HTTP handler of the API.
func Handler(d Daemon) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/devices", func(w http.ResponseWriter, _ *http.Request) {
		list := d.Status()
		out := make([]Device, 0, len(list))
		for _, s := range list {
			out = append(out, Device{
				ID: s.ID, Transport: s.Transport, Model: s.Model, Serial: s.Serial,
				Point: s.Point, State: string(s.State), Error: s.Error,
			})
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /v1/devices/{id}/eject", func(w http.ResponseWriter, r *http.Request) {
		command(w, d.Eject(r.PathValue("id")))
	})
	mux.HandleFunc("POST /v1/devices/{id}/mount", func(w http.ResponseWriter, r *http.Request) {
		command(w, d.Mount(r.PathValue("id")))
	})
	return mux
}

func command(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, manager.ErrUnknownDevice):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

// Listen creates the Unix socket at path with mode 0600. A socket left at
// path by an earlier run is replaced; any other kind of file there is an
// error. Closing the listener removes the socket.
func Listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("control socket: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("control socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control socket: %w", err)
	}
	return ln, nil
}

// Serve answers API requests on ln until ctx is cancelled, then closes ln.
func Serve(ctx context.Context, ln net.Listener, d Daemon) error {
	srv := &http.Server{Handler: Handler(d), ReadHeaderTimeout: 5 * time.Second}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			srv.Close()
		case <-stopped:
		}
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("control socket: %w", err)
	}
	return nil
}
