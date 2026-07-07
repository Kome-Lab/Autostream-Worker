package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const maxConfigureResponseBytes = 1 << 20
const nodeConfigDirMode = 0o750
const nodeConfigFileMode = 0o640
const nodeConfigSecureDir = "/etc/autostream-worker"
const nodeConfigServiceGroup = "autostream"

type configureAPIResponse struct {
	ConfigYML        string `json:"config_yml"`
	ConfigurationYML string `json:"configuration_yaml"`
}

type configureAPIError struct {
	Code string `json:"code"`
}

// RunConfigureCommand pairs this service binary with a Control Panel node and writes AUTOSTREAM_NODE_CONFIG.
func RunConfigureCommand(args []string, expectedType string, stdout io.Writer) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	panelURL := fs.String("panel-url", "", "Control Panel base URL")
	configureToken := fs.String("token", "", "one-time Configure Token")
	nodeID := fs.String("node", "", "Node ID")
	configPath := fs.String("config", "/etc/autostream-worker/config.yml", "path to write config.yml")
	timeout := fs.Duration("timeout", 15*time.Second, "configure request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if strings.TrimSpace(*panelURL) == "" {
		return errors.New("--panel-url is required")
	}
	if strings.TrimSpace(*configureToken) == "" {
		return errors.New("--token is required")
	}
	if strings.TrimSpace(*nodeID) == "" {
		return errors.New("--node is required")
	}
	if strings.TrimSpace(*configPath) == "" {
		return errors.New("--config is required")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if err := validateHTTPURL(*panelURL, "--panel-url"); err != nil {
		return err
	}
	configYML, err := fetchNodeConfig(context.Background(), strings.TrimSpace(*panelURL), strings.TrimSpace(*nodeID), strings.TrimSpace(*configureToken), *timeout)
	if err != nil {
		return err
	}
	nodeCfg, err := parseNodeAgentConfig([]byte(configYML))
	if err != nil {
		return fmt.Errorf("received invalid config.yml: %w", err)
	}
	if expectedType = strings.TrimSpace(expectedType); expectedType != "" && nodeCfg.NodeType != expectedType {
		return fmt.Errorf("received config for node.type %q, but this binary expects %q", nodeCfg.NodeType, expectedType)
	}
	if nodeCfg.NodeID != strings.TrimSpace(*nodeID) {
		return fmt.Errorf("received config for node %q, but requested %q", nodeCfg.NodeID, strings.TrimSpace(*nodeID))
	}
	if err := writeNodeConfigFile(*configPath, []byte(configYML)); err != nil {
		return err
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "configure succeeded: wrote %s for node %s (%s)\n", filepath.Clean(*configPath), nodeCfg.NodeID, nodeCfg.NodeType)
	}
	return nil
}

func fetchNodeConfig(ctx context.Context, panelURL, nodeID, configureToken string, timeout time.Duration) (string, error) {
	payload, err := json.Marshal(map[string]string{"nodeId": nodeID, "configureToken": configureToken})
	if err != nil {
		return "", err
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, strings.TrimRight(panelURL, "/")+"/api/node-agent/configure", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("control panel configure request failed: %w", err)
	}
	defer res.Body.Close()
	body, err := readLimitedConfigureBody(res.Body)
	if err != nil {
		return "", err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var apiErr configureAPIError
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Code != "" {
			return "", fmt.Errorf("control panel configure failed: HTTP %d code %s", res.StatusCode, apiErr.Code)
		}
		return "", fmt.Errorf("control panel configure failed: HTTP %d", res.StatusCode)
	}
	var response configureAPIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode control panel configure response: %w", err)
	}
	configYML := strings.TrimSpace(response.ConfigYML)
	if configYML == "" {
		configYML = strings.TrimSpace(response.ConfigurationYML)
	}
	if configYML == "" {
		return "", errors.New("control panel configure response did not include config_yml")
	}
	return configYML + "\n", nil
}

func readLimitedConfigureBody(reader io.Reader) ([]byte, error) {
	limited := &io.LimitedReader{R: reader, N: maxConfigureResponseBytes + 1}
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(body) > maxConfigureResponseBytes {
		return nil, errors.New("control panel configure response is too large")
	}
	return body, nil
}

func writeNodeConfigFile(path string, body []byte) error {
	cleanPath := filepath.Clean(strings.TrimSpace(path))
	dir := filepath.Dir(cleanPath)
	if err := os.MkdirAll(dir, nodeConfigDirMode); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := secureNodeConfigDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(cleanPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(nodeConfigFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := secureNodeConfigFile(tmpName); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, cleanPath); err != nil {
		if runtime.GOOS == "windows" {
			_ = os.Remove(cleanPath)
			if retryErr := os.Rename(tmpName, cleanPath); retryErr == nil {
				keepTemp = true
				_ = secureNodeConfigFile(cleanPath)
				return nil
			}
		}
		return fmt.Errorf("install config: %w", err)
	}
	keepTemp = true
	if err := secureNodeConfigFile(cleanPath); err != nil {
		return err
	}
	return nil
}

func secureNodeConfigDir(path string) error {
	if !shouldApplyAutostreamNodeOwnership(path) {
		return nil
	}
	if err := chownRootAutostream(path); err != nil {
		return fmt.Errorf("secure config directory: %w", err)
	}
	if err := os.Chmod(path, nodeConfigDirMode); err != nil {
		return fmt.Errorf("chmod config directory: %w", err)
	}
	return nil
}

func secureNodeConfigFile(path string) error {
	if shouldApplyAutostreamNodeOwnership(path) {
		if err := chownRootAutostream(path); err != nil {
			return fmt.Errorf("secure config file: %w", err)
		}
	}
	if err := os.Chmod(path, nodeConfigFileMode); err != nil {
		return fmt.Errorf("chmod config: %w", err)
	}
	return nil
}

func shouldApplyAutostreamNodeOwnership(path string) bool {
	if runtime.GOOS == "windows" || os.Geteuid() != 0 {
		return false
	}
	cleanPath := filepath.Clean(path)
	secureDir := filepath.Clean(nodeConfigSecureDir)
	return cleanPath == secureDir || strings.HasPrefix(cleanPath, secureDir+string(os.PathSeparator))
}

func chownRootAutostream(path string) error {
	group, err := user.LookupGroup(nodeConfigServiceGroup)
	if err != nil {
		return fmt.Errorf("lookup group %q: %w", nodeConfigServiceGroup, err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return fmt.Errorf("parse group %q gid %q: %w", nodeConfigServiceGroup, group.Gid, err)
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return fmt.Errorf("chown root:%s: %w", nodeConfigServiceGroup, err)
	}
	return nil
}
