package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaypanel/internal/domain"
	"relaypanel/internal/store"
)

func TestFreshInstallLoginAndDashboardAPIsReturnStableEmptyCollections(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(ctx, st, Options{AdminPassword: "fresh-install-password", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	loginBody := bytes.NewBufferString(`{"password":"fresh-install-password"}`)
	loginResponse, err := http.Post(httpServer.URL+"/api/v1/login", "application/json", loginBody)
	if err != nil {
		t.Fatal(err)
	}
	defer loginResponse.Body.Close()
	if loginResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(loginResponse.Body)
		t.Fatalf("login failed with %d: %s", loginResponse.StatusCode, body)
	}
	cookies := loginResponse.Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not return a session cookie")
	}

	paths := []string{
		"/api/v1/me",
		"/api/v1/dashboard",
		"/api/v1/nodes",
		"/api/v1/lines",
		"/api/v1/rules",
		"/api/v1/traffic?period=day",
		"/api/v1/traffic/rules",
		"/api/v1/probes",
		"/api/v1/target-probes",
	}
	for _, path := range paths {
		req, err := http.NewRequest(http.MethodGet, httpServer.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned %d: %s", path, response.StatusCode, body)
		}
		if path == "/api/v1/dashboard" && !bytes.Contains(body, []byte(`"recent_traffic":[]`)) {
			t.Fatalf("dashboard must return recent_traffic as [] on a fresh install: %s", body)
		}
		if path != "/api/v1/me" && path != "/api/v1/dashboard" && !bytes.Equal(bytes.TrimSpace(body), []byte("[]")) {
			t.Fatalf("GET %s must return [] on a fresh install: %s", path, body)
		}
	}
}

