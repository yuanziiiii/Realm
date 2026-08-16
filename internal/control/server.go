package control

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"relaypanel/internal/domain"
	"relaypanel/internal/store"
)

type Server struct {
	store         *store.Store
	log           *slog.Logger
	sessionSecret []byte
	secureCookies bool
	webProxy      *httputil.ReverseProxy
}

type Options struct {
	AdminPassword string
	SessionSecret string
	SecureCookies bool
	WebURL        string
	Logger        *slog.Logger
}

type configNodeReference struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Role domain.NodeRole `json:"role"`
}

type configurationBackup struct {
	Format        string                `json:"format"`
	SchemaVersion int                   `json:"schema_version"`
	ExportedAt    time.Time             `json:"exported_at"`
	Nodes         []configNodeReference `json:"required_nodes"`
	Lines         []domain.Line         `json:"lines"`
	Rules         []domain.ForwardRule  `json:"rules"`
}

type configurationImportResult struct {
	DryRun   bool  `json:"dry_run"`
	Lines    int   `json:"lines"`
	Rules    int   `json:"rules"`
	Revision int64 `json:"revision,omitempty"`
}

type ruleBatchItem struct {
	Name         string `json:"name"`
	Protocol     string `json:"protocol"`
	ListenPort   int    `json:"listen_port"`
	TargetHost   string `json:"target_host"`
	TargetPort   int    `json:"target_port"`
	UploadMbps   int    `json:"upload_mbps"`
	DownloadMbps int    `json:"download_mbps"`
	BurstKBytes  int    `json:"burst_kbytes"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

type ruleBatchImport struct {
	Format        string          `json:"format"`
	SchemaVersion int             `json:"schema_version"`
	LineID        string          `json:"line_id"`
	Rules         []ruleBatchItem `json:"rules"`
}

type ruleBatchImportResult struct {
	DryRun   bool  `json:"dry_run"`
	Rules    int   `json:"rules"`
	Revision int64 `json:"revision,omitempty"`
}

func New(ctx context.Context, st *store.Store, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := ensureAdmin(ctx, st, opts.AdminPassword); err != nil {
		return nil, err
	}
	if _, err := st.GetSetting(ctx, "session_generation"); errors.Is(err, store.ErrNotFound) {
		if err := st.SetSetting(ctx, "session_generation", "1"); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	secret := []byte(opts.SessionSecret)
	if len(secret) < 32 {
		stored, err := st.GetSetting(ctx, "session_secret")
		if err == nil {
			secret, err = base64.RawURLEncoding.DecodeString(stored)
			if err != nil {
				return nil, fmt.Errorf("decode session secret: %w", err)
			}
		} else {
			secret = make([]byte, 32)
			if _, err = rand.Read(secret); err != nil {
				return nil, err
			}
			if err = st.SetSetting(ctx, "session_secret", base64.RawURLEncoding.EncodeToString(secret)); err != nil {
				return nil, err
			}
		}
	}
	s := &Server{store: st, log: opts.Logger, sessionSecret: secret, secureCookies: opts.SecureCookies}
	if opts.WebURL != "" {
		target, err := url.Parse(opts.WebURL)
		if err != nil {
			return nil, err
		}
		s.webProxy = httputil.NewSingleHostReverseProxy(target)
	}
	return s, nil
}

func ensureAdmin(ctx context.Context, st *store.Store, password string) error {
	_, err := st.GetSetting(ctx, "admin_password_hash")
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if len(password) < 10 {
		return errors.New("RELAY_ADMIN_PASSWORD must contain at least 10 characters on first start")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, "admin_password_hash", string(hash))
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/logout", s.requireAdmin(s.logout))
	mux.HandleFunc("POST /api/v1/admin/password", s.requireAdmin(s.changePassword))
	mux.HandleFunc("GET /api/v1/me", s.requireAdmin(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"name": "管理员"})
	}))
	mux.HandleFunc("GET /api/v1/dashboard", s.requireAdmin(s.dashboard))
	mux.HandleFunc("GET /api/v1/nodes", s.requireAdmin(s.listNodes))
	mux.HandleFunc("POST /api/v1/nodes", s.requireAdmin(s.createNode))
	mux.HandleFunc("PUT /api/v1/nodes/{id}", s.requireAdmin(s.updateNode))
	mux.HandleFunc("DELETE /api/v1/nodes/{id}", s.requireAdmin(s.deleteNode))
	mux.HandleFunc("GET /api/v1/lines", s.requireAdmin(s.listLines))
	mux.HandleFunc("POST /api/v1/lines", s.requireAdmin(s.saveLine))
	mux.HandleFunc("PUT /api/v1/lines/{id}", s.requireAdmin(s.saveLine))
	mux.HandleFunc("DELETE /api/v1/lines/{id}", s.requireAdmin(s.deleteLine))
	mux.HandleFunc("GET /api/v1/rules", s.requireAdmin(s.listRules))
	mux.HandleFunc("POST /api/v1/rules", s.requireAdmin(s.saveRule))
	mux.HandleFunc("POST /api/v1/rules/import", s.requireAdmin(s.importRules))
	mux.HandleFunc("POST /api/v1/rules/import-text", s.requireAdmin(s.importTextRules))
	mux.HandleFunc("PUT /api/v1/rules/{id}", s.requireAdmin(s.saveRule))
	mux.HandleFunc("DELETE /api/v1/rules/{id}", s.requireAdmin(s.deleteRule))
	mux.HandleFunc("GET /api/v1/traffic", s.requireAdmin(s.traffic))
	mux.HandleFunc("GET /api/v1/traffic/rules", s.requireAdmin(s.ruleTraffic))
	mux.HandleFunc("GET /api/v1/probes", s.requireAdmin(s.listProbes))
	mux.HandleFunc("GET /api/v1/target-probes", s.requireAdmin(s.listTargetProbes))
	mux.HandleFunc("GET /api/v1/config/export", s.requireAdmin(s.exportConfiguration))
	mux.HandleFunc("POST /api/v1/config/import", s.requireAdmin(s.importConfiguration))
	mux.HandleFunc("POST /agent/v1/sync", s.agentSync)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if s.webProxy != nil {
			s.webProxy.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
	return requestLog(s.log, mux)
}

func (s *Server) exportConfiguration(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	lines, err := s.store.ListLines(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	rules, err := s.store.ListRules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	required := map[string]bool{}
	for _, line := range lines {
		required[line.IngressNodeID] = true
		for _, id := range line.EgressNodeIDs {
			required[id] = true
		}
	}
	for _, rule := range rules {
		required[rule.IngressNodeID] = true
		required[rule.EgressNodeID] = true
	}
	references := make([]configNodeReference, 0, len(required))
	for _, node := range nodes {
		if required[node.ID] {
			references = append(references, configNodeReference{ID: node.ID, Name: node.Name, Role: node.Role})
		}
	}
	now := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60))
	backup := configurationBackup{Format: "relay-panel-configuration", SchemaVersion: 2, ExportedAt: now, Nodes: references, Lines: nonNil(lines), Rules: nonNil(rules)}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="relay-panel-config-%s.json"`, now.Format("20060102-150405")))
	writeJSON(w, http.StatusOK, backup)
}

