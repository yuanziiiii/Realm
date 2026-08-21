package agent

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net"
	"sort"
	"strconv"
	"strings"

	"relaypanel/internal/domain"
)

type Command struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
}

type RateLimitSpec struct {
	RuleID          string
	Direction       string
	Interface       string
	SourceInterface string
	ListenPort      int
	ClassID         string
	Mark            uint32
	ConfiguredMbps  int
}

type Plan struct {
	NFTScript      string          `json:"nft_script"`
	TC             []Command       `json:"tc"`
	RealmConfig    []byte          `json:"realm_config,omitempty"`
	IngressRuleIDs []string        `json:"ingress_rule_ids"`
	ForwardMarks   []uint32        `json:"forward_marks,omitempty"`
	RateLimits     []RateLimitSpec `json:"-"`
}

func RenderPlan(node domain.Node, deployments []domain.Deployment, allowQdiscReplace bool) (Plan, error) {
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].Rule.ID < deployments[j].Rule.ID })
	var p Plan
	markOwners := map[uint32]string{}
	for _, deployment := range deployments {
		if deployment.Rule.Engine != "nftables" {
			continue
		}
		mark := uploadMark(deployment.Rule.ID)
		if owner := markOwners[mark]; owner != "" && owner != deployment.Rule.ID {
			return p, fmt.Errorf("traffic mark collision between rules %s and %s", owner, deployment.Rule.ID)
		}
		markOwners[mark] = deployment.Rule.ID
	}
	var b strings.Builder
	b.WriteString("flush table inet relay_panel\n")
	b.WriteString("table inet relay_panel {\n")
	for _, d := range deployments {
		if ownsTrafficCounters(d) {
			fmt.Fprintf(&b, "  counter %s { comment \"%s upload\"; }\n", counterName(d.Rule.ID, "up"), escapeComment(d.Rule.Name))
			fmt.Fprintf(&b, "  counter %s { comment \"%s download\"; }\n", counterName(d.Rule.ID, "down"), escapeComment(d.Rule.Name))
			p.IngressRuleIDs = append(p.IngressRuleIDs, d.Rule.ID)
		}
		if d.Rule.Engine == "nftables" {
			p.ForwardMarks = append(p.ForwardMarks, uploadMark(d.Rule.ID))
		}
	}
	b.WriteString("  chain prerouting { type nat hook prerouting priority dstnat; policy accept;\n")
	for _, d := range deployments {
		if d.Rule.Engine == "nftables" {
			if err := renderPrerouting(&b, node, d); err != nil {
				return p, err
			}
		}
	}
	b.WriteString("  }\n  chain postrouting { type nat hook postrouting priority srcnat; policy accept;\n")
	for _, d := range deployments {
		if d.Rule.Engine == "nftables" {
			if err := renderPostrouting(&b, node, d); err != nil {
				return p, err
			}
		}
	}
	b.WriteString("  }\n  chain forward_stats { type filter hook forward priority -10; policy accept;\n")
	for _, d := range deployments {
		if d.Rule.Engine == "nftables" && ownsTrafficCounters(d) {
			fmt.Fprintf(&b, "    ct mark %d ct direction original counter name %s meta mark set %d\n", uploadMark(d.Rule.ID), counterName(d.Rule.ID, "up"), uploadMark(d.Rule.ID))
			fmt.Fprintf(&b, "    ct mark %d ct direction reply counter name %s meta mark set %d\n", uploadMark(d.Rule.ID), counterName(d.Rule.ID, "down"), downloadMark(d.Rule.ID))
		}
	}
	b.WriteString("  }\n  chain input_stats { type filter hook input priority filter; policy accept;\n")
	for _, d := range deployments {
		if d.Rule.Engine == "realm" && (d.Role == domain.NodeRoleIngress || d.Role == domain.NodeRoleBoth) {
			for _, proto := range protocols(d.Rule.Protocol) {
				fmt.Fprintf(&b, "    iifname \"%s\" %s dport %d counter name %s\n", safeInterface(node.PublicInterface), proto, d.Rule.ListenPort, counterName(d.Rule.ID, "up"))
			}
		}
		if d.Rule.Engine == "realm" && d.Rule.Mode == domain.ForwardModeExitOnly && d.Role == domain.NodeRoleEgress {
			for _, proto := range protocols(d.Rule.Protocol) {
				fmt.Fprintf(&b, "    iifname \"%s\" ip daddr %s %s dport %d counter name %s\n", safeInterface(node.PrivateInterface), d.Rule.ListenAddress, proto, realmListenPort(d.Rule), counterName(d.Rule.ID, "up"))
			}
		}
	}
	b.WriteString("  }\n  chain output_stats { type filter hook output priority filter; policy accept;\n")
	for _, d := range deployments {
		if d.Rule.Engine == "realm" && (d.Role == domain.NodeRoleIngress || d.Role == domain.NodeRoleBoth) {
			for _, proto := range protocols(d.Rule.Protocol) {
				fmt.Fprintf(&b, "    oifname \"%s\" %s sport %d counter name %s meta mark set %d\n", safeInterface(node.PublicInterface), proto, d.Rule.ListenPort, counterName(d.Rule.ID, "down"), downloadMark(d.Rule.ID))
			}
		}
		if d.Rule.Engine == "realm" && d.Rule.Mode == domain.ForwardModeExitOnly && d.Role == domain.NodeRoleEgress {
			for _, proto := range protocols(d.Rule.Protocol) {
				if ip := net.ParseIP(d.Rule.TargetHost); ip != nil && ip.To4() != nil {
					fmt.Fprintf(&b, "    oifname \"%s\" ip daddr %s %s dport %d meta mark set %d\n", safeInterface(node.PublicInterface), ip.String(), proto, d.Rule.TargetPort, uploadMark(d.Rule.ID))
				}
				fmt.Fprintf(&b, "    oifname \"%s\" ip saddr %s %s sport %d counter name %s meta mark set %d\n", safeInterface(node.PrivateInterface), d.Rule.ListenAddress, proto, realmListenPort(d.Rule), counterName(d.Rule.ID, "down"), downloadMark(d.Rule.ID))
			}
		}
	}
	b.WriteString("  }\n  chain forward { type filter hook forward priority filter; policy accept; }\n}\n")
	p.NFTScript = b.String()
	p.TC, p.RateLimits = renderTC(node, deployments, allowQdiscReplace)
	classOwners := map[string]string{}
	for _, limit := range p.RateLimits {
		key := limit.Interface + "/" + limit.ClassID
		if owner := classOwners[key]; owner != "" && owner != limit.RuleID {
			return p, fmt.Errorf("tc class collision between rules %s and %s on %s", owner, limit.RuleID, limit.Interface)
		}
		classOwners[key] = limit.RuleID
	}
	p.RealmConfig = renderRealm(node, deployments)
	return p, nil
}

