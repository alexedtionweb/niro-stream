package niro

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	stdjson "encoding/json"
	"fmt"
	"hash"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CacheMode defines cache intent semantics.
type CacheMode uint8

const (
	// CacheAuto lets the provider choose best-effort cache behavior.
	CacheAuto CacheMode = iota
	// CachePrefer requests cache usage but does not fail if unsupported.
	CachePrefer
	// CacheRequire fails the request if cache semantics cannot be applied.
	CacheRequire
	// CacheBypass explicitly disables cache usage for this request.
	CacheBypass
)

// CacheScope defines the logical cache scope.
type CacheScope uint8

const (
	// CacheScopePrefix caches reusable prompt prefixes.
	// This is the only stable provider-agnostic scope currently supported.
	CacheScopePrefix CacheScope = iota
)

// CacheOptions declares cache intent in a provider-agnostic way.
//
// Zero value means best-effort prefix cache with no explicit key/ttl hint.
type CacheOptions struct {
	Mode  CacheMode
	Key   string        // Optional deterministic key (namespaced by tenant/client)
	TTL   time.Duration // Hint only; providers may ignore
	Scope CacheScope
}

// CacheCapabilities describes what a provider can honor.
type CacheCapabilities struct {
	SupportsPrefix       bool
	SupportsExplicitKeys bool
	SupportsTTL          bool
	SupportsBypass       bool
}

// SupportsHint returns whether these capabilities can honor a require-level hint.
func (c CacheCapabilities) SupportsHint(h CacheHint) bool {
	if !c.SupportsPrefix {
		return false
	}
	if h.Key != "" && !c.SupportsExplicitKeys {
		return false
	}
	if h.TTL > 0 && !c.SupportsTTL {
		return false
	}
	return true
}

// CacheCapableProvider optionally exposes provider cache capabilities.
// This is additive and keeps the Provider interface backward-compatible.
type CacheCapableProvider interface {
	Provider
	CacheCaps() CacheCapabilities
}

// ProviderCacheCaps returns provider cache capabilities if exposed.
func ProviderCacheCaps(p Provider) CacheCapabilities {
	if cp, ok := p.(CacheCapableProvider); ok {
		return cp.CacheCaps()
	}
	return CacheCapabilities{}
}

// CacheHint is normalized cache metadata attached to request context.
// It is derived once per Runtime.Generate call and reused for retries.
type CacheHint struct {
	Mode       CacheMode
	Scope      CacheScope
	Key        string
	TTL        time.Duration
	PrefixHash string
}

// PrefixNormalizer creates deterministic bytes for prefix hashing.
// Implementations should avoid non-deterministic ordering.
type PrefixNormalizer interface {
	NormalizePrefix(req *Request) ([]byte, error)
}

type prefixHashWriter interface {
	WritePrefixHash(h hash.Hash, req *Request) error
}

// PrefixNormalizerFunc adapts a function to PrefixNormalizer.
type PrefixNormalizerFunc func(req *Request) ([]byte, error)

func (f PrefixNormalizerFunc) NormalizePrefix(req *Request) ([]byte, error) { return f(req) }

// DefaultPrefixNormalizer normalizes model + effective messages as JSON.
type DefaultPrefixNormalizer struct{}