func (s *Server) importConfiguration(w http.ResponseWriter, r *http.Request) {
	var backup configurationBackup
	if err := decodeJSON(r, &backup); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("配置文件不是有效的 Relay Panel JSON"))
		return
	}
	lines, rules, err := s.prepareConfigurationImport(r.Context(), backup)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	dryRun := r.URL.Query().Get("dry_run") == "1"
	result := configurationImportResult{DryRun: dryRun, Lines: len(lines), Rules: len(rules)}
	if !dryRun {
		result.Revision, err = s.store.ImportTopology(r.Context(), lines, rules)
		if err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("导入失败，未写入任何配置：%w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) prepareConfigurationImport(ctx context.Context, backup configurationBackup) ([]domain.Line, []domain.ForwardRule, error) {
	if backup.Format != "relay-panel-configuration" || (backup.SchemaVersion != 1 && backup.SchemaVersion != 2) {
		return nil, nil, errors.New("配置文件格式或版本不受支持")
	}
	if len(backup.Lines) > 200 || len(backup.Rules) > 2000 {
		return nil, nil, errors.New("配置文件包含过多线路或规则")
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	nodeByID := make(map[string]domain.Node, len(nodes))
	for _, node := range nodes {
		nodeByID[node.ID] = node
	}
	existingLines, err := s.store.ListLines(ctx)
	if err != nil {
		return nil, nil, err
	}
	lineByID := make(map[string]domain.Line, len(existingLines)+len(backup.Lines))
	for _, line := range existingLines {
		lineByID[line.ID] = line
	}
	importedLineIDs := make(map[string]bool, len(backup.Lines))
	for i := range backup.Lines {
		line := backup.Lines[i]
		if line.ID == "" || importedLineIDs[line.ID] {
			return nil, nil, errors.New("配置文件包含空线路 ID 或重复线路")
		}
		importedLineIDs[line.ID] = true
		line.NormalizeEngines()
		if err := s.completeLine(ctx, &line); err != nil {
			return nil, nil, fmt.Errorf("线路 %q 无法导入：%w", line.Name, err)
		}
		if err := validateLine(line); err != nil {
			return nil, nil, fmt.Errorf("线路 %q 无法导入：%w", line.Name, err)
		}
		backup.Lines[i] = line
		lineByID[line.ID] = line
	}
	existingRules, err := s.store.ListRules(ctx)
	if err != nil {
		return nil, nil, err
	}
	importedRuleIDs := make(map[string]bool, len(backup.Rules))
	for _, rule := range backup.Rules {
		if rule.ID == "" || importedRuleIDs[rule.ID] {
			return nil, nil, errors.New("配置文件包含空规则 ID 或重复规则")
		}
		importedRuleIDs[rule.ID] = true
	}
	for _, existing := range existingRules {
		if importedLineIDs[existing.LineID] && !importedRuleIDs[existing.ID] {
			return nil, nil, fmt.Errorf("线路 %q 的备份不完整，缺少当前规则 %q", lineByID[existing.LineID].Name, existing.Name)
		}
	}
	for i := range backup.Rules {
		rule := backup.Rules[i]
		if rule.BurstKBytes == 0 {
			rule.BurstKBytes = 512
		}
		if rule.LineID != "" {
			line, ok := lineByID[rule.LineID]
			if !ok || !importedLineIDs[rule.LineID] {
				return nil, nil, fmt.Errorf("规则 %q 缺少对应的线路备份", rule.Name)
			}
			rule.Mode = line.Mode
			rule.IngressNodeID = line.IngressNodeID
			rule.EgressNodeID = line.EgressNodeID
			rule.ListenAddress = line.ListenAddress
			rule.Engine = line.Engine
			rule.IngressEngine = line.IngressEngine
			rule.EgressEngine = line.EgressEngine
			if line.Mode == domain.ForwardModeExitOnly {
				rule.RelayPort = rule.ListenPort
				rule.RelayPorts = map[string]int{line.EgressNodeID: rule.ListenPort}
			}
			ports := make(map[string]int, len(line.EgressNodeIDs))
			for _, egressID := range line.EgressNodeIDs {
				ranges, rangeErr := parsePortRanges(line.RelayPortRangeFor(egressID))
				if rangeErr != nil {
					return nil, nil, rangeErr
				}
				port := rule.RelayPortFor(egressID)
				if !portInRanges(port, ranges) {
					return nil, nil, fmt.Errorf("规则 %q 在出口 %s 的中继端口不在线路端口范围内", rule.Name, egressID)
				}
				ports[egressID] = port
			}
			rule.RelayPorts = ports
			rule.RelayPort = ports[line.EgressNodeID]
		} else if err := s.completeSimpleRule(ctx, &rule); err != nil {
			return nil, nil, fmt.Errorf("规则 %q 无法导入：%w", rule.Name, err)
		}
		rule.NormalizeEngines()
		if _, ok := nodeByID[rule.IngressNodeID]; !ok {
			return nil, nil, fmt.Errorf("规则 %q 引用的入口服务器不存在", rule.Name)
		}
		if _, ok := nodeByID[rule.EgressNodeID]; !ok {
			return nil, nil, fmt.Errorf("规则 %q 引用的出口服务器不存在", rule.Name)
		}
		if err := validateRule(rule); err != nil {
			return nil, nil, fmt.Errorf("规则 %q 无法导入：%w", rule.Name, err)
		}
		backup.Rules[i] = rule
	}
	finalRuleByID := make(map[string]domain.ForwardRule, len(existingRules)+len(backup.Rules))
	for _, rule := range existingRules {
		finalRuleByID[rule.ID] = rule
	}
	for _, rule := range backup.Rules {
		finalRuleByID[rule.ID] = rule
	}
	finalRules := make([]domain.ForwardRule, 0, len(finalRuleByID))
	for _, rule := range finalRuleByID {
		finalRules = append(finalRules, rule)
	}
	for i := 0; i < len(finalRules); i++ {
		for j := i + 1; j < len(finalRules); j++ {
			a, b := finalRules[i], finalRules[j]
			if !protocolsOverlap(a.Protocol, b.Protocol) {
				continue
			}
			if a.IngressNodeID == b.IngressNodeID && a.ListenPort == b.ListenPort {
				return nil, nil, fmt.Errorf("规则 %q 与 %q 的入口端口冲突", a.Name, b.Name)
			}
			for _, egressID := range ruleEgressIDs(a, lineByID) {
				if ruleUsesEgress(b, egressID, lineByID) && a.RelayPortFor(egressID) == b.RelayPortFor(egressID) {
					return nil, nil, fmt.Errorf("规则 %q 与 %q 在出口 %s 的中继端口冲突", a.Name, b.Name, egressID)
				}
			}
		}
	}
	return backup.Lines, backup.Rules, nil
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hash, err := s.store.GetSetting(r.Context(), "admin_password_hash")
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil {
		writeError(w, http.StatusUnauthorized, errors.New("密码错误"))
		return
	}
	generation, err := s.store.GetSetting(r.Context(), "session_generation")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	expires := time.Now().Add(12 * time.Hour)
	token := s.signSession(expires, generation)
	http.SetCookie(w, &http.Cookie{Name: "relay_session", Value: token, Path: "/", Expires: expires, HttpOnly: true, Secure: s.secureCookies || requestIsHTTPS(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": expires})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "relay_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureCookies || requestIsHTTPS(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hash, err := s.store.GetSetting(r.Context(), "admin_password_hash")
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.CurrentPassword)) != nil {
		writeError(w, http.StatusUnauthorized, errors.New("当前密码错误"))
		return
	}
	if len(body.NewPassword) < 10 {
		writeError(w, http.StatusUnprocessableEntity, errors.New("新密码至少需要 10 个字符"))
		return
	}
	if body.NewPassword == body.CurrentPassword {
		writeError(w, http.StatusUnprocessableEntity, errors.New("新密码不能与当前密码相同"))
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), 12)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	generationText, err := s.store.GetSetting(r.Context(), "session_generation")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	generation, err := strconv.ParseInt(generationText, 10, 64)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("登录会话版本无效"))
		return
	}
	if err := s.store.SetSettings(r.Context(), map[string]string{
		"admin_password_hash": string(newHash),
		"session_generation":  strconv.FormatInt(generation+1, 10),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "relay_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureCookies || requestIsHTTPS(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) signSession(expires time.Time, generation string) string {
	payload := strconv.FormatInt(expires.Unix(), 10) + "." + generation
	mac := hmac.New(sha256.New, s.sessionSecret)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validSession(ctx context.Context, token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return false
	}
	generation := "1"
	payload := parts[0]
	sigIndex := 1
	if len(parts) == 3 {
		generation = parts[1]
		payload += "." + generation
		sigIndex = 2
	}
	currentGeneration, err := s.store.GetSetting(ctx, "session_generation")
	if err != nil || generation != currentGeneration {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[sigIndex])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.sessionSecret)
	_, _ = mac.Write([]byte(payload))
	return hmac.Equal(sig, mac.Sum(nil))
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("relay_session")
		if err != nil || !s.validSession(r.Context(), cookie.Value) {
			writeError(w, http.StatusUnauthorized, errors.New("请先登录"))
			return
		}
		next(w, r)
	}
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.Summary(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	d.RecentTraffic = nonNil(d.RecentTraffic)
	writeJSON(w, 200, d)
}
func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.ListNodes(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(v))
}

func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	var n domain.Node
	if err := decodeJSON(r, &n); err != nil {
		writeError(w, 400, err)
		return
	}
	if err := validateNode(n); err != nil {
		writeError(w, 422, err)
		return
	}
	n.ID = randomID("node")
	if n.PublicInterface == "" {
		n.PublicInterface = "eth0"
	}
	if n.PrivateInterface == "" {
		n.PrivateInterface = "wg0"
	}
	n.CreatedAt = time.Now().UTC()
	token := randomToken()
	hash := sha256.Sum256([]byte(token))
	if err := s.store.CreateNode(r.Context(), n, hex.EncodeToString(hash[:])); err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 201, map[string]any{"node": n, "agent_token": token})
}