func renderPrerouting(b *strings.Builder, node domain.Node, d domain.Deployment) error {
	r := d.Rule
	if d.Role == domain.NodeRoleIngress || d.Role == domain.NodeRoleBoth {
		for _, proto := range protocols(r.Protocol) {
			fmt.Fprintf(b, "    iifname \"%s\" %s dport %d meta mark set %d ct mark set %d dnat ip to %s:%d\n", safeInterface(node.PublicInterface), proto, r.ListenPort, uploadMark(r.ID), uploadMark(r.ID), mustIPv4Placeholder(r.EgressNodeID), r.RelayPort)
		}
	}
	if d.Role == domain.NodeRoleEgress || d.Role == domain.NodeRoleBoth {
		if net.ParseIP(r.TargetHost) == nil {
			return fmt.Errorf("nftables rule %s target_host must be an IP address", r.Name)
		}
		for _, proto := range protocols(r.Protocol) {
			if r.Mode == domain.ForwardModeExitOnly {
				fmt.Fprintf(b, "    iifname \"%s\" ip daddr %s %s dport %d meta mark set %d ct mark set %d dnat ip to %s:%d\n", safeInterface(node.PrivateInterface), r.ListenAddress, proto, r.ListenPort, uploadMark(r.ID), uploadMark(r.ID), r.TargetHost, r.TargetPort)
			} else {
				fmt.Fprintf(b, "    iifname \"%s\" %s dport %d meta mark set %d ct mark set %d dnat ip to %s:%d\n", safeInterface(node.PrivateInterface), proto, r.RelayPort, uploadMark(r.ID), uploadMark(r.ID), r.TargetHost, r.TargetPort)
			}
		}
	}
	return nil
}

