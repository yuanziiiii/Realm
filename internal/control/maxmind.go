package control

import (
	"archive/zip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"relaypanel/internal/domain"
	"relaypanel/internal/store"
)

const (
	maxMindDownloadURL = "https://download.maxmind.com/geoip/databases/GeoLite2-City-CSV/download?suffix=zip"
	maxMindMaxArchive  = 128 << 20
)

var maxMindVersionPattern = regexp.MustCompile(`(?i)GeoLite2-City-CSV[_-]([0-9]{8})\.zip`)

type maxMindConfigRequest struct {
	AccountID  string `json:"account_id"`
	LicenseKey string `json:"license_key"`
	AutoUpdate bool   `json:"auto_update"`
}

type maxMindLocation struct {
	province string
	city     string
}

func settingTime(st *store.Store, ctx context.Context, key string) time.Time {
	value, err := st.GetSetting(ctx, key)
	if err != nil {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}

func settingValue(st *store.Store, ctx context.Context, key string) string {
	value, _ := st.GetSetting(ctx, key)
	return value
}

func (s *Server) maxMindStatus(ctx context.Context) (domain.MaxMindStatus, error) {
	accountID := strings.TrimSpace(settingValue(s.store, ctx, "maxmind_account_id"))
	license := settingValue(s.store, ctx, "maxmind_license_key")
	ranges, err := s.store.MaxMindRangeCount(ctx)
	if err != nil {
		return domain.MaxMindStatus{}, err
	}
	return domain.MaxMindStatus{
		Configured:    accountID != "" && license != "",
		AccountID:     accountID,
		HasLicenseKey: license != "",
		AutoUpdate:    settingValue(s.store, ctx, "maxmind_auto_update") == "true",
		Updating:      s.maxMindUpdating.Load(),
		Ranges:        ranges,
		Version:       settingValue(s.store, ctx, "maxmind_version"),
		CheckedAt:     settingTime(s.store, ctx, "maxmind_checked_at"),
		UpdatedAt:     settingTime(s.store, ctx, "maxmind_updated_at"),
		LastError:     settingValue(s.store, ctx, "maxmind_last_error"),
	}, nil
}

func (s *Server) getMaxMindSettings(w http.ResponseWriter, r *http.Request) {
	status, err := s.maxMindStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) saveMaxMindSettings(w http.ResponseWriter, r *http.Request) {
	var body maxMindConfigRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	body.AccountID = strings.TrimSpace(body.AccountID)
	body.LicenseKey = strings.TrimSpace(body.LicenseKey)
	currentLicense := settingValue(s.store, r.Context(), "maxmind_license_key")
	if body.AccountID == "" {
		writeError(w, http.StatusUnprocessableEntity, errors.New("请填写 MaxMind Account ID"))
		return
	}
	if body.LicenseKey == "" && currentLicense == "" {
		writeError(w, http.StatusUnprocessableEntity, errors.New("请填写 MaxMind License Key"))
		return
	}
	values := map[string]string{
		"maxmind_account_id":  body.AccountID,
		"maxmind_auto_update": strconv.FormatBool(body.AutoUpdate),
		"maxmind_last_error":  "",
	}
	if body.LicenseKey != "" {
		encrypted, err := s.encryptMaxMindSecret(body.LicenseKey)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		values["maxmind_license_key"] = encrypted
	}
	if err := s.store.SetSettings(r.Context(), values); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	status, err := s.maxMindStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
	if body.AutoUpdate && status.UpdatedAt.IsZero() {
		go s.updateMaxMind(context.Background(), false)
	}
}

func (s *Server) testMaxMindConnection(w http.ResponseWriter, r *http.Request) {
	version, modified, err := s.checkMaxMind(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	_ = s.store.SetSettings(r.Context(), map[string]string{
		"maxmind_checked_at": time.Now().UTC().Format(time.RFC3339),
		"maxmind_last_error": "",
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "last_modified": modified})
}

func (s *Server) triggerMaxMindUpdate(w http.ResponseWriter, _ *http.Request) {
	if !s.maxMindUpdating.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusAccepted, map[string]bool{"updating": true})
		return
	}
	go s.updateMaxMindLocked(context.Background(), true)
	writeJSON(w, http.StatusAccepted, map[string]bool{"updating": true})
}

