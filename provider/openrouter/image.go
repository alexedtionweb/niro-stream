package openrouter

import (
	"context"
	"encoding/base64"
	"strings"

	orsdk "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/types/stream"

	"github.com/alexedtionweb/niro-stream"
)

const defaultImageModel = "recraft/recraft-v4.1-flash-v2:free"

// ImageRequestHook allows modifying the raw SDK image request before it is sent.
type ImageRequestHook func(req *components.ImageGenerationRequest)

// GenerateImage implements [niro.ImageProvider].
//
// Calls POST /api/v1/images. Emits one [niro.KindImage] frame per result
// (decoded from b64_json). When req.Stream is true and the upstream supports
// SSE, partial previews are emitted as KindImage with [niro.CustomImagePartial].
func (p *Provider) GenerateImage(ctx context.Context, req *niro.ImageRequest) (*niro.Stream, error) {
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "nil request")
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "empty prompt")
	}

	model := req.Model
	if model == "" {
		model = p.imageModel
	}
	if model == "" {
		model = defaultImageModel
	}

	params := components.ImageGenerationRequest{
		Model:  model,
		Prompt: prompt,
		Stream: orsdk.Pointer(req.Stream),
	}
	if req.N > 0 {
		params.N = orsdk.Pointer(int64(req.N))
	}
	if req.Size != "" {
		params.Size = orsdk.Pointer(req.Size)
	}
	if req.AspectRatio != "" {
		ar := components.ImageGenerationRequestAspectRatio(req.AspectRatio)
		params.AspectRatio = &ar
	}
	if req.OutputFormat != "" {
		of := components.ImageGenerationRequestOutputFormat(req.OutputFormat)
		params.OutputFormat = &of
	}
	if req.Quality != "" {
		q := components.ImageGenerationRequestQuality(req.Quality)
		params.Quality = &q
	}
	for _, part := range req.InputImages {
		if ref := contentPartImage(part); ref != nil {
			params.InputReferences = append(params.InputReferences, *ref)
		}
	}

	for _, h := range p.imageHooks {
		h(&params)
	}
	if hook, ok := req.Extra.(ImageRequestHook); ok {
		hook(&params)
	}

	res, err := p.client.Images.Generate(ctx, params)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "openrouter images", err).WithProvider("openrouter")
	}
	if res == nil {
		return nil, niro.NewError(niro.ErrCodeProviderError, "openrouter: empty image response").WithProvider("openrouter")
	}

	stream, emitter := niro.NewStream(niro.DefaultStreamBuffer)
	if res.EventStream != nil {
		go consumeImageStream(ctx, res.EventStream, emitter)
		return stream, nil
	}
	if res.ImageGenerationResponse == nil {
		emitter.Close()
		return nil, niro.NewError(niro.ErrCodeProviderError, "openrouter: no image data").WithProvider("openrouter")
	}
	go emitBufferedImages(ctx, res.ImageGenerationResponse, emitter)
	return stream, nil
}

func emitBufferedImages(ctx context.Context, resp *components.ImageGenerationResponse, out *niro.Emitter) {
	defer out.Close()

	for _, item := range resp.Data {
		f, err := imageFrameFromB64(item.B64JSON, mimeFromPtr(item.MediaType), false)
		if err != nil {
			out.Error(err)
			return
		}
		if err := out.Emit(ctx, f); err != nil {
			return
		}
	}
	if resp.Usage != nil {
		_ = out.Emit(ctx, niro.UsageFrame(imageUsage(resp.Usage)))
	}
	out.SetResponse(&niro.ResponseMeta{
		Model: "",
		Usage: derefUsage(imageUsage(resp.Usage)),
	})
}

