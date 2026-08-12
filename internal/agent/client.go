package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"relaypanel/internal/domain"
)

type Client struct {
	baseURL, nodeID, token string
	http                   *http.Client
}

func NewClient(baseURL, nodeID, token string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), nodeID: nodeID, token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) Sync(ctx context.Context, req domain.SyncRequest) (domain.SyncResponse, error) {
	var result domain.SyncResponse
	req.NodeID = c.nodeID
	b, err := json.Marshal(req)
	if err != nil {
		return result, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/agent/v1/sync", bytes.NewReader(b))
	if err != nil {
		return result, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return result, err
	}
	if resp.StatusCode != 200 {
		return result, fmt.Errorf("controller returned %s: %s", resp.Status, string(body))
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return result, err
	}
	if result.Node.ID == "" {
		return result, errors.New("controller returned an empty node")
	}
	return result, nil
}
