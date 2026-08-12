package main

import (
	"io"
	"os"
	"slices"
	"testing"
)

func TestFFmpegBinaryFromEnv(t *testing.T) {
	t.Setenv("FFMPEG_BIN", "")
	if got := ffmpegBinaryFromEnv(); got != "ffmpeg" {
		t.Fatalf("default binary = %q", got)
	}
	t.Setenv("FFMPEG_BIN", "  /opt/autostream/ffmpeg  ")
	if got := ffmpegBinaryFromEnv(); got != "/opt/autostream/ffmpeg" {
		t.Fatalf("configured binary = %q", got)
	}
}

func TestRequireFFmpegBinaryUsesExecutableLookup(t *testing.T) {
	var gotArgs []string
	var gotInput []byte
	runner := func(_ string, args []string, stdin io.Reader) error {
		gotArgs = append([]string(nil), args...)
		var err error
		gotInput, err = io.ReadAll(stdin)
		return err
	}
	if err := requireFFmpegBinaryWithRunner(os.Args[0], runner); err != nil {
		t.Fatalf("current executable should be discoverable: %v", err)
	}
	for _, required := range []string{"rawvideo", "rgba", "libx264", "mpegts", "pipe:1"} {
		if !slices.Contains(gotArgs, required) {
			t.Fatalf("preflight args do not require %q: %v", required, gotArgs)
		}
	}
	if len(gotInput) != 2*2*4 {
		t.Fatalf("preflight raw frame bytes = %d", len(gotInput))
	}
	if err := requireFFmpegBinaryWithRunner("autostream-definitely-missing-ffmpeg-binary", runner); err == nil {
		t.Fatal("missing binary was accepted")
	}
}

func TestRequireFFmpegBinaryFailsIfCodecOrMuxerPreflightFails(t *testing.T) {
	err := requireFFmpegBinaryWithRunner(os.Args[0], func(string, []string, io.Reader) error {
		return io.ErrUnexpectedEOF
	})
	if err == nil {
		t.Fatal("failed libx264/MPEG-TS preflight was accepted")
	}
}