func consumeImageStream(ctx context.Context, sdk *stream.EventStream[components.ImageStreamingResponse], out *niro.Emitter) {
	defer out.Close()
	defer sdk.Close()

	var finalUsage *niro.Usage

	for sdk.Next() {
		ev := sdk.Value()
		if ev == nil {
			continue
		}
		data := ev.Data
		switch data.Type {
		case components.ImageStreamingResponseDataTypeImageGenerationPartialImage:
			if data.ImageGenPartialImageEvent == nil {
				continue
			}
			f, err := imageFrameFromB64(data.ImageGenPartialImageEvent.B64JSON, "image/png", true)
			if err != nil {
				out.Error(err)
				return
			}
			if err := out.Emit(ctx, f); err != nil {
				return
			}

		case components.ImageStreamingResponseDataTypeImageGenerationCompleted:
			if data.ImageGenCompletedEvent == nil {
				continue
			}
			done := data.ImageGenCompletedEvent
			f, err := imageFrameFromB64(done.B64JSON, mimeFromPtr(done.MediaType), false)
			if err != nil {
				out.Error(err)
				return
			}
			if err := out.Emit(ctx, f); err != nil {
				return
			}
			if done.Usage != nil {
				finalUsage = imageUsage(done.Usage)
				_ = out.Emit(ctx, niro.UsageFrame(finalUsage))
			}

		case components.ImageStreamingResponseDataTypeError:
			msg := "image stream error"
			if data.ImageGenStreamErrorEvent != nil {
				msg = data.ImageGenStreamErrorEvent.Error.Message
			}
			out.Error(niro.NewError(niro.ErrCodeProviderError, "openrouter: "+msg).WithProvider("openrouter"))
			return

		case components.ImageStreamingResponseDataTypeImageGenerationTextChunk:
			// Optional caption/text while generating — surface as KindText.
			if data.ImageGenTextChunkEvent != nil && data.ImageGenTextChunkEvent.Text != "" {
				_ = out.Emit(ctx, niro.TextFrame(data.ImageGenTextChunkEvent.Text))
			}
		}
	}

	if err := sdk.Err(); err != nil {
		out.Error(niro.WrapError(niro.ErrCodeStreamError, "image stream", err))
		return
	}
	if finalUsage != nil {
		out.SetResponse(&niro.ResponseMeta{Usage: *finalUsage})
	}
}

func imageFrameFromB64(b64, mime string, partial bool) (niro.Frame, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		// Some gateways omit padding; try RawStdEncoding.
		raw, err = base64.RawStdEncoding.DecodeString(b64)
		if err != nil {
			return niro.Frame{}, niro.WrapError(niro.ErrCodeProviderError, "decode image", err).WithProvider("openrouter")
		}
	}
	if mime == "" {
		mime = "image/png"
	}
	f := niro.ImageFrame(raw, mime)
	if partial {
		f.Custom = &niro.ExperimentalFrame{Type: niro.CustomImagePartial, Data: true}
	}
	return f, nil
}

func contentPartImage(part niro.Part) *components.ContentPartImage {
	if part.Kind != niro.KindImage {
		return nil
	}
	url := part.URL
	if url == "" && len(part.Data) > 0 {
		mime := part.Mime
		if mime == "" {
			mime = "image/png"
		}
		url = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(part.Data)
	}
	if url == "" {
		return nil
	}
	return &components.ContentPartImage{
		Type:     components.ContentPartImageTypeImageURL,
		ImageURL: components.ContentPartImageImageURL{URL: url},
	}
}

func imageUsage(u *components.ImageGenerationUsage) *niro.Usage {
	if u == nil {
		return &niro.Usage{}
	}
	usage := &niro.Usage{
		InputTokens:  int(u.PromptTokens),
		OutputTokens: int(u.CompletionTokens),
		TotalTokens:  int(u.TotalTokens),
	}
	if cost, ok := u.Cost.Get(); ok && cost != nil {
		if usage.Detail == nil {
			usage.Detail = map[string]int{}
		}
		usage.Detail[niro.UsageCostMicros] = int(*cost * 1e6)
	}
	return usage
}

func mimeFromPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefUsage(u *niro.Usage) niro.Usage {
	if u == nil {
		return niro.Usage{}
	}
	return *u
}
