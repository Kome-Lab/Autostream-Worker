package videoout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	srt "github.com/datarhei/gosrt"
)

var ErrInvalidIngestConfig = errors.New("invalid video ingest configuration")

var contractPassphrasePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

const (
	contractPassphraseMinBytes = 32
	contractPassphraseMaxBytes = 79
	contractPBKeylen           = 32
)

type SRTOptions struct {
	Passphrase        string
	PBKeylen          int
	StreamID          string
	ConnectionTimeout time.Duration
}

type SRTConn interface {
	io.WriteCloser
}

type SRTDialer interface {
	Dial(context.Context, string, SRTOptions) (SRTConn, error)
}

type srtDestination struct {
	address string
	options SRTOptions
}

func parseSRTDestination(rawURL, streamID, passphrase string, pbkeylen int, timeout time.Duration) (srtDestination, error) {
	if rawURL == "" || rawURL != strings.TrimSpace(rawURL) {
		return srtDestination{}, invalidIngest("URL is empty or padded")
	}
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Scheme, "srt") || u.Opaque != "" {
		return srtDestination{}, invalidIngest("URL must use the srt scheme")
	}
	if u.User != nil || u.Fragment != "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery {
		return srtDestination{}, invalidIngest("URL contains forbidden components")
	}
	if u.Hostname() == "" {
		return srtDestination{}, invalidIngest("URL host is required")
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil || strings.TrimSpace(host) == "" {
		return srtDestination{}, invalidIngest("URL must include an explicit host and port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return srtDestination{}, invalidIngest("URL port is invalid")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" || len(streamID) > srt.MAX_STREAMID_SIZE {
		return srtDestination{}, invalidIngest("stream_id is outside the SRT limits")
	}
	if len(passphrase) < contractPassphraseMinBytes || len(passphrase) > contractPassphraseMaxBytes {
		return srtDestination{}, invalidIngest("passphrase length is outside the ingest contract")
	}
	if !contractPassphrasePattern.MatchString(passphrase) {
		return srtDestination{}, invalidIngest("passphrase is outside the base64url ingest contract")
	}
	if pbkeylen != contractPBKeylen {
		return srtDestination{}, invalidIngest("pbkeylen must be 32")
	}
	if timeout <= 0 {
		return srtDestination{}, invalidIngest("connection timeout must be positive")
	}
	return srtDestination{
		address: u.Host,
		options: SRTOptions{
			Passphrase:        passphrase,
			PBKeylen:          pbkeylen,
			StreamID:          streamID,
			ConnectionTimeout: timeout,
		},
	}, nil
}

func invalidIngest(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidIngestConfig, reason)
}

type goSRTDialer struct {
	dial func(network, address string, options SRTOptions) (SRTConn, error)
}

func newGoSRTDialer() SRTDialer {
	return goSRTDialer{dial: dialGoSRT}
}

func (d goSRTDialer) Dial(ctx context.Context, address string, options SRTOptions) (SRTConn, error) {
	if d.dial == nil {
		return nil, errors.New("SRT dialer is unavailable")
	}
	type result struct {
		conn SRTConn
		err  error
	}
	resultCh := make(chan result)
	go func() {
		conn, err := d.dial("srt", address, options)
		select {
		case resultCh <- result{conn: conn, err: err}:
		case <-ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-resultCh:
		if err := ctx.Err(); err != nil {
			if result.conn != nil {
				_ = result.conn.Close()
			}
			return nil, err
		}
		return result.conn, result.err
	}
}

func dialGoSRT(network, address string, options SRTOptions) (SRTConn, error) {
	config := srt.DefaultConfig()
	config.ConnectionTimeout = options.ConnectionTimeout
	config.EnforcedEncryption = true
	config.MessageAPI = false
	config.Passphrase = options.Passphrase
	config.PBKeylen = options.PBKeylen
	config.StreamId = options.StreamID
	config.TransmissionType = "live"
	config.Logger = srt.NewLogger(nil)
	return srt.Dial(network, address, config)
}
