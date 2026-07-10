package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	mediafont "github.com/mosamlife/wpmgr/apps/api/internal/media/font"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot/capture"
)

// encoderOwnedKinds lists every River job kind this binary's own workers
// process. reconcileStrandedPublicJobs is STRICTLY scoped to these kinds so
// it can never read or mutate a row belonging to the API's own public-schema
// periodics (uptime_probe, backup_scheduler, site_connection_sweep, ...) —
// see GH #205.
//
// INVARIANT: every kind here MUST be disposable / server-re-derivable. The
// reconcile has at-least-once (not exactly-once) semantics and unconditionally
// cancels the public row even when the re-insert into the dedicated schema
// fails, so adding a kind whose loss or duplication would corrupt data (a
// backup, a billing charge, anything with side effects that must run exactly
// once) here would turn a best-effort sweep into silent data loss.
var encoderOwnedKinds = []string{
	model.EncodeArgs{}.Kind(),
	mediafont.TranscodeArgs{}.Kind(),
	screenshot.CaptureArgs{}.Kind(),
	capture.WeeklyFanoutArgs{}.Kind(),
}

// reconcileBatchSize bounds how many stranded rows a single transaction claims.
// The cutover boot carries the largest public backlog; batching keeps each
// transaction's row lock short and makes incremental progress instead of
// holding one long transaction that could delay the health-server port bind
// (and thus the Cloud Run startup probe). The loop repeats until a batch comes
// back short, so it still fully drains in one boot, just in bounded chunks.
const reconcileBatchSize = 500

// reconcileMaxBatches caps the sweep loop so a pathological state (e.g. rows
// that keep failing to cancel) can never spin the boot path forever.
const reconcileMaxBatches = 10000

// strandedJob is one non-terminal row read from the public river_job table
// that this process now owns on a dedicated media River schema.
type strandedJob struct {
	id   int64
	kind string
	args []byte
}

// reconcileStrandedPublicJobs re-enqueues encoder-owned jobs left behind in
// the shared/public River schema from before WPMGR_RIVER_MEDIA_SCHEMA was set
// to a dedicated schema (GH #205: while both processes shared the public
// schema, River leader election could hand this process leadership and
// silently stop the API's fleet periodics — the fix is schema isolation, but
// any encoder jobs already sitting in public.river_job at the moment of the
// fix would otherwise be stranded forever, since nothing polls that schema
// for these kinds anymore).
//
// Scoped STRICTLY to encoderOwnedKinds — it never reads or writes any other
// row, so the API's own public-schema jobs are untouched. Best-effort and
// non-fatal: called only when mediaSchema is a real dedicated schema, and any
// failure is logged and swallowed. It runs after the health server has already
// bound $PORT (see main.go), so even a large cutover sweep cannot delay the
// Cloud Run startup probe.
//
// Each stranded row is claimed with FOR UPDATE SKIP LOCKED (safe under
// rolling-deploy races between multiple encoder replicas), re-inserted into
// the dedicated-schema client when its args decode cleanly, and always marked
// 'cancelled' in the public table afterward — whether or not the re-insert
// succeeded. Semantics are AT LEAST ONCE into the dedicated schema, not exactly
// once: the re-insert auto-commits on its own connection before the public row
// is cancelled at tx.Commit, so a crash in that window re-selects the row on the
// next boot and re-inserts it. That is safe because encoderOwnedKinds are all
// disposable/idempotent (see the invariant above); the bias is deliberately
// toward re-running disposable work over stranding it in a schema nothing polls.
func reconcileStrandedPublicJobs(ctx context.Context, pool *db.Pool, client *river.Client[pgx.Tx], logger *slog.Logger) {
	totalReenqueued := 0
	totalDropped := 0
	for batch := 0; batch < reconcileMaxBatches; batch++ {
		claimed, reenqueued, dropped, err := reconcileOneBatch(ctx, pool, client, logger)
		totalReenqueued += reenqueued
		totalDropped += dropped
		if err != nil {
			break // already logged; stop rather than spin the boot path
		}
		if claimed < reconcileBatchSize {
			break // drained (any remainder is locked by another replica, which owns it)
		}
	}
	if totalReenqueued > 0 || totalDropped > 0 {
		logger.Info("reconciled stranded public-schema River jobs (GH #205)",
			slog.Int("reenqueued", totalReenqueued), slog.Int("dropped", totalDropped))
	}
}

