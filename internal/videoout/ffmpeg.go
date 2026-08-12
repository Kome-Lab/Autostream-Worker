package videoout

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() error
	Kill() error
}

type ProcessFactory interface {
	Start(binary string, args []string) (Process, error)
}

type execProcessFactory struct{}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

func (execProcessFactory) Start(binary string, args []string) (Process, error) {
	cmd := exec.Command(binary, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}
	return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

func (p *execProcess) Stdin() io.WriteCloser { return p.stdin }

func (p *execProcess) Stdout() io.ReadCloser { return p.stdout }

func (p *execProcess) Stderr() io.ReadCloser { return p.stderr }

func (p *execProcess) Wait() error { return p.cmd.Wait() }

func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func buildFFmpegArgs(config Config) []string {
	bitrate := strconv.Itoa(config.BitrateKbps) + "k"
	buffer := strconv.Itoa(config.BitrateKbps*2) + "k"
	gop := strconv.Itoa(config.FPS * 2)
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-f", "rawvideo",
		"-pixel_format", "rgba",
		"-video_size", fmt.Sprintf("%dx%d", config.Width, config.Height),
		"-framerate", strconv.Itoa(config.FPS),
		"-i", "pipe:0",
		"-map", "0:v:0",
		"-an",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		"-b:v", bitrate,
		"-minrate", bitrate,
		"-maxrate", bitrate,
		"-bufsize", buffer,
		"-r", strconv.Itoa(config.FPS),
		"-fps_mode", "cfr",
		"-g", gop,
		"-keyint_min", gop,
		"-sc_threshold", "0",
		"-x264-params", "nal-hrd=cbr:force-cfr=1:repeat-headers=1:open-gop=0",
		"-mpegts_flags", "+resend_headers",
		"-muxdelay", "0",
		"-muxpreload", "0",
		"-flush_packets", "1",
		"-f", "mpegts",
		"pipe:1",
	}
}

func defaultBitrateKbps(width, height int) int {
	pixels := width * height
	switch {
	case pixels >= 1920*1080:
		return 8000
	case pixels >= 1280*720:
		return 4500
	default:
		return 2500
	}
}
