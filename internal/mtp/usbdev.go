package mtp

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"droidfs/internal/usb"
)

// USBDevice is an attached device that announces an MTP interface.
type USBDevice struct {
	Bus, Addr uint8
	VID, PID  uint16
	Serial    string
	Product   string
	iface     usb.Interface
}

// Key identifies the physical attachment (it changes on re-plug).
func (d USBDevice) Key() string { return fmt.Sprintf("%d.%d.%04x.%04x", d.Bus, d.Addr, d.VID, d.PID) }

// shaped reports whether an interface has the pipes MTP needs.
func shaped(i usb.Interface) bool { return i.In != 0 && i.Out != 0 && i.Intr != 0 }

// Probe looks at one attached device and reports whether it is an Android
// device in MTP ("File transfer") mode. Android names the interface "MTP";
// cameras and iPhones, which also speak PTP, do not, and are left alone.
func Probe(info usb.Info) (USBDevice, bool) {
	var cands []usb.Interface
	for _, i := range info.Ifaces {
		still := i.Class == 6 && i.SubClass == 1 && i.Protocol == 1
		vendor := i.Class == 0xFF && i.SubClass == 0xFF && i.Protocol == 0
		if (still || vendor) && shaped(i) && i.NameIdx != 0 {
			cands = append(cands, i)
		}
	}
	if len(cands) == 0 {
		return USBDevice{}, false
	}
	dev, err := usb.Open(info.Bus, info.Addr)
	if err != nil {
		return USBDevice{}, false
	}
	defer dev.Close()
	for _, i := range cands {
		if strings.Contains(strings.ToUpper(dev.String(i.NameIdx)), "MTP") {
			return USBDevice{
				Bus: info.Bus, Addr: info.Addr, VID: info.VID, PID: info.PID,
				Serial: dev.Serial(), Product: dev.Product(), iface: i,
			}, true
		}
	}
	return USBDevice{}, false
}

type usbTransport struct {
	dev   *usb.Device
	iface usb.Interface
}

// OpenUSB opens the device and takes its MTP interface.
//
// On macOS the system's own camera helper (ptpcamerad) grabs every PTP-class
// interface it sees and holds it exclusively. With steal set, the helper is
// asked to quit so that the interface can be claimed; launchd restarts it on
// demand, and it cannot take the interface back while we hold it.
func OpenUSB(d USBDevice, steal bool) (Transport, error) {
	dev, err := usb.Open(d.Bus, d.Addr)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		err = dev.Claim(d.iface.Num)
		if err == nil {
			break
		}
		busy := errors.Is(err, usb.ErrAccess) || errors.Is(err, usb.ErrBusy)
		if !busy || !steal || attempt >= 5 {
			dev.Close()
			if busy {
				return nil, fmt.Errorf("MTP interface is held by another program (%w)", err)
			}
			return nil, err
		}
		exec.Command("/usr/bin/killall", "ptpcamerad").Run()
		time.Sleep(150 * time.Millisecond)
	}
	return &usbTransport{dev: dev, iface: d.iface}, nil
}

func (t *usbTransport) Write(b []byte, timeout time.Duration) error {
	n, err := t.dev.Bulk(t.iface.Out, b, timeout)
	if err != nil {
		return err
	}
	if n != len(b) {
		return fmt.Errorf("usb: short write (%d of %d)", n, len(b))
	}
	return nil
}

func (t *usbTransport) Read(b []byte, timeout time.Duration) (int, error) {
	return t.dev.Bulk(t.iface.In, b, timeout)
}

func (t *usbTransport) ReadEvent(b []byte, timeout time.Duration) (int, error) {
	n, err := t.dev.Interrupt(t.iface.Intr, b, timeout)
	if errors.Is(err, usb.ErrTimeout) {
		return 0, ErrNoEvent
	}
	return n, err
}

func (t *usbTransport) MaxPacket() (int, int) { return t.iface.InMax, t.iface.OutMax }

func (t *usbTransport) Close() { t.dev.Close() }