// reconcileOneBatch claims up to reconcileBatchSize stranded rows in a single
// short transaction and returns the number claimed plus the re-enqueued/dropped
// split. The table is qualified as public.river_job explicitly so the sweep is
// independent of the connection's search_path and can never read or cancel the
// dedicated media schema's own rows.
func reconcileOneBatch(ctx context.Context, pool *db.Pool, client *river.Client[pgx.Tx], logger *slog.Logger) (claimed, reenqueued, dropped int, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		logger.Warn("reconcile stranded public River jobs: begin tx failed", slog.Any("error", err))
		return 0, 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id, kind, args FROM public.river_job
		 WHERE kind = ANY($1) AND state IN ('available', 'scheduled', 'retryable')
		 FOR UPDATE SKIP LOCKED
		 LIMIT $2`,
		encoderOwnedKinds, reconcileBatchSize)
	if err != nil {
		logger.Warn("reconcile stranded public River jobs: query failed", slog.Any("error", err))
		return 0, 0, 0, err
	}
	var stranded []strandedJob
	for rows.Next() {
		var j strandedJob
		if scanErr := rows.Scan(&j.id, &j.kind, &j.args); scanErr != nil {
			rows.Close()
			logger.Warn("reconcile stranded public River jobs: scan failed", slog.Any("error", scanErr))
			return 0, 0, 0, scanErr
		}
		stranded = append(stranded, j)
	}
	if err = rows.Err(); err != nil {
		logger.Warn("reconcile stranded public River jobs: row iteration failed", slog.Any("error", err))
		return 0, 0, 0, err
	}
	if len(stranded) == 0 {
		return 0, 0, 0, nil
	}

	ids := make([]int64, 0, len(stranded))
	for _, j := range stranded {
		ids = append(ids, j.id)
		args, decErr := decodeEncoderArgs(j.kind, j.args)
		if decErr != nil {
			dropped++
			logger.Warn("reconcile stranded public River job: args decode failed, cancelling",
				slog.Int64("river_job_id", j.id), slog.String("kind", j.kind), slog.Any("error", decErr))
			continue
		}
		if _, insErr := client.Insert(ctx, args, nil); insErr != nil {
			dropped++
			logger.Warn("reconcile stranded public River job: re-insert failed, cancelling",
				slog.Int64("river_job_id", j.id), slog.String("kind", j.kind), slog.Any("error", insErr))
			continue
		}
		reenqueued++
	}

	if _, err = tx.Exec(ctx,
		`UPDATE public.river_job SET state = 'cancelled', finalized_at = now() WHERE id = ANY($1)`, ids); err != nil {
		logger.Warn("reconcile stranded public River jobs: cancel failed", slog.Any("error", err))
		return 0, 0, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		logger.Warn("reconcile stranded public River jobs: commit failed", slog.Any("error", err))
		return 0, 0, 0, err
	}
	return len(stranded), reenqueued, dropped, nil
}

// decodeEncoderArgs unmarshals a raw River args payload into the concrete
// JobArgs type for kind. Returns an error for any kind outside
// encoderOwnedKinds — reconcileStrandedPublicJobs's query already restricts
// to that set, so this is defense in depth against a future kind rename.
func decodeEncoderArgs(kind string, raw []byte) (river.JobArgs, error) {
	switch kind {
	case model.EncodeArgs{}.Kind():
		var a model.EncodeArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		return a, nil
	case mediafont.TranscodeArgs{}.Kind():
		var a mediafont.TranscodeArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		return a, nil
	case screenshot.CaptureArgs{}.Kind():
		var a screenshot.CaptureArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		return a, nil
	case capture.WeeklyFanoutArgs{}.Kind():
		var a capture.WeeklyFanoutArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		return a, nil
	default:
		return nil, fmt.Errorf("unrecognized encoder-owned job kind %q", kind)
	}
}
