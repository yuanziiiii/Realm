package agent

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"sort"
	"strings"

	"relaypanel/internal/domain"
)

type interfaceAddress struct {
	name string
	ip   net.IP
}

// DetectNetwork returns safe suggestions for the panel. The controller only
// uses them to fill blank fields, so an administrator's manual values win.
func DetectNetwork(ctx context.Context) domain.NetworkInfo {
	defaultInterface := defaultRouteInterface(ctx)
	addresses := localIPv4Addresses()
	info := chooseNetworkInfo(defaultInterface, addresses)
	info.PublicInterface = defaultInterface
	return info
}

func defaultRouteInterface(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "ip", "-j", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	var routes []struct {
		Dev string `json:"dev"`
	}
	if json.Unmarshal(out, &routes) != nil {
		return ""
	}
	for _, route := range routes {
		if route.Dev != "" {
			return route.Dev
		}
	}
	return ""
}

func localIPv4Addresses() []interfaceAddress {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var result []interfaceAddress
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || ignoredInterface(iface.Name) {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && ip.IsGlobalUnicast() {
				result = append(result, interfaceAddress{name: iface.Name, ip: ip.To4()})
			}
		}
	}
	return result
}

func chooseNetworkInfo(defaultInterface string, addresses []interfaceAddress) domain.NetworkInfo {
	var info domain.NetworkInfo
	for _, address := range addresses {
		if address.name == defaultInterface && !isInternalIPv4(address.ip) && info.PublicAddress == "" {
			info.PublicAddress = address.ip.String()
		}
	}
	candidates := append([]interfaceAddress(nil), addresses...)
	sort.SliceStable(candidates, func(i, j int) bool {
		return interfacePriority(candidates[i].name, defaultInterface) < interfacePriority(candidates[j].name, defaultInterface)
	})
	for _, address := range candidates {
		if isInternalIPv4(address.ip) {
			info.PrivateAddress = address.ip.String()
			info.PrivateInterface = address.name
			break
		}
	}
	return info
}

func interfacePriority(name, defaultInterface string) int {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "wg") || strings.HasPrefix(lower, "tun") || strings.HasPrefix(lower, "tailscale") {
		return 0
	}
	if name != defaultInterface {
		return 1
	}
	return 2
}

func ignoredInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func isInternalIPv4(ip net.IP) bool {
	if ip.IsPrivate() {
		return true
	}
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 // 100.64.0.0/10
}
