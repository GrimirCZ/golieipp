package stats

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ActionRecorder is the small part of Collector needed by the action
// instrumentation.  Keeping the interface here allows callers to use a
// disabled collector, a test recorder, or the SQLite collector without
// coupling the resource tracker to the persistence implementation.
type ActionRecorder interface {
	RecordAction(Action)
}

type runIDProvider interface {
	RunID() string
}

type resourcesProvider interface {
	ResourcesEnabled() bool
}

type enabledProvider interface {
	Enabled() bool
}

type statsEnabledProvider interface {
	StatsEnabled() bool
}

type spanContextKey struct{}

// Begin starts an action and returns a context carrying the resulting span.
// The initial action is copied.  A nil collector (or a typed nil collector)
// returns the original context and a nil span, which keeps disabled tracking
// out of the hot path.  The parent span, when present in ctx, supplies the
// parent ID and run ID unless the caller set them explicitly.
func Begin(ctx context.Context, collector ActionRecorder, initial Action) (context.Context, *Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	if isNilRecorder(collector) {
		return ctx, nil
	}
	// A concrete collector may remain installed while collection is disabled.
	// Support both common predicate names so the tracker does not depend on the
	// storage implementation's concrete type.
	if provider, ok := collector.(enabledProvider); ok && !provider.Enabled() {
		return ctx, nil
	}
	if provider, ok := collector.(statsEnabledProvider); ok && !provider.StatsEnabled() {
		return ctx, nil
	}

	parent := SpanFromContext(ctx)
	action := initial
	if action.ID == "" {
		action.ID = uuid.NewString()
	}
	if action.ParentID == "" && parent != nil {
		action.ParentID = parent.ID()
	}
	if action.RunID == "" {
		if parent != nil {
			action.RunID = parent.RunID()
		}
		if action.RunID == "" {
			if provider, ok := collector.(runIDProvider); ok {
				action.RunID = provider.RunID()
			}
		}
	}
	if action.StartedAt.IsZero() {
		action.StartedAt = time.Now().UTC()
	}
	if parent != nil {
		parent.copyIdentity(&action)
	}

	resources := true
	if provider, ok := collector.(resourcesProvider); ok {
		resources = provider.ResourcesEnabled()
	}
	s := &Span{
		collector:   collector,
		action:      action,
		resources:   resources,
		parent:      parent,
		started:     time.Now(),
		buffers:     make(map[string]int64),
		bufferPeaks: make(map[string]int64),
	}
	return context.WithValue(ctx, spanContextKey{}, s), s
}

// BeginChild starts an action using the collector carried by the active span.
// It is convenient for staging, translation, and transport sub-actions that
// should retain the request's action context without exposing Collector from
// the parent span.
func BeginChild(ctx context.Context, initial Action) (context.Context, *Span) {
	parent := SpanFromContext(ctx)
	if parent == nil {
		if ctx == nil {
			ctx = context.Background()
		}
		return ctx, nil
	}
	return Begin(ctx, parent.collector, initial)
}

func isNilRecorder(rec ActionRecorder) bool {
	if rec == nil {
		return true
	}
	v := reflect.ValueOf(rec)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// SpanFromContext returns the active action span, if any.
func SpanFromContext(ctx context.Context) *Span {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(spanContextKey{}).(*Span)
	return s
}

// Span records action fields and explicit resource lifetimes.  It is safe to
// update a span from concurrent readers/writers, although the usual action
// path has one goroutine updating it.
type Span struct {
	mu sync.Mutex

	collector ActionRecorder
	action    Action
	resources bool
	parent    *Span
	started   time.Time
	finished  bool

	bufferCurrent int64
	bufferPeak    int64
	buffers       map[string]int64
	bufferPeaks   map[string]int64
	tempCurrent   int64
	tempPeak      int64
	tempWritten   int64

	finishOnce atomic.Bool
}

// ID returns the stable action ID.
func (s *Span) ID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.action.ID
}

// RunID returns the process-run ID associated with this action.
func (s *Span) RunID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.action.RunID
}

