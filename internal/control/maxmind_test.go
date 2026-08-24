package control

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaypanel/internal/domain"
	"relaypanel/internal/store"
)

func TestMaxMindSettingsEncryptAndNeverReturnLicenseKey(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "maxmind-settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server := &Server{store: st, sessionSecret: []byte("01234567890123456789012345678901")}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/geo/maxmind", bytes.NewBufferString(`{"account_id":"12345","license_key":"secret-license-key","auto_update":false}`))
	recorder := httptest.NewRecorder()
	server.saveMaxMindSettings(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected response %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "secret-license-key") {
		t.Fatal("MaxMind License Key leaked in the API response")
	}
	var status domain.MaxMindStatus
	if err := json.NewDecoder(recorder.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.Configured || !status.HasLicenseKey || status.AccountID != "12345" {
		t.Fatalf("unexpected status: %#v", status)
	}
	stored, err := st.GetSetting(ctx, "maxmind_license_key")
	if err != nil {
		t.Fatal(err)
	}
	if stored == "secret-license-key" || !strings.HasPrefix(stored, "v1:") {
		t.Fatalf("License Key was not encrypted: %q", stored)
	}
	plain, err := server.decryptMaxMindSecret(stored)
	if err != nil || plain != "secret-license-key" {
		t.Fatalf("encrypted License Key cannot be restored: %q %v", plain, err)
	}
}

func TestParseMaxMindCityArchiveUsesChineseProvinceAndCity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "GeoLite2-City-CSV_test.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	files := map[string]string{
		"GeoLite2-City-CSV_test/GeoLite2-City-Locations-en.csv":    "geoname_id,locale_code,country_iso_code,subdivision_1_iso_code,subdivision_1_name,city_name\n1795565,en,CN,GD,Guangdong,Shenzhen\n",
		"GeoLite2-City-CSV_test/GeoLite2-City-Locations-zh-CN.csv": "geoname_id,locale_code,country_iso_code,subdivision_1_iso_code,subdivision_1_name,city_name\n1795565,zh-CN,CN,GD,广东,深圳\n",
		"GeoLite2-City-CSV_test/GeoLite2-City-Blocks-IPv4.csv":     "network,geoname_id,registered_country_geoname_id\n223.104.80.0/21,1795565,1814991\n",
	}
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ranges, err := parseMaxMindCityArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 {
		t.Fatalf("expected one China range, got %#v", ranges)
	}
	got := ranges[0]
	if got.StartIP != "223.104.80.0" || got.EndIP != "223.104.87.255" || got.Province != "广东省" || got.City != "深圳市" {
		t.Fatalf("unexpected MaxMind conversion: %#v", got)
	}
}

func TestParseDownloadedMaxMindArchive(t *testing.T) {
	path := os.Getenv("RELAY_TEST_MAXMIND_ARCHIVE")
	if path == "" {
		t.Skip("set RELAY_TEST_MAXMIND_ARCHIVE to run the downloaded-database integration test")
	}
	ranges, err := parseMaxMindCityArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range ranges {
		if item.StartIP == "223.104.80.0" && item.EndIP == "223.104.87.255" {
			if item.Province != "广东省" || item.City != "深圳市" {
				t.Fatalf("unexpected location for 223.104.86.85: %#v", item)
			}
			return
		}
	}
	t.Fatal("downloaded database did not contain the expected 223.104.80.0/21 range")
}
