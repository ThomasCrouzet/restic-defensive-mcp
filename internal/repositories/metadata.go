package repositories

import (
	"context"
	"strings"
	"sync"
	"time"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

// MetadataObservation describes whether a metadata result came from the
// optional in-memory cache and when it was observed.
type MetadataObservation struct {
	Enabled    bool
	Hit        bool
	ObservedAt time.Time
	AgeSeconds int64
	TTLSeconds int64
}

type metadataEntry struct {
	result   *restic.Result
	observed time.Time
}

type metadataCall struct {
	ctx    context.Context
	done   chan struct{}
	result *restic.Result
	err    error
	refs   int
	cancel context.CancelFunc
}

type metadataCoalescer struct {
	mu    sync.Mutex
	calls map[string]*metadataCall
	cache map[string]metadataEntry
	ttl   time.Duration
	max   int
	now   func() time.Time
}

func newMetadataCoalescer(ttl time.Duration, max int) *metadataCoalescer {
	if ttl <= 0 {
		max = 0
	}
	return &metadataCoalescer{
		calls: make(map[string]*metadataCall),
		cache: make(map[string]metadataEntry),
		ttl:   ttl,
		max:   max,
		now:   time.Now,
	}
}

func metadataKey(repoID string, argv []string) string {
	return repoID + "\x00" + strings.Join(argv, "\x00")
}

func (m *metadataCoalescer) cacheGet(key string) (*restic.Result, time.Time, bool) {
	if m.ttl <= 0 {
		return nil, time.Time{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.cache[key]
	if !ok {
		return nil, time.Time{}, false
	}
	age := m.now().Sub(entry.observed)
	if age < 0 || age >= m.ttl {
		delete(m.cache, key)
		return nil, time.Time{}, false
	}
	return copyResult(entry.result), entry.observed, true
}

func (m *metadataCoalescer) cachePut(key string, result *restic.Result) {
	if m.ttl <= 0 || result == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for cacheKey, entry := range m.cache {
		if now.Sub(entry.observed) >= m.ttl {
			delete(m.cache, cacheKey)
		}
	}
	for len(m.cache) >= m.max {
		var oldestKey string
		var oldest time.Time
		for cacheKey, entry := range m.cache {
			if oldestKey == "" || entry.observed.Before(oldest) {
				oldestKey = cacheKey
				oldest = entry.observed
			}
		}
		if oldestKey == "" {
			break
		}
		delete(m.cache, oldestKey)
	}
	m.cache[key] = metadataEntry{result: copyResult(result), observed: now}
}

func (m *metadataCoalescer) join(key string, parent context.Context) (*metadataCall, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if call, ok := m.calls[key]; ok {
		call.refs++
		return call, false
	}
	shared, cancel := context.WithCancel(context.WithoutCancel(parent))
	call := &metadataCall{
		ctx:    shared,
		done:   make(chan struct{}),
		refs:   1,
		cancel: cancel,
	}
	m.calls[key] = call
	return call, true
}

func (m *metadataCoalescer) release(key string, call *metadataCall) {
	m.mu.Lock()
	defer m.mu.Unlock()
	call.refs--
	if call.refs > 0 {
		return
	}
	select {
	case <-call.done:
		return
	default:
	}
	if m.calls[key] == call {
		delete(m.calls, key)
	}
	call.cancel()
}

func (m *metadataCoalescer) finish(key string, call *metadataCall, result *restic.Result, err error) {
	m.mu.Lock()
	call.result = copyResult(result)
	call.err = err
	delete(m.calls, key)
	close(call.done)
	call.cancel()
	m.mu.Unlock()
	if err == nil {
		m.cachePut(key, result)
	}
}

func copyResult(result *restic.Result) *restic.Result {
	if result == nil {
		return nil
	}
	copy := *result
	copy.Stdout = append([]byte(nil), result.Stdout...)
	copy.Stderr = append([]byte(nil), result.Stderr...)
	return &copy
}

// RunMetadata executes a light snapshot-metadata command, coalescing identical
// in-flight requests and optionally serving a short-lived bounded cache entry.
func (r *Registry) RunMetadata(ctx context.Context, repoID string, argv []string) (*restic.Result, MetadataObservation, error) {
	if _, err := r.Get(repoID); err != nil {
		return nil, MetadataObservation{}, err
	}
	if err := restic.AssertArgvAllowed(argv); err != nil {
		return nil, MetadataObservation{}, apperr.New(apperr.InternalError, "disallowed argv").WithCause(err)
	}
	key := metadataKey(repoID, argv)
	observation := MetadataObservation{Enabled: r.metadata.ttl > 0}
	if r.metadata.ttl > 0 {
		observation.TTLSeconds = int64(r.metadata.ttl / time.Second)
	}
	if result, observed, hit := r.metadata.cacheGet(key); hit {
		observation.Hit = true
		observation.ObservedAt = observed.UTC()
		observation.AgeSeconds = int64(r.metadata.now().Sub(observed) / time.Second)
		if observation.AgeSeconds < 0 {
			observation.AgeSeconds = 0
		}
		return result, observation, nil
	}

	call, leader := r.metadata.join(key, ctx)
	if leader {
		go func() {
			result, err := r.Run(call.ctx, repoID, argv, false)
			r.metadata.finish(key, call, result, err)
		}()
	}
	select {
	case <-call.done:
		r.metadata.release(key, call)
		if call.err != nil {
			return nil, observation, call.err
		}
		observation.ObservedAt = r.metadata.now().UTC()
		return copyResult(call.result), observation, nil
	case <-ctx.Done():
		r.metadata.release(key, call)
		return nil, observation, apperr.New(apperr.Cancelled, "request cancelled")
	}
}
