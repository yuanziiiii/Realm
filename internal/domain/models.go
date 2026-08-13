package domain

import "time"

type NodeRole string
type ForwardMode string

const (
	NodeRoleIngress NodeRole = "ingress"
	NodeRoleEgress  NodeRole = "egress"
	NodeRoleBoth    NodeRole = "both"

	ForwardModeDualManaged ForwardMode = "dual_managed"
	ForwardModeExitOnly    ForwardMode = "exit_only"
)

type Node struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Role             NodeRole  `json:"role"`
	PublicAddress    string    `json:"public_address"`
	PrivateAddress   string    `json:"private_address"`
	PublicInterface  string    `json:"public_interface"`
	PrivateInterface string    `json:"private_interface"`
	Status           string    `json:"status"`
	AgentVersion     string    `json:"agent_version,omitempty"`
	AppliedRevision  int64     `json:"applied_revision"`
	LastSeenAt       time.Time `json:"last_seen_at,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type ForwardRule struct {
	ID            string      `json:"id"`
	LineID        string      `json:"line_id"`
	Mode          ForwardMode `json:"mode"`
	Name          string      `json:"name"`
	Protocol      string      `json:"protocol"`
	IngressNodeID string      `json:"ingress_node_id"`
	EgressNodeID  string      `json:"egress_node_id"`
	ListenAddress string      `json:"listen_address"`
	ListenPort    int         `json:"listen_port"`
	RelayPort     int         `json:"relay_port"`
	TargetHost    string      `json:"target_host"`
	TargetPort    int         `json:"target_port"`
	Engine        string      `json:"engine"`
	UploadMbps    int         `json:"upload_mbps"`
	DownloadMbps  int         `json:"download_mbps"`
	BurstKBytes   int         `json:"burst_kbytes"`
	Enabled       bool        `json:"enabled"`
	Revision      int64       `json:"revision"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// Line keeps the stable network topology separate from individual port rules.
// A user chooses the servers and forwarding engine once on a line, then every
// rule on that line only needs a listen port and destination.
type Line struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Mode           ForwardMode `json:"mode"`
	IngressNodeID  string      `json:"ingress_node_id"`
	EgressNodeID   string      `json:"egress_node_id"`
	ListenAddress  string      `json:"listen_address"`
	RelayPortRange string      `json:"relay_port_range"`
	Engine         string      `json:"engine"`
	Enabled        bool        `json:"enabled"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

type Deployment struct {
	Rule ForwardRule `json:"rule"`
	Role NodeRole    `json:"role"`
}

type TrafficDelta struct {
	RuleID          string    `json:"rule_id"`
	CapturedAt      time.Time `json:"captured_at"`
	Cumulative      bool      `json:"cumulative,omitempty"`
	UploadBytes     int64     `json:"upload_bytes"`
	DownloadBytes   int64     `json:"download_bytes"`
	UploadPackets   int64     `json:"upload_packets"`
	DownloadPackets int64     `json:"download_packets"`
}

type TrafficPoint struct {
	Bucket          time.Time `json:"bucket"`
	UploadBytes     int64     `json:"upload_bytes"`
	DownloadBytes   int64     `json:"download_bytes"`
	UploadPackets   int64     `json:"upload_packets"`
	DownloadPackets int64     `json:"download_packets"`
}

type RuleTrafficSummary struct {
	RuleID                 string `json:"rule_id"`
	TotalUploadBytes       int64  `json:"total_upload_bytes"`
	TotalDownloadBytes     int64  `json:"total_download_bytes"`
	TodayUploadBytes       int64  `json:"today_upload_bytes"`
	TodayDownloadBytes     int64  `json:"today_download_bytes"`
	WeekUploadBytes        int64  `json:"week_upload_bytes"`
	WeekDownloadBytes      int64  `json:"week_download_bytes"`
	MonthUploadBytes       int64  `json:"month_upload_bytes"`
	MonthDownloadBytes     int64  `json:"month_download_bytes"`
	QuarterUploadBytes     int64  `json:"quarter_upload_bytes"`
	QuarterDownloadBytes   int64  `json:"quarter_download_bytes"`
	UploadBytesPerSecond   int64  `json:"upload_bytes_per_second"`
	DownloadBytesPerSecond int64  `json:"download_bytes_per_second"`
}

type NetworkInfo struct {
	PublicAddress    string `json:"public_address,omitempty"`
	PrivateAddress   string `json:"private_address,omitempty"`
	PublicInterface  string `json:"public_interface,omitempty"`
	PrivateInterface string `json:"private_interface,omitempty"`
}

type SyncRequest struct {
	NodeID          string         `json:"node_id"`
	AgentVersion    string         `json:"agent_version"`
	AppliedRevision int64          `json:"applied_revision"`
	ApplyStatus     string         `json:"apply_status"`
	ApplyError      string         `json:"apply_error,omitempty"`
	Network         NetworkInfo    `json:"network,omitempty"`
	Traffic         []TrafficDelta `json:"traffic,omitempty"`
}

type SyncResponse struct {
	Revision    int64        `json:"revision"`
	GeneratedAt time.Time    `json:"generated_at"`
	Node        Node         `json:"node"`
	Peers       []Node       `json:"peers"`
	Deployments []Deployment `json:"deployments"`
}

type DashboardSummary struct {
	OnlineNodes     int64          `json:"online_nodes"`
	TotalNodes      int64          `json:"total_nodes"`
	EnabledRules    int64          `json:"enabled_rules"`
	TotalRules      int64          `json:"total_rules"`
	TodayUpload     int64          `json:"today_upload"`
	TodayDownload   int64          `json:"today_download"`
	WeekUpload      int64          `json:"week_upload"`
	WeekDownload    int64          `json:"week_download"`
	MonthUpload     int64          `json:"month_upload"`
	MonthDownload   int64          `json:"month_download"`
	QuarterUpload   int64          `json:"quarter_upload"`
	QuarterDownload int64          `json:"quarter_download"`
	RecentTraffic   []TrafficPoint `json:"recent_traffic"`
}
