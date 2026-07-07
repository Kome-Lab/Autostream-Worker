package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

type nodeAgentConfig struct {
	PanelURL      string
	NodeID        string
	NodeName      string
	NodeType      string
	APIHost       string
	APIPort       int
	APISSLEnabled bool
	Token         string
}

func applyNodeConfigFromEnv(cfg *Config, expectedType string) {
	path := NodeConfigPathFromEnv()
	if path == "" {
		return
	}
	nodeCfg, err := loadNodeAgentConfig(path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		cfg.ConfigError = fmt.Sprintf("AUTOSTREAM_NODE_CONFIG: %v", err)
		return
	}
	if nodeCfg.NodeType != "" && expectedType != "" && nodeCfg.NodeType != expectedType {
		cfg.ConfigError = fmt.Sprintf("AUTOSTREAM_NODE_CONFIG node.type must be %q", expectedType)
		return
	}
	cfg.ControlPanelURL = nodeCfg.PanelURL
	cfg.Token = nodeCfg.Token
	cfg.ServiceID = nodeCfg.NodeID
	cfg.ServiceName = nodeCfg.NodeName
	cfg.ServicePublicURL = nodeAPIURL(nodeCfg.APIHost, nodeCfg.APIPort, nodeCfg.APISSLEnabled)
}

func NodeConfigPathFromEnv() string {
	return strings.TrimSpace(os.Getenv("AUTOSTREAM_NODE_CONFIG"))
}

func NodeConfigPendingFromEnv() bool {
	path := NodeConfigPathFromEnv()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

func NodeRuntimeTokenFromEnv() string {
	path := NodeConfigPathFromEnv()
	if path == "" {
		return ""
	}
	cfg, err := loadNodeAgentConfig(path)
	if err != nil {
		return ""
	}
	return cfg.Token
}

func loadNodeAgentConfig(path string) (nodeAgentConfig, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nodeAgentConfig{}, err
	}
	return parseNodeAgentConfig(body)
}

func parseNodeAgentConfig(body []byte) (nodeAgentConfig, error) {
	var cfg nodeAgentConfig
	section := ""
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(raw, " ") && strings.HasSuffix(line, ":") {
			section = strings.TrimSuffix(line, ":")
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = parseYAMLScalar(value)
		switch section + "." + key {
		case "panel.url":
			cfg.PanelURL = value
		case "node.id":
			cfg.NodeID = value
		case "node.name":
			cfg.NodeName = value
		case "node.type":
			cfg.NodeType = value
		case "api.host":
			cfg.APIHost = value
		case "api.port":
			cfg.APIPort, _ = strconv.Atoi(value)
		case "api.ssl_enabled":
			cfg.APISSLEnabled = strings.EqualFold(value, "true")
		case "auth.token":
			cfg.Token = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nodeAgentConfig{}, err
	}
	if cfg.PanelURL == "" || cfg.NodeID == "" || cfg.NodeName == "" || cfg.Token == "" || cfg.APIHost == "" || cfg.APIPort <= 0 {
		return nodeAgentConfig{}, fmt.Errorf("missing panel.url, node.id, node.name, api host/port, or auth.token")
	}
	return cfg, nil
}

func parseYAMLScalar(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(value, " ")
	if strings.HasPrefix(value, `"`) {
		var decoded string
		if err := json.Unmarshal([]byte(value), &decoded); err == nil {
			return decoded
		}
	}
	return strings.Trim(value, `'"`)
}

func nodeAPIURL(host string, port int, sslEnabled bool) string {
	host = strings.TrimSpace(host)
	if host == "" || port <= 0 {
		return ""
	}
	scheme := "http"
	if sslEnabled {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
}
