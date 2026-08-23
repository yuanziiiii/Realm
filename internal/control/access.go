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
			item.Country, item.Province, item.City, item.ISP = fields[1], fields[2], fields[3], fields[4]
		} else {
			item.Province, item.City, item.ISP = fields[1], fields[2], fields[3]
		}
		return item, item.Province != ""
	}
	start, startErr := netip.ParseAddr(fields[0])
	end, endErr := netip.ParseAddr(fields[1])
	if startErr != nil || endErr != nil || !start.Is4() || !end.Is4() || start.Compare(end) > 0 {
		return domain.GeoRange{}, false
	}
	item := domain.GeoRange{StartIP: start.String(), EndIP: end.String()}
	// ip2region's ip.merge.txt format is:
	// start|end|country|region|province|city|isp|...
	if len(fields) >= 7 {
		item.Country, item.Province, item.City, item.ISP = fields[2], fields[4], fields[5], fields[6]
	} else if len(fields) >= 6 {
		item.Country, item.Province, item.City, item.ISP = fields[2], fields[3], fields[4], fields[5]
	} else {
		item.Province, item.City = fields[2], fields[3]
		if len(fields) > 4 {
			item.ISP = fields[4]
		}
	}
	return item, item.Province != ""
}
