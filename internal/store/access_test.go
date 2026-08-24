package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"relaypanel/internal/domain"
)

func TestGeoAccessExpansionAndConnectionStatus(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "access.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "in", Name: "入口", Role: domain.NodeRoleIngress, PublicAddress: "198.51.100.2", PublicInterface: "eth0", PrivateAddress: "10.0.0.2", PrivateInterface: "eth1", CreatedAt: now},
		{ID: "out", Name: "出口", Role: domain.NodeRoleEgress, PublicAddress: "198.51.100.3", PublicInterface: "eth0", PrivateAddress: "10.0.0.3", PrivateInterface: "eth1", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "token-hash"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ReplaceGeoRanges(ctx, []domain.GeoRange{
		{StartIP: "1.0.0.0", EndIP: "1.0.0.127", Country: "中国", Province: "广东省", City: "深圳市", ISP: "电信"},
		{StartIP: "1.0.0.128", EndIP: "1.0.0.255", Country: "中国", Province: "广东省", City: "广州市", ISP: "联通"},
		{StartIP: "2.0.0.0", EndIP: "2.0.0.255", Country: "中国", Province: "北京市", City: "北京市", ISP: "移动"},
	}); err != nil {
		t.Fatal(err)
	}
	rule, err := st.SaveRule(ctx, domain.ForwardRule{
		ID: "rule", Mode: domain.ForwardModeDualManaged, Name: "测试规则", Protocol: "both",
		IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", ListenPort: 24444,
		RelayPort: 32444, TargetHost: "192.0.2.8", TargetPort: 443,
		IngressEngine: "realm", EgressEngine: "nftables", Enabled: true,
		AccessPolicy: domain.AccessPolicy{Enabled: true, AllowRegions: []string{"广东省"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := st.DeploymentsForNode(ctx, "in")
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 1 || len(deployments[0].Rule.AccessPolicy.ResolvedAllowRanges) != 1 || deployments[0].Rule.AccessPolicy.ResolvedAllowRanges[0] != "1.0.0.0-1.0.0.255" {
		t.Fatalf("geographic ranges were not compacted: %#v", deployments)
	}
	if err := st.UpsertConnectionReport(ctx, "in", domain.ConnectionReport{
		Available: true, CapturedAt: now, TotalTCPConnections: 27, TotalUDPSessions: 9,
		RuleIDs: []string{rule.ID},
		Entries: []domain.ConnectionEntry{{RuleID: rule.ID, NodeID: "in", SourceIP: "1.0.0.8", TCPConnections: 3, UDPSessions: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	connections, err := st.ListConnections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(connections.Statuses) != 1 || connections.Statuses[0].TotalTCPConnections != 27 || connections.Statuses[0].TotalUDPSessions != 9 {
		t.Fatalf("unexpected whole-node status: %#v", connections.Statuses)
	}
	if len(connections.Sources) != 1 || connections.Sources[0].Province != "广东省" || connections.Sources[0].City != "深圳市" || connections.Sources[0].ISP != "电信" {
		t.Fatalf("unexpected source geolocation: %#v", connections.Sources)
	}
}

func TestGeoStatusDoesNotDuplicateProvinceWithEmptyCity(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "geo-status.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReplaceGeoRanges(ctx, []domain.GeoRange{
		{StartIP: "1.0.0.0", EndIP: "1.0.0.127", Country: "中国", Province: "广东省"},
		{StartIP: "1.0.0.128", EndIP: "1.0.0.255", Country: "中国", Province: "广东省", City: "深圳市"},
	}); err != nil {
		t.Fatal(err)
	}
	status, err := st.GeoStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Regions) != 1 || status.Regions[0].Province != "广东省" || len(status.Regions[0].Cities) != 1 || status.Regions[0].Cities[0] != "深圳市" {
		t.Fatalf("unexpected regions: %#v", status.Regions)
	}
}

func TestGeoStatusHidesPreviouslyImportedGlobalRows(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "geo-global.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO geo_ip_ranges(start_ip,end_ip,country,province,city,isp) VALUES(?,?,?,?,?,?),(?,?,?,?,?,?)`,
		int64(1), int64(255), "Netherlands", "'s-Gravenzande", "0", "Vodafone",
		int64(256), int64(511), "中国", "广东省", "深圳市", "电信",
	); err != nil {
		t.Fatal(err)
	}
	status, err := st.GeoStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Ranges != 1 || len(status.Regions) != 1 || status.Regions[0].Province != "广东省" {
		t.Fatalf("global rows leaked into status: %#v", status)
	}
}

func TestMaxMindOnlySupplementsUnknownIP2RegionSpace(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "geo-supplement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReplaceGeoRanges(ctx, []domain.GeoRange{
		{StartIP: "223.104.80.0", EndIP: "223.104.83.255", Country: "中国", Province: "广东省", City: "广州市", ISP: "移动"},
		{StartIP: "223.104.84.0", EndIP: "223.104.87.255", Country: "中国", ISP: "移动"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceMaxMindRanges(ctx, []domain.GeoRange{
		{StartIP: "223.104.80.0", EndIP: "223.104.87.255", Country: "中国", Province: "广东省", City: "深圳市"},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.db.QueryContext(ctx, `SELECT start_ip,end_ip,province,city FROM geo_ip_ranges ORDER BY start_ip`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []struct {
		start, end     int64
		province, city string
	}
	for rows.Next() {
		var value struct {
			start, end     int64
			province, city string
		}
		if err := rows.Scan(&value.start, &value.end, &value.province, &value.city); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if len(values) != 2 || values[0].city != "广州市" || values[1].city != "深圳市" {
		t.Fatalf("ip2region priority or MaxMind supplement is wrong: %#v", values)
	}
}
