package main

// Every driver package registers itself in init(). Which ones are actually
// used is decided by the node config (drivers: section).
import (
	_ "github.com/finn-freitag/tb-agent/internal/driver/android"
	_ "github.com/finn-freitag/tb-agent/internal/driver/browser"
	_ "github.com/finn-freitag/tb-agent/internal/driver/dockerqemu"
	_ "github.com/finn-freitag/tb-agent/internal/driver/libvirt"
	_ "github.com/finn-freitag/tb-agent/internal/driver/lxc"
	_ "github.com/finn-freitag/tb-agent/internal/driver/orchard"
	_ "github.com/finn-freitag/tb-agent/internal/driver/usbphone"
)
