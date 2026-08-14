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
		{Name: "tc", Args: []string{"qdisc", "add", "dev", "eth0", "parent", "7a1:1"}},
		{Name: "tc", Args: []string{"qdisc", "add", "dev", "wg0", "root", "handle", "7a1:", "htb"}},
		{Name: "ip", Args: []string{"link", "show"}},
	}
	got := managedTCInterfaces(commands)
	if len(got) != 2 || got[0] != "eth0" || got[1] != "wg0" {
		t.Fatalf("unexpected managed interfaces: %v", got)
	}
}