func (s *Server) encryptMaxMindSecret(value string) (string, error) {
	key := sha256.Sum256(s.sessionSecret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(value), nil)
	return "v1:" + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *Server) decryptMaxMindSecret(value string) (string, error) {
	if !strings.HasPrefix(value, "v1:") {
		return value, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "v1:"))
	if err != nil {
		return "", errors.New("MaxMind License Key 保存格式无效")
	}
	key := sha256.Sum256(s.sessionSecret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(payload) < gcm.NonceSize() {
		return "", errors.New("MaxMind License Key 保存格式无效")
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("无法读取 MaxMind License Key，请重新填写")
	}
	return string(plain), nil
}

func (s *Server) maxMindCredentials(ctx context.Context) (string, string, error) {
	accountID := strings.TrimSpace(settingValue(s.store, ctx, "maxmind_account_id"))
	stored := settingValue(s.store, ctx, "maxmind_license_key")
	if accountID == "" || stored == "" {
		return "", "", errors.New("请先在系统设置中配置 MaxMind Account ID 和 License Key")
	}
	license, err := s.decryptMaxMindSecret(stored)
	return accountID, license, err
}

func (s *Server) newMaxMindRequest(ctx context.Context, method string) (*http.Request, error) {
	accountID, license, err := s.maxMindCredentials(ctx)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, maxMindDownloadURL, nil)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(accountID, license)
	request.Header.Set("User-Agent", "Relay-Panel-MaxMind-Updater/1")
	return request, nil
}

func maxMindRelease(response *http.Response) (string, string) {
	modified := response.Header.Get("Last-Modified")
	contentDisposition := response.Header.Get("Content-Disposition")
	version := ""
	if match := maxMindVersionPattern.FindStringSubmatch(contentDisposition); len(match) == 2 {
		version = match[1]
	}
	if version == "" && modified != "" {
		if parsed, err := http.ParseTime(modified); err == nil {
			version = parsed.UTC().Format("20060102")
		}
	}
	return version, modified
}

