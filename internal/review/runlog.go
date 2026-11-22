package review

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	// defaultRunLogMaxBytes bounds one run's stored log. OCR progress is chatty
	// but bounded; the cap protects the database from a runaway process.
	defaultRunLogMaxBytes = 10 << 20
	// runLogPublishEvery throttles the live notification. Lines arrive in
	// bursts and every SSE event makes the client fetch the tail again.
	runLogPublishEvery = 500 * time.Millisecond
	// runLogWriteTimeout bounds one insert so a wedged database cannot block
	// the review that is being logged.
	runLogWriteTimeout = 3 * time.Second
)

// Lifecycle phases the worker records at the milestones of a run. The panel
// groups the lines by phase, so the values are stable identifiers, not prose.
const (
	logPhaseClaim   = "claim"
	logPhasePrepare = "prepare"
	logPhaseClone   = "clone"
	logPhasePolicy  = "policy"
	logPhaseOCR     = "ocr"
	logPhaseChunks  = "chunks"
	logPhaseFinish  = "finish"
)

var (
	ansiEscape  = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	urlUserinfo = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)
	// runLogLevels keeps a typo out of the stored data; unknown levels become
	// info instead of making the UI guess.
	runLogLevels = map[string]bool{"info": true, "warn": true, "error": true}
)

// runLogSink writes one run's log lines to the database as they happen and
// throttles the live event that tells panels to fetch them. It is safe for
// concurrent use: the OCR stderr reader and the review pipeline both write.
type runLogSink struct {
	mu          sync.Mutex
	store       *store.Store
	events      events.Publisher
	runID       uint
	maxBytes    int
	seq         int64
	bytes       int
	secrets     []string
	truncated   bool
	lastPublish time.Time
	warned      bool
}

func newRunLogSink(st *store.Store, pub events.Publisher, runID uint, maxBytes int) *runLogSink {
	if maxBytes <= 0 {
		maxBytes = defaultRunLogMaxBytes
	}
	return &runLogSink{store: st, events: pub, runID: runID, maxBytes: maxBytes}
}

// AddSecret registers a value that must never reach the stored log.
func (s *runLogSink) AddSecret(secret []byte) {
	if s == nil || len(secret) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets = append(s.secrets, string(secret))
}

// Log appends one line. Logging is best effort: a failure is warned once and
// never interrupts the review.
func (s *runLogSink) Log(ctx context.Context, source, invocation, level, phase, line string) {
	if s == nil {
		return
	}
	line = strings.TrimRight(strings.TrimSpace(stripRunLogLine(line)), "\r")
	if line == "" {
		return
	}
	if !runLogLevels[level] {
		level = "info"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	line = redactRunLogLine(line, s.secrets...)
	if s.truncated {
		return
	}
	if s.bytes+len(line) > s.maxBytes {
		s.truncated = true
		s.appendLocked(ctx, store.ReviewRunLog{
			Source: "worker", Invocation: invocation, Level: "warn", Phase: phase,
			Line: "log truncado: se alcanzó el límite de tamaño; no se registran más líneas",
		})
		return
	}
	s.bytes += len(line)
	s.appendLocked(ctx, store.ReviewRunLog{
		Source: source, Invocation: invocation, Level: level, Phase: phase, Line: line,
	})
}

// OCRLogger returns the callback the native reviewer feeds with each stderr
// line. The invocation distinguishes the main review from each chunk.
func (s *runLogSink) OCRLogger(ctx context.Context, invocation string) func(string) {
	if s == nil {
		return nil
	}
	return func(line string) {
		s.Log(ctx, "ocr", invocation, classifyOCRLevel(line), "", line)
	}
}

func (s *runLogSink) appendLocked(ctx context.Context, entry store.ReviewRunLog) {
	s.seq++
	entry.RunID = s.runID
	entry.Seq = s.seq
	entry.TS = time.Now().UTC()

	if s.store != nil {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runLogWriteTimeout)
		err := s.store.AppendReviewRunLog(writeCtx, []store.ReviewRunLog{entry})
		cancel()
		if err != nil && !s.warned {
			s.warned = true
			slog.Warn("review log write failed", "run_id", s.runID, "error", err)
		}
	}

	if s.events == nil || time.Since(s.lastPublish) < runLogPublishEvery {
		return
	}
	s.lastPublish = time.Now()
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runLogWriteTimeout)
	defer cancel()
	if err := s.events.Publish(notifyCtx, events.Event{
		Type: "run.log", ID: strconv.FormatUint(uint64(s.runID), 10),
	}); err != nil && !s.warned {
		s.warned = true
		slog.Warn("review log event publish failed", "run_id", s.runID, "error", err)
	}
}

func stripRunLogLine(line string) string {
	return ansiEscape.ReplaceAllString(line, "")
}

// redactRunLogLine removes the values that must never be persisted: explicit
// secrets (API keys, clone tokens) and credentials embedded in a URL.
func redactRunLogLine(line string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			line = strings.ReplaceAll(line, secret, "[redactado]")
		}
	}
	return urlUserinfo.ReplaceAllString(line, "${1}[redactado]@")
}

func classifyOCRLevel(line string) string {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "error"), strings.Contains(lower, "failed"), strings.Contains(lower, "fatal"):
		return "error"
	case strings.Contains(lower, "warn"):
		return "warn"
	default:
		return "info"
	}
}