func (s *Server) updateNode(w http.ResponseWriter, r *http.Request) {
	var n domain.Node
	if err := decodeJSON(r, &n); err != nil {
		writeError(w, 400, err)
		return
	}
	n.ID = r.PathValue("id")
	if err := validateNode(n); err != nil {
		writeError(w, 422, err)
		return
	}
	if err := s.store.UpdateNode(r.Context(), n); err != nil {
		writeError(w, 404, err)
		return
	}
	updated, _, _ := s.store.GetNode(r.Context(), n.ID)
	writeJSON(w, 200, updated)
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteNode(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, err)
		return
	}
	if err != nil {
		writeError(w, 409, errors.New("节点仍被规则引用，不能删除"))
		return
	}
	w.WriteHeader(204)
}

func (s *Server) listLines(w http.ResponseWriter, r *http.Request) {
	lines, err := s.store.ListLines(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(lines))
}

func (s *Server) saveLine(w http.ResponseWriter, r *http.Request) {
	var line domain.Line
	if err := decodeJSON(r, &line); err != nil {
		writeError(w, 400, err)
		return
	}
	isCreate := r.PathValue("id") == ""
	var existing domain.Line
	if isCreate {
		line.ID = randomID("line")
		line.Enabled = true
	} else {
		line.ID = r.PathValue("id")
		var err error
		existing, err = s.store.GetLine(r.Context(), line.ID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 404, err)
			return
		}
		if err != nil {
			writeError(w, 500, err)
			return
		}
		line.CreatedAt = existing.CreatedAt
		if line.ActiveEgressNodeID == "" {
			for _, candidate := range line.EgressNodeIDs {
				if candidate == existing.ActiveEgressNodeID {
					line.ActiveEgressNodeID = existing.ActiveEgressNodeID
					break
				}
			}
		}
	}
	if line.Mode == "" {
		line.Mode = domain.ForwardModeDualManaged
	}
	line.NormalizeEngines()
	if err := s.completeLine(r.Context(), &line); err != nil {
		writeError(w, 422, err)
		return
	}
	if err := validateLine(line); err != nil {
		writeError(w, 422, err)
		return
	}
	plannedRules, err := s.rulesForLineUpdate(r.Context(), line)
	if err != nil {
		writeError(w, 422, err)
		return
	}
	saved, err := s.store.SaveLineRules(r.Context(), line, plannedRules)
	if err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 200, saved)
}

