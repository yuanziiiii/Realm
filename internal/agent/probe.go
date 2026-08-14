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
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			output, _ := exec.CommandContext(probeCtx, "ping", "-n", "-c", "3", "-W", "1", node.PrivateAddress).CombinedOutput()
			latency, loss, ok := parsePingOutput(string(output))
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
