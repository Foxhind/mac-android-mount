// End-to-end tests: the real agent (built for this machine and run as a
// child process instead of through adb), the real backend, VFS and NFS
// server, and a real kernel mount. Everything except the adb transport is
// exercised exactly as in production, including losing the "device".
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"droidfs/internal/device"
	"droidfs/internal/mount"
	"droidfs/internal/mtp"
	"droidfs/internal/mtp/mtpsim"
	"droidfs/internal/mtpfs"
	"droidfs/internal/nfs"
	"droidfs/internal/proto"
	"droidfs/internal/vfs"
)

// The server side runs in a child process (this test binary re-executed),
// exactly like the real daemon: a process must never do file I/O on a mount
// that it serves itself, or the two halves can deadlock inside the kernel.
func TestMain(m *testing.M) {
	if os.Getenv("DROIDFS_E2E_SERVER") != "" {
		serverMain()
		return
	}
	os.Exit(m.Run())
}

type procStream struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out io.ReadCloser
}

func (p *procStream) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *procStream) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *procStream) Close() error {
	p.in.Close()
	p.out.Close()
	p.cmd.Process.Kill()
	go p.cmd.Wait()
	return nil
}

// serverMain is the stand-in daemon. It is driven by one-word commands on
// stdin and answers each with one line on stdout.
func serverMain() {
	agent, backing := os.Getenv("DROIDFS_E2E_AGENT"), os.Getenv("DROIDFS_E2E_BACKING")
	var mu sync.Mutex
	var procs []*exec.Cmd
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) {
		cmd := exec.Command(agent)
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		banner := make([]byte, len(proto.Banner))
		if _, err := io.ReadFull(out, banner); err != nil || string(banner) != proto.Banner {
			cmd.Process.Kill()
			return nil, fmt.Errorf("bad banner %q: %v", banner, err)
		}
		mu.Lock()
		procs = append(procs, cmd)
		mu.Unlock()
		return &procStream{cmd, in, out}, nil
	}
	signalAgents := func(sig syscall.Signal) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range procs {
			c.Process.Signal(sig)
		}
		if sig == syscall.SIGKILL {
			procs = nil
		}
	}
	defer signalAgents(syscall.SIGKILL)

	// The same suite runs over both transports: the adb agent (here a child
	// process) and MTP (here a simulated phone speaking USB packets).
	type backend interface {
		vfs.Backend
		Connect(context.Context) error
		Online() bool
	}
	var be backend
	kill := func() { signalAgents(syscall.SIGKILL) }
	freeze := func() { signalAgents(syscall.SIGSTOP) }
	replug := func() {}
	if os.Getenv("DROIDFS_E2E_KIND") == "mtp" {
		sim := mtpsim.New(backing)
		opt := mtpfs.DefaultOptions()
		opt.MetaTimeout, opt.DataTimeout = 2*time.Second, 2*time.Second
		opt.MTP.ReplyTimeout = 3 * time.Second
		be = mtpfs.New(func(context.Context) (mtp.Transport, error) { return sim.Open() }, opt)
		kill, freeze, replug = sim.Unplug, sim.Freeze, sim.Replug
	} else {
		opt := device.DefaultOptions()
		opt.LinkTimeout = 2 * time.Second
		be = device.NewBackend(dial, backing, opt)
	}
	if err := be.Connect(context.Background()); err != nil {
		fmt.Println("ERR", err)
		return
	}
	fs := vfs.New(be, vfs.DefaultOptions())
	srv, err := nfs.Serve(fs)
	if err != nil {
		fmt.Println("ERR", err)
		return
	}
	if os.Getenv("DROIDFS_TRACE") != "" {
		srv.Trace = func(f string, a ...any) {
			fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05.000 ")+f+"\n", a...)
		}
	}
	fmt.Println("PORT", srv.Port())
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		switch sc.Text() {
		case "kill":
			kill()
		case "freeze":
			freeze()
		case "connect":
			replug()
			if err := be.Connect(context.Background()); err != nil {
				fmt.Println("ERR", err)
				continue
			}
		case "online":
			fmt.Println(be.Online())
			continue
		}
		fmt.Println("ok")
	}
}

