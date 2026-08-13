package agent

import (
	"net"
	"testing"
)

func TestChooseNetworkInfoPrefersTunnelForPrivatePath(t *testing.T) {
	info := chooseNetworkInfo("eth0", []interfaceAddress{
		{name: "eth0", ip: net.ParseIP("192.168.1.20")},
		{name: "wg0", ip: net.ParseIP("10.24.0.3")},
	})
	if info.PrivateAddress != "10.24.0.3" || info.PrivateInterface != "wg0" {
		t.Fatalf("unexpected private path: %+v", info)
	}
	if info.PublicInterface != "" {
		t.Fatalf("chooseNetworkInfo must not invent the default interface: %+v", info)
	}
}

func TestChooseNetworkInfoUsesDefaultNATAddressAndLocalPublicIP(t *testing.T) {
	nat := chooseNetworkInfo("eth0", []interfaceAddress{{name: "eth0", ip: net.ParseIP("100.64.12.8")}})
	if nat.PrivateAddress != "100.64.12.8" || nat.PrivateInterface != "eth0" {
		t.Fatalf("unexpected CGNAT detection: %+v", nat)
	}
	public := chooseNetworkInfo("ens3", []interfaceAddress{{name: "ens3", ip: net.ParseIP("203.0.113.18")}})
	if public.PublicAddress != "203.0.113.18" || public.PrivateAddress != "" {
		t.Fatalf("unexpected public detection: %+v", public)
	}
}

func TestIgnoredVirtualInterfaces(t *testing.T) {
	for _, name := range []string{"docker0", "br-123", "veth0", "virbr0", "cni0", "flannel.1"} {
		if !ignoredInterface(name) {
			t.Fatalf("expected %s to be ignored", name)
		}
	}
}
