package agent

import "testing"

func TestParsePingOutput(t *testing.T) {
	output := `3 packets transmitted, 3 received, 0% packet loss, time 2003ms
rtt min/avg/max/mdev = 12.100/13.250/14.800/0.700 ms`
	latency, loss, ok := parsePingOutput(output)
	if !ok || latency != 13.25 || loss != 0 {
		t.Fatalf("unexpected probe: latency=%v loss=%v ok=%v", latency, loss, ok)
	}

	latency, loss, ok = parsePingOutput("3 packets transmitted, 0 received, 100% packet loss")
	if !ok || latency != 0 || loss != 100 {
		t.Fatalf("unexpected failed probe: latency=%v loss=%v ok=%v", latency, loss, ok)
	}
}
