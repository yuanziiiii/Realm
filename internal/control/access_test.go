package control

import (
	"strings"
	"testing"
)

func TestParseGeoRangesAcceptsIP2RegionAndCIDR(t *testing.T) {
	ranges, err := parseGeoRanges(strings.NewReader(strings.Join([]string{
		"1.0.0.0|1.0.0.127|中国|0|广东省|深圳市|电信",
		"1.0.0.128/25,中国,广东省,广州市,联通",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 2 {
		t.Fatalf("expected 2 ranges, got %#v", ranges)
	}
	if got := ranges[0]; got.StartIP != "1.0.0.0" || got.EndIP != "1.0.0.127" || got.Province != "广东省" || got.City != "深圳市" || got.ISP != "电信" {
		t.Fatalf("unexpected ip2region row: %+v", got)
	}
	if got := ranges[1]; got.StartIP != "1.0.0.128" || got.EndIP != "1.0.0.255" || got.Province != "广东省" || got.City != "广州市" || got.ISP != "联通" {
		t.Fatalf("unexpected CIDR row: %+v", got)
	}
}

func TestParseGeoRangesRejectsInvalidRows(t *testing.T) {
	if _, err := parseGeoRanges(strings.NewReader("not-an-ip|still-not-an-ip|中国|广东省")); err == nil || !strings.Contains(err.Error(), "第 1 行") {
		t.Fatalf("expected line-numbered parse error, got %v", err)
	}
}
