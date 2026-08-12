package videoout

import (
	"strings"
	"testing"
)

func TestBuildFFmpegArgsAreVideoOnlyCBRAndContainNoIngestSecrets(t *testing.T) {
	cfg := Config{
		Width:          1920,
		Height:         1080,
		FPS:            60,
		BitrateKbps:    8000,
		IngestURL:      "srt://encoder.example:10080",
		Passphrase:     "super-secret-passphrase-32-bytes",
		PBKeylen:       32,
		FFmpegBinary:   "ffmpeg",
		StartupTimeout: 1,
	}

	args := buildFFmpegArgs(cfg)
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{cfg.IngestURL, cfg.Passphrase, "encoder.example", "srt://"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("ffmpeg argv leaked ingest material %q: %s", forbidden, joined)
		}
	}
	for _, required := range []string{
		"-f rawvideo",
		"-pixel_format rgba",
		"-video_size 1920x1080",
		"-framerate 60",
		"-i pipe:0",
		"-map 0:v:0",
		"-an",
		"-b:v 8000k",
		"-minrate 8000k",
		"-maxrate 8000k",
		"-bufsize 16000k",
		"-f mpegts",
		"pipe:1",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("ffmpeg argv missing %q: %s", required, joined)
		}
	}
}

func TestDefaultBitrateScalesByFrameSize(t *testing.T) {
	for _, test := range []struct {
		width, height int
		want          int
	}{
		{1920, 1080, 8000},
		{1280, 720, 4500},
		{854, 480, 2500},
	} {
		if got := defaultBitrateKbps(test.width, test.height); got != test.want {
			t.Fatalf("defaultBitrateKbps(%d, %d) = %d, want %d", test.width, test.height, got, test.want)
		}
	}
}
