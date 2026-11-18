package niro

import "context"

// ── Image generation ────────────────────────────────────────────────────────

// ImageProvider generates images from text (and optional reference images).
//
// The returned Stream emits [KindImage] frames (one per generated image).
// Providers may also emit [KindUsage] and, when streaming partials are
// supported, intermediate [KindImage] frames marked via [CustomImagePartial].
//
// Built-in: provider/openrouter.
// Custom: implement this interface or use [ImageFunc].
type ImageProvider interface {
	GenerateImage(ctx context.Context, req *ImageRequest) (*Stream, error)
}

// ImageFunc adapts a plain function to the [ImageProvider] interface.
type ImageFunc func(ctx context.Context, req *ImageRequest) (*Stream, error)

func (f ImageFunc) GenerateImage(ctx context.Context, req *ImageRequest) (*Stream, error) {
	return f(ctx, req)
}

// ImageRequest contains everything needed for an image generation call.
type ImageRequest struct {
	// Prompt is the text description of the desired image.
	Prompt string

	// Model selects the image model (provider-specific slug).
	// If empty, the provider's default image model is used.
	Model string

	// N is the upper bound on images to generate (1–10). Providers may return fewer.
	// Zero means provider default (usually 1).
	N int

	// Size is a convenience dimension hint: tier ("2K", "4K") or pixels ("1024x1024").
	Size string

	// AspectRatio is a normalized ratio (e.g. "16:9", "1:1").
	AspectRatio string

	// OutputFormat is the desired encoding (e.g. "png", "jpeg", "webp", "svg").
	OutputFormat string

	// Quality is a rendering quality hint (e.g. "low", "medium", "high").
	Quality string

	// InputImages are optional reference images for image-to-image generation.
	// Use [ImagePart] / [ImageURL] parts.
	InputImages []Part

	// Stream requests progressive partial images when the upstream supports it.
	Stream bool

	// Extra carries provider-specific configuration.
	Extra any
}

// ── Video generation ────────────────────────────────────────────────────────

// VideoProvider generates videos from text (and optional reference media).
//
// The returned Stream emits [KindVideo] frames when outputs are ready.
// Async backends may emit [KindCustom] progress frames ([CustomVideoStatus])
// while polling.
//
// Built-in: provider/openrouter.
// Custom: implement this interface or use [VideoFunc].
type VideoProvider interface {
	GenerateVideo(ctx context.Context, req *VideoRequest) (*Stream, error)
}

// VideoFunc adapts a plain function to the [VideoProvider] interface.
type VideoFunc func(ctx context.Context, req *VideoRequest) (*Stream, error)

func (f VideoFunc) GenerateVideo(ctx context.Context, req *VideoRequest) (*Stream, error) {
	return f(ctx, req)
}

// VideoRequest contains everything needed for a video generation call.
type VideoRequest struct {
	// Prompt describes the video to generate.
	Prompt string

	// Model selects the video model (provider-specific slug).
	Model string

	// Duration is the desired length in seconds (provider-dependent).
	Duration int

	// Size is exact pixels ("1280x720") or a provider size hint.
	Size string

	// AspectRatio is a normalized ratio (e.g. "16:9").
	AspectRatio string

	// Resolution is a normalized tier (e.g. "720p", "1080p").
	Resolution string

	// GenerateAudio requests an audio track when supported.
	GenerateAudio bool

	// Extra carries provider-specific configuration.
	Extra any
}

// VideoStatus is progress metadata for async video jobs ([CustomVideoStatus]).
type VideoStatus struct {
	JobID  string
	Status string // pending, in_progress, completed, failed, …
	Error  string
}