type env struct {
	t       *testing.T
	backing string // the "device" storage
	point   string // where it is mounted
	server  *exec.Cmd
	ctlIn   io.WriteCloser
	ctlOut  *bufio.Scanner
}

// ctl sends a command to the server process and returns its answer.
func (e *env) ctl(cmd string) string {
	e.t.Helper()
	fmt.Fprintln(e.ctlIn, cmd)
	if !e.ctlOut.Scan() {
		e.t.Fatalf("server process died on %q", cmd)
	}
	return e.ctlOut.Text()
}

func setup(t *testing.T, kind string) *env {
	t.Helper()
	tmp, err := os.MkdirTemp("", "droidfs-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	tmp, _ = filepath.EvalSymlinks(tmp)
	agent := filepath.Join(tmp, "agent")
	e := &env{t: t, backing: filepath.Join(tmp, "device"), point: filepath.Join(tmp, "mnt")}
	os.Mkdir(e.backing, 0o755)
	if out, err := exec.Command("go", "build", "-o", agent, "droidfs/cmd/droidfs-agent").CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	e.server = exec.Command(os.Args[0])
	e.server.Env = append(os.Environ(), "DROIDFS_E2E_SERVER=1", "DROIDFS_E2E_KIND="+kind, "DROIDFS_E2E_AGENT="+agent, "DROIDFS_E2E_BACKING="+e.backing)
	e.server.Stderr = os.Stderr
	e.ctlIn, _ = e.server.StdinPipe()
	out, _ := e.server.StdoutPipe()
	if err := e.server.Start(); err != nil {
		t.Fatal(err)
	}
	e.ctlOut = bufio.NewScanner(out)
	var port int
	if !e.ctlOut.Scan() {
		t.Fatal("server process did not start")
	}
	if _, err := fmt.Sscanf(e.ctlOut.Text(), "PORT %d", &port); err != nil {
		t.Fatalf("server process: %s", e.ctlOut.Text())
	}
	t.Cleanup(func() {
		if err := mount.Unmount(e.point, true); err != nil {
			t.Errorf("unmount: %v", err)
		}
		e.ctlIn.Close()
		e.server.Process.Kill()
		e.server.Wait()
		if !mount.IsMounted(e.point) {
			os.RemoveAll(tmp)
		}
	})
	if err := mount.Mount(context.Background(), e.point, mount.Options{Port: port, Name: "e2e"}); err != nil {
		t.Skipf("cannot mount (sandboxed?): %v", err)
	}
	return e
}

// within fails the test if fn takes longer than d: the whole point of the
// project is that nothing hangs.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		t.Logf("%s: %s", what, time.Since(start).Round(time.Millisecond))
	case <-time.After(d):
		t.Fatalf("%s: still blocked after %s", what, d)
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func TestMount(t *testing.T) {
	for _, kind := range []string{"adb", "mtp"} {
		t.Run(kind, func(t *testing.T) { suite(t, setup(t, kind)) })
	}
}

func suite(t *testing.T, e *env) {
	mnt, dev := e.point, e.backing

	t.Run("write and read back", func(t *testing.T) {
		data := []byte("hello android\n")
		if err := os.WriteFile(filepath.Join(mnt, "a.txt"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dev, "a.txt"))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("device has %q, %v", got, err)
		}
		got, err = os.ReadFile(filepath.Join(mnt, "a.txt"))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("mount has %q, %v", got, err)
		}
	})

	t.Run("file created on the device shows up", func(t *testing.T) {
		data := randBytes(3<<20 + 123)
		os.WriteFile(filepath.Join(dev, "from-device.bin"), data, 0o644)
		time.Sleep(4 * time.Second) // let directory caches (ours and the kernel's) expire
		got, err := os.ReadFile(filepath.Join(mnt, "from-device.bin"))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read %d bytes, %v", len(got), err)
		}
	})

	t.Run("large file both ways", func(t *testing.T) {
		data := randBytes(40<<20 + 4567)
		start := time.Now()
		if err := os.WriteFile(filepath.Join(mnt, "big.bin"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote 40 MiB in %s", time.Since(start).Round(time.Millisecond))
		got, err := os.ReadFile(filepath.Join(dev, "big.bin"))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("device copy differs (len %d, %v)", len(got), err)
		}
		// Drop the kernel's cached pages so the read really goes through us.
		exec.Command("/usr/sbin/purge").Run()
		start = time.Now()
		got, err = os.ReadFile(filepath.Join(mnt, "big.bin"))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read back differs (len %d, %v)", len(got), err)
		}
		t.Logf("read 40 MiB in %s", time.Since(start).Round(time.Millisecond))
	})

	t.Run("random access", func(t *testing.T) {
		p := filepath.Join(mnt, "sparse.bin")
		f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteAt([]byte("tail"), 1<<20)
		f.WriteAt([]byte("head"), 0)
		f.WriteAt([]byte("mid!"), 500000)
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(1<<20 + 2); err != nil {
			t.Fatal(err)
		}
		f.Close()
		got, _ := os.ReadFile(filepath.Join(dev, "sparse.bin"))
		if len(got) != 1<<20+2 || string(got[:4]) != "head" || string(got[500000:500004]) != "mid!" || string(got[1<<20:]) != "ta" {
			t.Fatalf("unexpected content, len %d", len(got))
		}
		// append
		f, _ = os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
		f.WriteString("END")
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		got, _ = os.ReadFile(filepath.Join(dev, "sparse.bin"))
		if len(got) != 1<<20+5 || string(got[len(got)-3:]) != "END" {
			t.Fatalf("append failed, len %d", len(got))
		}
	})

	t.Run("directories, rename, remove", func(t *testing.T) {
		d := filepath.Join(mnt, "dir", "sub")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644)
		if err := os.Rename(filepath.Join(mnt, "dir"), filepath.Join(mnt, "moved")); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(mnt, "moved", "sub", "f")); err != nil || string(b) != "x" {
			t.Fatalf("after rename: %q %v", b, err)
		}
		if _, err := os.Stat(filepath.Join(dev, "moved", "sub", "f")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(mnt, "moved")); err == nil {
			t.Fatal("removed a non-empty directory")
		}
		if err := os.RemoveAll(filepath.Join(mnt, "moved")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dev, "moved")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("still on device: %v", err)
		}
		// overwrite by rename (how most editors save)
		os.WriteFile(filepath.Join(mnt, "doc.txt"), []byte("old"), 0o644)
		os.WriteFile(filepath.Join(mnt, "doc.tmp"), []byte("new"), 0o644)
		if err := os.Rename(filepath.Join(mnt, "doc.tmp"), filepath.Join(mnt, "doc.txt")); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(filepath.Join(mnt, "doc.txt")); string(b) != "new" {
			t.Fatalf("got %q", b)
		}
	})

	t.Run("exclusive create", func(t *testing.T) {
		p := filepath.Join(mnt, "excl")
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if _, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); !errors.Is(err, os.ErrExist) {
			t.Fatalf("second exclusive create: %v", err)
		}
	})

	t.Run("big directory", func(t *testing.T) {
		const n = 3000
		d := filepath.Join(dev, "many")
		os.Mkdir(d, 0o755)
		for i := 0; i < n; i++ {
			os.WriteFile(filepath.Join(d, fmt.Sprintf("file-%05d.txt", i)), nil, 0o644)
		}
		time.Sleep(4 * time.Second)
		ents, err := os.ReadDir(filepath.Join(mnt, "many"))
		if err != nil || len(ents) != n {
			t.Fatalf("listed %d entries, %v", len(ents), err)
		}
		if err := os.RemoveAll(filepath.Join(mnt, "many")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("still on device: %v", err)
		}
	})

	t.Run("unicode names stay precomposed", func(t *testing.T) {
		name := "Привет, ёжик й.txt" // NFC
		if err := os.WriteFile(filepath.Join(mnt, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		ents, _ := os.ReadDir(dev)
		found := false
		for _, e := range ents {
			if e.Name() == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("exact (NFC) name not found on device")
		}
		if _, err := os.Stat(filepath.Join(mnt, name)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("mac metadata never reaches the device", func(t *testing.T) {
		os.WriteFile(filepath.Join(mnt, "pic.jpg"), []byte("jpeg"), 0o644)
		for _, n := range []string{"._pic.jpg", ".DS_Store"} {
			if err := os.WriteFile(filepath.Join(mnt, n), []byte("apple"), 0o644); err != nil {
				t.Fatalf("%s: %v", n, err)
			}
			if b, err := os.ReadFile(filepath.Join(mnt, n)); err != nil || string(b) != "apple" {
				t.Fatalf("%s reads back %q, %v", n, b, err)
			}
			if _, err := os.Stat(filepath.Join(dev, n)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s leaked to the device (%v)", n, err)
			}
		}
		// extended attributes are what produce ._ files in real life
		exec.Command("/usr/bin/xattr", "-w", "com.example.test", "v", filepath.Join(mnt, "pic.jpg")).Run()
		ents, _ := os.ReadDir(dev)
		for _, e := range ents {
			if len(e.Name()) > 1 && e.Name()[:2] == "._" {
				t.Fatalf("%s leaked to the device", e.Name())
			}
		}
	})

	t.Run("statfs", func(t *testing.T) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(mnt, &st); err != nil || st.Blocks == 0 {
			t.Fatalf("statfs: %+v %v", st.Blocks, err)
		}
	})

	// ------------------------------------------------ losing the device

	t.Run("device vanishes: errors, not hangs", func(t *testing.T) {
		os.Mkdir(filepath.Join(dev, "gone"), 0o755)
		os.WriteFile(filepath.Join(dev, "gone", "f"), randBytes(1<<20), 0o644)
		e.ctl("kill")
		within(t, 5*time.Second, "stat after kill", func() {
			if _, err := os.Stat(filepath.Join(mnt, "gone", "f")); err == nil {
				t.Error("stat succeeded with the device gone")
			} else {
				t.Logf("stat: %v", err)
			}
		})
		within(t, 5*time.Second, "readdir after kill", func() {
			if _, err := os.ReadDir(filepath.Join(mnt, "gone")); err == nil {
				t.Error("readdir succeeded with the device gone")
			}
		})
		within(t, 5*time.Second, "write after kill", func() {
			if err := os.WriteFile(filepath.Join(mnt, "nope"), []byte("x"), 0o644); err == nil {
				t.Error("write succeeded with the device gone")
			}
		})
		if e.ctl("online") != "false" {
			t.Fatal("backend still thinks it is online")
		}
	})

	t.Run("device returns: same mount works again", func(t *testing.T) {
		if r := e.ctl("connect"); r != "ok" {
			t.Fatal(r)
		}
		within(t, 5*time.Second, "read after reconnect", func() {
			b, err := os.ReadFile(filepath.Join(mnt, "gone", "f"))
			if err != nil || len(b) != 1<<20 {
				t.Errorf("read %d bytes: %v", len(b), err)
			}
		})
		if b, err := os.ReadFile(filepath.Join(mnt, "a.txt")); err != nil || string(b) != "hello android\n" {
			t.Fatalf("old file: %q %v", b, err)
		}
	})

	t.Run("device freezes mid-copy: bounded failure", func(t *testing.T) {
		// A file in a folder that has never been listed: looking at it
		// needs the device, whatever is cached.
		os.Mkdir(filepath.Join(dev, "unlisted"), 0o755)
		os.WriteFile(filepath.Join(dev, "unlisted", "unseen.txt"), []byte("x"), 0o644)
		waitFor(t, 15*time.Second, "new folder visible", func() bool {
			_, err := os.Stat(filepath.Join(mnt, "unlisted"))
			return err == nil
		})
		chunk := randBytes(4 << 20)
		errc := make(chan error, 1)
		var written int
		go func() {
			f, err := os.Create(filepath.Join(mnt, "interrupted.bin"))
			if err != nil {
				errc <- err
				return
			}
			defer f.Close()
			for i := 0; i < 5000; i++ {
				if _, err := f.Write(chunk); err != nil {
					errc <- err
					return
				}
				if err := f.Sync(); err != nil {
					errc <- err
					return
				}
				written += len(chunk)
			}
			errc <- nil
		}()
		time.Sleep(300 * time.Millisecond)
		e.ctl("freeze") // alive but silent: the nasty case
		start := time.Now()
		within(t, 8*time.Second, "stat of an uncached file while frozen", func() {
			_, err := os.Stat(filepath.Join(mnt, "unlisted", "unseen.txt"))
			if err == nil {
				t.Error("stat succeeded while the device was frozen")
			} else if errors.Is(err, os.ErrNotExist) {
				t.Errorf("stat claims the file does not exist: %v", err)
			}

		})
		select {
		case err := <-errc:
			if err == nil {
				t.Fatal("copy finished before the freeze; test proves nothing")
			}
			t.Logf("copy failed %s after the freeze (%d MiB in): %v", time.Since(start).Round(time.Millisecond), written>>20, err)
		case <-time.After(20 * time.Second):
			t.Fatal("copy still blocked 20s after the device froze")
		}
		e.ctl("kill")
		if r := e.ctl("connect"); r != "ok" {
			t.Fatal(r)
		}
		within(t, 5*time.Second, "write after recovery", func() {
			if err := os.WriteFile(filepath.Join(mnt, "after.txt"), []byte("ok"), 0o644); err != nil {
				t.Errorf("write after recovery: %v", err)
			}
		})
	})

	t.Run("cp -R and diff", func(t *testing.T) {
		src := filepath.Join(filepath.Dir(dev), "src")
		for i := 0; i < 40; i++ {
			d := filepath.Join(src, fmt.Sprintf("d%d", i%4), fmt.Sprintf("s%d", i%3))
			os.MkdirAll(d, 0o755)
			os.WriteFile(filepath.Join(d, fmt.Sprintf("f%d.bin", i)), randBytes(1000*i+1), 0o644)
		}
		if out, err := exec.Command("/bin/cp", "-Rp", src, filepath.Join(mnt, "copied")).CombinedOutput(); err != nil {
			t.Fatalf("cp: %v\n%s", err, out)
		}
		if out, err := exec.Command("/usr/bin/diff", "-r", src, filepath.Join(dev, "copied")).CombinedOutput(); err != nil {
			t.Fatalf("device differs from source: %v\n%s", err, out)
		}
		if out, err := exec.Command("/usr/bin/diff", "-r", src, filepath.Join(mnt, "copied")).CombinedOutput(); err != nil {
			t.Fatalf("mount differs from source: %v\n%s", err, out)
		}
		if out, err := exec.Command("/bin/mv", filepath.Join(mnt, "copied"), filepath.Join(filepath.Dir(dev), "back")).CombinedOutput(); err != nil {
			t.Fatalf("mv off the device: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dev, "copied")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("mv left the tree on the device: %v", err)
		}
	})
}

// If the daemon itself dies, the kernel must not leave processes stuck on
// the orphaned mount: calls have to fail and the mount has to be removable.
func TestDaemonDies(t *testing.T) {
	e := setup(t, "adb")
	os.WriteFile(filepath.Join(e.backing, "f"), []byte("x"), 0o644)
	if _, err := os.ReadDir(e.point); err != nil {
		t.Fatal(err)
	}
	e.server.Process.Kill() // no goodbye: like a crash
	within(t, 120*time.Second, "stat on an orphaned mount", func() {
		_, err := os.Stat(filepath.Join(e.point, "never-looked-up"))
		t.Logf("stat: %v", err)
	})
	within(t, 40*time.Second, "force unmount of an orphaned mount", func() {
		if err := mount.Unmount(e.point, true); err != nil {
			t.Errorf("unmount: %v", err)
		}
	})
	if mount.IsMounted(e.point) {
		t.Fatal("still mounted")
	}
}
