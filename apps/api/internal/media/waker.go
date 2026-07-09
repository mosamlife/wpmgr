package media

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/riverutil"
)

// metadataIdentityURL is the GCE / Cloud Run metadata endpoint that mints an OIDC
// ID token for the runtime service account, scoped to a target audience. It is
// how the CP authenticates its service-to-service wake call to the media-encoder
// (a private, IAM-gated, internal-ingress Cloud Run service).
const metadataIdentityURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"

// EncoderWaker keeps the scale-to-zero media-encoder alive while encoder-owned
// River queues have work.
//
// The encoder is a PULL worker: it polls Postgres for media_encode jobs. At
// min-instances=0 nothing would ever process an enqueued job, because enqueuing
// is a DB write by THIS process (control plane), not an HTTP call to the encoder,
// and Cloud Run only cold-starts an instance on an inbound request. This waker is
// that request.
//
// It runs a single reconcile loop: on each tick (or an enqueue Kick) it counts
// live encoder jobs and, if any exist, holds a blocking POST to the encoder's
// /internal/drain endpoint. The encoder keeps that request open — and thus the
// instance alive — until the queue drains, then returns 200. Exactly one hold is
// active at a time (the loop blocks in holdDrain), so this is naturally
// singleflighted and self-healing across CP restarts: a restarted loop re-detects
// live work within one tick and re-establishes the hold. River job durability
// covers any in-flight job interrupted by a mid-drain scale-down.
//
// Disabled (drainURL == "") on self-host, where the media-encoder runs as a
// long-lived always-on container in the base docker-compose stack and needs no
// waking. Both Run and Kick become no-ops.
type EncoderWaker struct {
	pool     *db.Pool
	queues   []string
	schema   string
	drainURL string // <encoder-base>/internal/drain ; empty disables the waker
	audience string // <encoder-base> ; the ID-token audience for Cloud Run IAM
	tick     time.Duration
	http     *http.Client
	logger   *slog.Logger
	kick     chan struct{}

	// mintToken returns a bearer token for the drain request. Defaults to the
	// metadata-server ID token (idToken); overridable in tests.
	mintToken func(ctx context.Context) (string, error)

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewEncoderWaker builds the waker. encoderBaseURL is WPMGR_MEDIA_ENCODER_URL —
// the encoder's Cloud Run URL with NO path (e.g. https://wpmgr-media-encoder-…run.app).
// When empty (self-host / not configured) the waker is disabled.
func NewEncoderWaker(pool *db.Pool, encoderBaseURL string, logger *slog.Logger, mediaSchema string, queues ...string) *EncoderWaker {
	if logger == nil {
		logger = slog.Default()
	}
	if len(queues) == 0 {
		queues = []string{model.MediaEncodeQueue}
	}
	w := &EncoderWaker{
		pool:   pool,
		queues: queues,
		schema: mediaSchema,
		tick:   60 * time.Second,
		logger: logger,
		kick:   make(chan struct{}, 1),
	}
	base := strings.TrimRight(strings.TrimSpace(encoderBaseURL), "/")
	if base != "" {
		w.audience = base
		w.drainURL = base + "/internal/drain"
		// A long-timeout client: holdDrain blocks until the queue empties. Bounded
		// so a hung encoder cannot leak the goroutine forever; the loop re-holds on
		// the next tick. Kept just under the encoder's request timeout.
		w.http = &http.Client{Timeout: 55 * time.Minute}
	}
	w.mintToken = w.idToken
	return w
}

// Enabled reports whether the waker will actually poke an encoder.
func (w *EncoderWaker) Enabled() bool { return w != nil && w.drainURL != "" }

// Kick nudges the reconcile loop to check immediately (non-blocking). The
// encode-ready handler calls it right after a successful enqueue so a freshly
// queued job wakes the encoder without waiting for the next tick.
func (w *EncoderWaker) Kick() {
	if !w.Enabled() {
		return
	}
	select {
	case w.kick <- struct{}{}:
	default: // a check is already pending; the loop will observe the new job
	}
}

// Run is the reconcile loop. It blocks until ctx is done; start it in a goroutine.
func (w *EncoderWaker) Run(ctx context.Context) {
	if !w.Enabled() {
		return
	}
	w.logger.Info("media-encoder waker started",
		slog.String("drain_url", w.drainURL), slog.Duration("tick", w.tick))
	t := time.NewTicker(w.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
		n, err := w.liveJobCount(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.logger.Warn("media-encoder waker: live-count failed", slog.Any("error", err))
			continue
		}
		if n == 0 {
			continue
		}
		// Block here, holding the encoder alive until it drains (or the hold times
		// out / ctx cancels). One hold at a time — the loop IS the singleflight.
		w.holdDrain(ctx, n)
	}
}

// liveJobCount counts jobs that need an awake encoder: available
// (ready now), running (in flight), retryable (failed, retry imminent). It
// excludes scheduled (future retries) — those flip to available at their due time
// and the next tick re-wakes. River stores jobs in river_job.
func (w *EncoderWaker) liveJobCount(ctx context.Context) (int, error) {
	q, err := w.liveJobCountQuery()
	if err != nil {
		return 0, err
	}
	var n int
	if err := w.pool.QueryRow(ctx, q, w.queues).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// liveJobCountQuery builds the schema-aware query used to count encoder jobs
// that should keep the media-encoder awake. It mirrors the encoder's own drain
// query so both sides agree on whether work is pending.
func (w *EncoderWaker) liveJobCountQuery() (string, error) {
	table, err := riverutil.QualifiedTable(w.schema, "river_job")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`SELECT count(*) FROM %s WHERE queue = ANY($1) AND state IN ('available','running','retryable')`, table), nil
}

// holdDrain mints a Cloud Run ID token and POSTs /internal/drain, blocking until
// the encoder reports the queue drained (200) or the request ends. Best-effort:
// any error simply returns and the loop retries on the next tick.
func (w *EncoderWaker) holdDrain(ctx context.Context, live int) {
	token, err := w.mintToken(ctx)
	if err != nil {
		// Still attempt without a token: on Cloud Run the encoder is IAM-gated and
		// will 403 (logged below); on a non-GCE deploy that set the URL anyway the
		// encoder may not require auth. Either way we surface the mint failure.
		w.logger.Warn("media-encoder waker: id-token mint failed", slog.Any("error", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.drainURL, nil)
	if err != nil {
		w.logger.Warn("media-encoder waker: build request failed", slog.Any("error", err))
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w.logger.Info("media-encoder waker: holding drain", slog.Int("live_jobs", live))
	start := time.Now()
	resp, err := w.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.logger.Warn("media-encoder waker: drain request failed",
			slog.Any("error", err), slog.Duration("held", time.Since(start)))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		w.logger.Warn("media-encoder waker: drain non-200",
			slog.Int("status", resp.StatusCode), slog.Duration("held", time.Since(start)))
		return
	}
	w.logger.Info("media-encoder waker: drain complete", slog.Duration("held", time.Since(start)))
}

// idToken returns a cached or freshly minted OIDC ID token for the encoder
// audience, obtained from the GCE / Cloud Run metadata server. Cached ~50 min
// (tokens are valid ~1h).
func (w *EncoderWaker) idToken(ctx context.Context) (string, error) {
	w.mu.Lock()
	if w.token != "" && time.Now().Before(w.tokenExp) {
		tok := w.token
		w.mu.Unlock()
		return tok, nil
	}
	w.mu.Unlock()

	u := metadataIdentityURL + "?audience=" + url.QueryEscape(w.audience)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata identity %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	tok := strings.TrimSpace(string(body))
	if tok == "" {
		return "", fmt.Errorf("metadata identity returned an empty token")
	}
	w.mu.Lock()
	w.token = tok
	w.tokenExp = time.Now().Add(50 * time.Minute)
	w.mu.Unlock()
	return tok, nil
}
