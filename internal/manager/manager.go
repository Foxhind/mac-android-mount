// Package manager watches adb for devices and keeps one mount per device
// alive across disconnects.
//
// Life of a device ("unit"):
//
//	appears in adb   -> start agent, mount
//	link lost        -> every file operation fails at once with EIO;
//	                    the mount is kept for the grace period
//	back in time     -> reconnect; the same mount keeps working
//	grace expired    -> force-unmount; remount when the device returns
package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"droidfs/internal/adb"
	"droidfs/internal/device"
	"droidfs/internal/mount"
	"droidfs/internal/mtp"
	"droidfs/internal/mtpfs"
	"droidfs/internal/nfs"
	"droidfs/internal/usb"
	"droidfs/internal/vfs"
)

// Config holds the daemon settings, mostly taken from command-line flags.
type Config struct {
	Mode            string        // "auto", "adb" or "mtp"
	Serial          string        // only this device; empty = all
	Root            string        // directory on the device to export (adb)
	MountDir        string        // parent directory of the mount points
	Grace           time.Duration // how long a mount survives a lost link
	ReadOnly        bool
	KeepMacMetadata bool
	KeepPTPCamerad  bool // never ask macOS's camera helper to let go of a device
	Open            bool // reveal the volume in Finder after mounting
	Trace           bool // log every NFS request
	Log             *log.Logger
}

// In auto mode a device seen over USB is given this long to show up in adb
// as well before it is mounted over MTP: adb is the better transport.
const mtpSettle = 2500 * time.Millisecond

type mtpSeen struct {
	dev   mtp.USBDevice
	since time.Time
}

// Manager tracks the devices seen over adb and USB and runs one mounted
// unit per device.
type Manager struct {
	cfg Config
	adb *adb.Client

	mu       sync.Mutex
	units    map[string]*unit      // "adb:<serial>" / "mtp:<id>"
	adbReady map[string]adb.Device // by serial
	mtpNow   map[string]mtpSeen    // by id
	hinted   map[string]string     // serial -> last non-ready state we complained about
}

// New returns a manager for cfg; an empty Mode means "auto".
func New(cfg Config) *Manager {
	if cfg.Mode == "" {
		cfg.Mode = "auto"
	}
	return &Manager{
		cfg: cfg, adb: adb.New(),
		units: map[string]*unit{}, adbReady: map[string]adb.Device{},
		mtpNow: map[string]mtpSeen{}, hinted: map[string]string{},
	}
}

// Run blocks until ctx is cancelled, then unmounts everything.
func (m *Manager) Run(ctx context.Context) error {
	if err := os.MkdirAll(m.cfg.MountDir, 0o755); err != nil {
		return err
	}
	// Mounts left behind by a daemon that died: their server is gone, so
	// they are dead weight (and would only return errors).
	for _, st := range mount.Ours() {
		m.cfg.Log.Printf("removing stale mount %s", st.Point)
		if err := mount.Unmount(st.Point, true); err != nil {
			m.cfg.Log.Printf("  %v", err)
		}
		os.Remove(st.Point)
	}

	useADB := m.cfg.Mode != "mtp"
	useMTP := m.cfg.Mode != "adb"
	if useMTP && !usb.Available {
		if m.cfg.Mode == "mtp" {
			return fmt.Errorf("this build has no USB support (built without cgo/libusb), so MTP mode is unavailable")
		}
		useMTP = false
	}
	switch {
	case useADB && useMTP:
		m.cfg.Log.Printf("waiting for Android devices (adb, or MTP \"File transfer\" mode)...")
	case useADB:
		m.cfg.Log.Printf("waiting for Android devices (USB debugging must be enabled)...")
	default:
		m.cfg.Log.Printf("waiting for Android devices in MTP \"File transfer\" mode...")
	}
	var wg sync.WaitGroup
	if useADB {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.watchADB(ctx)
		}()
	}
	if useMTP {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.watchUSB(ctx)
		}()
	}
	<-ctx.Done()
	wg.Wait()

	m.mu.Lock()
	units := make([]*unit, 0, len(m.units))
	for _, u := range m.units {
		units = append(units, u)
	}
	m.mu.Unlock()
	for _, u := range units {
		close(u.stop)
	}
	for _, u := range units {
		<-u.done
	}
	return nil
}