// The ingress DNAT address is replaced in FinalizePlan after the controller's
// node inventory has supplied the egress node private address.
func mustIPv4Placeholder(nodeID string) string {
	return "__NODE_" + strings.ToUpper(strings.ReplaceAll(nodeID, "-", "_")) + "__"
}

func FinalizePlan(p Plan, deployments []domain.Deployment, nodes map[string]domain.Node) (Plan, error) {
	for _, d := range deployments {
		if d.Role != domain.NodeRoleIngress && d.Role != domain.NodeRoleBoth {
			continue
		}
		peer, ok := nodes[d.Rule.EgressNodeID]
		if !ok {
			return p, fmt.Errorf("egress node %s missing from sync response", d.Rule.EgressNodeID)
		}
		ip := net.ParseIP(peer.PrivateAddress)
		if ip == nil || ip.To4() == nil {
			return p, fmt.Errorf("egress node %s needs an IPv4 private address", peer.Name)
		}
		placeholder := mustIPv4Placeholder(peer.ID)
		p.NFTScript = strings.ReplaceAll(p.NFTScript, placeholder, ip.String())
		p.RealmConfig = bytesReplaceAll(p.RealmConfig, placeholder, ip.String())
	}
	return p, nil
}

func bytesReplaceAll(in []byte, old, new string) []byte {
	if len(in) == 0 {
		return in
	}
	return []byte(strings.ReplaceAll(string(in), old, new))
}

func renderPostrouting(b *strings.Builder, node domain.Node, d domain.Deployment) error {
	r := d.Rule
	if d.Role == domain.NodeRoleIngress || d.Role == domain.NodeRoleBoth {
		for _, proto := range protocols(r.Protocol) {
			fmt.Fprintf(b, "    oifname \"%s\" %s dport %d masquerade\n", safeInterface(node.PrivateInterface), proto, r.RelayPort)
		}
	}
	if d.Role == domain.NodeRoleEgress || d.Role == domain.NodeRoleBoth {
		for _, proto := range protocols(r.Protocol) {
			// PublicAddress is often a provider-side NAT address and is not
			// assigned to the host. Masquerade selects the interface's real source
			// address and works for both directly routed and cloud NAT servers.
			fmt.Fprintf(b, "    oifname \"%s\" ip daddr %s %s dport %d masquerade\n", safeInterface(node.PublicInterface), r.TargetHost, proto, r.TargetPort)
		}
	}
	return nil
}