func (DefaultPrefixNormalizer) NormalizePrefix(req *Request) ([]byte, error) {
	var b strings.Builder
	if err := writeCanonicalPrefix(&b, req); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func (DefaultPrefixNormalizer) WritePrefixHash(h hash.Hash, req *Request) error {
	return writePrefixHash(h, req)
}

// CacheEngine is an optional pluggable local cache extension point.
// Core runtime works without any engine; providers may consume this via context.
type CacheEngine interface {
	ResolvePrefixHash(ctx context.Context, req *Request, scope CacheScope) (key string, ok bool, err error)
	StorePrefix(ctx context.Context, key string, scope CacheScope, ttl time.Duration, meta map[string]string) error
	LookupPrefix(ctx context.Context, key string, scope CacheScope) (meta map[string]string, ok bool, err error)
}

type cacheCtxKey struct{}

type cacheContext struct {
	Hint   CacheHint
	Engine CacheEngine
}

// AttachCacheContext stores cache metadata in one context.WithValue call.
func AttachCacheContext(ctx context.Context, hint CacheHint, engine CacheEngine) context.Context {
	return context.WithValue(ctx, cacheCtxKey{}, cacheContext{Hint: hint, Engine: engine})
}

// WithCacheHint stores normalized cache metadata in context.
func WithCacheHint(ctx context.Context, hint CacheHint) context.Context {
	c := cacheContext{Hint: hint}
	if prev, ok := ctx.Value(cacheCtxKey{}).(cacheContext); ok {
		c.Engine = prev.Engine
	}
	return context.WithValue(ctx, cacheCtxKey{}, c)
}

// GetCacheHint retrieves normalized cache metadata from context.
func GetCacheHint(ctx context.Context) (CacheHint, bool) {
	c, ok := ctx.Value(cacheCtxKey{}).(cacheContext)
	if !ok {
		return CacheHint{}, false
	}
	return c.Hint, true
}

// WithCacheEngine stores an optional CacheEngine in context.
func WithCacheEngine(ctx context.Context, engine CacheEngine) context.Context {
	c := cacheContext{Engine: engine}
	if prev, ok := ctx.Value(cacheCtxKey{}).(cacheContext); ok {
		c.Hint = prev.Hint
	}
	return context.WithValue(ctx, cacheCtxKey{}, c)
}

// GetCacheEngine retrieves an optional CacheEngine from context.
func GetCacheEngine(ctx context.Context) (CacheEngine, bool) {
	c, ok := ctx.Value(cacheCtxKey{}).(cacheContext)
	if !ok || c.Engine == nil {
		return nil, false
	}
	return c.Engine, true
}

// NormalizeCacheOptions validates and normalizes cache metadata.
//
// Key ownership is enforced to avoid cross-tenant data leakage:
// finalKey = Request.Client + ":" + CacheOptions.Key.
func NormalizeCacheOptions(req *Request, normalizer PrefixNormalizer) (CacheHint, *Error) {
	if req == nil {
		return CacheHint{}, NewError(ErrCodeInvalidRequest, "request is nil")
	}

	opts := req.Options.Cache
	if opts == nil {
		return CacheHint{}, nil
	}

	if err := validateCacheOptions(opts); err != nil {
		return CacheHint{}, NewErrorf(ErrCodeInvalidRequest, "invalid cache options: %v", err)
	}

	mode := opts.Mode
	scope := opts.Scope
	h := CacheHint{
		Mode:  mode,
		Scope: scope,
		TTL:   opts.TTL,
	}
	if mode == CacheBypass {
		return h, nil
	}

	tenant := strings.TrimSpace(req.Client)
	if tenant == "" {
		return CacheHint{}, NewError(ErrCodeInvalidRequest, "cache requires Request.Client tenant namespace")
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "(default)"
	}
	userKey := strings.TrimSpace(opts.Key)

	if userKey != "" {
		h.Key = tenant + ":" + userKey
		return h, nil
	}

	if normalizer == nil {
		normalizer = DefaultPrefixNormalizer{}
	}

	hashValue, err := derivePrefixHash(req, normalizer, tenant, model)
	if err != nil {
		if mode == CacheRequire {
			return CacheHint{}, NewErrorf(ErrCodeInvalidRequest, "cache normalization failed: %v", err)
		}
		return h, nil
	}
	if hashValue == "" {
		return h, nil
	}

	h.PrefixHash = hashValue
	h.Key = tenant + ":" + hashValue
	return h, nil
}

func derivePrefixHash(req *Request, normalizer PrefixNormalizer, tenant, model string) (string, error) {
	hasher := &hashStringWriter{Hash: sha256.New()}

	if _, err := io.WriteString(hasher, tenant); err != nil {
		return "", err
	}
	if err := writeByte(hasher, ':'); err != nil {
		return "", err
	}
	if _, err := io.WriteString(hasher, model); err != nil {
		return "", err
	}
	if err := writeByte(hasher, ':'); err != nil {
		return "", err
	}

	if hw, ok := normalizer.(prefixHashWriter); ok {
		if err := hw.WritePrefixHash(hasher, req); err != nil {
			return "", err
		}
	} else {
		prefix, err := normalizer.NormalizePrefix(req)
		if err != nil {
			return "", err
		}
		if len(prefix) == 0 {
			return "", nil
		}
		if _, err := hasher.Write(prefix); err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func writePrefixHash(h hash.Hash, req *Request) error {
	if req == nil {
		return nil
	}
	if err := writeHashString(h, req.Model); err != nil {
		return err
	}
	msgs := req.EffectiveMessages()
	if err := writeHashUvarint(h, uint64(len(msgs))); err != nil {
		return err
	}
	for i := range msgs {
		if err := writePrefixHashMessage(h, &msgs[i]); err != nil {
			return err
		}
	}
	return nil
}

func writePrefixHashMessage(h hash.Hash, msg *Message) error {
	if err := writeHashString(h, string(msg.Role)); err != nil {
		return err
	}
	if err := writeHashUvarint(h, uint64(len(msg.Parts))); err != nil {
		return err
	}
	for i := range msg.Parts {
		if err := writePrefixHashPart(h, &msg.Parts[i]); err != nil {
			return err
		}
	}
	return nil
}

func writePrefixHashPart(h hash.Hash, part *Part) error {
	if err := writeHashUvarint(h, uint64(part.Kind)); err != nil {
		return err
	}
	if err := writeHashString(h, normalizeTextWhitespace(part.Text)); err != nil {
		return err
	}
	if err := writeHashString(h, part.Mime); err != nil {
		return err
	}
	if err := writeHashString(h, part.URL); err != nil {
		return err
	}
	if len(part.Data) > 0 {
		if err := writeHashBool(h, true); err != nil {
			return err
		}
		sum := sha256.Sum256(part.Data)
		if _, err := h.Write(sum[:]); err != nil {
			return err
		}
	} else {
		if err := writeHashBool(h, false); err != nil {
			return err
		}
	}

	if part.Tool != nil {
		if err := writeHashBool(h, true); err != nil {
			return err
		}
		if err := writeHashString(h, part.Tool.ID); err != nil {
			return err
		}
		if err := writeHashString(h, part.Tool.Name); err != nil {
			return err
		}
		if err := writeCanonicalJSONBytesHash(h, part.Tool.Args); err != nil {
			return err
		}
	} else {
		if err := writeHashBool(h, false); err != nil {
			return err
		}
	}

	if part.Result != nil {
		if err := writeHashBool(h, true); err != nil {
			return err
		}
		if err := writeHashString(h, part.Result.CallID); err != nil {
			return err
		}
		if err := writeHashString(h, normalizeTextWhitespace(part.Result.Content)); err != nil {
			return err
		}
		if err := writeHashBool(h, part.Result.IsError); err != nil {
			return err
		}
	} else {
		if err := writeHashBool(h, false); err != nil {
			return err
		}
	}
	return nil
}

func writeCanonicalPrefix(w io.Writer, req *Request) error {
	if req == nil {
		return nil
	}

	if _, err := io.WriteString(w, `{"model":`); err != nil {
		return err
	}
	if err := writeJSONString(w, req.Model); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `,"messages":[`); err != nil {
		return err
	}

	msgs := req.EffectiveMessages()
	for i, m := range msgs {
		if i > 0 {
			if err := writeByte(w, ','); err != nil {
				return err
			}
		}
		if err := writeCanonicalMessage(w, &m); err != nil {
			return err
		}
	}

	if _, err := io.WriteString(w, `]}`); err != nil {
		return err
	}
	return nil
}

func writeCanonicalMessage(w io.Writer, msg *Message) error {
	if _, err := io.WriteString(w, `{"role":`); err != nil {
		return err
	}
	if err := writeJSONString(w, string(msg.Role)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `,"parts":[`); err != nil {
		return err
	}

	for i := range msg.Parts {
		if i > 0 {
			if err := writeByte(w, ','); err != nil {
				return err
			}
		}
		if err := writeCanonicalPart(w, &msg.Parts[i]); err != nil {
			return err
		}
	}

	if _, err := io.WriteString(w, `]}`); err != nil {
		return err
	}
	return nil
}

func writeCanonicalPart(w io.Writer, part *Part) error {
	if _, err := io.WriteString(w, `{"kind":`); err != nil {
		return err
	}
	if _, err := io.WriteString(w, strconv.Itoa(int(part.Kind))); err != nil {
		return err
	}
	if part.Text != "" {
		if _, err := io.WriteString(w, `,"text":`); err != nil {
			return err
		}
		if err := writeJSONString(w, normalizeTextWhitespace(part.Text)); err != nil {
			return err
		}
	}
	if part.Mime != "" {
		if _, err := io.WriteString(w, `,"mime":`); err != nil {
			return err
		}
		if err := writeJSONString(w, part.Mime); err != nil {
			return err
		}
	}
	if part.URL != "" {
		if _, err := io.WriteString(w, `,"url":`); err != nil {
			return err
		}
		if err := writeJSONString(w, part.URL); err != nil {
			return err
		}
	}
	if len(part.Data) > 0 {
		sum := sha256.Sum256(part.Data)
		if _, err := io.WriteString(w, `,"data_sha256":"`); err != nil {
			return err
		}
		var enc [64]byte
		hex.Encode(enc[:], sum[:])
		if _, err := w.Write(enc[:]); err != nil {
			return err
		}
		if err := writeByte(w, '"'); err != nil {
			return err
		}
	}

	if part.Tool != nil {
		if _, err := io.WriteString(w, `,"tool":{`); err != nil {
			return err
		}
		first := true
		writeStringField := func(name, value string) error {
			if value == "" {
				return nil
			}
			if !first {
				if err := writeByte(w, ','); err != nil {
					return err
				}
			}
			first = false
			if err := writeJSONString(w, name); err != nil {
				return err
			}
			if err := writeByte(w, ':'); err != nil {
				return err
			}
			return writeJSONString(w, value)
		}
		if err := writeStringField("id", part.Tool.ID); err != nil {
			return err
		}
		if err := writeStringField("name", part.Tool.Name); err != nil {
			return err
		}
		if len(part.Tool.Args) > 0 {
			if !first {
				if err := writeByte(w, ','); err != nil {
					return err
				}
			}
			first = false
			if err := writeJSONString(w, "args"); err != nil {
				return err
			}
			if err := writeByte(w, ':'); err != nil {
				return err
			}
			if err := writeCanonicalJSONBytes(w, part.Tool.Args); err != nil {
				return err
			}
		}
		if err := writeByte(w, '}'); err != nil {
			return err
		}
	}

	if part.Result != nil {
		if _, err := io.WriteString(w, `,"result":{`); err != nil {
			return err
		}
		first := true
		writeField := func(name, value string) error {
			if value == "" {
				return nil
			}
			if !first {
				if err := writeByte(w, ','); err != nil {
					return err
				}
			}
			first = false
			if err := writeJSONString(w, name); err != nil {
				return err
			}
			if err := writeByte(w, ':'); err != nil {
				return err
			}
			return writeJSONString(w, value)
		}
		if err := writeField("call_id", part.Result.CallID); err != nil {
			return err
		}
		if err := writeField("content", normalizeTextWhitespace(part.Result.Content)); err != nil {
			return err
		}
		if part.Result.IsError {
			if !first {
				if err := writeByte(w, ','); err != nil {
					return err
				}
			}
			if _, err := io.WriteString(w, `"is_error":true`); err != nil {
				return err
			}
		}
		if err := writeByte(w, '}'); err != nil {
			return err
		}
	}

	if err := writeByte(w, '}'); err != nil {
		return err
	}
	return nil
}

func writeCanonicalJSONBytes(w io.Writer, raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		if _, err := io.WriteString(w, "null"); err != nil {
			return err
		}
		return nil
	}

	var value any
	if err := stdjson.Unmarshal(trimmed, &value); err != nil {
		return err
	}
	return writeCanonicalJSONValue(w, value)
}

func writeCanonicalJSONBytesHash(h hash.Hash, raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return writeHashBool(h, false)
	}
	if err := writeHashBool(h, true); err != nil {
		return err
	}

	var value any
	if err := stdjson.Unmarshal(trimmed, &value); err != nil {
		return err
	}
	return writeCanonicalJSONValueHash(h, value)
}