func (s *Server) rulesForLineUpdate(ctx context.Context, line domain.Line) ([]domain.ForwardRule, error) {
	normalizeLineEgressIDs(&line)
	all, err := s.store.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	lines, err := s.store.ListLines(ctx)
	if err != nil {
		return nil, err
	}
	lineByID := make(map[string]domain.Line, len(lines)+1)
	for _, existingLine := range lines {
		lineByID[existingLine.ID] = existingLine
	}
	lineByID[line.ID] = line
	owned := make(map[string]bool)
	for _, rule := range all {
		if rule.LineID == line.ID {
			owned[rule.ID] = true
		}
	}
	occupied := make([]domain.ForwardRule, 0, len(all))
	for _, rule := range all {
		if !owned[rule.ID] {
			occupied = append(occupied, rule)
		}
	}
	planned := make([]domain.ForwardRule, 0, len(owned))
	for _, rule := range all {
		if !owned[rule.ID] {
			continue
		}
		rule.Mode = line.Mode
		rule.IngressNodeID = line.IngressNodeID
		rule.EgressNodeID = line.EgressNodeID
		rule.ListenAddress = line.ListenAddress
		rule.Engine = line.Engine
		rule.IngressEngine = line.IngressEngine
		rule.EgressEngine = line.EgressEngine
		rule.NormalizeEngines()
		for _, other := range occupied {
			if other.IngressNodeID == rule.IngressNodeID && other.ListenPort == rule.ListenPort && protocolsOverlap(other.Protocol, rule.Protocol) {
				return nil, fmt.Errorf("入口服务器的端口 %d 已被规则 %q 使用", rule.ListenPort, other.Name)
			}
		}
		if line.Mode == domain.ForwardModeExitOnly {
			ranges, rangeErr := parsePortRanges(line.RelayPortRangeFor(line.EgressNodeID))
			if rangeErr != nil {
				return nil, rangeErr
			}
			rule.RelayPorts = map[string]int{line.EgressNodeID: rule.ListenPort}
			rule.RelayPort = rule.ListenPort
			if !portInRanges(rule.RelayPort, ranges) {
				return nil, fmt.Errorf("规则 %q 的接入端口 %d 不在出口可用端口范围 %s 内", rule.Name, rule.RelayPort, displayPortRange(line.RelayPortRangeFor(line.EgressNodeID)))
			}
		} else {
			if err := allocateRulePortsForLine(&rule, line, occupied, lineByID); err != nil {
				return nil, fmt.Errorf("规则 %q：%w", rule.Name, err)
			}
		}
		if err := validateRule(rule); err != nil {
			return nil, fmt.Errorf("规则 %q 无法迁移：%w", rule.Name, err)
		}
		planned = append(planned, rule)
		occupied = append(occupied, rule)
	}
	return planned, nil
}

func (s *Server) deleteLine(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteLine(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, err)
		return
	}
	if err != nil {
		writeError(w, 409, errors.New("线路下仍有转发规则，请先删除规则"))
		return
	}
	w.WriteHeader(204)
}

func (s *Server) completeLine(ctx context.Context, line *domain.Line) error {
	normalizeLineEgressIDs(line)
	line.NormalizeEgressPortRanges()
	egress, _, err := s.store.GetNode(ctx, line.EgressNodeID)
	if err != nil || (egress.Role != domain.NodeRoleEgress && egress.Role != domain.NodeRoleBoth) {
		return errors.New("请选择有效的出口服务器")
	}
	if line.Mode == domain.ForwardModeExitOnly {
		line.EgressNodeIDs = []string{line.EgressNodeID}
		line.ActiveEgressNodeID = line.EgressNodeID
		line.FailoverEnabled = false
		line.IngressNodeID = line.EgressNodeID
		if line.ListenAddress == "" || line.ListenAddress == "0.0.0.0" {
			line.ListenAddress = egress.PrivateAddress
		}
		if line.ListenAddress == "" {
			return errors.New("出口服务器还没有配置内网 IP")
		}
		return nil
	}
	ingress, _, err := s.store.GetNode(ctx, line.IngressNodeID)
	if err != nil || (ingress.Role != domain.NodeRoleIngress && ingress.Role != domain.NodeRoleBoth) {
		return errors.New("请选择有效的入口服务器")
	}
	if ingress.ID == egress.ID {
		return errors.New("双端托管需要不同的入口和出口服务器")
	}
	if egress.PrivateAddress == "" {
		return errors.New("出口服务器还没有配置内网 IP")
	}
	for _, egressID := range line.EgressNodeIDs {
		candidate, _, err := s.store.GetNode(ctx, egressID)
		if err != nil || (candidate.Role != domain.NodeRoleEgress && candidate.Role != domain.NodeRoleBoth) {
			return fmt.Errorf("备用出口 %s 不存在或用途不是出口", egressID)
		}
		if candidate.ID == ingress.ID {
			return errors.New("入口服务器不能同时作为该线路的出口")
		}
		if candidate.PrivateAddress == "" {
			return fmt.Errorf("出口服务器 %s 还没有配置内网 IP", candidate.Name)
		}
	}
	if line.FailoverEnabled && len(line.EgressNodeIDs) < 2 {
		return errors.New("自动故障切换至少需要两个出口服务器")
	}
	if line.ListenAddress == "" {
		line.ListenAddress = "0.0.0.0"
	}
	return nil
}

func normalizeLineEgressIDs(line *domain.Line) {
	seen := map[string]bool{}
	ids := make([]string, 0, len(line.EgressNodeIDs)+1)
	if line.EgressNodeID != "" {
		seen[line.EgressNodeID] = true
		ids = append(ids, line.EgressNodeID)
	}
	for _, id := range line.EgressNodeIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	line.EgressNodeIDs = ids
	line.NormalizeEgressPortRanges()
	for id := range line.EgressPortRanges {
		if !seen[id] {
			delete(line.EgressPortRanges, id)
		}
	}
	if !seen[line.ActiveEgressNodeID] {
		line.ActiveEgressNodeID = line.EgressNodeID
	}
}
func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.ListRules(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(v))
}

func (s *Server) saveRule(w http.ResponseWriter, r *http.Request) {
	var rule domain.ForwardRule
	if err := decodeJSON(r, &rule); err != nil {
		writeError(w, 400, err)
		return
	}
	isCreate := r.PathValue("id") == ""
	var existing domain.ForwardRule
	if id := r.PathValue("id"); id != "" {
		rule.ID = id
		var err error
		existing, err = s.store.GetRule(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 404, err)
			return
		}
		if err != nil {
			writeError(w, 500, err)
			return
		}
		if rule.RelayPort == 0 {
			rule.RelayPort = existing.RelayPort
		}
		if len(rule.RelayPorts) == 0 {
			rule.RelayPorts = existing.RelayPorts
		}
		rule.CreatedAt = existing.CreatedAt
	} else {
		rule.ID = randomID("rule")
	}
	if isCreate {
		rule.Enabled = true
	}
	if rule.Mode == "" {
		rule.Mode = domain.ForwardModeDualManaged
	}
	if rule.Protocol == "" {
		rule.Protocol = "both"
	}
	rule.NormalizeEngines()
	if rule.BurstKBytes == 0 {
		rule.BurstKBytes = 512
	}
	if rule.TargetPort == 0 {
		rule.TargetPort = rule.ListenPort
	}
	if rule.Name == "" && rule.TargetHost != "" {
		rule.Name = fmt.Sprintf("%s:%d", rule.TargetHost, rule.TargetPort)
	}
	if rule.LineID != "" {
		line, err := s.store.GetLine(r.Context(), rule.LineID)
		if err != nil || !line.Enabled {
			writeError(w, 422, errors.New("请选择可用线路"))
			return
		}
		rule.Mode = line.Mode
		rule.IngressNodeID = line.IngressNodeID
		rule.EgressNodeID = line.EgressNodeID
		rule.ListenAddress = line.ListenAddress
		rule.Engine = line.Engine
		rule.IngressEngine = line.IngressEngine
		rule.EgressEngine = line.EgressEngine
		rule.NormalizeEngines()
	}
	if err := s.completeSimpleRule(r.Context(), &rule); err != nil {
		writeError(w, 422, err)
		return
	}
	if err := validateRule(rule); err != nil {
		writeError(w, 422, err)
		return
	}
	if err := s.ensureRulePortsAvailable(r.Context(), rule); err != nil {
		writeError(w, 409, err)
		return
	}
	if _, _, err := s.store.GetNode(r.Context(), rule.IngressNodeID); err != nil {
		writeError(w, 422, errors.New("入口节点不存在"))
		return
	}
	if _, _, err := s.store.GetNode(r.Context(), rule.EgressNodeID); err != nil {
		writeError(w, 422, errors.New("出口节点不存在"))
		return
	}
	saved, err := s.store.SaveRule(r.Context(), rule)
	if err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 200, saved)
}