func renderTC(node domain.Node, deployments []domain.Deployment, allowReplace bool) ([]Command, []RateLimitSpec) {
	type rateEntry struct {
		rule         domain.ForwardRule
		rate         int
		mark         uint32
		direction    string
		directListen bool
		source       string
		matchPort    int
	}
	byInterface := map[string][]rateEntry{}
	ifbEntries := map[string][]rateEntry{}
	for _, d := range deployments {
		if d.Role != domain.NodeRoleIngress && d.Role != domain.NodeRoleBoth && d.Rule.Mode != domain.ForwardModeExitOnly {
			continue
		}
		r := d.Rule
		matchPort := r.ListenPort
		if r.Engine == "realm" {
			matchPort = realmListenPort(r)
		}
		if r.Mode == domain.ForwardModeExitOnly {
			if r.DownloadMbps > 0 {
				byInterface[node.PrivateInterface] = append(byInterface[node.PrivateInterface], rateEntry{rule: r, rate: r.DownloadMbps, mark: downloadMark(r.ID), direction: "download", directListen: r.Engine == "realm", matchPort: matchPort})
			}
			if r.UploadMbps > 0 {
				if r.Engine == "realm" {
					ifbEntries[node.PrivateInterface] = append(ifbEntries[node.PrivateInterface], rateEntry{rule: r, rate: r.UploadMbps, mark: uploadMark(r.ID), direction: "upload", source: node.PrivateInterface, matchPort: matchPort})
				} else {
					byInterface[node.PublicInterface] = append(byInterface[node.PublicInterface], rateEntry{rule: r, rate: r.UploadMbps, mark: uploadMark(r.ID), direction: "upload", matchPort: matchPort})
				}
			}
			continue
		}
		if r.DownloadMbps > 0 {
			byInterface[node.PublicInterface] = append(byInterface[node.PublicInterface], rateEntry{rule: r, rate: r.DownloadMbps, mark: downloadMark(r.ID), direction: "download", directListen: r.Engine == "realm", matchPort: matchPort})
		}
		if r.UploadMbps > 0 {
			if r.Engine == "realm" {
				ifbEntries[node.PublicInterface] = append(ifbEntries[node.PublicInterface], rateEntry{rule: r, rate: r.UploadMbps, mark: uploadMark(r.ID), direction: "upload", source: node.PublicInterface, matchPort: matchPort})
			} else {
				byInterface[node.PrivateInterface] = append(byInterface[node.PrivateInterface], rateEntry{rule: r, rate: r.UploadMbps, mark: uploadMark(r.ID), direction: "upload", matchPort: matchPort})
			}
		}
	}
	var cmds []Command
	if len(ifbEntries) > 0 {
		cmds = append(cmds, Command{"modprobe", []string{"ifb"}}, Command{"ip", []string{"link", "add", "ifb-relay0", "type", "ifb"}}, Command{"ip", []string{"link", "set", "ifb-relay0", "up"}})
		ifbSources := make([]string, 0, len(ifbEntries))
		for source := range ifbEntries {
			if source != "" {
				ifbSources = append(ifbSources, source)
			}
		}
		sort.Strings(ifbSources)
		pref := 100
		for _, source := range ifbSources {
			// An ingress qdisc is exclusive and cannot be replaced in place on
			// several kernels. Add it once, then reuse it on later reconciles.
			cmds = append(cmds, Command{"tc", []string{"qdisc", "add", "dev", source, "handle", "ffff:", "ingress"}})
			for _, e := range ifbEntries[source] {
				for _, proto := range protocols(e.rule.Protocol) {
					args := []string{"filter", "replace", "dev", source, "parent", "ffff:", "protocol", "ip", "pref", strconv.Itoa(pref), "flower", "ip_proto", proto, "dst_port", strconv.Itoa(e.matchPort), "action", "skbedit", "mark", strconv.FormatUint(uint64(e.mark), 10), "action", "mirred", "egress", "redirect", "dev", "ifb-relay0"}
					cmds = append(cmds, Command{"tc", args})
					pref++
				}
			}
		}
		for _, entries := range ifbEntries {
			for _, e := range entries {
				byInterface["ifb-relay0"] = append(byInterface["ifb-relay0"], e)
			}
		}
	}
	interfaces := make([]string, 0, len(byInterface))
	for iface := range byInterface {
		if iface != "" {
			interfaces = append(interfaces, iface)
		}
	}
	sort.Strings(interfaces)
	var specs []RateLimitSpec
	for _, iface := range interfaces {
		entries := byInterface[iface]
		verb := "add"
		if allowReplace {
			verb = "replace"
		}
		cmds = append(cmds, Command{"tc", []string{"qdisc", verb, "dev", iface, "root", "handle", "7a1:", "htb", "default", "999"}}, Command{"tc", []string{"class", "replace", "dev", iface, "parent", "7a1:", "classid", "7a1:1", "htb", "rate", "10000mbit", "ceil", "10000mbit", "quantum", "60000"}}, Command{"tc", []string{"class", "replace", "dev", iface, "parent", "7a1:1", "classid", "7a1:999", "htb", "rate", "10000mbit", "ceil", "10000mbit", "quantum", "60000"}})
		for _, e := range entries {
			minor := classMinor(e.rule.ID, e.mark == downloadMark(e.rule.ID))
			classID := fmt.Sprintf("7a1:%d", minor)
			burst := e.rule.BurstKBytes
			if burst <= 0 {
				burst = 512
			}
			cmds = append(cmds, Command{"tc", []string{"class", "replace", "dev", iface, "parent", "7a1:1", "classid", classID, "htb", "rate", fmt.Sprintf("%dmbit", e.rate), "ceil", fmt.Sprintf("%dmbit", e.rate), "burst", fmt.Sprintf("%dk", burst), "quantum", "60000"}}, Command{"tc", []string{"qdisc", "replace", "dev", iface, "parent", classID, "handle", fmt.Sprintf("%x:", minor), "fq_codel"}})
			if e.directListen {
				for protocolIndex, proto := range protocols(e.rule.Protocol) {
					cmds = append(cmds, Command{"tc", []string{"filter", "replace", "dev", iface, "parent", "7a1:", "protocol", "ip", "pref", strconv.Itoa(minor + protocolIndex*10000), "flower", "ip_proto", proto, "src_port", strconv.Itoa(e.matchPort), "flowid", classID}})
				}
			} else {
				cmds = append(cmds, Command{"tc", []string{"filter", "replace", "dev", iface, "parent", "7a1:", "protocol", "ip", "pref", strconv.Itoa(minor), "handle", strconv.FormatUint(uint64(e.mark), 10), "fw", "flowid", classID}})
			}
			specs = append(specs, RateLimitSpec{RuleID: e.rule.ID, Direction: e.direction, Interface: iface, SourceInterface: e.source, ListenPort: e.matchPort, ClassID: classID, Mark: e.mark, ConfiguredMbps: e.rate})
		}
	}
	return cmds, specs
}

