package control

import (
	"strings"
	"testing"
)

func TestParseGeoRangesAcceptsIP2RegionAndCIDR(t *testing.T) {
	ranges, err := parseGeoRanges(strings.NewReader(strings.Join([]string{
		"1.0.0.0|1.0.0.127|中国|0|广东省|深圳市|电信",
		"1.0.1.0|1.0.3.255|中国|福建省|福州市|中国电信|CN",
		"1.0.0.128/25,中国,广东省,广州市,联通",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 3 {
		t.Fatalf("expected 3 ranges, got %#v", ranges)
	}
	if got := ranges[0]; got.StartIP != "1.0.0.0" || got.EndIP != "1.0.0.127" || got.Province != "广东省" || got.City != "深圳市" || got.ISP != "电信" {
		t.Fatalf("unexpected ip2region row: %+v", got)
	}
	if got := ranges[1]; got.StartIP != "1.0.1.0" || got.EndIP != "1.0.3.255" || got.Province != "福建省" || got.City != "福州市" || got.ISP != "中国电信" {
		t.Fatalf("unexpected current ip2region row: %+v", got)
	}
	if got := ranges[2]; got.StartIP != "1.0.0.128" || got.EndIP != "1.0.0.255" || got.Province != "广东省" || got.City != "广州市" || got.ISP != "联通" {
		t.Fatalf("unexpected CIDR row: %+v", got)
	}
}

func TestParseGeoRangesFiltersGlobalAndEmptyRegions(t *testing.T) {
	ranges, err := parseGeoRanges(strings.NewReader(strings.Join([]string{
		"0.0.0.0|0.255.255.255|Reserved|Reserved|Reserved|0|0",
		"1.0.0.0|1.0.0.255|Australia|Queensland|0|0|AU",
		"1.0.1.0|1.0.3.255|中国|福建省|福州市|中国电信|CN",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0].Province != "福建省" || ranges[0].City != "福州市" {
		t.Fatalf("unexpected filtered rows: %#v", ranges)
	}
}

func TestParseGeoRangesNormalizesChineseProvinceAliases(t *testing.T) {
	ranges, err := parseGeoRanges(strings.NewReader(strings.Join([]string{
		"1.0.1.0|1.0.1.255|中国|北京市|北京市|联通|CN",
		"1.0.2.0|1.0.2.255|中国|广西壮族自治区|南宁市|电信|CN",
		"1.0.3.0|1.0.3.255|中国|广东省|Shenzhen|联通|CN",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if ranges[0].Province != "北京" || ranges[1].Province != "广西" || ranges[2].City != "深圳市" {
		t.Fatalf("unexpected normalized provinces: %#v", ranges)
	}
}

func TestParseGeoRangesRejectsInvalidRows(t *testing.T) {
	if _, err := parseGeoRanges(strings.NewReader("not-an-ip|still-not-an-ip|中国|广东省")); err == nil || !strings.Contains(err.Error(), "第 1 行") {
		t.Fatalf("expected line-numbered parse error, got %v", err)
	}
}
