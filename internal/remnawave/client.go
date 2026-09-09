package remnawave

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func New(baseURL, token string, client *http.Client) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: client}
}

type node struct {
	UUID       string `json:"uuid"`
	Name       string `json:"name"`
	Address    string `json:"address"`
	IsDisabled bool   `json:"isDisabled"`
	Profile    struct {
		UUID     string `json:"activeConfigProfileUuid"`
		Inbounds []struct {
			UUID string `json:"uuid"`
		} `json:"activeInbounds"`
	} `json:"configProfile"`
}

type host struct {
	UUID       string   `json:"uuid"`
	Address    string   `json:"address"`
	Port       int      `json:"port"`
	SNI        string   `json:"sni"`
	IsDisabled bool     `json:"isDisabled"`
	Nodes      []string `json:"nodes"`
	Inbound    struct {
		ProfileUUID string `json:"configProfileUuid"`
		UUID        string `json:"configProfileInboundUuid"`
	} `json:"inbound"`
}

func (c *Client) Discover(ctx context.Context) ([]monitor.Target, []string, error) {
	var nodes []node
	if err := c.get(ctx, "/api/nodes", &nodes); err != nil {
		return nil, nil, err
	}
	for _, node := range nodes {
		if node.UUID == "" || node.Name == "" || node.Address == "" {
			return nil, nil, fmt.Errorf("Remnawave node is missing identity fields")
		}
	}
	var hosts []host
	if err := c.get(ctx, "/api/hosts", &hosts); err != nil {
		return nil, nil, err
	}
	for _, host := range hosts {
		if host.UUID == "" || host.Address == "" || host.Port <= 0 {
			return nil, nil, fmt.Errorf("Remnawave host is missing identity fields")
		}
	}
	targets, warnings := selectTargets(nodes, hosts)
	return targets, warnings, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Remnawave %s: %w", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Remnawave %s: HTTP %d", path, res.StatusCode)
	}
	var envelope struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("Remnawave response: %w", err)
	}
	// A missing response must not erase the saved inventory.
	if len(envelope.Response) == 0 || strings.TrimSpace(string(envelope.Response)) == "null" {
		return fmt.Errorf("Remnawave missing response array")
	}
	if err := json.Unmarshal(envelope.Response, out); err != nil {
		return fmt.Errorf("Remnawave response array: %w", err)
	}
	return nil
}

func selectTargets(nodes []node, hosts []host) ([]monitor.Target, []string) {
	byAddress := make(map[string][]string)
	var warnings []string
	for _, host := range hosts {
		if host.IsDisabled {
			continue
		}
		var names []string
		for _, node := range nodes {
			if node.IsDisabled || node.Profile.UUID != host.Inbound.ProfileUUID {
				continue
			}
			if len(host.Nodes) > 0 && !slices.Contains(host.Nodes, node.UUID) {
				continue
			}
			for _, inbound := range node.Profile.Inbounds {
				if inbound.UUID != "" && inbound.UUID == host.Inbound.UUID {
					names = append(names, node.Name)
					break
				}
			}
		}
		if len(names) == 0 {
			warnings = append(warnings, fmt.Sprintf("host %s skipped: no eligible active node/inbound", host.UUID))
			continue
		}
		if host.Port != 443 {
			warnings = append(warnings, fmt.Sprintf("host %s skipped: Cheburcheck tests port 443, host uses %d", host.UUID, host.Port))
			continue
		}
		address, err := normalizeTarget(host.Address)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("host %s skipped: %v", host.UUID, err))
			continue
		}
		if host.SNI != "" && host.SNI != host.Address {
			warnings = append(warnings, fmt.Sprintf("host %s: address/SNI differ; Cheburcheck does not reproduce the VPN handshake", host.UUID))
		}
		byAddress[address] = append(byAddress[address], names...)
	}
	targets := make([]monitor.Target, 0, len(byAddress))
	for address, names := range byAddress {
		sort.Strings(names)
		targets = append(targets, monitor.Target{Address: address, Names: slices.Compact(names)})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Address < targets[j].Address })
	return targets, warnings
}

func normalizeTarget(address string) (string, error) {
	address = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(address), "."))
	if ip := net.ParseIP(address); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			return "", fmt.Errorf("non-public IP")
		}
		return ip.String(), nil
	}
	if len(address) > 253 || !strings.Contains(address, ".") {
		return "", fmt.Errorf("expected public IP or DNS name")
	}
	for _, label := range strings.Split(address, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid DNS name")
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return "", fmt.Errorf("DNS name contains unsupported characters or a template")
			}
		}
	}
	return address, nil
}
