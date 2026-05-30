package backup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TestEventsSSEWireFormat exercises the SSE plumbing end-to-end: hub
// subscription, initial snapshot frame, live event fan-out, and the exact
// "event: progress\ndata: <json>\n\n" framing the frontend codes to. It does
// NOT cover the tenant-resolve / DB-fetch path (that lives in the integration
// test layer); instead it drives the inner streaming loop with a real hub +
// a synthetic snapshot.
//
// IMPORTANT: this test mirrors the real handler's behavior of NOT closing on
// terminal snapshot.status. See the long comment on Handler.events() — a
// snapshot is a long-lived entity that can have restore events overlaid AFTER
// the backup completed. Terminal detection is the browser's job (see
// use-backup-stream.ts).
func TestEventsSSEWireFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hub := NewHub()
	snapID := uuid.New()

	// Build a tiny gin engine whose handler mirrors h.events() AFTER the
	// tenant/snapshot resolution step. We seed an in-memory "snapshot" so the
	// initial frame has a known shape.
	seed := Snapshot{
		ID:        snapID,
		Status:    StatusRunning,
		Progress:  []byte(`{"phase":"dumping_db","phase_detail":{"rows":12345}}`),
		UpdatedAt: time.Date(2026, 5, 28, 17, 55, 0, 0, time.UTC),
	}

	r := gin.New()
	r.GET("/api/v1/backups/:snapshotId/events", func(c *gin.Context) {
		flusher, ok := c.Writer.(http.Flusher)
		if !ok {
			t.Fatal("gin writer is not a flusher")
		}
		ch, unsub := hub.Subscribe(snapID)
		defer unsub()
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
		writeBackupEvent(c.Writer, snapshotToEvent(seed))
		flusher.Flush()
		ctx := c.Request.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, open := <-ch:
				if !open {
					return
				}
				writeBackupEvent(c.Writer, ev)
				flusher.Flush()
				// No terminal-status close — see test docstring + handler comment.
			}
		}
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/backups/"+snapID.String()+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", resp.Header.Get("Cache-Control"))
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", resp.Header.Get("X-Accel-Buffering"))
	}

	// Read the first frame (initial snapshot), then publish two events:
	// one running progress, then a terminal completed. Expect three frames.
	frames := make(chan string, 4)
	go func() {
		buf := make([]byte, 4096)
		acc := ""
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				acc += string(buf[:n])
				for {
					idx := strings.Index(acc, "\n\n")
					if idx < 0 {
						break
					}
					frames <- acc[:idx+2]
					acc = acc[idx+2:]
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Frame 1: initial snapshot (phase from seed.Progress).
	select {
	case f := <-frames:
		ev := mustParseFrame(t, f)
		if ev.Phase != "dumping_db" || ev.Status != StatusRunning || ev.SnapshotID != snapID {
			t.Fatalf("initial frame mismatch: %+v", ev)
		}
		if got, _ := ev.PhaseDetail["rows"].(float64); got != 12345 {
			t.Fatalf("initial phase_detail lost: %+v", ev.PhaseDetail)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for initial frame")
	}

	// Publish a running event.
	hub.Publish(BackupEvent{
		SnapshotID:  snapID,
		Phase:       "encrypting_uploading",
		PhaseDetail: map[string]any{"chunks_done": 17, "chunks_total": 42},
		Status:      StatusRunning,
		Timestamp:   time.Now().UTC(),
	})
	select {
	case f := <-frames:
		ev := mustParseFrame(t, f)
		if ev.Phase != "encrypting_uploading" || ev.Status != StatusRunning {
			t.Fatalf("running frame mismatch: %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for running frame")
	}

	// Publish a terminal event. Unlike the prior design, the handler MUST NOT
	// close the stream after delivering it — a snapshot is long-lived (restore
	// events can be overlaid post-backup-completion). Terminal detection is
	// the browser's job (use-backup-stream.ts). We assert here that the frame
	// is delivered and the stream stays open for a follow-up restore event.
	hub.Publish(BackupEvent{
		SnapshotID: snapID,
		Phase:      "completed",
		Status:     StatusCompleted,
		Timestamp:  time.Now().UTC(),
	})
	select {
	case f := <-frames:
		ev := mustParseFrame(t, f)
		if ev.Phase != "completed" || ev.Status != StatusCompleted {
			t.Fatalf("terminal frame mismatch: %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for terminal frame")
	}

	// Now publish a RESTORE event on the same snapshot — it must be delivered
	// to the still-open stream (the bug we're guarding against: a CP-side
	// auto-close on the prior 'completed' frame would drop this entirely).
	hub.Publish(BackupEvent{
		SnapshotID:  snapID,
		Phase:       "preflight",
		PhaseDetail: map[string]any{"selection": "full"},
		Status:      StatusCompleted, // snapshot status STAYS completed during restore
		Timestamp:   time.Now().UTC(),
	})
	select {
	case f := <-frames:
		ev := mustParseFrame(t, f)
		if ev.Phase != "preflight" {
			t.Fatalf("restore frame mismatch: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("stream closed after terminal event — restore overlay would be dropped")
	}
}

// mustParseFrame parses one "event: progress\ndata: <json>\n\n" frame into a
// BackupEvent (the wire shape the frontend agent codes to).
func mustParseFrame(t *testing.T, frame string) BackupEvent {
	t.Helper()
	if !strings.HasPrefix(frame, "event: progress\ndata: ") {
		t.Fatalf("frame missing prefix: %q", frame)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(frame, "event: progress\ndata: "), "\n\n")
	var ev BackupEvent
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		t.Fatalf("frame json: %v (body=%q)", err, body)
	}
	return ev
}
