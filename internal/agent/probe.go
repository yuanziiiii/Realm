package agent

import (
	"context"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
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
	var addresses []string
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
	}
	if len(rules) == 0 {
		return nil
	}
	type result struct {
		latency float64
		loss    float64
		ok      bool
	}
	results := make([]result, len(addresses))
	// A personal panel may still have many rules. Bound concurrent ping
	// processes so target monitoring never causes a CPU/process spike.
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
	wg.Wait()
	byAddress := make(map[string]result, len(addresses))
	for i, address := range addresses {
		byAddress[address] = results[i]
	}
	checkedAt := time.Now().UTC()
	probes := make([]domain.TargetProbe, 0, len(rules))
	for _, rule := range rules {
		measured := byAddress[rule.TargetHost]
		probes = append(probes, domain.TargetProbe{
			RuleID: rule.ID, NodeID: node.ID, Address: rule.TargetHost, Port: rule.TargetPort,
			LatencyMS: measured.latency, PacketLoss: measured.loss,
			Success: measured.ok && measured.loss < 100, CheckedAt: checkedAt,
		})
	}
	return probes
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