func TestConfigurationExportDryRunAndImportRestoreTopology(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, node := range []domain.Node{
		{ID: "in", Name: "入口", Role: domain.NodeRoleIngress, CreatedAt: time.Now().UTC()},
		{ID: "out", Name: "出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.2", CreatedAt: time.Now().UTC()},
	} {
		if err := st.CreateNode(ctx, node, "secret-token-hash"); err != nil {
			t.Fatal(err)
		}
	}
	line, err := st.SaveLine(ctx, domain.Line{ID: "line", Name: "测试线路", Mode: domain.ForwardModeDualManaged, IngressNodeID: "in", EgressNodeID: "out", EgressNodeIDs: []string{"out"}, ActiveEgressNodeID: "out", ListenAddress: "0.0.0.0", Engine: "nftables", IngressEngine: "nftables", EgressEngine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.SaveRule(ctx, domain.ForwardRule{ID: "rule", LineID: line.ID, Name: "网站", Mode: line.Mode, Protocol: "tcp", IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", ListenPort: 10000, RelayPort: 30000, TargetHost: "192.0.2.8", TargetPort: 443, Engine: "nftables", IngressEngine: "nftables", EgressEngine: "nftables", BurstKBytes: 512, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	exportResponse := httptest.NewRecorder()
	server.exportConfiguration(exportResponse, httptest.NewRequest(http.MethodGet, "/api/v1/config/export", nil))
	if exportResponse.Code != http.StatusOK || !strings.Contains(exportResponse.Header().Get("Content-Disposition"), ".json") {
		t.Fatalf("unexpected export response: %d %s", exportResponse.Code, exportResponse.Body.String())
	}
	if bytes.Contains(exportResponse.Body.Bytes(), []byte("secret-token-hash")) || bytes.Contains(exportResponse.Body.Bytes(), []byte("agent_token")) {
		t.Fatal("configuration export leaked Agent credentials")
	}
	var backup configurationBackup
	if err := json.Unmarshal(exportResponse.Body.Bytes(), &backup); err != nil {
		t.Fatal(err)
	}
	if len(backup.Lines) != 1 || len(backup.Rules) != 1 || len(backup.Nodes) != 2 {
		t.Fatalf("unexpected export: %+v", backup)
	}
	if err := st.DeleteRule(ctx, "rule"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteLine(ctx, "line"); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(backup)
	dryRunResponse := httptest.NewRecorder()
	server.importConfiguration(dryRunResponse, httptest.NewRequest(http.MethodPost, "/api/v1/config/import?dry_run=1", bytes.NewReader(payload)))
	if dryRunResponse.Code != http.StatusOK || !bytes.Contains(dryRunResponse.Body.Bytes(), []byte(`"dry_run":true`)) {
		t.Fatalf("unexpected dry-run response: %d %s", dryRunResponse.Code, dryRunResponse.Body.String())
	}
	if lines, _ := st.ListLines(ctx); len(lines) != 0 {
		t.Fatal("dry run unexpectedly wrote lines")
	}
	importResponse := httptest.NewRecorder()
	server.importConfiguration(importResponse, httptest.NewRequest(http.MethodPost, "/api/v1/config/import", bytes.NewReader(payload)))
	if importResponse.Code != http.StatusOK {
		t.Fatalf("unexpected import response: %d %s", importResponse.Code, importResponse.Body.String())
	}
	lines, _ := st.ListLines(ctx)
	rules, _ := st.ListRules(ctx)
	if len(lines) != 1 || lines[0].ID != "line" || len(rules) != 1 || rules[0].ID != "rule" {
		t.Fatalf("topology was not restored: lines=%+v rules=%+v", lines, rules)
	}
}

func TestChangePasswordVerifiesCurrentPasswordAndInvalidatesSessions(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(ctx, st, Options{AdminPassword: "old-password-123", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	login := func(password string) *http.Response {
		t.Helper()
		response, err := http.Post(httpServer.URL+"/api/v1/login", "application/json", bytes.NewBufferString(`{"password":"`+password+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	loginResponse := login("old-password-123")
	if loginResponse.StatusCode != http.StatusOK || len(loginResponse.Cookies()) == 0 {
		body, _ := io.ReadAll(loginResponse.Body)
		loginResponse.Body.Close()
		t.Fatalf("initial login failed: %d %s", loginResponse.StatusCode, body)
	}
	session := loginResponse.Cookies()[0]
	loginResponse.Body.Close()

	change := func(current, next string) *http.Response {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"current_password": current, "new_password": next})
		req, err := http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/admin/password", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(session)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	wrong := change("wrong-password", "new-password-456")
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong current password returned %d", wrong.StatusCode)
	}
	changed := change("old-password-123", "new-password-456")
	changed.Body.Close()
	if changed.StatusCode != http.StatusOK {
		t.Fatalf("password change returned %d", changed.StatusCode)
	}

	me, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	me.AddCookie(session)
	meResponse, err := http.DefaultClient.Do(me)
	if err != nil {
		t.Fatal(err)
	}
	meResponse.Body.Close()
	if meResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session remained valid after password change: %d", meResponse.StatusCode)
	}
	oldLogin := login("old-password-123")
	oldLogin.Body.Close()
	if oldLogin.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password remained valid: %d", oldLogin.StatusCode)
	}
	newLogin := login("new-password-456")
	newLogin.Body.Close()
	if newLogin.StatusCode != http.StatusOK {
		t.Fatalf("new password login failed: %d", newLogin.StatusCode)
	}
}

func TestSaveRuleUpdatePreservesRelayPortAndChangesFields(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "in", Name: "入口", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.0.2", CreatedAt: now},
		{ID: "out", Name: "出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.3", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	line, err := st.SaveLine(ctx, domain.Line{ID: "line", Name: "线路", Mode: domain.ForwardModeDualManaged, IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	original, err := st.SaveRule(ctx, domain.ForwardRule{ID: "rule", LineID: line.ID, Name: "旧目标", Mode: line.Mode, Protocol: "both", IngressNodeID: line.IngressNodeID, EgressNodeID: line.EgressNodeID, ListenAddress: line.ListenAddress, ListenPort: 10000, RelayPort: 31000, TargetHost: "192.0.2.1", TargetPort: 80, Engine: line.Engine, BurstKBytes: 512, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"line_id": line.ID, "name": "新目标", "protocol": "tcp", "listen_port": 10001,
		"target_host": "192.0.2.2", "target_port": 443, "upload_mbps": 20,
		"download_mbps": 50, "burst_kbytes": 256, "enabled": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/rules/rule", bytes.NewReader(body))
	req.SetPathValue("id", original.ID)
	recorder := httptest.NewRecorder()
	(&Server{store: st}).saveRule(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", recorder.Code, recorder.Body.String())
	}
	var updated domain.ForwardRule
	if err := json.NewDecoder(recorder.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	if updated.ListenPort != 10001 || updated.TargetHost != "192.0.2.2" || updated.TargetPort != 443 || updated.Protocol != "tcp" {
		t.Fatalf("editable fields were not updated: %+v", updated)
	}
	if updated.UploadMbps != 20 || updated.DownloadMbps != 50 || updated.BurstKBytes != 256 {
		t.Fatalf("rate limits were not updated: %+v", updated)
	}
	if updated.RelayPort != original.RelayPort {
		t.Fatalf("relay port changed unexpectedly: got %d, want %d", updated.RelayPort, original.RelayPort)
	}
}

func TestRequestPublicAddressSupportsDirectAndHTTPSProxyRequests(t *testing.T) {
	direct := httptest.NewRequest(http.MethodPost, "/agent/v1/sync", nil)
	direct.RemoteAddr = "198.51.100.20:43210"
	if got := requestPublicAddress(direct); got != "198.51.100.20" {
		t.Fatalf("unexpected direct public address: %q", got)
	}
	proxied := httptest.NewRequest(http.MethodPost, "/agent/v1/sync", nil)
	proxied.RemoteAddr = "172.18.0.2:1234"
	proxied.Header.Set("X-Forwarded-For", "203.0.113.88, 172.18.0.2")
	if got := requestPublicAddress(proxied); got != "203.0.113.88" {
		t.Fatalf("unexpected proxied public address: %q", got)
	}
	spoofed := httptest.NewRequest(http.MethodPost, "/agent/v1/sync", nil)
	spoofed.RemoteAddr = "198.51.100.20:43210"
	spoofed.Header.Set("X-Forwarded-For", "203.0.113.99")
	if got := requestPublicAddress(spoofed); got != "198.51.100.20" {
		t.Fatalf("untrusted forwarding header was accepted: %q", got)
	}
}

func TestRequestIsHTTPSOnlyTrustsLocalProxyHeaders(t *testing.T) {
	proxied := httptest.NewRequest(http.MethodPost, "/api/v1/login", nil)
	proxied.RemoteAddr = "172.18.0.2:1234"
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if !requestIsHTTPS(proxied) {
		t.Fatal("HTTPS reverse proxy request was not detected")
	}
	spoofed := httptest.NewRequest(http.MethodPost, "/api/v1/login", nil)
	spoofed.RemoteAddr = "198.51.100.20:1234"
	spoofed.Header.Set("X-Forwarded-Proto", "https")
	if requestIsHTTPS(spoofed) {
		t.Fatal("untrusted X-Forwarded-Proto header was accepted")
	}
}

func TestValidateRuleRejectsHostnameForNFTablesButAllowsRealm(t *testing.T) {
	rule := domain.ForwardRule{Mode: domain.ForwardModeDualManaged, Name: "域名目标", Protocol: "tcp", IngressNodeID: "in", EgressNodeID: "out", ListenPort: 10000, RelayPort: 30000, TargetHost: "origin.example.com", TargetPort: 443, Engine: "nftables"}
	if err := validateRule(rule); err == nil {
		t.Fatal("nftables hostname should be rejected before deployment")
	}
	rule.Engine = "realm"
	if err := validateRule(rule); err != nil {
		t.Fatalf("Realm should allow a hostname target: %v", err)
	}
	rule.IngressEngine = "nftables"
	rule.EgressEngine = "realm"
	if err := validateRule(rule); err != nil {
		t.Fatalf("target validation must use the Realm egress engine: %v", err)
	}
	rule.IngressEngine = "realm"
	rule.EgressEngine = "nftables"
	if err := validateRule(rule); err == nil {
		t.Fatal("nftables egress must reject a hostname even when ingress uses Realm")
	}
}

func TestCompleteSimpleRuleSelectsNodesAndRelayPort(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{{ID: "ingress", Name: "入口", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.0.2", CreatedAt: now}, {ID: "egress", Name: "出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.3", CreatedAt: now}} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: st}
	rule := domain.ForwardRule{Mode: domain.ForwardModeDualManaged, Protocol: "both", ListenPort: 24444, TargetHost: "192.0.2.88", TargetPort: 24444}
	if err := s.completeSimpleRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.IngressNodeID != "ingress" || rule.EgressNodeID != "egress" {
		t.Fatalf("unexpected route: %+v", rule)
	}
	if rule.RelayPort < 30000 || rule.RelayPort >= 60000 {
		t.Fatalf("unexpected relay port: %d", rule.RelayPort)
	}
}

func TestCompleteExitOnlyRuleUsesEgressPrivateListener(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	node := domain.Node{ID: "egress", Name: "香港出口", Role: domain.NodeRoleEgress, PublicAddress: "198.51.100.24", PrivateAddress: "10.24.0.3", CreatedAt: now}
	if err := st.CreateNode(ctx, node, "hash"); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	rule := domain.ForwardRule{Mode: domain.ForwardModeExitOnly, EgressNodeID: node.ID, Protocol: "both", ListenPort: 24444, TargetHost: "192.0.2.88", TargetPort: 443}
	if err := s.completeSimpleRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.IngressNodeID != node.ID || rule.EgressNodeID != node.ID || rule.ListenAddress != node.PrivateAddress || rule.RelayPort != rule.ListenPort {
		t.Fatalf("unexpected exit-only route: %+v", rule)
	}
}

func TestCompleteLineValidatesRolesAndFillsListener(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "ingress", Name: "广州入口", Role: domain.NodeRoleIngress, PrivateAddress: "10.24.0.2", CreatedAt: now},
		{ID: "egress", Name: "香港出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.24.0.3", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: st}

	dual := domain.Line{Mode: domain.ForwardModeDualManaged, IngressNodeID: "ingress", EgressNodeID: "egress"}
	if err := s.completeLine(ctx, &dual); err != nil {
		t.Fatal(err)
	}
	if dual.ListenAddress != "0.0.0.0" {
		t.Fatalf("unexpected dual listener: %q", dual.ListenAddress)
	}

	exitOnly := domain.Line{Mode: domain.ForwardModeExitOnly, EgressNodeID: "egress", ListenAddress: "0.0.0.0"}
	if err := s.completeLine(ctx, &exitOnly); err != nil {
		t.Fatal(err)
	}
	if exitOnly.IngressNodeID != "egress" || exitOnly.ListenAddress != "10.24.0.3" {
		t.Fatalf("unexpected exit-only line: %+v", exitOnly)
	}

	invalid := domain.Line{Mode: domain.ForwardModeDualManaged, IngressNodeID: "egress", EgressNodeID: "ingress"}
	if err := s.completeLine(ctx, &invalid); err == nil {
		t.Fatal("expected invalid node roles to be rejected")
	}
}

func TestCompleteSimpleRuleUsesConfiguredExitPortPool(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "in", Name: "入口", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.0.2", CreatedAt: now},
		{ID: "out", Name: "NAT 出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.3", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	line, err := st.SaveLine(ctx, domain.Line{ID: "line", Name: "NAT 线路", Mode: domain.ForwardModeDualManaged, IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", RelayPortRange: "20000-20002", Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.SaveRule(ctx, domain.ForwardRule{ID: "used", Name: "占用端口", Mode: domain.ForwardModeDualManaged, Protocol: "tcp", IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", ListenPort: 10001, RelayPort: 20000, TargetHost: "192.0.2.1", TargetPort: 80, Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	rule := domain.ForwardRule{LineID: line.ID, Mode: line.Mode, Protocol: "tcp", IngressNodeID: "in", EgressNodeID: "out", ListenPort: 10002, TargetHost: "192.0.2.2", TargetPort: 80}
	if err := s.completeSimpleRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.RelayPort != 20001 {
		t.Fatalf("expected first free NAT port 20001, got %d", rule.RelayPort)
	}
}

func TestDiscreteProviderPortsAreAcceptedAndAllocated(t *testing.T) {
	ranges, err := parsePortRanges("12001,16388,22000-22002")
	if err != nil {
		t.Fatal(err)
	}
	rules := []domain.ForwardRule{{ID: "used", EgressNodeID: "out", RelayPort: 12001, Protocol: "both"}}
	if got := allocateRelayPort(ranges, rules, "out", "tcp", ""); got != 16388 {
		t.Fatalf("expected first unused provider port 16388, got %d", got)
	}
}

func TestRelayPortAllocationChecksEveryFailoverEgress(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "in-a", Name: "入口 A", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.0.2", CreatedAt: now},
		{ID: "in-b", Name: "入口 B", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.1.2", CreatedAt: now},
		{ID: "out-a", Name: "出口 A", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.3", CreatedAt: now},
		{ID: "out-b", Name: "共享备用出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.4", CreatedAt: now},
		{ID: "out-c", Name: "出口 C", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.1.3", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	lineA, err := st.SaveLine(ctx, domain.Line{ID: "line-a", Name: "线路 A", Mode: domain.ForwardModeDualManaged, IngressNodeID: "in-a", EgressNodeID: "out-a", EgressNodeIDs: []string{"out-a", "out-b"}, ActiveEgressNodeID: "out-a", FailoverEnabled: true, ListenAddress: "0.0.0.0", RelayPortRange: "20000-20001", Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	lineB, err := st.SaveLine(ctx, domain.Line{ID: "line-b", Name: "线路 B", Mode: domain.ForwardModeDualManaged, IngressNodeID: "in-b", EgressNodeID: "out-c", EgressNodeIDs: []string{"out-c", "out-b"}, ActiveEgressNodeID: "out-c", FailoverEnabled: true, ListenAddress: "0.0.0.0", RelayPortRange: "20000-20001", Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveRule(ctx, domain.ForwardRule{ID: "used", LineID: lineA.ID, Name: "已占用", Mode: lineA.Mode, Protocol: "tcp", IngressNodeID: "in-a", EgressNodeID: "out-a", ListenAddress: "0.0.0.0", ListenPort: 10000, RelayPort: 20000, TargetHost: "192.0.2.1", TargetPort: 80, Engine: "nftables", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	rule := domain.ForwardRule{LineID: lineB.ID, Mode: lineB.Mode, Protocol: "tcp", IngressNodeID: "in-b", EgressNodeID: "out-c", ListenPort: 10001, TargetHost: "192.0.2.2", TargetPort: 80}
	if err := s.completeSimpleRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.RelayPort != 20001 {
		t.Fatalf("shared standby port conflict was missed: got %d, want 20001", rule.RelayPort)
	}
}

func TestLineUpdateMigratesExistingRulesToNewServers(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "in-a", Name: "入口 A", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.0.2", CreatedAt: now},
		{ID: "out-a", Name: "出口 A", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.3", CreatedAt: now},
		{ID: "out-b", Name: "出口 B", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.4", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	line, err := st.SaveLine(ctx, domain.Line{ID: "line", Name: "线路", Mode: domain.ForwardModeDualManaged, IngressNodeID: "in-a", EgressNodeID: "out-a", ListenAddress: "0.0.0.0", Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.SaveRule(ctx, domain.ForwardRule{ID: "rule", LineID: line.ID, Name: "网站", Mode: line.Mode, Protocol: "both", IngressNodeID: line.IngressNodeID, EgressNodeID: line.EgressNodeID, ListenAddress: line.ListenAddress, ListenPort: 24444, RelayPort: 32444, TargetHost: "192.0.2.88", TargetPort: 443, Engine: line.Engine, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	updated := line
	updated.EgressNodeID = "out-b"
	updated.RelayPortRange = "41000-41001"
	updated.Engine = "realm"
	s := &Server{store: st}
	rules, err := s.rulesForLineUpdate(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].EgressNodeID != "out-b" || rules[0].RelayPort != 41000 || rules[0].Engine != "realm" {
		t.Fatalf("unexpected migrated rule: %+v", rules)
	}
	if _, err := st.SaveLineRules(ctx, updated, rules); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetRule(ctx, "rule")
	if err != nil || stored.EgressNodeID != "out-b" || stored.RelayPort != 41000 || stored.Engine != "realm" {
		t.Fatalf("rule migration was not stored: %+v, %v", stored, err)
	}
}
