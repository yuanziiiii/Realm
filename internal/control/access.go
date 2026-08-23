package control

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"

	"relaypanel/internal/domain"
)

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	connections, err := s.store.ListConnections(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	connections.Sources = nonNil(connections.Sources)
	connections.Statuses = nonNil(connections.Statuses)
	writeJSON(w, http.StatusOK, connections)
}

func (s *Server) geoStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.store.GeoStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	status.Regions = nonNil(status.Regions)
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) importGeoDatabase(w http.ResponseWriter, r *http.Request) {
	const maxBytes = 64 << 20
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(payload) > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errors.New("IP 地区库不能超过 64 MB"))
		return
	}
	ranges, err := parseGeoRanges(bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if len(ranges) > 1_000_000 {
		writeError(w, http.StatusRequestEntityTooLarge, errors.New("IP 地区库不能超过 100 万个网段"))
		return
	}
	if err := s.store.ReplaceGeoRanges(r.Context(), ranges); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	status, err := s.store.GeoStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func parseGeoRanges(reader io.Reader) ([]domain.GeoRange, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64<<20)
	var ranges []domain.GeoRange
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		separator := "|"
		if !strings.Contains(line, separator) {
			if strings.Contains(line, "\t") {
				separator = "\t"
			} else {
				separator = ","
			}
		}
		fields := strings.Split(line, separator)
		for index := range fields {
			fields[index] = strings.TrimSpace(fields[index])
		}
		item, ok := parseGeoFields(fields)
		if !ok {
			if lineNumber == 1 && strings.Contains(strings.ToLower(line), "province") {
				continue
			}
			return nil, fmt.Errorf("IP 地区库第 %d 行格式无效", lineNumber)
		}
		// Access policies currently expose Chinese province/city selectors. Keep
		// custom rows without a country, but discard unrelated global ranges from
		// sources such as ip2region's ipv4_source.txt.
		if item.Country != "" && !isChinaCountry(item.Country) {
			continue
		}
		if item.Province == "" {
			continue
		}
		ranges = append(ranges, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(ranges) == 0 {
		return nil, errors.New("IP 地区库没有识别到有效网段")
	}
	return ranges, nil
}

func parseGeoFields(fields []string) (domain.GeoRange, bool) {
	if len(fields) < 4 {
		return domain.GeoRange{}, false
	}
	if prefix, err := netip.ParsePrefix(fields[0]); err == nil && prefix.Addr().Is4() {
		prefix = prefix.Masked()
		start := prefix.Addr()
		base := start.As4()
		value := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
		hostBits := 32 - prefix.Bits()
		var endValue uint32 = value
		if hostBits == 32 {
			endValue = ^uint32(0)
		} else if hostBits > 0 {
			endValue = value | (uint32(1)<<hostBits - 1)
		}
		end := netip.AddrFrom4([4]byte{byte(endValue >> 24), byte(endValue >> 16), byte(endValue >> 8), byte(endValue)}).String()
		item := domain.GeoRange{StartIP: start.String(), EndIP: end}
		if len(fields) >= 5 {
			item.Country, item.Province, item.City, item.ISP = cleanGeoField(fields[1]), cleanGeoField(fields[2]), cleanGeoField(fields[3]), cleanGeoField(fields[4])
		} else {
			item.Province, item.City, item.ISP = cleanGeoField(fields[1]), cleanGeoField(fields[2]), cleanGeoField(fields[3])
		}
		return item, true
	}
	start, startErr := netip.ParseAddr(fields[0])
	end, endErr := netip.ParseAddr(fields[1])
	if startErr != nil || endErr != nil || !start.Is4() || !end.Is4() || start.Compare(end) > 0 {
		return domain.GeoRange{}, false
	}
	item := domain.GeoRange{StartIP: start.String(), EndIP: end.String()}
	// Current ip2region ipv4_source.txt format is:
	// start|end|country|province|city|isp|iso-alpha2-code
	// The legacy ip.merge.txt format is:
	// start|end|country|region|province|city|isp|...
	if len(fields) >= 7 && isISOAlpha2(fields[6]) {
		item.Country, item.Province, item.City, item.ISP = fields[2], fields[3], fields[4], fields[5]
	} else if len(fields) >= 7 {
		item.Country, item.Province, item.City, item.ISP = fields[2], fields[4], fields[5], fields[6]
	} else if len(fields) >= 6 {
		item.Country, item.Province, item.City, item.ISP = fields[2], fields[3], fields[4], fields[5]
	} else {
		item.Province, item.City = fields[2], fields[3]
		if len(fields) > 4 {
			item.ISP = fields[4]
		}
	}
	item.Country = cleanGeoField(item.Country)
	item.Province = cleanGeoField(item.Province)
	item.City = cleanGeoField(item.City)
	item.ISP = cleanGeoField(item.ISP)
	if isChinaCountry(item.Country) {
		item.Province = normalizeChinaProvince(item.Province)
		item.City = normalizeChinaCity(item.City)
	}
	return item, true
}

func cleanGeoField(value string) string {
	value = strings.TrimSpace(value)
	if value == "0" || strings.EqualFold(value, "reserved") {
		return ""
	}
	return value
}

func isISOAlpha2(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 2 {
		return false
	}
	for _, char := range value {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}

func isChinaCountry(value string) bool {
	value = strings.TrimSpace(value)
	return value == "中国" || strings.EqualFold(value, "china") || strings.EqualFold(value, "cn")
}

func normalizeChinaProvince(value string) string {
	aliases := map[string]string{
		"北京市":      "北京",
		"上海市":      "上海",
		"天津市":      "天津",
		"重庆市":      "重庆",
		"广西壮族自治区":  "广西",
		"宁夏回族自治区":  "宁夏",
		"新疆维吾尔自治区": "新疆",
		"内蒙古自治区":   "内蒙古",
		"西藏自治区":    "西藏",
		"香港":       "香港特别行政区",
		"澳门":       "澳门特别行政区",
		"台北市":      "台湾省",
		"基隆市":      "台湾省",
		"彰化县":      "台湾省",
		"新竹县":      "台湾省",
		"台中市":      "台湾省",
		"桃园市":      "台湾省",
		"UEruemqi": "新疆",
	}
	if normalized, ok := aliases[value]; ok {
		return normalized
	}
	return value
}

func normalizeChinaCity(value string) string {
	aliases := map[string]string{
		"Taipei City":      "台北市",
		"Zhongli District": "桃园市",
		"Beijing":          "北京市",
		"Kowloon":          "九龙",
		"Shenzhen":         "深圳市",
		"Keelung":          "基隆市",
		"Ningbo Shi":       "宁波市",
		"Maoming Shi":      "茂名市",
		"Changhua":         "彰化县",
		"Nantong Shi":      "南通市",
		"Shanghai":         "上海市",
		"Guangzhou Shi":    "广州市",
		"Dongguan Shi":     "东莞市",
		"Foshan Shi":       "佛山市",
		"Ganzhou Shi":      "赣州市",
		"Hsinchu":          "新竹县",
		"Taitung":          "台东县",
		"Changsha Shi":     "长沙市",
		"Hanshan Qu":       "邯郸市",
		"Zhoukou Shi":      "周口市",
		"Hefei Shi":        "合肥市",
		"Nanyang Shi":      "南阳市",
		"Wuhan Shi":        "武汉市",
		"Changchun Shi":    "长春市",
		"Linyi Xian":       "临沂市",
		"Tianjin":          "天津市",
		"Zhengzhou Shi":    "郑州市",
		"Zhumadian Shi":    "驻马店市",
		"Shijiazhuang Shi": "石家庄市",
		"Heze Shi":         "菏泽市",
		"Tongshan":         "徐州市",
		"Xi'an Shi":        "西安市",
		"Fuyang Shi":       "阜阳市",
		"Baoding Shi":      "保定市",
		"Shenyang Shi":     "沈阳市",
		"Jining Shi":       "济宁市",
		"Nanjing Shi":      "南京市",
		"Hengyang Xian":    "衡阳市",
		"Weifang Shi":      "潍坊市",
		"Chongqing":        "重庆市",
		"Yancheng Shi":     "盐城市",
		"Nanning Shi":      "南宁市",
		"Chengdu Shi":      "成都市",
		"Wulumuqi":         "乌鲁木齐市",
		"Fengyuan":         "台中市",
		"Xiangyang":        "襄阳市",
		"Guozhen":          "宝鸡市",
		"Zhuhai Shi":       "珠海市",
		"Langfang Shi":     "廊坊市",
	}
	if normalized, ok := aliases[value]; ok {
		return normalized
	}
	return value
}
