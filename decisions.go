package niro

import "context"

// ── Decisions ───────────────────────────────────────────────────────────────

// DecisionProvider evaluates typed questions against content (state).
//
// Unlike [Provider] chat streaming, Decide is synchronous: one request in,
// one typed [DecisionResult] out. Question kinds are noul (boolean), choice,
// score, and box (bounding box).
//
// Built-in: provider/openrouter (Alpha), provider/openai (native /v1/decisions
// or chat fallback), provider/compat (chat; Ollama/vLLM).
// Custom: implement this interface or use [DecisionFunc] / [DecideViaChat].
type DecisionProvider interface {
	Decide(ctx context.Context, req *DecisionRequest) (*DecisionResult, error)
}

// DecisionFunc adapts a plain function to the [DecisionProvider] interface.
type DecisionFunc func(ctx context.Context, req *DecisionRequest) (*DecisionResult, error)

func (f DecisionFunc) Decide(ctx context.Context, req *DecisionRequest) (*DecisionResult, error) {
	return f(ctx, req)
}

// DecisionKind identifies a question or answer type.
type DecisionKind string

const (
	DecisionNoul    DecisionKind = "noul"
	DecisionChoice  DecisionKind = "choice"
	DecisionScore   DecisionKind = "score"
	DecisionBox     DecisionKind = "box"
	DecisionRefusal DecisionKind = "refusal"

	// DecisionPredicate is the OpenAI Decisions API name for [DecisionNoul].
	DecisionPredicate DecisionKind = DecisionNoul
)

// DecisionRequest contains everything needed for a Decisions call.
type DecisionRequest struct {
	// Model selects the decisions model (provider-specific slug).
	// If empty, the provider's default is used.
	Model string

	// State is plain-text content to evaluate.
	// Ignored when StateData is non-nil.
	State string

	// StateData is structured context (typically map[string]any or []any).
	// When set, it is sent instead of State.
	StateData any

	// Questions maps stable IDs to typed questions.
	Questions map[string]DecisionQuestion

	// Assets are optional images keyed by reference string. Reference a key
	// from State / instructions / criteria; format is "png", "jpeg", or "jpg".
	Assets map[string]DecisionAsset

	// SessionID groups related requests for observability (never sent upstream).
	SessionID string

	// User is an optional end-user identifier for abuse tracking.
	User string

	// Extra carries provider-specific configuration.
	Extra any
}

// DecisionQuestion is one typed evaluation against state.
type DecisionQuestion struct {
	Kind DecisionKind

	// Instructions describe what to decide (required for all kinds).
	Instructions string

	// TrueCriteria / FalseCriteria guide noul answers (optional).
	TrueCriteria  string
	FalseCriteria string

	// Criteria maps choice labels → guidance (required for choice).
	Criteria map[string]string

	// Levels are ordered score criteria from low→high (required for score).
	Levels []string
}

// DecisionAsset is an image referenced from state or question text.
type DecisionAsset struct {
	Data   []byte
	Format string // png, jpeg, jpg
}

// DecisionResult is the typed response for a Decisions call.
type DecisionResult struct {
	ID       string
	Model    string
	Provider string
	Answers  map[string]DecisionAnswer
	Usage    Usage
}

// DecisionAnswer is one typed answer (or a refusal).
type DecisionAnswer struct {
	Kind DecisionKind

	// Noul is P(true) in [0, 1] when Kind is DecisionNoul.
	Noul float64

	// Choice is the selected label when Kind is DecisionChoice.
	Choice string

	// Score is the numeric score when Kind is DecisionScore.
	Score float64

	// Box is the bounding box when Kind is DecisionBox.
	Box *BoundingBox

	// Confidence is set when the provider reports it.
	Confidence *float64

	// Probabilities are per-label / per-level probabilities when available.
	Probabilities map[string]float64

	// Legend maps score keys to human-readable labels when available.
	Legend map[string]string

	// Refused is true when the model declined the question.
	Refused bool
}

// BoundingBox is a normalized rectangle (fractions of the image).
type BoundingBox struct {
	Top    float64
	Bottom float64
	Left   float64
	Right  float64
}

// True reports whether a noul answer is at least 0.5.
func (a DecisionAnswer) True() bool {
	return a.Kind == DecisionNoul && !a.Refused && a.Noul >= 0.5
}

// ── Question constructors ───────────────────────────────────────────────────

// Noul builds a boolean (noul) question. trueCrit / falseCrit are optional.
func Noul(instructions, trueCrit, falseCrit string) DecisionQuestion {
	return DecisionQuestion{
		Kind:          DecisionNoul,
		Instructions:  instructions,
		TrueCriteria:  trueCrit,
		FalseCriteria: falseCrit,
	}
}

// Choice builds a multi-option question. criteria maps label → guidance.
func Choice(instructions string, criteria map[string]string) DecisionQuestion {
	return DecisionQuestion{
		Kind:         DecisionChoice,
		Instructions: instructions,
		Criteria:     criteria,
	}
}

// Score builds an ordered-score question. levels are low→high criteria strings.
func Score(instructions string, levels ...string) DecisionQuestion {
	return DecisionQuestion{
		Kind:         DecisionScore,
		Instructions: instructions,
		Levels:       levels,
	}
}

// Box builds a bounding-box question. instructions name the target (and image).
func Box(instructions string) DecisionQuestion {
	return DecisionQuestion{
		Kind:         DecisionBox,
		Instructions: instructions,
	}
}