func (s *Server) importRules(w http.ResponseWriter, r *http.Request) {
	var batch ruleBatchImport
	if err := decodeJSON(r, &batch); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("批量规则文件不是有效的 Relay Panel JSON"))
		return
	}
	rules, err := s.prepareRuleBatch(r.Context(), batch)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	dryRun := r.URL.Query().Get("dry_run") == "1"
	result := ruleBatchImportResult{DryRun: dryRun, Rules: len(rules)}
	if !dryRun {
		result.Revision, err = s.store.ImportRules(r.Context(), rules)
		if err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("批量导入失败，未写入任何规则：%w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) importTextRules(w http.ResponseWriter, r *http.Request) {
	const maxTextBytes = 1024 * 1024
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxTextBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("无法读取 TXT 批量规则文件"))
		return
	}
	if len(payload) > maxTextBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errors.New("批量规则文件不能超过 1 MB"))
		return
	}
	items, err := parseRuleBatchText(string(payload))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	batch := ruleBatchImport{
		Format: "relay-panel-rule-batch", SchemaVersion: 1,
		LineID: strings.TrimSpace(r.URL.Query().Get("line_id")), Rules: items,
	}
	rules, err := s.prepareRuleBatch(r.Context(), batch)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	dryRun := r.URL.Query().Get("dry_run") == "1"
	result := ruleBatchImportResult{DryRun: dryRun, Rules: len(rules)}
	if !dryRun {
		result.Revision, err = s.store.ImportRules(r.Context(), rules)
		if err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("批量添加失败，未写入任何规则：%w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func parseRuleBatchText(text string) ([]ruleBatchItem, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	items := make([]ruleBatchItem, 0, len(lines))
	for lineNumber, raw := range lines {
		line := strings.TrimSpace(raw)
		if comment := strings.IndexByte(line, '#'); comment >= 0 {
			line = strings.TrimSpace(line[:comment])
		}
		if line == "" {
			continue
		}
		fields := strings.Fields(strings.ReplaceAll(line, ",", " "))
		if len(fields) < 3 {
			return nil, fmt.Errorf("TXT 第 %d 行格式错误：应填写入口端口、落地 IP、落地端口", lineNumber+1)
		}
		listenPort, err := strconv.Atoi(fields[0])
		if err != nil || listenPort < 1 || listenPort > 65535 {
			return nil, fmt.Errorf("TXT 第 %d 行入口端口无效", lineNumber+1)
		}
		targetPort, err := strconv.Atoi(fields[2])
		if err != nil || targetPort < 1 || targetPort > 65535 {
			return nil, fmt.Errorf("TXT 第 %d 行落地端口无效", lineNumber+1)
		}
		protocol := "both"
		nameStart := 3
		if len(fields) > 3 {
			candidate := strings.ToLower(fields[3])
			if candidate == "tcp" || candidate == "udp" || candidate == "both" {
				protocol = candidate
				nameStart = 4
			}
		}
		name := ""
		if len(fields) > nameStart {
			name = strings.Join(fields[nameStart:], " ")
		}
		items = append(items, ruleBatchItem{
			Name: name, Protocol: protocol, ListenPort: listenPort,
			TargetHost: fields[1], TargetPort: targetPort,
		})
	}
	if len(items) == 0 {
		return nil, errors.New("TXT 中没有可导入的规则")
	}
	return items, nil
}

func (s *Server) prepareRuleBatch(ctx context.Context, batch ruleBatchImport) ([]domain.ForwardRule, error) {
	if batch.Format != "relay-panel-rule-batch" || batch.SchemaVersion != 1 {
		return nil, errors.New("批量规则文件格式或版本不受支持")
	}
	if len(batch.Rules) == 0 {
		return nil, errors.New("批量规则文件中没有规则")
	}
	if len(batch.Rules) > 500 {
		return nil, errors.New("一次最多导入 500 条规则")
	}
	line, err := s.store.GetLine(ctx, strings.TrimSpace(batch.LineID))
	if err != nil || !line.Enabled {
		return nil, errors.New("请选择可用线路")
	}
	lines, err := s.store.ListLines(ctx)
	if err != nil {
		return nil, err
	}
	lineByID := make(map[string]domain.Line, len(lines))
	for _, existingLine := range lines {
		lineByID[existingLine.ID] = existingLine
	}
	lineByID[line.ID] = line
	occupied, err := s.store.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	prepared := make([]domain.ForwardRule, 0, len(batch.Rules))
	for index, item := range batch.Rules {
		protocol := strings.ToLower(strings.TrimSpace(item.Protocol))
		if protocol == "" {
			protocol = "both"
		}
		targetPort := item.TargetPort
		if targetPort == 0 {
			targetPort = item.ListenPort
		}
		name := strings.TrimSpace(item.Name)
		if name == "" && strings.TrimSpace(item.TargetHost) != "" {
			name = fmt.Sprintf("%s:%d", strings.TrimSpace(item.TargetHost), targetPort)
		}
		enabled := true
		if item.Enabled != nil {
			enabled = *item.Enabled
		}
		burst := item.BurstKBytes
		if burst == 0 {
			burst = 512
		}
		rule := domain.ForwardRule{
			ID: randomID("rule"), LineID: line.ID, Mode: line.Mode, Name: name, Protocol: protocol,
			IngressNodeID: line.IngressNodeID, EgressNodeID: line.EgressNodeID, ListenAddress: line.ListenAddress,
			ListenPort: item.ListenPort, TargetHost: strings.TrimSpace(item.TargetHost), TargetPort: targetPort,
			Engine: line.Engine, IngressEngine: line.IngressEngine, EgressEngine: line.EgressEngine,
			UploadMbps: item.UploadMbps, DownloadMbps: item.DownloadMbps, BurstKBytes: burst, Enabled: enabled,
		}
		if line.Mode == domain.ForwardModeExitOnly {
			ranges, rangeErr := parsePortRanges(line.RelayPortRangeFor(line.EgressNodeID))
			if rangeErr != nil {
				return nil, rangeErr
			}
			rule.RelayPort = rule.ListenPort
			rule.RelayPorts = map[string]int{line.EgressNodeID: rule.ListenPort}
			if !portInRanges(rule.ListenPort, ranges) {
				return nil, fmt.Errorf("第 %d 条规则：接入端口 %d 不在出口可用端口范围 %s 内", index+1, rule.ListenPort, displayPortRange(line.RelayPortRangeFor(line.EgressNodeID)))
			}
		} else if err := allocateRulePortsForLine(&rule, line, occupied, lineByID); err != nil {
			return nil, fmt.Errorf("第 %d 条规则：%w", index+1, err)
		}
		if err := validateRule(rule); err != nil {
			return nil, fmt.Errorf("第 %d 条规则：%w", index+1, err)
		}
		if err := ensureRulePortsAvailableAgainst(rule, occupied, lineByID); err != nil {
			return nil, fmt.Errorf("第 %d 条规则：%w", index+1, err)
		}
		prepared = append(prepared, rule)
		occupied = append(occupied, rule)
	}
	return prepared, nil
}

func (s *Server) ensureRulePortsAvailable(ctx context.Context, rule domain.ForwardRule) error {
	rules, err := s.store.ListRules(ctx)
	if err != nil {
		return err
	}
	lines, err := s.store.ListLines(ctx)
	if err != nil {
		return err
	}
	lineByID := make(map[string]domain.Line, len(lines))
	for _, line := range lines {
		lineByID[line.ID] = line
	}
	return ensureRulePortsAvailableAgainst(rule, rules, lineByID)
}

func ensureRulePortsAvailableAgainst(rule domain.ForwardRule, rules []domain.ForwardRule, lineByID map[string]domain.Line) error {
	targetEgresses := ruleEgressIDs(rule, lineByID)
	for _, other := range rules {
		if other.ID == rule.ID || !protocolsOverlap(other.Protocol, rule.Protocol) {
			continue
		}
		if other.IngressNodeID == rule.IngressNodeID && other.ListenPort == rule.ListenPort {
			return fmt.Errorf("入口端口 %d 已被规则 %q 占用", rule.ListenPort, other.Name)
		}
		for _, egressID := range targetEgresses {
			if ruleUsesEgress(other, egressID, lineByID) && other.RelayPortFor(egressID) == rule.RelayPortFor(egressID) {
				return fmt.Errorf("出口 %s 的中继端口 %d 已被规则 %q 占用", egressID, rule.RelayPortFor(egressID), other.Name)
			}
		}
	}
	return nil
}

func ruleEgressIDs(rule domain.ForwardRule, lines map[string]domain.Line) []string {
	if line, ok := lines[rule.LineID]; ok && len(line.EgressNodeIDs) > 0 {
		return line.EgressNodeIDs
	}
	return []string{rule.EgressNodeID}
}

func egressSetsOverlap(a, b []string) bool {
	seen := make(map[string]bool, len(a))
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if seen[id] {
			return true
		}
	}
	return false
}

