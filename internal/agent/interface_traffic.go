package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"relaypanel/internal/domain"
)

// ReadInterfaceTraffic reads the kernel's cumulative byte counters. These are
// deliberately separate from per-rule nftables counters: they represent the
// provider-facing whole-server interface and are suitable for traffic quotas.
func ReadInterfaceTraffic(iface string) (domain.NodeTrafficSample, error) {
	iface = strings.TrimSpace(iface)
	if iface == "" || strings.ContainsAny(iface, `/\\`) {
		return domain.NodeTrafficSample{}, fmt.Errorf("invalid traffic interface %q", iface)
	}
	read := func(name string) (int64, error) {
		value, err := os.ReadFile(filepath.Join("/sys/class/net", iface, "statistics", name))
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(strings.TrimSpace(string(value)), 10, 64)
	}
	rx, err := read("rx_bytes")
	if err != nil {
		return domain.NodeTrafficSample{}, fmt.Errorf("read %s rx_bytes: %w", iface, err)
	}
	tx, err := read("tx_bytes")
	if err != nil {
		return domain.NodeTrafficSample{}, fmt.Errorf("read %s tx_bytes: %w", iface, err)
	}
	return domain.NodeTrafficSample{Interface: iface, CapturedAt: time.Now().UTC(), Cumulative: true, RXBytes: rx, TXBytes: tx}, nil
}