// Action returns a snapshot of the current action. It is primarily useful to
// inspect a span in tests; Finish records the final snapshot.
func (s *Span) Action() Action {
	if s == nil {
		return Action{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// Finish completes the action. Calling Finish more than once records only the
// first completion. Resource handles may still be released after Finish; such
// releases update no persisted action and are safe for failure defers.
func (s *Span) Finish(outcome string) {
	if s == nil || !s.finishOnce.CompareAndSwap(false, true) {
		return
	}

	s.mu.Lock()
	if outcome != "" {
		s.action.Outcome = outcome
	}
	finished := time.Now().UTC()
	s.action.FinishedAt = finished
	if !s.action.StartedAt.IsZero() {
		duration := finished.Sub(s.started)
		if s.started.IsZero() {
			duration = finished.Sub(s.action.StartedAt)
		}
		if duration < 0 {
			duration = 0
		}
		s.action.DurationNS = duration.Nanoseconds()
	}
	s.finished = true
	action := s.snapshotLocked()
	collector := s.collector
	s.mu.Unlock()

	if collector != nil {
		collector.RecordAction(action)
	}
}

// SetOutcome updates the outcome before Finish. Finish's argument remains the
// final value when non-empty.
func (s *Span) SetOutcome(outcome string) {
	s.update(func(a *Action) { a.Outcome = outcome })
}

func (s *Span) SetReason(reason string) {
	s.update(func(a *Action) { a.Reason = reason })
}

func (s *Span) SetHTTPStatus(status int) {
	s.update(func(a *Action) { a.HTTPStatus = status })
}

func (s *Span) SetIPPStatus(status int) {
	s.update(func(a *Action) { a.IPPStatus = status })
}

func (s *Span) SetQueue(queue string) {
	s.update(func(a *Action) { a.Queue = queue })
}

func (s *Span) SetUser(user string) {
	s.update(func(a *Action) { a.User = user })
}

func (s *Span) SetOwner(owner string) {
	s.update(func(a *Action) { a.Owner = owner })
}

func (s *Span) SetOperation(operation string) { s.update(func(a *Action) { a.Operation = operation }) }
func (s *Span) SetConcurrency(n int64)        { s.update(func(a *Action) { a.Concurrency = n }) }
func (s *Span) SetUserConcurrency(n int64)    { s.update(func(a *Action) { a.UserConcurrency = n }) }

func (s *Span) SetJob(registryID string, jobID int) {
	s.update(func(a *Action) {
		a.RegistryID = registryID
		a.JobID = jobID
	})
}

func (s *Span) SetClientBytes(bytes int64) {
	s.update(func(a *Action) { a.ClientBytes = clampNonNegative(bytes) })
}

func (s *Span) AddClientBytes(bytes int64) {
	s.update(func(a *Action) { a.ClientBytes = addSaturating(a.ClientBytes, bytes) })
}

func (s *Span) SetUpstreamBytes(bytes int64) {
	s.update(func(a *Action) { a.UpstreamBytes = clampNonNegative(bytes) })
}

func (s *Span) AddUpstreamBytes(bytes int64) {
	s.update(func(a *Action) { a.UpstreamBytes = addSaturating(a.UpstreamBytes, bytes) })
}

func (s *Span) SetOutputBytes(bytes int64) {
	s.update(func(a *Action) { a.OutputBytes = clampNonNegative(bytes) })
}

func (s *Span) AddOutputBytes(bytes int64) {
	s.update(func(a *Action) { a.OutputBytes = addSaturating(a.OutputBytes, bytes) })
}

// AcquireBuffer tracks the actual capacity of an explicitly allocated buffer.
// It returns a handle that must be released when the buffer stops being held.
// The handle is nil when resource collection is disabled.
func (s *Span) AcquireBuffer(component string, capacity int64) *Resource {
	if s == nil || !s.resources || capacity <= 0 {
		return nil
	}
	return s.acquire(resourceBuffer, component, capacity)
}

// AcquireTemp tracks the logical size of an open temporary file. The initial
// size is normally zero; callers should call SetSize after writes and Release
// on every exit path.
func (s *Span) AcquireTemp(component string, logicalSize int64) *Resource {
	if s == nil || !s.resources {
		return nil
	}
	if logicalSize < 0 {
		logicalSize = 0
	}
	return s.acquire(resourceTemp, component, logicalSize)
}

// AddTempWritten records bytes written to a temporary file. It counts writes
// even when they overwrite existing logical bytes.
func (s *Span) AddTempWritten(bytes int64) {
	if s == nil || !s.resources || bytes <= 0 {
		return
	}
	s.mu.Lock()
	s.tempWritten = addSaturating(s.tempWritten, bytes)
	s.mu.Unlock()
}

// Resource is a one-shot explicit resource lease. A lease can be released
// after its parent span has finished; the release remains safe and idempotent.
type Resource struct {
	span      *Span
	kind      resourceKind
	component string
	current   int64
	parents   []*Resource
	released  atomic.Bool
}

type resourceKind uint8

const (
	resourceBuffer resourceKind = iota + 1
	resourceTemp
)

func (s *Span) acquire(kind resourceKind, component string, size int64) *Resource {
	if size < 0 {
		size = 0
	}
	if component == "" {
		component = "unspecified"
	}
	r := &Resource{span: s, kind: kind, component: component, current: size}
	s.mu.Lock()
	if kind == resourceBuffer {
		s.bufferCurrent = addSaturating(s.bufferCurrent, size)
		if s.bufferCurrent > s.bufferPeak {
			s.bufferPeak = s.bufferCurrent
		}
		s.buffers[component] = addSaturating(s.buffers[component], size)
		if s.buffers[component] > s.bufferPeaks[component] {
			s.bufferPeaks[component] = s.buffers[component]
		}
	} else {
		s.tempCurrent = addSaturating(s.tempCurrent, size)
		if s.tempCurrent > s.tempPeak {
			s.tempPeak = s.tempCurrent
		}
	}
	s.mu.Unlock()
	if s.parent != nil {
		r.parents = append(r.parents, s.parent.acquire(kind, component, size))
	}
	return r
}

// SetSize changes a temporary resource's logical size and therefore its
// simultaneously held peak. It is a no-op for buffer resources.
func (r *Resource) SetSize(size int64) {
	if r == nil || r.kind != resourceTemp || r.released.Load() {
		return
	}
	if size < 0 {
		size = 0
	}
	r.span.mu.Lock()
	if r.released.Load() {
		r.span.mu.Unlock()
		return
	}
	if size > r.current {
		r.span.tempCurrent = addSaturating(r.span.tempCurrent, size-r.current)
	} else {
		r.span.tempCurrent -= r.current - size
		if r.span.tempCurrent < 0 {
			r.span.tempCurrent = 0
		}
	}
	r.current = size
	if r.span.tempCurrent > r.span.tempPeak {
		r.span.tempPeak = r.span.tempCurrent
	}
	r.span.mu.Unlock()
	for _, p := range r.parents {
		p.SetSize(size)
	}
}

// Resize is an alias for SetSize.
func (r *Resource) Resize(size int64) { r.SetSize(size) }

// Written records bytes physically written to a temporary resource, including
// overwrites. It also updates logical size when the write extends the file.
func (r *Resource) Written(bytes int64) {
	if r == nil || r.kind != resourceTemp || bytes <= 0 || r.released.Load() {
		return
	}
	r.span.mu.Lock()
	if r.released.Load() {
		r.span.mu.Unlock()
		return
	}
	r.span.tempWritten = addSaturating(r.span.tempWritten, bytes)
	// Written is intentionally not assumed to extend the file: callers that
	// write after a seek should call SetSize with the actual end position.
	r.span.mu.Unlock()
	for _, p := range r.parents {
		p.Written(bytes)
	}
}

// Write is an alias for Written.
func (r *Resource) Write(bytes int64) { r.Written(bytes) }

// Release releases the current logical resource size exactly once.
func (r *Resource) Release() {
	if r == nil || !r.released.CompareAndSwap(false, true) {
		return
	}
	s := r.span
	if s == nil {
		return
	}
	s.mu.Lock()
	if r.kind == resourceBuffer {
		s.bufferCurrent -= r.current
		if s.bufferCurrent < 0 {
			s.bufferCurrent = 0
		}
		s.buffers[r.component] -= r.current
		if s.buffers[r.component] <= 0 {
			delete(s.buffers, r.component)
		}
	} else {
		s.tempCurrent -= r.current
		if s.tempCurrent < 0 {
			s.tempCurrent = 0
		}
	}
	r.current = 0
	s.mu.Unlock()
	for _, parent := range r.parents {
		parent.Release()
	}
}

func (s *Span) copyIdentity(action *Action) {
	if s == nil || action == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if action.Queue == "" {
		action.Queue = s.action.Queue
	}
	if action.User == "" {
		action.User = s.action.User
	}
	if action.Owner == "" {
		action.Owner = s.action.Owner
	}
	if action.RegistryID == "" {
		action.RegistryID = s.action.RegistryID
	}
	if action.JobID == 0 {
		action.JobID = s.action.JobID
	}
}

func (s *Span) update(fn func(*Action)) {
	if s == nil || fn == nil {
		return
	}
	s.mu.Lock()
	if !s.finished {
		fn(&s.action)
	}
	s.mu.Unlock()
}

func (s *Span) snapshotLocked() Action {
	action := s.action
	action.BufferPeakBytes = s.bufferPeak
	action.TempPeakBytes = s.tempPeak
	action.TempWrittenBytes = s.tempWritten
	if len(s.bufferPeaks) > 0 {
		action.BufferComponents = make(map[string]int64, len(s.bufferPeaks))
		for component, peak := range s.bufferPeaks {
			action.BufferComponents[component] = peak
		}
	}
	return action
}

func clampNonNegative(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

func addSaturating(a, b int64) int64 {
	if b < 0 {
		if b == -int64(^uint64(0)>>1)-1 || a < -b {
			return 0
		}
		return a + b
	}
	if b == 0 {
		return a
	}
	if a > int64(^uint64(0)>>1)-b {
		return int64(^uint64(0) >> 1)
	}
	return a + b
}