func ruleUsesEgress(rule domain.ForwardRule, egressID string, lines map[string]domain.Line) bool {
	for _, id := range ruleEgressIDs(rule, lines) {
		if id == egressID {
			return true
		}
	}
	return false
}

func relayPortUsedOnEgress(rules []domain.ForwardRule, lines map[string]domain.Line, egressID string, port int, protocol, ignoreID string) bool {
	for _, rule := range rules {
		if rule.ID != ignoreID && protocolsOverlap(rule.Protocol, protocol) && ruleUsesEgress(rule, egressID, lines) && rule.RelayPortFor(egressID) == port {
			return true
		}
	}
	return false
}

func allocateRelayPortOnEgress(ranges []portInterval, rules []domain.ForwardRule, lines map[string]domain.Line, egressID, protocol, ignoreID string) int {
	for _, interval := range ranges {
		start := interval.first
		if interval.first <= 30000 && interval.last >= 30000 {
			start = 30000
		}
		for port := start; port <= interval.last; port++ {
			if !relayPortUsedOnEgress(rules, lines, egressID, port, protocol, ignoreID) {
				return port
			}
		}
		for port := interval.first; port < start; port++ {
			if !relayPortUsedOnEgress(rules, lines, egressID, port, protocol, ignoreID) {
				return port
			}
		}
	}
	return 0
}

func allocateRulePortsForLine(rule *domain.ForwardRule, line domain.Line, rules []domain.ForwardRule, lines map[string]domain.Line) error {
	line.NormalizeEgressPortRanges()
	ports := make(map[string]int, len(line.EgressNodeIDs))
	for _, egressID := range line.EgressNodeIDs {
		ranges, err := parsePortRanges(line.RelayPortRangeFor(egressID))
		if err != nil {
			return fmt.Errorf("出口 %s 的端口范围无效：%w", egressID, err)
		}
		port := rule.RelayPorts[egressID]
		if port == 0 {
			// Preserve the shared port used by rules created before per-egress
			// allocation whenever it is valid on this exit.
			port = rule.RelayPort
		}
		if !portInRanges(port, ranges) || relayPortUsedOnEgress(rules, lines, egressID, port, rule.Protocol, rule.ID) {
			port = allocateRelayPortOnEgress(ranges, rules, lines, egressID, rule.Protocol, rule.ID)
		}
		if port == 0 {
			return fmt.Errorf("出口 %s 的可用端口范围 %s 已用完", egressID, displayPortRange(line.RelayPortRangeFor(egressID)))
		}
		ports[egressID] = port
	}
	rule.RelayPorts = ports
	rule.RelayPort = ports[line.EgressNodeID]
	return nil
}

func relayPortUsedAcross(rules []domain.ForwardRule, lines map[string]domain.Line, egressIDs []string, port int, protocol, ignoreID string) bool {
	for _, rule := range rules {
		if rule.ID == ignoreID || !protocolsOverlap(rule.Protocol, protocol) {
			continue
		}
		for _, egressID := range egressIDs {
			if ruleUsesEgress(rule, egressID, lines) && rule.RelayPortFor(egressID) == port {
				return true
			}
		}
	}
	return false
}

func allocateRelayPortAcross(ranges []portInterval, rules []domain.ForwardRule, lines map[string]domain.Line, egressIDs []string, protocol, ignoreID string) int {
	for _, interval := range ranges {
		start := interval.first
		if interval.first <= 30000 && interval.last >= 30000 {
			start = 30000
		}
		for port := start; port <= interval.last; port++ {
			if !relayPortUsedAcross(rules, lines, egressIDs, port, protocol, ignoreID) {
				return port
			}
		}
		for port := interval.first; port < start; port++ {
			if !relayPortUsedAcross(rules, lines, egressIDs, port, protocol, ignoreID) {
				return port
			}
		}
	}
	return 0
}

