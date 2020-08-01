package lxc

import (
	"context"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
	lxc "gopkg.in/lxc/go-lxc.v2"
)

func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	// start reconciler when we start fingerprinting
	// this is the only method called when driver is launched properly
	d.reconciler.Start()

	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)
	return ch, nil
}

// setFingerprintSuccess marks the driver as having fingerprinted successfully
func (d *Driver) setFingerprintSuccess() {
	d.fingerprint.Set()
}

// setFingerprintFailure marks the driver as having failed fingerprinting
func (d *Driver) setFingerprintFailure() {
	d.fingerprint.UnSet()
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	var health drivers.HealthState
	var desc string
	attrs := map[string]*pstructs.Attribute{}

	lxcVersion := lxc.Version()

	if d.config.Enabled && lxcVersion != "" {
		health = drivers.HealthStateHealthy
		desc = "ready"
		attrs["driver.lxc"] = pstructs.NewBoolAttribute(true)
		attrs["driver.lxc.version"] = pstructs.NewStringAttribute(lxcVersion)
		d.setFingerprintSuccess()
	} else {
		health = drivers.HealthStateUndetected
		desc = "disabled"
		d.setFingerprintFailure()
	}

	if d.config.AllowVolumes {
		attrs["driver.lxc.volumes.enabled"] = pstructs.NewBoolAttribute(true)
	}

	return &drivers.Fingerprint{
		Attributes:        attrs,
		Health:            health,
		HealthDescription: desc,
	}
}
