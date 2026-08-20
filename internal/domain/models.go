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
	ID                    string    `json:"id"`
	Name                  string    `json:"name"`
	Role                  NodeRole  `json:"role"`
	PublicAddress         string    `json:"public_address"`
	PrivateAddress        string    `json:"private_address"`
	PublicInterface       string    `json:"public_interface"`
	PrivateInterface      string    `json:"private_interface"`
	DefaultRelayPortRange string    `json:"default_relay_port_range"`
	Status                string    `json:"status"`
	AgentVersion          string    `json:"agent_version,omitempty"`
	AppliedRevision       int64     `json:"applied_revision"`
	ApplyStatus           string    `json:"apply_status"`
	ApplyError            string    `json:"apply_error,omitempty"`
	LastSeenAt            time.Time `json:"last_seen_at,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
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
	// RelayPorts stores the independently allocated relay port for each egress.
	// RelayPort remains the primary egress value for older Agents and clients.
	RelayPorts map[string]int `json:"relay_ports"`
	TargetHost string         `json:"target_host"`
	TargetPort int            `json:"target_port"`
	// Engine is retained as a compatibility field for older Agents and API
	// clients. The controller rewrites it to the engine for each deployment.
	Engine        string    `json:"engine"`
	IngressEngine string    `json:"ingress_engine"`
	EgressEngine  string    `json:"egress_engine"`
	UploadMbps    int       `json:"upload_mbps"`
	DownloadMbps  int       `json:"download_mbps"`
	BurstKBytes   int       `json:"burst_kbytes"`
	Enabled       bool      `json:"enabled"`
	Revision      int64     `json:"revision"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Line keeps the stable network topology separate from individual port rules.
