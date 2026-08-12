package videoout

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseSRTDestinationKeepsSecretsInMemoryAndAllowsOnlyStreamIDQuery(t *testing.T) {
	destination, err := parseSRTDestination(
		"srt://encoder.example:10080",
		"stream-01",
		"job-scoped-passphrase-32-bytes-ok",
		32,
		3*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if destination.address != "encoder.example:10080" {
		t.Fatalf("address = %q", destination.address)
	}
	if destination.options.StreamID != "stream-01" {
		t.Fatalf("stream ID = %q", destination.options.StreamID)
	}
	if destination.options.Passphrase != "job-scoped-passphrase-32-bytes-ok" || destination.options.PBKeylen != 32 {
		t.Fatalf("encryption options were lost: %#v", destination.options)
	}
}

func TestParseSRTDestinationFailsClosed(t *testing.T) {
	for _, rawURL := range []string{
		"http://encoder.example:10080",
		"srt://user:password@encoder.example:10080",
		"srt://encoder.example",
		"srt://encoder.example:10080/path",
		"srt://encoder.example:10080?",
		"srt://encoder.example:10080?streamid=one&streamid=two",
		"srt://encoder.example:10080?streamid=publish&passphrase=leak",
		"srt://encoder.example:10080?streamid=publish&latency=120",
	} {
		t.Run(rawURL, func(t *testing.T) {
			if _, err := parseSRTDestination(rawURL, "stream-01", "job-scoped-passphrase-32-bytes-ok", 32, time.Second); !errors.Is(err, ErrInvalidIngestConfig) {
				t.Fatalf("error = %v, want ErrInvalidIngestConfig", err)
			}
		})
	}

	for _, passphrase := range []string{strings.Repeat("x", 31), strings.Repeat("x", 80)} {
		if _, err := parseSRTDestination("srt://encoder.example:10080", "stream-01", passphrase, 32, time.Second); !errors.Is(err, ErrInvalidIngestConfig) {
			t.Fatalf("passphrase length %d error = %v", len(passphrase), err)
		}
	}
	if _, err := parseSRTDestination("srt://encoder.example:10080", "stream-01", strings.Repeat("あ", 32), 32, time.Second); !errors.Is(err, ErrInvalidIngestConfig) {
		t.Fatalf("non-base64url passphrase error = %v", err)
	}
	for _, keyLength := range []int{0, 16, 24, 64} {
		if _, err := parseSRTDestination("srt://encoder.example:10080", "stream-01", "job-scoped-passphrase-32-bytes-ok", keyLength, time.Second); !errors.Is(err, ErrInvalidIngestConfig) {
			t.Fatalf("pbkeylen %d error = %v", keyLength, err)
		}
	}
	if _, err := parseSRTDestination("srt://encoder.example:10080", "", "job-scoped-passphrase-32-bytes-ok", 32, time.Second); !errors.Is(err, ErrInvalidIngestConfig) {
		t.Fatalf("blank stream_id error = %v", err)
	}
}

func TestGoSRTDialerUsesEncryptedCallerConfigWithoutLogging(t *testing.T) {
	var gotAddress string
	var gotOptions SRTOptions
	dial := goSRTDialer{
		dial: func(network, address string, options SRTOptions) (SRTConn, error) {
			if network != "srt" {
				t.Fatalf("network = %q", network)
			}
			gotAddress = address
			gotOptions = options
			return nopSRTConn{}, nil
		},
	}
	options := SRTOptions{
		Passphrase:        "job-scoped-passphrase-32-bytes-ok",
		PBKeylen:          32,
		StreamID:          "publish:stream-01",
		ConnectionTimeout: 2 * time.Second,
	}
	conn, err := dial.Dial(context.Background(), "encoder.example:10080", options)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if gotAddress != "encoder.example:10080" || gotOptions != options {
		t.Fatalf("dial options changed: address=%q options=%#v", gotAddress, gotOptions)
	}
}

type nopSRTConn struct{}

func (nopSRTConn) Write(p []byte) (int, error) { return len(p), nil }
func (nopSRTConn) Close() error                { return nil }
