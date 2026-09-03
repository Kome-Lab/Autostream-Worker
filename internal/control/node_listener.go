package control

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/example/autostream-contracts/pkg/contracts"
)

// The Node configuration explicitly selects the credential. systemd supplies
// it from the root-owned listener projection; Compose mounts the same v2 data.
// No bind address or revision is inferred from the public API endpoint.
func loadNodeListenerConfig(node nodeAgentConfig) (contracts.NodeListenerConfig, error) {
	directory := strings.TrimSpace(os.Getenv("CREDENTIALS_DIRECTORY"))
	if node.ListenerCredential != "node-listener.json" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return contracts.NodeListenerConfig{}, errors.New("the configured listener credential directory is required")
	}
	path := filepath.Join(directory, node.ListenerCredential)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || info.Size() < 1 || info.Size() > 4096 {
		return contracts.NodeListenerConfig{}, errors.New("listener credential must be a bounded non-writable regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return contracts.NodeListenerConfig{}, errors.New("open listener credential")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || info.Size() != opened.Size() || info.Mode() != opened.Mode() || !info.ModTime().Equal(opened.ModTime()) {
		return contracts.NodeListenerConfig{}, errors.New("listener credential changed during open")
	}
	body, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(body) > 4096 {
		return contracts.NodeListenerConfig{}, errors.New("read listener credential")
	}
	listener, err := contracts.ParseNodeListenerConfig(body)
	if err != nil || listener.ServiceType != node.NodeType {
		return contracts.NodeListenerConfig{}, errors.New("listener credential does not match the v2 Node configuration")
	}
	return listener, nil
}
