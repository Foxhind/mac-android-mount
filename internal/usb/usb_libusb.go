//go:build darwin && cgo && !nomtp

// Package usb is a thin layer over libusb: enumerate devices, open one,
// and do bulk/interrupt transfers that always carry a timeout.
package usb

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
*/
import "C"

import (
	"sync"
	"time"
	"unsafe"
)

// Available reports whether USB access was compiled in.
const Available = true

var (
	initOnce sync.Once
	initErr  error
	ctx      *C.libusb_context
)

func initLib() error {
	initOnce.Do(func() {
		if rc := C.libusb_init(&ctx); rc != 0 {
			initErr = Error(rc)
		}
	})
	return initErr
}

// Error is a libusb error code.
type Error int

const (
	ErrIO       Error = -1
	ErrAccess   Error = -3
	ErrNoDevice Error = -4
	ErrNotFound Error = -5
	ErrBusy     Error = -6
	ErrTimeout  Error = -7
	ErrOverflow Error = -8
	ErrPipe     Error = -9
)

// Error implements error with libusb's description of the code.
func (e Error) Error() string { return "usb: " + C.GoString(C.libusb_strerror(C.int(e))) }

// Interface describes the first alternate setting of one interface.
type Interface struct {
	Num, Class, SubClass, Protocol uint8
	NameIdx                        uint8
	In, Out, Intr                  uint8 // endpoint addresses, 0 if absent
	InMax, OutMax                  int   // max packet sizes
	Endpoints                      int
}

// Info identifies an attached device without opening it.
type Info struct {
	Bus, Addr uint8
	VID, PID  uint16
	Ifaces    []Interface
}

func describe(dev *C.libusb_device) (Info, C.struct_libusb_device_descriptor, bool) {
	var dd C.struct_libusb_device_descriptor
	if C.libusb_get_device_descriptor(dev, &dd) != 0 {
		return Info{}, dd, false
	}
	info := Info{
		Bus:  uint8(C.libusb_get_bus_number(dev)),
		Addr: uint8(C.libusb_get_device_address(dev)),
		VID:  uint16(dd.idVendor),
		PID:  uint16(dd.idProduct),
	}
	var cfg *C.struct_libusb_config_descriptor
	if C.libusb_get_active_config_descriptor(dev, &cfg) != 0 {
		if C.libusb_get_config_descriptor(dev, 0, &cfg) != 0 {
			return info, dd, true
		}
	}
	defer C.libusb_free_config_descriptor(cfg)
	ifaces := unsafe.Slice(cfg._interface, int(cfg.bNumInterfaces))
	for _, ifc := range ifaces {
		if ifc.num_altsetting < 1 {
			continue
		}
		alt := ifc.altsetting // first alternate setting
		out := Interface{
			Num:       uint8(alt.bInterfaceNumber),
			Class:     uint8(alt.bInterfaceClass),
			SubClass:  uint8(alt.bInterfaceSubClass),
			Protocol:  uint8(alt.bInterfaceProtocol),
			NameIdx:   uint8(alt.iInterface),
			Endpoints: int(alt.bNumEndpoints),
		}
		if alt.bNumEndpoints > 0 && alt.endpoint != nil {
			for _, ep := range unsafe.Slice(alt.endpoint, int(alt.bNumEndpoints)) {
				addr, kind := uint8(ep.bEndpointAddress), uint8(ep.bmAttributes)&3
				switch {
				case kind == 2 && addr&0x80 != 0 && out.In == 0:
					out.In, out.InMax = addr, int(ep.wMaxPacketSize)
				case kind == 2 && addr&0x80 == 0 && out.Out == 0:
					out.Out, out.OutMax = addr, int(ep.wMaxPacketSize)
				case kind == 3 && addr&0x80 != 0 && out.Intr == 0:
					out.Intr = addr
				}
			}
		}
		info.Ifaces = append(info.Ifaces, out)
	}
	return info, dd, true
}

// List returns the attached devices.
func List() ([]Info, error) {
	if err := initLib(); err != nil {
		return nil, err
	}
	var list **C.libusb_device
	n := C.libusb_get_device_list(ctx, &list)
	if n < 0 {
		return nil, Error(n)
	}
	defer C.libusb_free_device_list(list, 1)
	var out []Info
	for _, dev := range unsafe.Slice(list, int(n)) {
		if info, _, ok := describe(dev); ok {
			out = append(out, info)
		}
	}
	return out, nil
}

