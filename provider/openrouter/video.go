package openrouter

import (
	"context"
	"io"
	"strings"
	"time"

	orsdk "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"

	"github.com/alexedtionweb/niro-stream"
)

const (
	defaultVideoModel   = "google/veo-3"
	defaultVideoPollEvery = 2 * time.Second
)

// VideoRequestHook allows modifying the raw SDK video request before it is sent.
type VideoRequestHook func(req *components.VideoGenerationRequest)

// GenerateVideo implements [niro.VideoProvider].
//
// Submits POST /api/v1/videos, polls until completion, downloads content via
// GetVideoContent, and emits [niro.KindVideo] frames. Progress is surfaced as
// [niro.CustomVideoStatus] frames while waiting.
func (p *Provider) GenerateVideo(ctx context.Context, req *niro.VideoRequest) (*niro.Stream, error) {
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "nil request")
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "empty prompt")
	}

	model := req.Model
	if model == "" {
		model = p.videoModel
	}
	if model == "" {
		model = defaultVideoModel
	}

	params := components.VideoGenerationRequest{
		Model:  model,
		Prompt: orsdk.Pointer(prompt),
	}
	if req.Duration > 0 {
		params.Duration = orsdk.Pointer(int64(req.Duration))
	}
	if req.Size != "" {
		params.Size = orsdk.Pointer(req.Size)
	}
	if req.AspectRatio != "" {
		ar := components.VideoGenerationRequestAspectRatio(req.AspectRatio)
		params.AspectRatio = &ar
	}
	if req.Resolution != "" {
		res := components.VideoGenerationRequestResolution(req.Resolution)
		params.Resolution = &res
	}
	if req.GenerateAudio {
		params.GenerateAudio = orsdk.Pointer(true)
	}

	for _, h := range p.videoHooks {
		h(&params)
	}
	if hook, ok := req.Extra.(VideoRequestHook); ok {
		hook(&params)
	}

	job, err := p.client.VideoGeneration.Generate(ctx, params)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "openrouter videos", err).WithProvider("openrouter")
	}
	if job == nil || job.ID == "" {
		return nil, niro.NewError(niro.ErrCodeProviderError, "openrouter: empty video job").WithProvider("openrouter")
	}

	stream, emitter := niro.NewStream(niro.DefaultStreamBuffer)
	go p.pollVideo(ctx, job.ID, emitter)
	return stream, nil
}

func (p *Provider) pollVideo(ctx context.Context, jobID string, out *niro.Emitter) {
	defer out.Close()

	_ = out.Emit(ctx, niro.CustomFrame(&niro.ExperimentalFrame{
		Type: niro.CustomVideoStatus,
		Data: niro.VideoStatus{JobID: jobID, Status: string(components.VideoGenerationResponseStatusPending)},
	}))

	ticker := time.NewTicker(defaultVideoPollEvery)
	defer ticker.Stop()

	for {
		job, err := p.client.VideoGeneration.GetGeneration(ctx, jobID)
		if err != nil {
			out.Error(niro.WrapError(niro.ErrCodeProviderError, "poll video", err).WithProvider("openrouter"))
			return
		}
		if job == nil {
			out.Error(niro.NewError(niro.ErrCodeProviderError, "openrouter: nil video job").WithProvider("openrouter"))
			return
		}

		status := string(job.Status)
		errMsg := ""
		if job.Error != nil {
			errMsg = *job.Error
		}
		_ = out.Emit(ctx, niro.CustomFrame(&niro.ExperimentalFrame{
			Type: niro.CustomVideoStatus,
			Data: niro.VideoStatus{JobID: job.ID, Status: status, Error: errMsg},
		}))

		switch job.Status {
		case components.VideoGenerationResponseStatusCompleted:
			if err := p.emitVideoOutputs(ctx, job, out); err != nil {
				out.Error(err)
			}
			return

		case components.VideoGenerationResponseStatusFailed,
			components.VideoGenerationResponseStatusCancelled,
			components.VideoGenerationResponseStatusExpired:
			msg := status
			if errMsg != "" {
				msg = errMsg
			}
			out.Error(niro.NewError(niro.ErrCodeProviderError, "openrouter video: "+msg).WithProvider("openrouter"))
			return
		}

		select {
		case <-ctx.Done():
			out.Error(niro.WrapError(niro.ErrCodeContextCancelled, "video", ctx.Err()))
			return
		case <-ticker.C:
		}
	}
}

func (p *Provider) emitVideoOutputs(ctx context.Context, job *components.VideoGenerationResponse, out *niro.Emitter) error {
	// Prefer downloading via authenticated content API so we own the bytes.
	for i := 0; ; i++ {
		idx := optionalnullable.From(orsdk.Pointer(int64(i)))
		body, err := p.client.VideoGeneration.GetVideoContent(ctx, job.ID, idx)
		if err != nil {
			if i == 0 {
				return niro.WrapError(niro.ErrCodeProviderError, "download video", err).WithProvider("openrouter")
			}
			break
		}
		data, readErr := io.ReadAll(body)
		_ = body.Close()
		if readErr != nil {
			return niro.WrapError(niro.ErrCodeStreamError, "read video", readErr).WithProvider("openrouter")
		}
		if len(data) == 0 {
			break
		}
		if err := out.Emit(ctx, niro.VideoFrame(data, "video/mp4")); err != nil {
			return err
		}
	}

	if job.Usage != nil {
		u := &niro.Usage{}
		if cost, ok := job.Usage.Cost.Get(); ok && cost != nil {
			u.Detail = map[string]int{niro.UsageCostMicros: int(*cost * 1e6)}
		}
		_ = out.Emit(ctx, niro.UsageFrame(u))
		out.SetResponse(&niro.ResponseMeta{ID: job.ID, Usage: *u})
	} else {
		out.SetResponse(&niro.ResponseMeta{ID: job.ID})
	}
	return nil
}