func writeCanonicalJSONValue(w io.Writer, value any) error {
	switch v := value.(type) {
	case nil:
		_, err := io.WriteString(w, "null")
		return err
	case bool:
		if v {
			_, err := io.WriteString(w, "true")
			return err
		}
		_, err := io.WriteString(w, "false")
		return err
	case float64:
		var buf [32]byte
		b := strconv.AppendFloat(buf[:0], v, 'g', -1, 64)
		_, err := w.Write(b)
		return err
	case string:
		return writeJSONString(w, v)
	case []any:
		if err := writeByte(w, '['); err != nil {
			return err
		}
		for i := range v {
			if i > 0 {
				if err := writeByte(w, ','); err != nil {
					return err
				}
			}
			if err := writeCanonicalJSONValue(w, v[i]); err != nil {
				return err
			}
		}
		return writeByte(w, ']')
	case map[string]any:
		if err := writeByte(w, '{'); err != nil {
			return err
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if i > 0 {
				if err := writeByte(w, ','); err != nil {
					return err
				}
			}
			if err := writeJSONString(w, key); err != nil {
				return err
			}
			if err := writeByte(w, ':'); err != nil {
				return err
			}
			if err := writeCanonicalJSONValue(w, v[key]); err != nil {
				return err
			}
		}
		return writeByte(w, '}')
	default:
		b, err := stdjson.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}
}

