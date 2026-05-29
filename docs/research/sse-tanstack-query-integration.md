# SSE + TanStack Query v5 Integration

Research dossier for streaming long-running backend progress (e.g. backup snapshots)
into a single React component with TanStack Query v5 + React 19 + Vite + Zod 4.

## 1. The canonical pattern

TanStack Query has **no built-in SSE primitive**. The maintainer-blessed pattern
(Tanner Linsley in [discussion #418](https://github.com/TanStack/query/discussions/418),
TkDodo in [Using WebSockets with React Query](https://tkdodo.eu/blog/using-web-sockets-with-react-query))
is:

1. Keep a normal `useQuery` for the *current* resource (`GET /backups/{id}`).
2. Add a sibling `useEffect` that opens the `EventSource` and on each message either:
   - **`setQueryData`** — patch the cache directly. Best for high-frequency progress
     updates where the full server state is too expensive to refetch and the message
     payload *is* the new state (or a deterministic patch).
   - **`invalidateQueries`** — let the cached query refetch. Best when the SSE event
     is just a "something changed" signal and the source of truth is the REST endpoint.
     Avoids "over-pushing" — if the screen isn't mounted, nothing happens.

Do **not** put the `EventSource` inside `queryFn`. Query functions get re-invoked on
background refetches and there is no cleanup hook for the EventSource — you leak a
socket per refetch (see fragmentedthought.com analysis).

A third option, the experimental
[`experimental_streamedQuery`](https://tanstack.com/query/latest/docs/reference/streamedQuery),
wraps an `AsyncIterable` and is great for AI token streams but is opinionated about
*append* semantics. For a progress-percent stream we want `replace`, and the API is
still flagged experimental, so for now prefer the manual `useEffect + setQueryData`
pattern.

| Approach            | Reactivity | Type safety | Refetch traffic | Notes              |
| ------------------- | ---------- | ----------- | --------------- | ------------------ |
| `setQueryData`      | Instant    | Manual      | None            | Best for progress  |
| `invalidateQueries` | 1 RTT lag  | Free        | One refetch     | Best for "changed" |
| `streamedQuery`     | Instant    | Manual      | None            | Experimental, append-only by default |

## 2. Lifecycle

- Open the `EventSource` in `useEffect`, return `() => es.close()`. The `queryClient`
  goes in the dep array (it is stable, so the effect runs once).
- **React 19 / StrictMode double-mount**: the effect will run → cleanup → run again.
  Because cleanup closes the socket, this is correct but wasteful. If your server can't
  cope with the open/close/open burst, gate with `AbortController` (close on `abort`,
  recreate on next mount) or use `useRef` to dedupe within a microtask.
- Tie the effect's enabling to `enabled: !!snapshotId && status === 'running'` so we
  don't subscribe after the backup is finished.

## 3. Authentication

`EventSource` is a crippled fetch — **no custom headers**, GET-only, no body. The
realistic options:

- **Same-origin cookies** — Just works. Pass `{ withCredentials: true }` and the
  browser ships the session cookie. This is the right choice for wpmgr where the
  agent and the web UI are reverse-proxied through the same host.
- **Token in URL** — `?token=…`. Works everywhere but the token lands in access logs,
  the browser history, and any `Referer` header on linked assets. Acceptable only for
  short-lived (≤60s) signed tokens issued from a `/sse-ticket` endpoint.
- **`fetch` + `ReadableStream`** — A full SSE-over-fetch reimplementation. Lets you
  send `Authorization: Bearer …`. Libraries: `@microsoft/fetch-event-source`,
  `eventsource-parser`. Cost: ~3 KB and you write your own reconnect.

Recommendation: **same-origin cookies in prod, query-string ticket in dev.**

## 4. Reconnect

Native `EventSource` auto-reconnects with a default 3 s back-off the server can tune
via the `retry:` field. On reconnect the browser sends `Last-Event-ID`, so server
implementations should set `id:` on each event and replay-from-id on reconnect.

Hard cap retries in userland by counting `onerror` invocations within a window;
after N (we use 6 in ~30 s), `close()` and fall back to polling.

## 5. SSE fallback

Pitfalls in real deployments:

- Nginx default `proxy_buffering on` will silently buffer the stream — set
  `X-Accel-Buffering: no` and `proxy_buffering off`.
- Cloudflare Free tier and most CDNs buffer too; needs Enterprise or a Worker.
- HTTP/1.1 caps ~6 connections per origin → a noisy tab will starve other SSE.

If `onerror` fires before any message arrives, downgrade gracefully to TanStack
Query's `refetchInterval: 2000` poll on the same `useBackup` query.

## 6. TanStack Query v5 specifics

- No first-class SSE helper. The "official" guidance is TkDodo's WebSocket post —
  it translates verbatim to SSE.
- `experimental_streamedQuery` (v5.65+) handles `AsyncIterable` and is the closest
  thing to first-class support.
- Community libs: `react-query-subscription` (npm) wraps the pattern but is unmaintained.
  Not worth the dependency.

## 7. TypeScript + Zod 4

Model the wire as a discriminated union and validate at the boundary:

```ts
import { z } from 'zod';

export const BackupEvent = z.discriminatedUnion('type', [
  z.object({ type: z.literal('progress'), percent: z.number().min(0).max(100), step: z.string() }),
  z.object({ type: z.literal('log'),      level: z.enum(['info','warn','error']), msg: z.string() }),
  z.object({ type: z.literal('done'),     snapshotId: z.string(), bytes: z.number() }),
  z.object({ type: z.literal('error'),    code: z.string(), msg: z.string() }),
]);
export type BackupEvent = z.infer<typeof BackupEvent>;
```

## Recommended hook

```ts
// src/features/backups/use-backup-progress.ts
import { useEffect, useRef } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { z } from 'zod';
import { BackupEvent } from './backup-events';
import { fetchBackup, type Backup } from '@/lib/api/backups';

export const backupKey = (id: string) => ['backup', id] as const;

export function useBackupProgress(snapshotId: string) {
  const qc = useQueryClient();
  const failuresRef = useRef(0);
  const pollFallbackRef = useRef(false);

  const query = useQuery<Backup>({
    queryKey: backupKey(snapshotId),
    queryFn: ({ signal }) => fetchBackup(snapshotId, signal),
    enabled: !!snapshotId,
    // Poll only if SSE has died; otherwise SSE pushes updates.
    refetchInterval: () => (pollFallbackRef.current ? 2_000 : false),
    staleTime: Infinity,
  });

  useEffect(() => {
    if (!snapshotId) return;
    const url = `/api/v1/backups/${encodeURIComponent(snapshotId)}/events`;
    const es = new EventSource(url, { withCredentials: true });

    const onMessage = (raw: MessageEvent) => {
      const parsed = BackupEvent.safeParse(JSON.parse(raw.data));
      if (!parsed.success) {
        console.warn('[sse] dropping invalid event', parsed.error.format());
        return;
      }
      const evt = parsed.data;

      qc.setQueryData<Backup>(backupKey(snapshotId), (prev) => {
        if (!prev) return prev;
        switch (evt.type) {
          case 'progress': return { ...prev, percent: evt.percent, step: evt.step };
          case 'log':      return { ...prev, logs: [...(prev.logs ?? []), evt].slice(-200) };
          case 'done':     return { ...prev, status: 'done', percent: 100, bytes: evt.bytes };
          case 'error':    return { ...prev, status: 'error', error: evt };
        }
      });

      if (evt.type === 'done' || evt.type === 'error') es.close();
    };

    es.addEventListener('message', onMessage);

    es.onerror = () => {
      failuresRef.current += 1;
      if (failuresRef.current >= 6) {
        es.close();
        pollFallbackRef.current = true;
        qc.invalidateQueries({ queryKey: backupKey(snapshotId) });
      }
    };

    return () => es.close();
  }, [snapshotId, qc]);

  return query;
}
```

Consumers just call `useBackup(snapshotId)` (the existing query) anywhere in the
tree — they'll see the live-patched cache automatically. Only the screen that
*owns* the progress UI calls `useBackupProgress`, so exactly one EventSource is
open per snapshot at a time.

## Sources

- [TanStack discussion #418 — stream-based flow](https://github.com/TanStack/query/discussions/418)
- [TanStack discussion #9065 — streamedQuery feedback](https://github.com/TanStack/query/discussions/9065)
- [streamedQuery reference docs](https://tanstack.com/query/latest/docs/reference/streamedQuery)
- [TkDodo — Using WebSockets with React Query](https://tkdodo.eu/blog/using-web-sockets-with-react-query)
- [ollioddi — SSE with TanStack Start & Query](https://ollioddi.dev/blog/tanstack-sse-guide) ([repo](https://github.com/ollioddi/tanstack-server-sent-events))
- [Fragmented Thought — React Query and SSE](https://fragmentedthought.com/blog/2025/react-query-caching-with-server-side-events)
- [Aman Ahmed — EventSource with React Query](https://rustedcompiler.medium.com/using-eventsource-sse-with-react-query-b72e20923d8c)
- [Odilon Hugonnot — SSE with fetch + ReadableStream](https://www.web-developpeur.com/en/blog/sse-fetch-readable-stream-api-key)
