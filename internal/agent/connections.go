package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"relaypanel/internal/domain"
)

type conntrackTuple struct {
	protocol string
	source   string
	target   string
	dport    int
}

func clientSideDeployment(deployment domain.Deployment) bool {
	return deployment.Role == domain.NodeRoleIngress || deployment.Role == domain.NodeRoleBoth || (deployment.Rule.Mode == domain.ForwardModeExitOnly && deployment.Role == domain.NodeRoleEgress)
}

func readConntrack(ctx context.Context) (io.ReadCloser, error) {
	for _, path := range []string{"/proc/net/nf_conntrack", "/proc/net/ip_conntrack"} {
		file, err := os.Open(path)
		if err == nil {
			return file, nil
		}
	}
	commandCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, "conntrack", "-L", "-f", "ipv4", "-o", "extended").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("内核未开放连接跟踪信息")
	}
	return io.NopCloser(bytes.NewReader(output)), nil
}

func parseConntrack(reader io.Reader) []conntrackTuple {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var tuples []conntrackTuple
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		var tuple conntrackTuple
		for _, field := range fields {
			if tuple.protocol == "" && (field == "tcp" || field == "udp") {
				tuple.protocol = field
				continue
			}
			if tuple.protocol == "" {
				continue
			}
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch key {
			case "src":
				if tuple.source == "" {
					tuple.source = value
				}
			case "dst":
				if tuple.target == "" {
					tuple.target = value
				}
			case "dport":
				if tuple.dport == 0 {
					tuple.dport, _ = strconv.Atoi(value)
				}
			}
			if tuple.source != "" && tuple.target != "" && tuple.dport > 0 {
				break
			}
		}
		if tuple.protocol != "" && tuple.source != "" && tuple.dport > 0 {
			tuples = append(tuples, tuple)
		}
	}
	return tuples
}

// CollectConnections reports only the client-facing side. In a managed
// two-hop line this prevents the ingress-to-egress and egress-to-landing
// tuples from being counted as additional client connections.
func CollectConnections(ctx context.Context, node domain.Node, deployments []domain.Deployment) domain.ConnectionReport {
	report := domain.ConnectionReport{CapturedAt: time.Now().UTC()}
	type ruleMatch struct {
		id       string
		port     int
		protocol string
	}
	var matches []ruleMatch
	for _, deployment := range deployments {
		if !clientSideDeployment(deployment) || !deployment.Rule.Enabled {
			continue
		}
		port := deployment.Rule.ListenPort
		if deployment.Rule.Engine == "realm" {
			port = realmListenPort(deployment.Rule)
		}
		matches = append(matches, ruleMatch{id: deployment.Rule.ID, port: port, protocol: deployment.Rule.Protocol})
		report.RuleIDs = append(report.RuleIDs, deployment.Rule.ID)
	}
	reader, err := readConntrack(ctx)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	defer reader.Close()
	type key struct{ ruleID, source string }
	entries := map[key]*domain.ConnectionEntry{}
	for _, tuple := range parseConntrack(reader) {
		if tuple.protocol == "tcp" {
			report.TotalTCPConnections++
		} else if tuple.protocol == "udp" {
			report.TotalUDPSessions++
		}
		for _, match := range matches {
			if tuple.dport != match.port || (match.protocol != "both" && match.protocol != tuple.protocol) {
				continue
			}
			itemKey := key{ruleID: match.id, source: tuple.source}
			entry := entries[itemKey]
			if entry == nil {
				entry = &domain.ConnectionEntry{RuleID: match.id, NodeID: node.ID, SourceIP: tuple.source, CapturedAt: report.CapturedAt}
				entries[itemKey] = entry
			}
			if tuple.protocol == "tcp" {
				entry.TCPConnections++
			} else {
				entry.UDPSessions++
			}
			break
		}
	}
	for _, entry := range entries {
		report.Entries = append(report.Entries, *entry)
	}
	sort.Slice(report.Entries, func(i, j int) bool {
		left := report.Entries[i].TCPConnections + report.Entries[i].UDPSessions
		right := report.Entries[j].TCPConnections + report.Entries[j].UDPSessions
		if left == right {
			return report.Entries[i].SourceIP < report.Entries[j].SourceIP
		}
		return left > right
	})
	if len(report.Entries) > 5000 {
		report.Entries = report.Entries[:5000]
	}
	report.Available = true
	return report
}
