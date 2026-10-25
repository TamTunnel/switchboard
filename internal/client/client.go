// Package client talks to the daemon over its Unix socket.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/TamTunnel/switchboard/internal/envelope"
)

// Client is a daemon API client.
type Client struct {
	http *http.Client
}

// New dials the daemon socket at sockPath.
func New(sockPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}
	return &Client{http: &http.Client{Transport: transport}}
}

// Health checks the daemon is up.
func (c *Client) Health() error {
	resp, err := c.http.Get("http://switchboard/v1/health")
	if err != nil {
		return fmt.Errorf("daemon not reachable (is `switchboard daemon` running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon unhealthy: %s", resp.Status)
	}
	return nil
}

// Send submits an envelope; returns its id.
func (c *Client) Send(env *envelope.Envelope) (string, error) {
	data, _ := json.Marshal(env)
	resp, err := c.http.Post("http://switchboard/v1/send", "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("send: daemon not reachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("send: %s", string(body))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Inbox lists incoming work and result notifications for an agent.
func (c *Client) Inbox(agent string) ([]*envelope.Envelope, error) {
	resp, err := c.http.Get("http://switchboard/v1/inbox?agent=" + url.QueryEscape(agent))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("inbox: %s", string(body))
	}
	var out []*envelope.Envelope
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Status fetches one envelope by id.
func (c *Client) Status(id string) (*envelope.Envelope, error) {
	resp, err := c.http.Get("http://switchboard/v1/status?id=" + url.QueryEscape(id))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("envelope %s not found", id)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status: %s", string(body))
	}
	var env envelope.Envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Logs fetches an envelope's adapter run log.
func (c *Client) Logs(id string) (string, error) {
	resp, err := c.http.Get("http://switchboard/v1/logs?id=" + url.QueryEscape(id))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("logs: %s", string(body))
	}
	return string(body), nil
}
