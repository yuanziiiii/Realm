package agent

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"relaypanel/internal/domain"
)

var (
	packetLossPattern = regexp.MustCompile(`(?m)([0-9]+(?:\.[0-9]+)?)% packet loss`)
	rttPattern        = regexp.MustCompile(`(?m)(?:rtt|round-trip)[^=]*=\s*[^/]+/([^/]+)/`)
)

// ProbeLinks measures the same private addresses that ingress forwarding uses.
// Targets are measured concurrently so several backup exits do not extend the
// Agent's sync loop by several seconds each.
func ProbeLinks(ctx context.Context, ingress domain.Node, targets []domain.Node) []domain.LinkProbe {
	if ingress.Role == domain.NodeRoleEgress || len(targets) == 0 {
		return nil
	}
	seen := map[string]bool{}
	filtered := make([]domain.Node, 0, len(targets))
	for _, target := range targets {
		if target.ID == "" || target.ID == ingress.ID || seen[target.ID] {
			continue
		}
		if ip := net.ParseIP(target.PrivateAddress); ip == nil {
			continue
		}
		seen[target.ID] = true
		filtered = append(filtered, target)
	}
	probes := make([]domain.LinkProbe, len(filtered))
	var wg sync.WaitGroup
	for i, target := range filtered {
		wg.Add(1)
		go func(index int, node domain.Node) {
			defer wg.Done()
			latency, loss, ok := probeAddress(ctx, node.PrivateAddress)
			probes[index] = domain.LinkProbe{
				IngressNodeID: ingress.ID,
				EgressNodeID:  node.ID,
				Address:       node.PrivateAddress,
				LatencyMS:     latency,
				PacketLoss:    loss,
				Success:       ok && loss < 100,
				CheckedAt:     time.Now().UTC(),
			}
		}(i, target)
	}
	wg.Wait()
	return probes
}

// ProbeRuleTargets measures each unique landing IP once, then fans the result
// out to every deployed rule. It only runs on the egress side of a deployment;
// dual-managed ingress Agents continue to probe exit private IPs separately.
func ProbeRuleTargets(ctx context.Context, node domain.Node, deployments []domain.Deployment) []domain.TargetProbe {
	var rules []domain.ForwardRule
	seenAddresses := map[string]bool{}
	seenEndpoints := map[string]bool{}
	var addresses []string
	type endpoint struct {
		host string
		port int
		key  string
	}
	var endpoints []endpoint
	for _, deployment := range deployments {
		if deployment.Role != domain.NodeRoleEgress && deployment.Role != domain.NodeRoleBoth {
			continue
		}
		rule := deployment.Rule
		if !rule.Enabled || rule.ID == "" || rule.TargetHost == "" {
			continue
		}
		rules = append(rules, rule)
		if !seenAddresses[rule.TargetHost] {
			seenAddresses[rule.TargetHost] = true
			addresses = append(addresses, rule.TargetHost)
		}
		if rule.Protocol != "udp" {
			key := net.JoinHostPort(rule.TargetHost, strconv.Itoa(rule.TargetPort))
			if !seenEndpoints[key] {
				seenEndpoints[key] = true
				endpoints = append(endpoints, endpoint{host: rule.TargetHost, port: rule.TargetPort, key: key})
			}
		}
	}
	if len(rules) == 0 {
		return nil
	}
	type result struct {
		latency float64
		loss    float64
		ok      bool
	}
	type tcpResult struct {
		latency float64
		ok      bool
		errCode string
	}
	results := make([]result, len(addresses))
	tcpResults := make([]tcpResult, len(endpoints))
	// ICMP and TCP checks share one concurrency limit so a personal node with
	// many rules never creates a process/socket spike.
	limit := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, address := range addresses {
		wg.Add(1)
		go func(index int, host string) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			results[index].latency, results[index].loss, results[index].ok = probeAddress(ctx, host)
		}(i, address)
	}
	for i, target := range endpoints {
		wg.Add(1)
		go func(index int, target endpoint) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			tcpResults[index].latency, tcpResults[index].ok, tcpResults[index].errCode = probeTCP(ctx, target.host, target.port)
		}(i, target)
	}
	wg.Wait()
	byAddress := make(map[string]result, len(addresses))
	for i, address := range addresses {
		byAddress[address] = results[i]
	}
	byEndpoint := make(map[string]tcpResult, len(endpoints))
	for i, target := range endpoints {
		byEndpoint[target.key] = tcpResults[i]
	}
	checkedAt := time.Now().UTC()
	probes := make([]domain.TargetProbe, 0, len(rules))
	for _, rule := range rules {
		measured := byAddress[rule.TargetHost]
		probe := domain.TargetProbe{
			RuleID: rule.ID, NodeID: node.ID, Address: rule.TargetHost, Port: rule.TargetPort,
			LatencyMS: measured.latency, PacketLoss: measured.loss,
			Success: measured.ok && measured.loss < 100, CheckedAt: checkedAt,
		}
		if rule.Protocol != "udp" {
			measuredTCP := byEndpoint[net.JoinHostPort(rule.TargetHost, strconv.Itoa(rule.TargetPort))]
			probe.TCPChecked = true
			probe.TCPSuccess = measuredTCP.ok
			probe.TCPLatencyMS = measuredTCP.latency
			probe.TCPError = measuredTCP.errCode
		}
		probes = append(probes, probe)
	}
	return probes
}

// probeTCP performs only a TCP handshake to the real business endpoint. It
// closes the socket immediately and never sends application data.
func probeTCP(ctx context.Context, host string, port int) (latencyMS float64, ok bool, errCode string) {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	started := time.Now()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	latencyMS = float64(time.Since(started).Microseconds()) / 1000
	if err == nil {
		_ = conn.Close()
		return latencyMS, true, ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, false, "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return 0, false, "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return 0, false, "refused"
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return 0, false, "unreachable"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return 0, false, "dns"
	}
	return 0, false, "error"
}

func probeAddress(ctx context.Context, address string) (latency, loss float64, ok bool) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, _ := exec.CommandContext(probeCtx, "ping", "-n", "-c", "3", "-W", "1", address).CombinedOutput()
	return parsePingOutput(string(output))
}

func parsePingOutput(output string) (latencyMS, packetLoss float64, ok bool) {
	lossMatch := packetLossPattern.FindStringSubmatch(output)
	if len(lossMatch) != 2 {
		return 0, 100, false
	}
	packetLoss, err := strconv.ParseFloat(lossMatch[1], 64)
	if err != nil {
		return 0, 100, false
	}
	rttMatch := rttPattern.FindStringSubmatch(output)
	if len(rttMatch) == 2 {
		latencyMS, _ = strconv.ParseFloat(rttMatch[1], 64)
	}
	return latencyMS, packetLoss, true
}