func writeCanonicalJSONValueHash(h hash.Hash, value any) error {
	switch v := value.(type) {
	case nil:
		return writeByte(h, 'n')
	case bool:
		if err := writeByte(h, 'b'); err != nil {
			return err
		}
		return writeHashBool(h, v)
	case float64:
		if err := writeByte(h, 'f'); err != nil {
			return err
		}
		return writeHashUint64(h, math.Float64bits(v))
	case string:
		if err := writeByte(h, 's'); err != nil {
			return err
		}
		return writeHashString(h, v)
	case []any:
		if err := writeByte(h, 'a'); err != nil {
			return err
		}
		if err := writeHashUvarint(h, uint64(len(v))); err != nil {
			return err
		}
		for i := range v {
			if err := writeCanonicalJSONValueHash(h, v[i]); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if err := writeByte(h, 'm'); err != nil {
			return err
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if err := writeHashUvarint(h, uint64(len(keys))); err != nil {
			return err
		}
		for _, key := range keys {
			if err := writeHashString(h, key); err != nil {
				return err
			}
			if err := writeCanonicalJSONValueHash(h, v[key]); err != nil {
				return err
			}
		}
		return nil
	default:
		b, err := stdjson.Marshal(v)
		if err != nil {
			return err
		}
		if err := writeByte(h, 'r'); err != nil {
			return err
		}
		if err := writeHashUvarint(h, uint64(len(b))); err != nil {
			return err
		}
		_, err = h.Write(b)
		return err
	}
}

func writeJSONString(w io.Writer, s string) error {
	b, err := stdjson.Marshal(s)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func writeByte(w io.Writer, b byte) error {
	var buf [1]byte
	buf[0] = b
	_, err := w.Write(buf[:])
	return err
}

func writeHashBool(h hash.Hash, v bool) error {
	if v {
		return writeByte(h, 1)
	}
	return writeByte(h, 0)
}

func writeHashString(h hash.Hash, s string) error {
	if err := writeHashUvarint(h, uint64(len(s))); err != nil {
		return err
	}
	if len(s) == 0 {
		return nil
	}
	_, err := io.WriteString(h, s)
	return err
}

func writeHashUvarint(h hash.Hash, value uint64) error {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], value)
	_, err := h.Write(buf[:n])
	return err
}

func writeHashUint64(h hash.Hash, value uint64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], value)
	_, err := h.Write(buf[:])
	return err
}

