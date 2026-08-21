package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestDryRunExecutorRequiresFirstReconcile(t *testing.T) {
	executor := NewExecutor(false, t.TempDir(), "realm", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if executor.Healthy(context.Background()) {
		t.Fatal("a new executor must reconcile once even when the persisted revision matches")
	}
	if err := executor.Reconcile(context.Background(), Plan{}); err != nil {
		t.Fatal(err)
	}
	if !executor.Healthy(context.Background()) {
		t.Fatal("a successfully reconciled dry-run executor should be healthy")
	}
}

func TestManagedTCInterfacesAreUnique(t *testing.T) {
	commands := []Command{
		{Name: "tc", Args: []string{"qdisc", "add", "dev", "eth0", "root", "handle", "7a1:", "htb"}},
		{Name: "tc", Args: []string{"qdisc", "add", "dev", "eth0", "handle", "ffff:", "ingress"}},
		{Name: "tc", Args: []string{"qdisc", "add", "dev", "wg0", "root", "handle", "7a1:", "htb"}},
		{Name: "ip", Args: []string{"link", "show"}},
	}
	got := managedTCInterfaces(commands)
	if len(got) != 2 || got[0] != "eth0" || got[1] != "wg0" {
		t.Fatalf("unexpected managed interfaces: %v", got)
	}
}

func TestManagedIngressInterfacesAreSeparateFromRootQdiscs(t *testing.T) {
	commands := []Command{
		{Name: "tc", Args: []string{"qdisc", "add", "dev", "eth0", "handle", "ffff:", "ingress"}},
		{Name: "tc", Args: []string{"qdisc", "add", "dev", "ifb-relay0", "root", "handle", "7a1:", "htb"}},
	}
	if got := managedTCInterfaces(commands); len(got) != 1 || got[0] != "ifb-relay0" {
		t.Fatalf("unexpected root qdisc interfaces: %v", got)
	}
	if got := managedIngressInterfaces(commands); len(got) != 1 || got[0] != "eth0" {
		t.Fatalf("unexpected ingress interfaces: %v", got)
	}
}

func TestCompatibleManagedQdiscRecognizesReusableIngress(t *testing.T) {
	ingressArgs := []string{"qdisc", "add", "dev", "eth0", "handle", "ffff:", "ingress"}
	if !compatibleManagedQdisc(ingressArgs, "qdisc ingress ffff: parent ffff:fff1") {
		t.Fatal("existing ingress qdisc should be reusable")
	}
	if compatibleManagedQdisc(ingressArgs, "qdisc clsact ffff: parent ffff:fff1") {
		t.Fatal("clsact must not be mistaken for the requested ingress qdisc")
	}
	rootArgs := []string{"qdisc", "add", "dev", "eth0", "root", "handle", "7a1:", "htb"}
	if !compatibleManagedQdisc(rootArgs, "qdisc htb 7a1: root refcnt 2") {
		t.Fatal("existing Relay Panel HTB qdisc should be reusable")
	}
}

func TestQdiscConflictRecognizesKernelMessages(t *testing.T) {
	for _, output := range []string{"RTNETLINK answers: File exists", "Error: Exclusivity flag on, cannot modify."} {
		if !isQdiscConflict([]byte(output)) {
			t.Fatalf("expected qdisc conflict for %q", output)
		}
	}
}

func TestManagedIngressFilterPrefsOnlySelectsRelayRedirects(t *testing.T) {
	listed := `filter protocol ip pref 100 flower chain 0
filter protocol ip pref 100 flower chain 0 handle 0x1
  action order 1: skbedit mark 0x1a2b
  action order 2: mirred (Egress Redirect to device ifb-relay0)
filter protocol ip pref 200 flower chain 0
filter protocol ip pref 200 flower chain 0 handle 0x2
  action order 1: mirred (Egress Redirect to device ifb-admin0)
filter protocol ip pref 300 flower chain 0
filter protocol ip pref 300 flower chain 0 handle 0x3
  action order 1: mirred (Egress Redirect to device ifb-relay0)`
	got := managedIngressFilterPrefs(listed)
	if len(got) != 1 || got[0] != "100" {
		t.Fatalf("unexpected managed prefs: %v", got)
	}
}

func TestTCFlowTargetSupportsIPRouteOutputVariants(t *testing.T) {
	for _, listed := range []string{
		"filter protocol ip pref 20 fw handle 0x7b flowid 7a1:20",
		"filter protocol ip pref 20 fw chain 0 handle 0x7b classid 7a1:20",
	} {
		if !hasTCFlowTarget(listed, "7a1:20") {
			t.Fatalf("expected class target in %q", listed)
		}
	}
	if hasTCFlowTarget("classid 7a1:21", "7a1:20") {
		t.Fatal("must not accept another class")
	}
}