func (s *Server) checkMaxMind(ctx context.Context) (string, string, error) {
	request, err := s.newMaxMindRequest(ctx, http.MethodHead)
	if err != nil {
		return "", "", err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("连接 MaxMind 失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("MaxMind 返回 HTTP %d，请检查账号、密钥和网络", response.StatusCode)
	}
	version, modified := maxMindRelease(response)
	return version, modified, nil
}

func (s *Server) updateMaxMind(ctx context.Context, force bool) {
	if !s.maxMindUpdating.CompareAndSwap(false, true) {
		return
	}
	s.updateMaxMindLocked(ctx, force)
}

func (s *Server) updateMaxMindLocked(ctx context.Context, force bool) {
	defer s.maxMindUpdating.Store(false)
	checkedAt := time.Now().UTC()
	version, modified, err := s.checkMaxMind(ctx)
	if err != nil {
		s.recordMaxMindFailure(ctx, checkedAt, err)
		return
	}
	currentVersion := settingValue(s.store, ctx, "maxmind_version")
	currentModified := settingValue(s.store, ctx, "maxmind_last_modified")
	if !force && ((version != "" && version == currentVersion) || (modified != "" && modified == currentModified)) {
		_ = s.store.SetSettings(ctx, map[string]string{
			"maxmind_checked_at": checkedAt.Format(time.RFC3339),
			"maxmind_last_error": "",
		})
		return
	}
	request, err := s.newMaxMindRequest(ctx, http.MethodGet)
	if err != nil {
		s.recordMaxMindFailure(ctx, checkedAt, err)
		return
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		s.recordMaxMindFailure(ctx, checkedAt, fmt.Errorf("下载 MaxMind 数据库失败：%w", err))
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		s.recordMaxMindFailure(ctx, checkedAt, fmt.Errorf("MaxMind 下载返回 HTTP %d", response.StatusCode))
		return
	}
	if responseVersion, responseModified := maxMindRelease(response); responseVersion != "" || responseModified != "" {
		if responseVersion != "" {
			version = responseVersion
		}
		if responseModified != "" {
			modified = responseModified
		}
	}
	temp, err := os.CreateTemp("", "relay-maxmind-*.zip")
	if err != nil {
		s.recordMaxMindFailure(ctx, checkedAt, err)
		return
	}
	path := temp.Name()
	defer os.Remove(path)
	written, copyErr := io.Copy(temp, io.LimitReader(response.Body, maxMindMaxArchive+1))
	closeErr := temp.Close()
	if copyErr != nil || closeErr != nil || written > maxMindMaxArchive {
		if copyErr == nil {
			copyErr = closeErr
		}
		if written > maxMindMaxArchive {
			copyErr = errors.New("MaxMind 压缩包超过 128 MB 安全限制")
		}
		s.recordMaxMindFailure(ctx, checkedAt, copyErr)
		return
	}
	ranges, err := parseMaxMindCityArchive(path)
	if err != nil {
		s.recordMaxMindFailure(ctx, checkedAt, err)
		return
	}
	if err := s.store.ReplaceMaxMindRanges(ctx, ranges); err != nil {
		s.recordMaxMindFailure(ctx, checkedAt, err)
		return
	}
	_ = s.store.SetSettings(ctx, map[string]string{
		"maxmind_checked_at":    checkedAt.Format(time.RFC3339),
		"maxmind_last_error":    "",
		"maxmind_last_modified": modified,
		"maxmind_version":       version,
	})
	s.log.Info("MaxMind database updated", "version", version, "ranges", len(ranges))
}

func (s *Server) recordMaxMindFailure(ctx context.Context, checkedAt time.Time, updateErr error) {
	message := "更新失败"
	if updateErr != nil {
		message = updateErr.Error()
	}
	_ = s.store.SetSettings(ctx, map[string]string{
		"maxmind_checked_at": checkedAt.Format(time.RFC3339),
		"maxmind_last_error": message,
	})
	s.log.Warn("MaxMind update failed", "error", updateErr)
}

func (s *Server) StartBackground(ctx context.Context) {
	go func() {
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			s.runScheduledMaxMindUpdate(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) runScheduledMaxMindUpdate(ctx context.Context) {
	if settingValue(s.store, ctx, "maxmind_auto_update") != "true" {
		return
	}
	checked := settingTime(s.store, ctx, "maxmind_checked_at")
	interval := 7 * 24 * time.Hour
	if settingValue(s.store, ctx, "maxmind_last_error") != "" {
		interval = 24 * time.Hour
	}
	if checked.IsZero() || time.Since(checked) >= interval {
		s.updateMaxMind(ctx, false)
	}
}

func parseMaxMindCityArchive(path string) ([]domain.GeoRange, error) {
	archive, err := zip.OpenReader(filepath.Clean(path))
	if err != nil {
		return nil, errors.New("MaxMind 文件不是有效的 ZIP 压缩包")
	}
	defer archive.Close()
	var blocks *zip.File
	locationFiles := map[string]*zip.File{}
	for _, file := range archive.File {
		name := filepath.Base(file.Name)
		switch {
		case strings.HasSuffix(name, "City-Blocks-IPv4.csv"):
			blocks = file
		case strings.HasSuffix(name, "City-Locations-en.csv"):
			locationFiles["en"] = file
		case strings.HasSuffix(name, "City-Locations-zh-CN.csv"):
			locationFiles["zh-CN"] = file
		}
	}
	if blocks == nil || locationFiles["en"] == nil {
		return nil, errors.New("压缩包中缺少 GeoLite2 City IPv4 或位置文件")
	}
	locations := map[string]maxMindLocation{}
	for _, locale := range []string{"en", "zh-CN"} {
		file := locationFiles[locale]
		if file == nil {
			continue
		}
		if err := readMaxMindLocations(file, locations, locale == "zh-CN"); err != nil {
			return nil, err
		}
	}
	reader, err := blocks.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	csvReader := csv.NewReader(reader)
	header, err := csvReader.Read()
	if err != nil {
		return nil, errors.New("无法读取 MaxMind IPv4 表头")
	}
	indexes := csvIndexes(header)
	networkIndex, networkOK := indexes["network"]
	geoIndex, geoOK := indexes["geoname_id"]
	if !networkOK || !geoOK {
		return nil, errors.New("MaxMind IPv4 文件缺少 network 或 geoname_id")
	}
	var ranges []domain.GeoRange
	for {
		record, err := csvReader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 MaxMind IPv4 数据失败：%w", err)
		}
		if networkIndex >= len(record) || geoIndex >= len(record) {
			continue
		}
		location, ok := locations[record[geoIndex]]
		if !ok || location.province == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(record[networkIndex])
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		prefix = prefix.Masked()
		start := prefix.Addr()
		base := start.As4()
		value := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
		hostBits := 32 - prefix.Bits()
		endValue := value
		if hostBits == 32 {
			endValue = ^uint32(0)
		} else if hostBits > 0 {
			endValue = value | (uint32(1)<<hostBits - 1)
		}
		end := netip.AddrFrom4([4]byte{byte(endValue >> 24), byte(endValue >> 16), byte(endValue >> 8), byte(endValue)}).String()
		ranges = append(ranges, domain.GeoRange{StartIP: start.String(), EndIP: end, Country: "中国", Province: location.province, City: location.city})
	}
	if len(ranges) == 0 {
		return nil, errors.New("MaxMind 数据库没有识别到中国省级 IPv4 网段")
	}
	return ranges, nil
}

func csvIndexes(header []string) map[string]int {
	result := make(map[string]int, len(header))
	for index, name := range header {
		result[strings.TrimSpace(strings.TrimPrefix(name, "\ufeff"))] = index
	}
	return result
}

func readMaxMindLocations(file *zip.File, locations map[string]maxMindLocation, localized bool) error {
	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	csvReader := csv.NewReader(reader)
	header, err := csvReader.Read()
	if err != nil {
		return errors.New("无法读取 MaxMind 位置表头")
	}
	indexes := csvIndexes(header)
	required := []string{"geoname_id", "country_iso_code", "subdivision_1_iso_code", "subdivision_1_name", "city_name"}
	for _, name := range required {
		if _, ok := indexes[name]; !ok {
			return fmt.Errorf("MaxMind 位置文件缺少 %s", name)
		}
	}
	for {
		record, err := csvReader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取 MaxMind 位置数据失败：%w", err)
		}
		if record[indexes["country_iso_code"]] != "CN" {
			continue
		}
		province := maxMindChinaProvince(record[indexes["subdivision_1_iso_code"]])
		if province == "" {
			province = normalizeChinaProvince(cleanGeoField(record[indexes["subdivision_1_name"]]))
		}
		if province == "" {
			continue
		}
		city := ""
		if localized {
			city = normalizeChinaCity(cleanGeoField(record[indexes["city_name"]]))
		}
		locations[record[indexes["geoname_id"]]] = maxMindLocation{province: province, city: city}
	}
}

func maxMindChinaProvince(code string) string {
	return map[string]string{
		"AH": "安徽省", "BJ": "北京", "CQ": "重庆", "FJ": "福建省",
		"GD": "广东省", "GS": "甘肃省", "GX": "广西", "GZ": "贵州省",
		"HA": "河南省", "HB": "湖北省", "HE": "河北省", "HI": "海南省",
		"HL": "黑龙江省", "HN": "湖南省", "JL": "吉林省", "JS": "江苏省",
		"JX": "江西省", "LN": "辽宁省", "NM": "内蒙古", "NX": "宁夏",
		"QH": "青海省", "SC": "四川省", "SD": "山东省", "SH": "上海",
		"SN": "陕西省", "SX": "山西省", "TJ": "天津", "TW": "台湾省",
		"XJ": "新疆", "XZ": "西藏", "YN": "云南省", "ZJ": "浙江省",
	}[strings.ToUpper(strings.TrimSpace(code))]
}
