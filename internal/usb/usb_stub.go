//go:build !(darwin && cgo && !nomtp)

// Package usb: stub used when the program is built without libusb (no cgo,
// or the nomtp build tag). MTP mode is then unavailable.
package usb

import (
	"errors"
	"time"
)

// Available reports whether USB access was compiled in.
const Available = false

var errUnavailable = errors.New("usb: built without libusb support")

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

// Error implements error.
func (e Error) Error() string { return "usb error" }

// Interface describes the first alternate setting of one interface.
type Interface struct {
	Num, Class, SubClass, Protocol uint8
	NameIdx                        uint8
	In, Out, Intr                  uint8
	InMax, OutMax                  int
	Endpoints                      int
}

// Info identifies an attached USB device.
type Info struct {
	Bus, Addr uint8
	VID, PID  uint16
	Ifaces    []Interface
}

// Device is an open USB device; this build cannot open any.
type Device struct{ Info }

// List returns no devices in this build.
func List() ([]Info, error) { return nil, nil }

// Open always fails in this build.
func Open(bus, addr uint8) (*Device, error) { return nil, errUnavailable }

// String returns a string descriptor; empty in this build.
func (d *Device) String(uint8) string { return "" }

// Manufacturer returns the manufacturer string descriptor.
func (d *Device) Manufacturer() string { return "" }

// Product returns the product string descriptor.
func (d *Device) Product() string { return "" }

// Serial returns the serial number string descriptor.
func (d *Device) Serial() string { return "" }

// Claim claims an interface; it always fails in this build.
func (d *Device) Claim(uint8) error { return errUnavailable }

// Bulk transfers on a bulk endpoint; it always fails in this build.
func (d *Device) Bulk(uint8, []byte, time.Duration) (int, error) {
	return 0, errUnavailable
}

// Interrupt reads an interrupt endpoint; it always fails in this build.
func (d *Device) Interrupt(uint8, []byte, time.Duration) (int, error) {
	return 0, errUnavailable
}

// ClearHalt clears an endpoint stall; it always fails in this build.
func (d *Device) ClearHalt(uint8) error { return errUnavailable }

// Reset resets the device; it always fails in this build.
func (d *Device) Reset() error { return errUnavailable }

// Close releases the device.
func (d *Device) Close() {}
