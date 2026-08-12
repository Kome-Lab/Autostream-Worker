package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/videoout"
)

type jobVideoOutput struct {
	manager *videoout.Manager
}

type jobVideoSource interface {
	videoout.FrameSource
	HandleVideoOutputFailure(streamID string, generation uint64)
}

func newJobVideoOutput(source jobVideoSource) jobs.VideoOutput {
	return jobVideoOutput{manager: videoout.NewManager(source, videoout.Options{
		OnFailure: source.HandleVideoOutputFailure,
	})}
}

func (o jobVideoOutput) Start(ctx context.Context, stream jobs.StreamContext, scene jobs.VideoSceneConfig) error {
	return o.manager.Start(ctx, videoout.Config{
		StreamID:     stream.StreamID,
		Generation:   scene.Generation,
		IngestURL:    stream.VideoIngestURL,
		Passphrase:   stream.VideoIngestPassphrase,
		PBKeylen:     stream.VideoIngestPBKeylen,
		Width:        scene.Width,
		Height:       scene.Height,
		FPS:          scene.FPS,
		FFmpegBinary: ffmpegBinaryFromEnv(),
	})
}

func (o jobVideoOutput) Stop(ctx context.Context, _ string) error {
	return o.manager.Stop(ctx)
}

func ffmpegBinaryFromEnv() string {
	binary := strings.TrimSpace(os.Getenv("FFMPEG_BIN"))
	if binary == "" {
		return "ffmpeg"
	}
	return binary
}

func requireFFmpegBinary(binary string) error {
	return requireFFmpegBinaryWithRunner(binary, runFFmpegPreflight)
}

type ffmpegPreflightRunner func(binary string, args []string, stdin io.Reader) error

func requireFFmpegBinaryWithRunner(binary string, run ffmpegPreflightRunner) error {
	binary = strings.TrimSpace(binary)
	if binary == "" {
		return errors.New("FFMPEG_BIN is empty")
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return errors.New("FFmpeg binary was not found")
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", "rgba", "-video_size", "2x2", "-framerate", "1", "-i", "pipe:0",
		"-frames:v", "1", "-an", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-f", "mpegts", "pipe:1",
	}
	if run == nil || run(resolved, args, bytes.NewReader(make([]byte, 2*2*4))) != nil {
		return errors.New("FFmpeg does not provide the required raw RGBA, libx264, and MPEG-TS pipeline")
	}
	return nil
}

func runFFmpegPreflight(binary string, args []string, stdin io.Reader) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = stdin
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}
