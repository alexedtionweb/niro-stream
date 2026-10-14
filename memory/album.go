package memory

import (
	"context"
	"sync"

	"github.com/alexedtionweb/niro-stream"
)

// Album turns a record Ref into the image or video a vision model should see.
// The store keeps the pointer. The album keeps the bytes.
type Album interface {
	Open(ctx context.Context, ref string) (niro.Part, bool, error)
}

// Reel is an in-process album. cap is the most frames kept; zero keeps them all.
// Putting the same ref again replaces that frame and makes it the newest.
type Reel struct {
	mu    sync.Mutex
	cap   int
	order []string
	shot  map[string]niro.Part
}

// NewReel returns an in-process album.
func NewReel(cap int) *Reel {
	return &Reel{cap: cap, shot: make(map[string]niro.Part)}
}

// Put stores a copy of an image or video frame under ref.
func (r *Reel) Put(ref string, part niro.Part) error {
	if r == nil {
		return niro.NewError(niro.ErrCodeInvalidRequest, "memory album is nil")
	}
	if ref == "" {
		return niro.NewError(niro.ErrCodeInvalidRequest, "media ref is empty")
	}
	if part.Kind != niro.KindImage && part.Kind != niro.KindVideo {
		return niro.NewError(niro.ErrCodeInvalidRequest, "media must be an image or video")
	}
	if len(part.Data) == 0 && part.URL == "" {
		return niro.NewError(niro.ErrCodeInvalidRequest, "media is empty")
	}
	part = clonePart(part)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.shot[ref]; ok {
		r.order = drop(r.order, ref)
	}
	r.shot[ref] = part
	r.order = append(r.order, ref)
	if r.cap > 0 {
		for len(r.order) > r.cap {
			old := r.order[0]
			r.order = r.order[1:]
			delete(r.shot, old)
		}
	}
	return nil
}

// Open returns a copy of the frame stored at ref.
func (r *Reel) Open(_ context.Context, ref string) (niro.Part, bool, error) {
	if r == nil {
		return niro.Part{}, false, niro.NewError(niro.ErrCodeInvalidRequest, "memory album is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	part, ok := r.shot[ref]
	if !ok {
		return niro.Part{}, false, nil
	}
	return clonePart(part), true, nil
}

func clonePart(p niro.Part) niro.Part {
	if p.Data != nil {
		p.Data = append([]byte(nil), p.Data...)
	}
	return p
}

func drop(order []string, ref string) []string {
	kept := order[:0]
	for _, id := range order {
		if id != ref {
			kept = append(kept, id)
		}
	}
	return kept
}
