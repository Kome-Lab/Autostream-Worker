package main

import (
	"context"

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
		StreamID:   stream.StreamID,
		Generation: scene.Generation,
		IngestURL:  stream.VideoIngestURL,
		Passphrase: stream.VideoIngestPassphrase,
		PBKeylen:   stream.VideoIngestPBKeylen,
		Width:      scene.Width,
		Height:     scene.Height,
	})
}

func (o jobVideoOutput) Stop(ctx context.Context, _ string) error {
	return o.manager.Stop(ctx)
}