// Device is an open device.
type Device struct {
	Info
	h                      *C.libusb_device_handle
	iManuf, iProd, iSerial uint8
	claimed                []uint8
	mu                     sync.Mutex
	closed                 bool
}

// Open opens the device currently at bus/addr.
func Open(bus, addr uint8) (*Device, error) {
	if err := initLib(); err != nil {
		return nil, err
	}
	var list **C.libusb_device
	n := C.libusb_get_device_list(ctx, &list)
	if n < 0 {
		return nil, Error(n)
	}
	defer C.libusb_free_device_list(list, 1)
	for _, dev := range unsafe.Slice(list, int(n)) {
		if uint8(C.libusb_get_bus_number(dev)) != bus || uint8(C.libusb_get_device_address(dev)) != addr {
			continue
		}
		info, dd, ok := describe(dev)
		if !ok {
			return nil, ErrIO
		}
		var h *C.libusb_device_handle
		if rc := C.libusb_open(dev, &h); rc != 0 {
			return nil, Error(rc)
		}
		return &Device{
			Info: info, h: h,
			iManuf: uint8(dd.iManufacturer), iProd: uint8(dd.iProduct), iSerial: uint8(dd.iSerialNumber),
		}, nil
	}
	return nil, ErrNoDevice
}

// String reads a string descriptor ("" if there is none).
func (d *Device) String(idx uint8) string {
	if idx == 0 {
		return ""
	}
	var buf [256]C.uchar
	n := C.libusb_get_string_descriptor_ascii(d.h, C.uint8_t(idx), &buf[0], C.int(len(buf)))
	if n <= 0 {
		return ""
	}
	return C.GoStringN((*C.char)(unsafe.Pointer(&buf[0])), n)
}

// Manufacturer returns the manufacturer string descriptor.
func (d *Device) Manufacturer() string { return d.String(d.iManuf) }

// Product returns the product string descriptor.
func (d *Device) Product() string { return d.String(d.iProd) }

// Serial returns the serial number string descriptor.
func (d *Device) Serial() string { return d.String(d.iSerial) }

// Claim claims interface iface; Close releases it.
func (d *Device) Claim(iface uint8) error {
	if rc := C.libusb_claim_interface(d.h, C.int(iface)); rc != 0 {
		return Error(rc)
	}
	d.claimed = append(d.claimed, iface)
	return nil
}

func ms(t time.Duration) C.uint {
	v := t.Milliseconds()
	if v < 1 {
		v = 1 // 0 would mean "wait forever"
	}
	return C.uint(v)
}

// Bulk transfers buf on a bulk endpoint (direction given by the address).
// It returns after at most timeout.
func (d *Device) Bulk(ep uint8, buf []byte, timeout time.Duration) (int, error) {
	var p *C.uchar
	if len(buf) > 0 {
		p = (*C.uchar)(unsafe.Pointer(&buf[0]))
	}
	var done C.int
	rc := C.libusb_bulk_transfer(d.h, C.uchar(ep), p, C.int(len(buf)), &done, ms(timeout))
	if rc != 0 {
		return int(done), Error(rc)
	}
	return int(done), nil
}

// Interrupt reads from an interrupt endpoint.
func (d *Device) Interrupt(ep uint8, buf []byte, timeout time.Duration) (int, error) {
	var done C.int
	rc := C.libusb_interrupt_transfer(d.h, C.uchar(ep), (*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf)), &done, ms(timeout))
	if rc != 0 {
		return int(done), Error(rc)
	}
	return int(done), nil
}

// ClearHalt clears a stall condition on endpoint ep.
func (d *Device) ClearHalt(ep uint8) error {
	if rc := C.libusb_clear_halt(d.h, C.uchar(ep)); rc != 0 {
		return Error(rc)
	}
	return nil
}

// Reset performs a USB port reset of the device.
func (d *Device) Reset() error {
	if rc := C.libusb_reset_device(d.h); rc != 0 {
		return Error(rc)
	}
	return nil
}

// Close releases the device. It must not be called while a transfer is in
// progress on another goroutine.
func (d *Device) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	for _, i := range d.claimed {
		C.libusb_release_interface(d.h, C.int(i))
	}
	C.libusb_close(d.h)
}
