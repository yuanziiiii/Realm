package agent

import (
	"strings"
	"testing"
)

func TestParseConntrackUsesOriginalTupleOnly(t *testing.T) {
	input := `ipv4 2 tcp 6 431999 ESTABLISHED src=203.0.113.8 dst=198.51.100.2 sport=51822 dport=24444 src=192.0.2.88 dst=203.0.113.8 sport=24444 dport=51822 [ASSURED] mark=0 use=1
ipv4 2 udp 17 119 src=203.0.113.9 dst=198.51.100.2 sport=51000 dport=24444 src=192.0.2.88 dst=203.0.113.9 sport=24444 dport=51000 mark=0 use=1`
	tuples := parseConntrack(strings.NewReader(input))
	if len(tuples) != 2 || tuples[0].protocol != "tcp" || tuples[0].source != "203.0.113.8" || tuples[0].dport != 24444 || tuples[1].protocol != "udp" || tuples[1].source != "203.0.113.9" {
		t.Fatalf("unexpected conntrack tuples: %+v", tuples)
	}
}