type hashStringWriter struct {
	hash.Hash
	scratch []byte
}

func (h *hashStringWriter) WriteString(s string) (int, error) {
	if len(s) == 0 {
		return 0, nil
	}
	if cap(h.scratch) < len(s) {
		h.scratch = make([]byte, len(s))
	}
	b := h.scratch[:len(s)]
	copy(b, s)
	_, err := h.Hash.Write(b)
	return len(s), err
}

func normalizeTextWhitespace(value string) string {
	if value == "" {
		return value
	}
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.TrimSpace(value)
}

func validateCacheOptions(c *CacheOptions) error {
	if c == nil {
		return nil
	}

	switch c.Mode {
	case CacheAuto, CachePrefer, CacheRequire, CacheBypass:
	default:
		return fmt.Errorf("mode must be one of auto|prefer|require|bypass")
	}

	// Scope API is intentionally conservative for now.
	if c.Scope != CacheScopePrefix {
		return fmt.Errorf("scope %d is not supported", c.Scope)
	}

	if c.TTL < 0 {
		return fmt.Errorf("ttl must be >= 0")
	}

	if strings.Contains(c.Key, ":") {
		return fmt.Errorf("key must not contain ':' (tenant namespace is prepended by runtime)")
	}

	return nil
}

// SetCacheUsageDetail writes canonical provider-agnostic cache metrics.
func SetCacheUsageDetail(u *Usage, attempted, hit, write bool, cachedInputTokens int, latencySavedMS int) {
	if u == nil {
		return
	}
	if !attempted && !hit && !write && cachedInputTokens == 0 && latencySavedMS == 0 {
		return
	}
	if u.Detail == nil {
		u.Detail = make(map[string]int, 8)
	}
	if attempted {
		u.Detail[UsageCacheAttempted] = 1
	} else {
		u.Detail[UsageCacheAttempted] = 0
	}
	if hit {
		u.Detail[UsageCacheHit] = 1
	} else {
		u.Detail[UsageCacheHit] = 0
	}
	if write {
		u.Detail[UsageCacheWrite] = 1
	} else {
		u.Detail[UsageCacheWrite] = 0
	}
	u.Detail[UsageCachedInputTokens] = cachedInputTokens
	u.Detail[UsageCacheLatencySavedMS] = latencySavedMS
}