func (s *Server) completeSimpleRule(ctx context.Context, rule *domain.ForwardRule) error {
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return err
	}
	portRange := ""
	var selectedLine *domain.Line
	if rule.LineID != "" {
		line, err := s.store.GetLine(ctx, rule.LineID)
		if err != nil {
			return errors.New("线路不存在")
		}
		line.NormalizeEgressPortRanges()
		portRange = line.RelayPortRangeFor(line.EgressNodeID)
		selectedLine = &line
	}
	ranges, err := parsePortRanges(portRange)
	if err != nil {
		return err
	}
	if rule.Mode == domain.ForwardModeExitOnly {
		if rule.EgressNodeID == "" {
			for _, node := range nodes {
				if node.Role == domain.NodeRoleEgress || node.Role == domain.NodeRoleBoth {
					rule.EgressNodeID = node.ID
					break
				}
			}
		}
		if rule.EgressNodeID == "" {
			return errors.New("请选择安装了 Agent 的出口服务器")
		}
		var egress domain.Node
		for _, node := range nodes {
			if node.ID == rule.EgressNodeID {
				egress = node
				break
			}
		}
		if egress.ID == "" || (egress.Role != domain.NodeRoleEgress && egress.Role != domain.NodeRoleBoth) {
			return errors.New("仅出口接管模式必须选择出口服务器")
		}
		if rule.ListenAddress == "" || rule.ListenAddress == "0.0.0.0" {
			rule.ListenAddress = egress.PrivateAddress
		}
		rule.IngressNodeID = rule.EgressNodeID
		rule.RelayPort = rule.ListenPort
		rule.RelayPorts = map[string]int{rule.EgressNodeID: rule.ListenPort}
		if !portInRanges(rule.RelayPort, ranges) {
			return fmt.Errorf("接入端口 %d 不在出口可用端口范围 %s 内", rule.RelayPort, displayPortRange(portRange))
		}
		return nil
	}
	if rule.ListenAddress == "" {
		rule.ListenAddress = "0.0.0.0"
	}
	if rule.IngressNodeID == "" {
		for _, node := range nodes {
			if node.Role == domain.NodeRoleIngress || node.Role == domain.NodeRoleBoth {
				rule.IngressNodeID = node.ID
				break
			}
		}
	}
	if rule.EgressNodeID == "" {
		for _, node := range nodes {
			if node.ID != rule.IngressNodeID && (node.Role == domain.NodeRoleEgress || node.Role == domain.NodeRoleBoth) {
				rule.EgressNodeID = node.ID
				break
			}
		}
	}
	if rule.IngressNodeID == "" || rule.EgressNodeID == "" {
		return errors.New("请先让一台入口节点和一台出口节点上线")
	}
	rules, err := s.store.ListRules(ctx)
	if err != nil {
		return err
	}
	if selectedLine != nil {
		lines, err := s.store.ListLines(ctx)
		if err != nil {
			return err
		}
		lineByID := make(map[string]domain.Line, len(lines))
		for _, line := range lines {
			lineByID[line.ID] = line
		}
		lineByID[selectedLine.ID] = *selectedLine
		if err := allocateRulePortsForLine(rule, *selectedLine, rules, lineByID); err != nil {
			return err
		}
	} else if rule.RelayPort == 0 || !portInRanges(rule.RelayPort, ranges) || relayPortUsed(rules, rule.EgressNodeID, rule.RelayPort, rule.Protocol, rule.ID) {
		rule.RelayPort = allocateRelayPort(ranges, rules, rule.EgressNodeID, rule.Protocol, rule.ID)
	}
	if rule.RelayPort == 0 {
		return errors.New("出口可用端口范围内没有剩余的中继端口")
	}
	if selectedLine == nil {
		rule.RelayPorts = map[string]int{rule.EgressNodeID: rule.RelayPort}
	}
	return nil
}

type portInterval struct{ first, last int }

func parsePortRanges(spec string) ([]portInterval, error) {
	spec = strings.TrimSpace(strings.ReplaceAll(spec, "，", ","))
	if spec == "" {
		return []portInterval{{first: 1, last: 65535}}, nil
	}
	var ranges []portInterval
	for _, raw := range strings.Split(spec, ",") {
		part := strings.TrimSpace(raw)
		if part == "" {
			return nil, errors.New("出口端口范围格式错误，请使用 20000-20999,25000")
		}
		bounds := strings.Split(part, "-")
		if len(bounds) > 2 {
			return nil, fmt.Errorf("出口端口范围 %q 格式错误", part)
		}
		first, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
		if err != nil {
			return nil, fmt.Errorf("出口端口 %q 不是有效数字", part)
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err != nil {
				return nil, fmt.Errorf("出口端口范围 %q 格式错误", part)
			}
		}
		if first < 1 || last > 65535 || first > last {
			return nil, fmt.Errorf("出口端口范围 %q 必须在 1-65535 内且起始端口不大于结束端口", part)
		}
		ranges = append(ranges, portInterval{first: first, last: last})
	}
	return ranges, nil
}

func portInRanges(port int, ranges []portInterval) bool {
	for _, item := range ranges {
		if port >= item.first && port <= item.last {
			return true
		}
	}
	return false
}

func protocolsOverlap(a, b string) bool { return a == "both" || b == "both" || a == b }

func relayPortUsed(rules []domain.ForwardRule, nodeID string, port int, protocol, excludeID string) bool {
	for _, existing := range rules {
		if existing.ID != excludeID && existing.EgressNodeID == nodeID && existing.RelayPortFor(nodeID) == port && protocolsOverlap(existing.Protocol, protocol) {
			return true
		}
	}
	return false
}

func allocateRelayPort(ranges []portInterval, rules []domain.ForwardRule, nodeID, protocol, excludeID string) int {
	for _, item := range ranges {
		start := item.first
		if item.first <= 30000 && item.last >= 30000 {
			start = 30000
		}
		for candidate := start; candidate <= item.last; candidate++ {
			if !relayPortUsed(rules, nodeID, candidate, protocol, excludeID) {
				return candidate
			}
		}
		for candidate := item.first; candidate < start; candidate++ {
			if !relayPortUsed(rules, nodeID, candidate, protocol, excludeID) {
				return candidate
			}
		}
	}
	return 0
}

func displayPortRange(spec string) string {
	if strings.TrimSpace(spec) == "" {
		return "不限制"
	}
	return spec
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteRule(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, err)
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) traffic(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	ruleID := r.URL.Query().Get("rule_id")
	now := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60))
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var points []domain.TrafficPoint
	var err error
	switch period {
	case "week":
		weekday := (int(now.Weekday()) + 6) % 7
		points, err = s.store.DailyTraffic(r.Context(), ruleID, day.AddDate(0, 0, -weekday))
	case "month":
		points, err = s.store.DailyTraffic(r.Context(), ruleID, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()))
	case "quarter":
		quarterMonth := time.Month(((int(now.Month()) - 1) / 3 * 3) + 1)
		points, err = s.store.DailyTraffic(r.Context(), ruleID, time.Date(now.Year(), quarterMonth, 1, 0, 0, 0, 0, now.Location()))
	default:
		points, err = s.store.Traffic(r.Context(), ruleID, day)
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(points))
}

func (s *Server) ruleTraffic(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.RuleTrafficSummaries(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(items))
}

func (s *Server) listProbes(w http.ResponseWriter, r *http.Request) {
	probes, err := s.store.ListLinkProbes(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(probes))
}