func renderRealm(node domain.Node, deployments []domain.Deployment) []byte {
	type endpoint struct {
		Listen    string          `json:"listen"`
		Remote    string          `json:"remote"`
		Through   string          `json:"through,omitempty"`
		Interface string          `json:"interface,omitempty"`
		Network   map[string]bool `json:"network"`
	}
	var endpoints []endpoint
	for _, d := range deployments {
		if d.Rule.Engine != "realm" {
			continue
		}
		r := d.Rule
		network := map[string]bool{"no_tcp": r.Protocol == "udp", "use_udp": r.Protocol != "tcp"}
		if d.Role == domain.NodeRoleIngress || d.Role == domain.NodeRoleBoth {
			endpoints = append(endpoints, endpoint{Listen: net.JoinHostPort(r.ListenAddress, strconv.Itoa(r.ListenPort)), Remote: net.JoinHostPort(mustIPv4Placeholder(r.EgressNodeID), strconv.Itoa(r.RelayPort)), Network: network})
		} else {
			listenAddress := node.PrivateAddress
			if r.Mode == domain.ForwardModeExitOnly {
				listenAddress = r.ListenAddress
			}
			// PublicAddress may only exist on the cloud provider's NAT gateway.
			// Pin the outbound interface and let the kernel choose a local source.
			endpoints = append(endpoints, endpoint{Listen: net.JoinHostPort(listenAddress, strconv.Itoa(r.RelayPort)), Remote: net.JoinHostPort(r.TargetHost, strconv.Itoa(r.TargetPort)), Interface: node.PublicInterface, Network: network})
		}
	}
	if len(endpoints) == 0 {
		return nil
	}
	payload := map[string]any{"log": map[string]string{"level": "warn", "output": "stdout"}, "endpoints": endpoints}
	b, _ := json.MarshalIndent(payload, "", "  ")
	return b
}

func ownsTrafficCounters(d domain.Deployment) bool {
	return d.Role == domain.NodeRoleIngress || d.Role == domain.NodeRoleBoth || (d.Rule.Mode == domain.ForwardModeExitOnly && d.Role == domain.NodeRoleEgress)
}

func realmListenPort(rule domain.ForwardRule) int {
	if rule.Mode == domain.ForwardModeExitOnly && rule.RelayPort > 0 {
		return rule.RelayPort
	}
	return rule.ListenPort
}

func protocols(p string) []string {
	if p == "both" {
		return []string{"tcp", "udp"}
	}
	return []string{p}
}
func safeInterface(v string) string { return strings.ReplaceAll(v, "\"", "") }
func escapeComment(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, "\\", ""), "\"", "")
}
func counterName(id, direction string) string {
	return "rp_" + strings.TrimPrefix(id, "rule_") + "_" + direction
}
func baseMark(id string) uint32     { return crc32.ChecksumIEEE([]byte(id))%60000 + 1000 }
func uploadMark(id string) uint32   { return 0x10000 | baseMark(id) }
func downloadMark(id string) uint32 { return 0x20000 | baseMark(id) }
func classMinor(id string, download bool) int {
	offset := 0
	if download {
		offset = 4000
	}
	return int(crc32.ChecksumIEEE([]byte(id))%3900) + 10 + offset
}