func (m *Manager) watchADB(ctx context.Context) {
	lastErr := ""
	for ctx.Err() == nil {
		err := m.adb.Track(ctx, func(list []adb.Device) {
			lastErr = ""
			m.onADB(list)
		})
		if ctx.Err() != nil {
			return
		}
		// Lost the adb server itself: every adb device is unreachable.
		m.onADB(nil)
		if msg := fmt.Sprint(err); msg != lastErr {
			m.cfg.Log.Printf("adb: %v (will keep trying)", err)
			lastErr = msg
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *Manager) onADB(list []adb.Device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adbReady = map[string]adb.Device{}
	for _, d := range list {
		if m.cfg.Serial != "" && d.Serial != m.cfg.Serial {
			continue
		}
		if !d.Ready() {
			if m.hinted[d.Serial] != d.State {
				m.hinted[d.Serial] = d.State
				hint := ""
				if d.State == "unauthorized" {
					hint = " - confirm the \"Allow USB debugging\" prompt on the device"
				}
				m.cfg.Log.Printf("%s: %s%s", d.Serial, d.State, hint)
			}
			continue
		}
		delete(m.hinted, d.Serial)
		m.adbReady[d.Serial] = d
	}
	m.reconcileLocked()
}

// watchUSB polls the USB bus for Android devices in MTP mode. A device is
// opened (briefly, to read its descriptor strings) only once per attachment.
func (m *Manager) watchUSB(ctx context.Context) {
	type probed struct {
		dev mtp.USBDevice
		ok  bool
	}
	cache := map[string]probed{}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		infos, err := usb.List()
		if err == nil {
			attached := map[string]bool{}
			now := map[string]mtp.USBDevice{}
			for _, info := range infos {
				key := fmt.Sprintf("%d.%d.%04x.%04x", info.Bus, info.Addr, info.VID, info.PID)
				attached[key] = true
				p, seen := cache[key]
				if !seen {
					p.dev, p.ok = mtp.Probe(info)
					cache[key] = p
				}
				if !p.ok || (m.cfg.Serial != "" && p.dev.Serial != m.cfg.Serial) {
					continue
				}
				id := p.dev.Serial
				if id == "" {
					id = fmt.Sprintf("%04x-%04x", info.VID, info.PID)
				}
				now[id] = p.dev
			}
			for key := range cache {
				if !attached[key] {
					delete(cache, key)
				}
			}
			m.mu.Lock()
			for id := range m.mtpNow {
				if _, ok := now[id]; !ok {
					delete(m.mtpNow, id)
				}
			}
			for id, dev := range now {
				seen, ok := m.mtpNow[id]
				if !ok {
					seen.since = time.Now()
				}
				seen.dev = dev
				m.mtpNow[id] = seen
			}
			m.reconcileLocked()
			m.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reconcileLocked brings the set of units and their presence in line with
// what adb and the USB bus currently show.
func (m *Manager) reconcileLocked() {
	present := map[string]bool{}
	for serial, d := range m.adbReady {
		key := "adb:" + serial
		present[key] = true
		if m.units[key] == nil {
			dialer := &device.ADBDialer{ADB: m.adb, Serial: serial}
			be := device.NewBackend(dialer.Dial, m.cfg.Root, device.DefaultOptions())
			m.startUnitLocked(key, strings.ReplaceAll(d.Model, "_", " "), serial, be)
		}
	}
	for id, seen := range m.mtpNow {
		if m.cfg.Mode == "auto" {
			if _, viaADB := m.adbReady[seen.dev.Serial]; viaADB && seen.dev.Serial != "" {
				continue // same phone, better transport
			}
			if time.Since(seen.since) < mtpSettle {
				continue
			}
		}
		key := "mtp:" + id
		present[key] = true
		u := m.units[key]
		if u == nil {
			u = m.startUnitLocked(key, seen.dev.Product, id, nil)
			steal := !m.cfg.KeepPTPCamerad
			u.be = mtpfs.New(func(context.Context) (mtp.Transport, error) {
				u.mu.Lock()
				dev := u.usbDev
				u.mu.Unlock()
				return mtp.OpenUSB(dev, steal)
			}, mtpfs.DefaultOptions())
			u.usbDev = seen.dev
			go u.run()
		}
		u.mu.Lock()
		u.usbDev = seen.dev
		u.mu.Unlock()
	}
	for key, u := range m.units {
		u.setPresent(present[key])
	}
}

// ------------------------------------------------------------------- unit

// backend is what a unit needs from a transport: the file operations plus
// connection management. Both the adb agent and MTP provide it.
type backend interface {
	vfs.Backend
	Connect(ctx context.Context) error
	Disconnect()
	Online() bool
	Down() <-chan struct{}
	Root() string
}

type unit struct {
	m         *Manager
	key       string // "adb:<serial>" / "mtp:<id>"
	transport string // "adb" or "mtp"
	model     string
	serial    string
	name      string
	point     string
	log       *log.Logger

	be     backend
	usbDev mtp.USBDevice // MTP units: where the device currently sits
	fs     *vfs.FS
	srv    *nfs.Server

	mu       sync.Mutex
	present  bool
	arrived  bool // set on every absent -> present transition
	ejectReq bool // the user asked to unmount
	mountReq bool // the user asked to mount again after an eject
	// What run last published, for Status.
	mounted bool
	ejected bool
	lastErr string
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == ':' || r == 0 || r < 0x20:
			return '-'
		}
		return r
	}, strings.TrimSpace(s))
	return strings.Trim(s, ".- ")
}

// startUnitLocked creates a unit. With a nil backend the caller sets one
// and starts the unit itself.
func (m *Manager) startUnitLocked(key, model, id string, be backend) *unit {
	name := sanitize(model)
	if name == "" {
		name = sanitize(id)
	}
	point := filepath.Join(m.cfg.MountDir, name)
	for _, other := range m.units {
		if other.point == point {
			point = filepath.Join(m.cfg.MountDir, name+" "+sanitize(id))
		}
	}
	transport, _, _ := strings.Cut(key, ":")
	u := &unit{
		m:         m,
		key:       key,
		transport: transport,
		model:     model,
		serial:    id,
		name:      sanitize(strings.ReplaceAll(key, ":", "-")),
		point:     point,
		log:       log.New(m.cfg.Log.Writer(), m.cfg.Log.Prefix()+"["+name+"] ", m.cfg.Log.Flags()),
		be:        be,
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	m.units[key] = u
	if be != nil {
		go u.run()
	}
	return u
}

func (u *unit) setPresent(p bool) {
	u.mu.Lock()
	if p && !u.present {
		u.arrived = true
	}
	changed := u.present != p
	u.present = p
	u.mu.Unlock()
	if changed {
		u.poke()
	}
}

// poke wakes run without blocking.
func (u *unit) poke() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

// publish records run's view of the unit for Status.
func (u *unit) publish(mounted, ejected bool, lastErr string) {
	u.mu.Lock()
	u.mounted, u.ejected, u.lastErr = mounted, ejected, lastErr
	u.mu.Unlock()
}

func (u *unit) run() {
	defer close(u.done)
	cfg := u.m.cfg

	// ctx is cancelled when the unit is stopped, so a connect or mount in
	// progress gives up at once.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-u.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	vopt := vfs.DefaultOptions()
	vopt.ReadOnly = cfg.ReadOnly
	vopt.KeepMacMetadata = cfg.KeepMacMetadata
	u.fs = vfs.New(u.be, vopt)
	defer u.fs.Close()
	srv, err := nfs.Serve(u.fs)
	if err != nil {
		u.log.Printf("cannot start NFS server: %v", err)
		return
	}
	u.srv = srv
	defer srv.Close()
	if cfg.Trace {
		srv.Trace = u.log.Printf
	}

	var (
		mounted      bool
		ejected      bool // the user unmounted it; wait for a re-plug
		offlineSince time.Time
		nextTry      time.Time
		backoff      = 500 * time.Millisecond
		lastErr      string
	)
	unmount := func(force bool, why string) {
		if !mounted {
			return
		}
		u.log.Printf("unmounting %s (%s)", u.point, why)
		if err := mount.Unmount(u.point, force); err != nil {
			u.log.Printf("unmount failed: %v", err)
		} else {
			os.Remove(u.point) // only succeeds on an empty, unmounted directory
		}
		mounted = false
	}

	for {
		u.mu.Lock()
		present, arrived := u.present, u.arrived
		ejectReq, mountReq := u.ejectReq, u.mountReq
		u.arrived, u.ejectReq, u.mountReq = false, false, false
		u.mu.Unlock()
		now := time.Now()
		if arrived || mountReq {
			ejected = false
			nextTry, backoff = time.Time{}, 500*time.Millisecond
		}
		if ejectReq && !ejected {
			// Like ejecting in Finder: a busy volume stays mounted.
			if mounted {
				if err := mount.Unmount(u.point, false); err != nil {
					u.log.Printf("eject failed: %v", err)
					lastErr = "eject failed: " + err.Error()
				} else {
					u.log.Printf("ejected %s; it stays unmounted until you mount it again or re-plug the device", u.point)
					os.Remove(u.point)
					mounted, ejected, lastErr = false, true, ""
				}
			} else {
				ejected = true
			}
		}

		online := u.be.Online()
		if online && !present {
			// adb no longer lists it: do not wait for the watchdog.
			u.be.Disconnect()
			online = false
		}
		if !online && present && !now.Before(nextTry) {
			cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
			err := u.be.Connect(cctx)
			ccancel()
			if ctx.Err() != nil {
				continue // stopping; the select below returns
			}
			if err != nil {
				if msg := err.Error(); msg != lastErr {
					u.log.Printf("cannot connect: %v", err)
					lastErr = msg
				}
				nextTry = time.Now().Add(backoff)
				backoff = min(backoff*2, 5*time.Second)
			} else {
				online, lastErr, backoff = true, "", 500*time.Millisecond
				if mounted {
					u.log.Printf("reconnected; %s is live again", u.point)
				} else {
					u.log.Printf("connected (exporting %s)", u.be.Root())
				}
			}
		}

		if mounted && !mount.IsMounted(u.point) {
			u.log.Printf("%s was unmounted; re-plug the device to mount it again", u.point)
			os.Remove(u.point)
			mounted, ejected = false, true
		}

		if online {
			offlineSince = time.Time{}
			if !mounted && !ejected && !now.Before(nextTry) {
				err := mount.Mount(ctx, u.point, mount.Options{
					Port: u.srv.Port(), Name: u.name, ReadOnly: cfg.ReadOnly,
				})
				if err != nil {
					u.log.Printf("%v", err)
					lastErr = err.Error()
					nextTry = time.Now().Add(3 * time.Second)
				} else {
					mounted, lastErr = true, ""
					u.log.Printf("mounted at %s", u.point)
					if cfg.Open {
						exec.Command("/usr/bin/open", u.point).Start()
					}
				}
			}
		} else {
			if offlineSince.IsZero() {
				offlineSince = now
				if mounted {
					u.log.Printf("device link lost; keeping the mount for %s", cfg.Grace)
				}
			}
			if mounted && now.Sub(offlineSince) >= cfg.Grace {
				unmount(true, "device gone")
			}
		}

		u.publish(mounted, ejected, lastErr)

		// Sleep until something can change.
		wait := 2 * time.Second // periodic check for user-initiated unmounts
		if !online {
			if present {
				wait = min(wait, max(time.Until(nextTry), 50*time.Millisecond))
			}
			if mounted {
				wait = min(wait, max(cfg.Grace-time.Since(offlineSince), 50*time.Millisecond))
			}
		} else if !mounted && !ejected {
			wait = min(wait, max(time.Until(nextTry), 50*time.Millisecond))
		}
		var down <-chan struct{}
		if online {
			down = u.be.Down()
		}
		t := time.NewTimer(wait)
		select {
		case <-u.stop:
			t.Stop()
			unmount(!u.be.Online(), "shutting down")
			u.be.Disconnect()
			return
		case <-u.wake:
		case <-down:
		case <-t.C:
		}
		t.Stop()
	}
}

// State is where a device stands, as shown to the user.
type State string

// The states a device can be in.
const (
	StateMounted      State = "mounted"      // online and mounted
	StateOffline      State = "offline"      // link lost; the mount is kept for the grace period
	StateConnecting   State = "connecting"   // present, not mounted yet
	StateEjected      State = "ejected"      // unmounted by the user; stays so until Mount or a re-plug
	StateDisconnected State = "disconnected" // not attached
)

// DeviceStatus describes one device the daemon has seen since it started.
type DeviceStatus struct {
	ID        string // stable key for Eject and Mount
	Transport string // "adb" or "mtp"
	Model     string
	Serial    string
	Point     string // mount point, also while unmounted
	State     State
	Error     string // last connect, mount or eject failure, if any
}

// ErrUnknownDevice is returned for an ID that Status never listed.
var ErrUnknownDevice = errors.New("unknown device")

// Status lists every device seen since the daemon started, sorted by ID.
func (m *Manager) Status() []DeviceStatus {
	m.mu.Lock()
	units := make([]*unit, 0, len(m.units))
	for _, u := range m.units {
		units = append(units, u)
	}
	m.mu.Unlock()
	out := make([]DeviceStatus, 0, len(units))
	for _, u := range units {
		out = append(out, u.status())
	}
	slices.SortFunc(out, func(a, b DeviceStatus) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func (u *unit) status() DeviceStatus {
	online := u.be != nil && u.be.Online()
	u.mu.Lock()
	defer u.mu.Unlock()
	st := StateDisconnected
	switch {
	case u.mounted && online:
		st = StateMounted
	case u.mounted:
		st = StateOffline
	case u.ejected:
		st = StateEjected
	case u.present:
		st = StateConnecting
	}
	return DeviceStatus{
		ID: u.key, Transport: u.transport, Model: u.model, Serial: u.serial,
		Point: u.point, State: st, Error: u.lastErr,
	}
}

// Eject unmounts a device's volume, as ejecting it in Finder would; it is
// not mounted again until Mount is called or the device is re-plugged. It
// returns before the unmount is done: Status shows the outcome.
func (m *Manager) Eject(id string) error {
	return m.request(id, func(u *unit) { u.ejectReq = true })
}

// Mount mounts an ejected device's volume again. Like Eject, it returns
// before the work is done.
func (m *Manager) Mount(id string) error {
	return m.request(id, func(u *unit) { u.mountReq = true })
}

func (m *Manager) request(id string, set func(*unit)) error {
	m.mu.Lock()
	u := m.units[id]
	m.mu.Unlock()
	if u == nil {
		return fmt.Errorf("%w: %s", ErrUnknownDevice, id)
	}
	u.mu.Lock()
	set(u)
	u.mu.Unlock()
	u.poke()
	return nil
}

// Devices lists what adb currently sees.
func Devices(ctx context.Context) ([]adb.Device, error) {
	return adb.New().Devices(ctx)
}

// UnmountAll force-unmounts every droidfs mount, whoever created it.
func UnmountAll(l *log.Logger) error {
	var firstErr error
	for _, st := range mount.Ours() {
		l.Printf("unmounting %s", st.Point)
		if err := mount.Unmount(st.Point, true); err != nil {
			l.Printf("  %v", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("could not unmount %s: %w", st.Point, err)
			}
			continue
		}
		os.Remove(st.Point)
	}
	return firstErr
}

// MTPDevices lists attached Android devices that are in MTP mode.
func MTPDevices() []mtp.USBDevice {
	infos, _ := usb.List()
	var out []mtp.USBDevice
	for _, info := range infos {
		if d, ok := mtp.Probe(info); ok {
			out = append(out, d)
		}
	}
	return out
}