// A user chooses the servers and per-hop forwarding engines once on a line,
// then every rule on that line only needs a listen port and destination.
type Line struct {
	ID                 string      `json:"id"`
	Name               string      `json:"name"`
	Mode               ForwardMode `json:"mode"`
	IngressNodeID      string      `json:"ingress_node_id"`
	EgressNodeID       string      `json:"egress_node_id"`
	EgressNodeIDs      []string    `json:"egress_node_ids"`
	ActiveEgressNodeID string      `json:"active_egress_node_id"`
	FailoverEnabled    bool        `json:"failover_enabled"`
	ListenAddress      string      `json:"listen_address"`
	RelayPortRange     string      `json:"relay_port_range"`
	// EgressPortRanges allows NAT exits on the same managed line to use
	// different provider-assigned port ranges. RelayPortRange mirrors the
	// primary exit range for compatibility.
	EgressPortRanges map[string]string `json:"egress_port_ranges"`
	// Engine mirrors EgressEngine for compatibility with older clients.
	Engine        string    `json:"engine"`
	IngressEngine string    `json:"ingress_engine"`
	EgressEngine  string    `json:"egress_engine"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func NormalizeEngines(legacy, ingress, egress string, mode ForwardMode) (string, string, string) {
	legacyProvided := legacy != ""
	if legacy == "" {
		legacy = "nftables"
	}
	// An older client may read a new response, keep the unknown split fields,
	// and only edit Engine. When both split values still match, treat that as a
	// deliberate legacy update of the whole line.
	if legacyProvided && ingress != "" && ingress == egress && legacy != egress {
		ingress = legacy
		egress = legacy
	}
	if ingress == "" {
		ingress = legacy
	}
	if egress == "" {
		egress = legacy
	}
	if mode == ForwardModeExitOnly {
		ingress = egress
	}
	return egress, ingress, egress
}

func (line *Line) NormalizeEngines() {
	line.Engine, line.IngressEngine, line.EgressEngine = NormalizeEngines(line.Engine, line.IngressEngine, line.EgressEngine, line.Mode)
}

func (line *Line) NormalizeEgressPortRanges() {
	if line.EgressPortRanges == nil {
		line.EgressPortRanges = map[string]string{}
	}
	if line.EgressNodeID != "" {
		if _, ok := line.EgressPortRanges[line.EgressNodeID]; !ok {
			line.EgressPortRanges[line.EgressNodeID] = line.RelayPortRange
		}
		line.RelayPortRange = line.EgressPortRanges[line.EgressNodeID]
	}
}

func (line Line) RelayPortRangeFor(egressNodeID string) string {
	if value, ok := line.EgressPortRanges[egressNodeID]; ok {
		return value
	}
	return line.RelayPortRange
}

func (rule *ForwardRule) NormalizeEngines() {
	rule.Engine, rule.IngressEngine, rule.EgressEngine = NormalizeEngines(rule.Engine, rule.IngressEngine, rule.EgressEngine, rule.Mode)
}

func (rule *ForwardRule) NormalizeRelayPorts() {
	if rule.RelayPorts == nil {
		rule.RelayPorts = map[string]int{}
	}
	if rule.EgressNodeID != "" {
		if _, ok := rule.RelayPorts[rule.EgressNodeID]; !ok && rule.RelayPort > 0 {
			rule.RelayPorts[rule.EgressNodeID] = rule.RelayPort
		}
		if value := rule.RelayPorts[rule.EgressNodeID]; value > 0 {
			rule.RelayPort = value
		}
	}
}

func (rule ForwardRule) RelayPortFor(egressNodeID string) int {
	if value := rule.RelayPorts[egressNodeID]; value > 0 {
		return value
	}
	// A rule created before per-egress ports used the same RelayPort on every
	// managed exit. Keep that behavior until the controller next saves it.
	return rule.RelayPort
}

func (rule *ForwardRule) SetRelayPortFor(egressNodeID string, port int) {
	if rule.RelayPorts == nil {
		rule.RelayPorts = map[string]int{}
	}
	rule.RelayPorts[egressNodeID] = port
	if egressNodeID == rule.EgressNodeID {
		rule.RelayPort = port
	}
}

// EngineForRole makes a mixed-engine line compatible with Agents that only
// know the legacy Rule.Engine field.
func (rule ForwardRule) EngineForRole(role NodeRole) string {
	rule.NormalizeEngines()
	if rule.Mode != ForwardModeExitOnly && role != NodeRoleEgress {
		return rule.IngressEngine
	}
	return rule.EgressEngine
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

type LinkProbe struct {
	IngressNodeID string    `json:"ingress_node_id"`
	EgressNodeID  string    `json:"egress_node_id"`
	Address       string    `json:"address"`
	LatencyMS     float64   `json:"latency_ms"`
	PacketLoss    float64   `json:"packet_loss"`
	Success       bool      `json:"success"`
	HasSucceeded  bool      `json:"has_succeeded"`
	FailureCount  int       `json:"failure_count"`
	SuccessCount  int       `json:"success_count"`
	CheckedAt     time.Time `json:"checked_at"`
}

// TargetProbe measures the path from an egress Agent to a rule's landing host.
// NodeID is included because managed failover may deploy one rule to several
// exits at the same time.
type TargetProbe struct {
	RuleID       string    `json:"rule_id"`
	NodeID       string    `json:"node_id"`
	Address      string    `json:"address"`
	Port         int       `json:"port"`
	LatencyMS    float64   `json:"latency_ms"`
	PacketLoss   float64   `json:"packet_loss"`
	Success      bool      `json:"success"`
	HasSucceeded bool      `json:"has_succeeded"`
	FailureCount int       `json:"failure_count"`
	SuccessCount int       `json:"success_count"`
	TCPChecked   bool      `json:"tcp_checked"`
	TCPSuccess   bool      `json:"tcp_success"`
	TCPLatencyMS float64   `json:"tcp_latency_ms"`
	TCPError     string    `json:"tcp_error,omitempty"`
	CheckedAt    time.Time `json:"checked_at"`
}

type SyncRequest struct {
	NodeID          string         `json:"node_id"`
	AgentVersion    string         `json:"agent_version"`
	AppliedRevision int64          `json:"applied_revision"`
	ApplyStatus     string         `json:"apply_status"`
	ApplyError      string         `json:"apply_error,omitempty"`
	Network         NetworkInfo    `json:"network,omitempty"`
	Traffic         []TrafficDelta `json:"traffic,omitempty"`
	Probes          []LinkProbe    `json:"probes,omitempty"`
	TargetProbes    []TargetProbe  `json:"target_probes,omitempty"`
}

type SyncResponse struct {
	Revision     int64        `json:"revision"`
	GeneratedAt  time.Time    `json:"generated_at"`
	Node         Node         `json:"node"`
	Peers        []Node       `json:"peers"`
	ProbeTargets []Node       `json:"probe_targets"`
	Deployments  []Deployment `json:"deployments"`
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
