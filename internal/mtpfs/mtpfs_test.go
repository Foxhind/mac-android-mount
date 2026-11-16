package mtpfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"droidfs/internal/mtp"
	"droidfs/internal/mtp/mtpsim"
	"droidfs/internal/proto"
)

func newBackend(t *testing.T, sim *mtpsim.Device) *Backend {
	t.Helper()
	opt := DefaultOptions()
	opt.MetaTimeout, opt.DataTimeout = 2*time.Second, 2*time.Second
	opt.MTP.ReplyTimeout = 3 * time.Second
	opt.DirTTL = 200 * time.Millisecond
	b := New(func(context.Context) (mtp.Transport, error) { return sim.Open() }, opt)
	if err := b.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Disconnect)
	return b
}

func want(t *testing.T, err error, st proto.Status) {
	t.Helper()
	if !errors.Is(err, st) {
		t.Fatalf("got %v, want %v", err, st)
	}
}

func TestBackend(t *testing.T) {
	for _, noZLP := range []bool{false, true} {
		t.Run(fmt.Sprintf("noZLP=%v", noZLP), func(t *testing.T) {
			root := t.TempDir()
			sim := mtpsim.New(root)
			sim.NoZLP = noZLP
			b := newBackend(t, sim)

			// create, write at offsets, read back, sizes while editing
			if _, err := b.Create("a.bin", 0o644, true, false); err != nil {
				t.Fatal(err)
			}
			_, err := b.Create("a.bin", 0o644, true, false)
			want(t, err, proto.ErrExist)
			data := make([]byte, 300000)
			rand.Read(data)
			if _, post, err := b.Write("a.bin", 0, data); err != nil || post.Size != 300000 {
				t.Fatalf("write: size %d, %v", post.Size, err)
			}
			// Packet-boundary sizes exercise the zero-length-packet rules
			// in both directions (12-byte header + 500 = 512).
			if _, post, err := b.Write("a.bin", 300000, data[:500]); err != nil || post.Size != 300500 {
				t.Fatalf("write 2: size %d, %v", post.Size, err)
			}
			if a, err := b.Stat("a.bin"); err != nil || a.Size != 300500 {
				t.Fatalf("stat during edit: %d, %v", a.Size, err)
			}
			if err := b.Fsync("a.bin"); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(root, "a.bin"))
			if !bytes.Equal(got[:300000], data) || !bytes.Equal(got[300000:], data[:500]) {
				t.Fatal("device content differs")
			}
			for _, n := range []uint32{500, 1012, 1024, 65536, 262144} {
				out, _, _, err := b.Read("a.bin", 0, n)
				if err != nil || !bytes.Equal(out, got[:n]) {
					t.Fatalf("read %d: got %d bytes, %v", n, len(out), err)
				}
			}
			out, eof, _, err := b.Read("a.bin", 300000, 4096)
			if err != nil || !eof || len(out) != 500 {
				t.Fatalf("tail read: %d bytes eof=%v %v", len(out), eof, err)
			}
			if err := b.Truncate("a.bin", 10); err != nil {
				t.Fatal(err)
			}
			b.Fsync("a.bin")
			if fi, _ := os.Stat(filepath.Join(root, "a.bin")); fi.Size() != 10 {
				t.Fatalf("truncate: device size %d", fi.Size())
			}

			// directories; rmdir must refuse a non-empty folder
			if _, err := b.Mkdir("d", 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Mkdir("d/sub", 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Create("d/sub/f", 0o644, false, false); err != nil {
				t.Fatal(err)
			}
			want(t, b.Rmdir("d"), proto.ErrNotEmpty)
			want(t, b.Rmdir("d/sub"), proto.ErrNotEmpty)
			if _, err := os.Stat(filepath.Join(root, "d/sub/f")); err != nil {
				t.Fatalf("rmdir of a non-empty folder destroyed its contents: %v", err)
			}
			// a file added behind our back must also block rmdir
			os.Mkdir(filepath.Join(root, "quiet"), 0o755)
			time.Sleep(300 * time.Millisecond)
			if _, err := b.Stat("quiet"); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(root, "quiet", "late"), []byte("x"), 0o644)
			want(t, b.Rmdir("quiet"), proto.ErrNotEmpty)
			want(t, b.Remove("d"), proto.ErrIsDir)
			want(t, b.Rmdir("a.bin"), proto.ErrNotDir)

			// rename: same folder, across folders, onto an existing file
			if err := b.Rename("d/sub/f", "d/sub/g"); err != nil {
				t.Fatal(err)
			}
			if err := b.Rename("d/sub/g", "g2"); err != nil {
				t.Fatal(err)
			}
			if err := b.Rename("g2", "a.bin"); err != nil {
				t.Fatal(err)
			}
			if fi, err := os.Stat(filepath.Join(root, "a.bin")); err != nil || fi.Size() != 0 {
				t.Fatalf("rename over existing: %v", err)
			}
			if err := b.Rename("d", "moved"); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Stat("moved/sub"); err != nil {
				t.Fatal(err)
			}
			_, err = b.Stat("d")
			want(t, err, proto.ErrNoEnt)
			ents, err := b.ReadDir("")
			if err != nil || len(ents) != 3 {
				t.Fatalf("root listing: %d entries, %v", len(ents), err)
			}

			// re-plug: handles change, paths keep working
			sim.Replug()
			if b.Online() {
				select {
				case <-b.Down():
				case <-time.After(3 * time.Second):
					t.Fatal("re-plug not noticed")
				}
			}
			_, err = b.Stat("a.bin")
			if err == nil {
				t.Fatal("stat worked while offline")
			}
			if err := b.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Stat("moved/sub"); err != nil {
				t.Fatal(err)
			}
			if err := b.Rmdir("moved/sub"); err != nil {
				t.Fatal(err)
			}

			// frozen device: bounded failure, then recovery
			sim.Freeze()
			start := time.Now()
			if _, err := b.Stat("a.bin"); err == nil {
				// may be served from cache; force a device round trip
				_, err = b.Create("frozen", 0o644, false, false)
				if err == nil {
					t.Fatal("create succeeded on a frozen device")
				}
			}
			if d := time.Since(start); d > 4*time.Second {
				t.Fatalf("took %s to fail on a frozen device", d)
			}
			select {
			case <-b.Down():
			case <-time.After(5 * time.Second):
				t.Fatal("frozen device never declared dead")
			}
			sim.Replug()
			if err := b.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Create("after", 0o644, false, false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSeveralStorages(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Internal"), 0o755)
	os.MkdirAll(filepath.Join(root, "SD card", "DCIM"), 0o755)
	sim := mtpsim.New(root)
	sim.Storages = []string{"Internal", "SD card"}
	b := newBackend(t, sim)
	ents, err := b.ReadDir("")
	if err != nil || len(ents) != 2 {
		t.Fatalf("root: %v, %v", ents, err)
	}
	if _, err := b.Stat("SD card/DCIM"); err != nil {
		t.Fatal(err)
	}
	_, err = b.Create("top-level-file", 0o644, false, false)
	want(t, err, proto.ErrAcces)
	if _, err := b.Create("Internal/x", 0o644, false, false); err != nil {
		t.Fatal(err)
	}
	want(t, b.Rename("Internal/x", "SD card/x"), proto.ErrXDev)
	if _, err := b.Statfs(); err != nil {
		t.Fatal(err)
	}
}