func (s *Server) listTargetProbes(w http.ResponseWriter, r *http.Request) {
	probes, err := s.store.ListTargetProbes(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(probes))
}

func (s *Server) agentSync(w http.ResponseWriter, r *http.Request) {
	var req domain.SyncRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	node, hash, err := s.store.GetNode(r.Context(), req.NodeID)
	if err != nil {
		writeError(w, 401, errors.New("未知节点"))
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	sum := sha256.Sum256([]byte(token))
	expected, err := hex.DecodeString(hash)
	if err != nil || !hmac.Equal(sum[:], expected) {
		writeError(w, 401, errors.New("Agent 凭据无效"))
		return
	}
	if req.ApplyStatus == "" {
		req.ApplyStatus = "normal"
	}
	if address := requestPublicAddress(r); address != "" {
		req.Network.PublicAddress = address
	}
	if err := s.store.UpdateHeartbeat(r.Context(), req.NodeID, req.AgentVersion, req.ApplyStatus, req.ApplyError, req.AppliedRevision, req.Network); err != nil {
		writeError(w, 500, err)
		return
	}
	if refreshed, refreshedHash, refreshErr := s.store.GetNode(r.Context(), req.NodeID); refreshErr == nil {
		node, hash = refreshed, refreshedHash
	}
	if len(req.Traffic) > 0 {
		if err := s.store.AddTraffic(r.Context(), req.NodeID, req.Traffic); err != nil {
			writeError(w, 500, err)
			return
		}
	}
	if len(req.Probes) > 0 {
		if err := s.store.UpsertLinkProbes(r.Context(), req.NodeID, req.Probes); err != nil {
			writeError(w, 500, err)
			return
		}
	}
	if len(req.TargetProbes) > 0 {
		if err := s.store.UpsertTargetProbes(r.Context(), req.NodeID, req.TargetProbes); err != nil {
			writeError(w, 500, err)
			return
		}
	}
	if _, err := s.store.ReconcileFailover(r.Context(), time.Now().UTC()); err != nil {
		writeError(w, 500, err)
		return
	}
	revision, err := s.store.Revision(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	deployments, err := s.store.DeploymentsForNode(r.Context(), req.NodeID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	peerIDs := map[string]bool{}
	for _, d := range deployments {
		peerIDs[d.Rule.IngressNodeID] = true
		peerIDs[d.Rule.EgressNodeID] = true
	}
	delete(peerIDs, req.NodeID)
	peers := make([]domain.Node, 0, len(peerIDs))
	for id := range peerIDs {
		peer, _, err := s.store.GetNode(r.Context(), id)
		if err == nil {
			peers = append(peers, peer)
		}
	}
	probeTargets, err := s.store.ProbeTargetsForIngress(r.Context(), req.NodeID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	node.Status = "online"
	node.LastSeenAt = time.Now().UTC()
	writeJSON(w, 200, domain.SyncResponse{Revision: revision, GeneratedAt: time.Now().UTC(), Node: node, Peers: peers, ProbeTargets: probeTargets, Deployments: deployments})
}

func requestPublicAddress(r *http.Request) string {
	candidates := []string{r.RemoteAddr}
	if trustedProxyRequest(r) {
		candidates = append([]string{strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0], r.Header.Get("X-Real-IP")}, candidates...)
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if host, _, err := net.SplitHostPort(candidate); err == nil {
			candidate = host
		}
		ip := net.ParseIP(candidate)
		if ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
			return ip.String()
		}
	}
	return ""
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return trustedProxyRequest(r) && strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https")
}

func trustedProxyRequest(r *http.Request) bool {
	host := strings.TrimSpace(r.RemoteAddr)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func validateNode(n domain.Node) error {
	if strings.TrimSpace(n.Name) == "" {
		return errors.New("节点名称不能为空")
	}
	if n.Role != domain.NodeRoleIngress && n.Role != domain.NodeRoleEgress && n.Role != domain.NodeRoleBoth {
		return errors.New("节点角色无效")
	}
	return nil
}
func validateLine(line domain.Line) error {
	line.NormalizeEngines()
	line.NormalizeEgressPortRanges()
	if strings.TrimSpace(line.Name) == "" {
		return errors.New("线路名称不能为空")
	}
	if line.Mode != domain.ForwardModeDualManaged && line.Mode != domain.ForwardModeExitOnly {
		return errors.New("线路模式无效")
	}
	if line.IngressNodeID == "" || line.EgressNodeID == "" {
		return errors.New("请选择线路服务器")
	}
	if line.Mode == domain.ForwardModeDualManaged && line.IngressNodeID == line.EgressNodeID {
		return errors.New("双端托管需要不同的入口和出口服务器")
	}
	if line.IngressEngine != "nftables" && line.IngressEngine != "realm" {
		return errors.New("入口引擎必须为 nftables 或 realm")
	}
	if line.EgressEngine != "nftables" && line.EgressEngine != "realm" {
		return errors.New("出口引擎必须为 nftables 或 realm")
	}
	for _, egressID := range line.EgressNodeIDs {
		if _, err := parsePortRanges(line.RelayPortRangeFor(egressID)); err != nil {
			return fmt.Errorf("出口 %s 的端口范围无效：%w", egressID, err)
		}
	}
	return nil
}
func validateRule(r domain.ForwardRule) error {
	r.NormalizeEngines()
	if r.Mode != domain.ForwardModeDualManaged && r.Mode != domain.ForwardModeExitOnly {
		return errors.New("转发模式无效")
	}
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("规则名称不能为空")
	}
	if r.IngressNodeID == "" || r.EgressNodeID == "" {
		return errors.New("必须选择入口和出口节点")
	}
	if r.Mode == domain.ForwardModeDualManaged && r.IngressNodeID == r.EgressNodeID {
		return errors.New("当前版本要求入口和出口为不同节点")
	}
	if r.Protocol != "tcp" && r.Protocol != "udp" && r.Protocol != "both" {
		return errors.New("协议必须为 tcp、udp 或 both")
	}
	if r.IngressEngine != "nftables" && r.IngressEngine != "realm" {
		return errors.New("入口引擎必须为 nftables 或 realm")
	}
	if r.EgressEngine != "nftables" && r.EgressEngine != "realm" {
		return errors.New("出口引擎必须为 nftables 或 realm")
	}
	for _, port := range []int{r.ListenPort, r.RelayPort, r.TargetPort} {
		if port < 1 || port > 65535 {
			return errors.New("端口必须在 1-65535 之间")
		}
	}
	if r.TargetHost == "" {
		return errors.New("目标地址不能为空")
	}
	if r.EgressEngine == "nftables" {
		ip := net.ParseIP(strings.TrimSpace(r.TargetHost))
		if ip == nil || ip.To4() == nil {
			return errors.New("nftables 的落地地址必须是 IPv4；域名请使用 Realm")
		}
	}
	if r.UploadMbps < 0 || r.DownloadMbps < 0 {
		return errors.New("限速不能为负数")
	}
	return nil
}

func randomID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}
func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func requestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}
