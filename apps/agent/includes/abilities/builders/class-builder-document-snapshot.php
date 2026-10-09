<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The snapshot of one page taken before a builder write: the stored bytes a
 * restore puts back.
 *
 * take() reads with SQL only, never through get_post(), get_post_meta() or
 * the object cache, so a snapshot holds what the database holds:
 *
 * - the posts row's RESTORE_POST_COLUMNS, which include every column the
 *   builder document fingerprint covers;
 * - every postmeta row of the post except the edit lock (EDIT_LOCK_KEY, byte
 *   for byte), as [meta_key, meta_value] in meta_id order: every key, so a
 *   row any plugin writes during a save can be put back too, and a key with
 *   several rows keeps their count and order;
 * - the ids of the post's revisions, in id order.
 *
 * The stored text is json_encode, default flags, of
 *
 *   {"version":1,"request_id":<uuid>,"post_id":<int>,"taken_at":<unix>,
 *    "post":{<column>:<base64>,...},"meta":[[<key>,<base64>],...],
 *    "revisions":[<id>,...]}
 *
 * with the members in that order and the columns in RESTORE_POST_COLUMNS
 * order. Every stored value is base64, so each byte survives JSON; an
 * integer column is its decimal text, and a meta_value the database holds as
 * NULL is null. Default flags escape every non-ASCII character, so the text
 * is ASCII. A meta key that is not valid UTF-8 cannot be encoded, and the
 * snapshot fails.
 *
 * The text is one non-autoloaded option named OPTION_PREFIX followed by the
 * request id in lowercase, and is never longer than
 * BuilderContract::MAX_SNAPSHOT_BYTES. take() reads the stored bytes back
 * with SQL and keeps the snapshot only when they are the bytes it encoded.
 * The sha256 of the stored bytes is the hash the ledger records and an undo
 * echoes back; load() computes it over the bytes stored when it is called.
 *
 * A snapshot is kept for RETENTION_SECONDS, one day past the control plane's
 * 14-day undo window. sweep() deletes older ones, and uninstall deletes every
 * row under OPTION_PREFIX.
 */
final class BuilderDocumentSnapshot
{
    /** Format version of the stored text. */
    public const VERSION = 1;

    /** Option name prefix; the rest of the name is the request id in lowercase. */
    public const OPTION_PREFIX = 'wpmgr_ability_snap_';

    /**
     * The posts columns a snapshot keeps and a restore puts back: every
     * column the builder document fingerprint covers, in its order, then the
     * modified time in site time, the filtered content, the author and the
     * dates.
     */
    public const RESTORE_POST_COLUMNS = [
        ...BuilderDocumentFingerprint::POST_FIELDS,
        ...BuilderDocumentFingerprint::PLACEMENT_FIELDS,
        'post_modified',
        'post_content_filtered',
        'post_author',
        'post_date',
        'post_date_gmt',
    ];

    /** The integer columns among RESTORE_POST_COLUMNS. */
    public const INT_COLUMNS = ['post_parent', 'menu_order', 'post_author'];

    /** The one postmeta key a snapshot leaves out: the editor's lock. */
    public const EDIT_LOCK_KEY = '_edit_lock';

    /** Seconds a snapshot is kept: 15 days, one day past the 14-day undo window. */
    public const RETENTION_SECONDS = 1296000;

    /** At most this many of the oldest rows under the prefix are examined per sweep. */
    public const SWEEP_WINDOW = 200;

    /** Bytes of a stored text a sweep reads: every member before "post" fits. */
    private const HEAD_BYTES = 160;

    /** Nesting depth of the stored text: the object, the meta list, one row. */
    private const JSON_DEPTH = 4;

    private const RE_REQUEST_ID = '/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/D';

    /** The start of a version 1 stored text, up to the "post" member. */
    private const RE_HEAD = '/^\{"version":1,"request_id":"([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})","post_id":[1-9][0-9]{0,18},"taken_at":(0|[1-9][0-9]{0,17}),"post":\{/';

    /** The members of a stored text, in order. */
    private const MEMBERS = ['version', 'request_id', 'post_id', 'taken_at', 'post', 'meta', 'revisions'];

    /**
     * Takes the snapshot of a post and stores it under the request.
     *
     * @param int    $postId    Post ID.
     * @param string $requestId The request's UUID, in any case.
     * @return array{ok:bool,sha256?:string,bytes?:int,code?:string} On success
     *         the sha256 and the length of the stored text. Otherwise a code,
     *         and this call has left no row behind: snapshot_too_large when
     *         the text would be longer than MAX_SNAPSHOT_BYTES, snapshot_failed
     *         when the post cannot be read, the text cannot be stored, or the
     *         stored bytes are not the bytes encoded. A row already stored
     *         under the request's name is never changed or removed.
     * @throws \InvalidArgumentException When the post ID or the request id is not valid.
     */
    public static function take(int $postId, string $requestId): array
    {
        if ($postId < 1) {
            throw new \InvalidArgumentException('the post ID must be at least 1');
        }
        $id = self::requestId($requestId);

        global $wpdb;
        if (!is_object($wpdb)) {
            return self::fail('snapshot_failed');
        }
        /** @var \wpdb $wpdb */
        try {
            if (self::encodedLowerBound($wpdb, $postId) > BuilderContract::MAX_SNAPSHOT_BYTES) {
                return self::fail('snapshot_too_large');
            }
            $read = self::read($wpdb, $postId);
            if ($read === null) {
                return self::fail('snapshot_failed');
            }
            $json = json_encode(
                [
                    'version'    => self::VERSION,
                    'request_id' => $id,
                    'post_id'    => $postId,
                    'taken_at'   => time(),
                    'post'       => $read['post'],
                    'meta'       => $read['meta'],
                    'revisions'  => $read['revisions'],
                ],
                JSON_THROW_ON_ERROR
            );
        } catch (\RuntimeException | \JsonException $e) {
            return self::fail('snapshot_failed');
        }
        if (strlen($json) > BuilderContract::MAX_SNAPSHOT_BYTES) {
            return self::fail('snapshot_too_large');
        }

        // A string that is not serialized data is stored as it is.
        $name = self::OPTION_PREFIX . $id;
        if (!add_option($name, $json, '', false)) {
            return self::fail('snapshot_failed');
        }

        try {
            $stored = self::storedText($wpdb, $name);
        } catch (\RuntimeException $e) {
            $stored = null;
        }
        if ($stored === null || !hash_equals(hash('sha256', $json), hash('sha256', $stored))) {
            delete_option($name);

            return self::fail('snapshot_failed');
        }

        return ['ok' => true, 'sha256' => hash('sha256', $stored), 'bytes' => strlen($stored)];
    }

    /**
     * The stored text of a request's snapshot and the sha256 of those bytes,
     * read with SQL, never through get_option() or the object cache.
     *
     * @param string $requestId The request's UUID, in any case.
     * @return array{json?:string,sha256?:string,code?:string} The text and its
     *         hash; otherwise code snapshot_missing when no snapshot is stored
     *         under the request, or snapshot_unreadable when the database
     *         cannot answer.
     * @throws \InvalidArgumentException When the request id is not a UUID.
     */
    public static function load(string $requestId): array
    {
        $name = self::OPTION_PREFIX . self::requestId($requestId);

        global $wpdb;
        if (!is_object($wpdb)) {
            return ['code' => 'snapshot_unreadable'];
        }
        /** @var \wpdb $wpdb */
        try {
            $stored = self::storedText($wpdb, $name);
        } catch (\RuntimeException $e) {
            return ['code' => 'snapshot_unreadable'];
        }
        if ($stored === null) {
            return ['code' => 'snapshot_missing'];
        }

        return ['json' => $stored, 'sha256' => hash('sha256', $stored)];
    }

    /**
     * The snapshot a stored text holds, with every value decoded back to its
     * stored bytes, or null when the text is not exactly a version 1 snapshot
     * of this request and this post: every member present once and in order,
     * every column present in order, canonical base64, integer text in the
     * integer columns, no edit lock row, and revision ids rising.
     *
     * @param string $json      A stored text.
     * @param string $requestId The request's UUID, in any case.
     * @param int    $postId    The post the snapshot must be of.
     * @return array{request_id:string,post_id:int,taken_at:int,post:array<string,string>,meta:list<array{0:string,1:string|null}>,revisions:list<int>}|null
     * @throws \InvalidArgumentException When the request id is not a UUID.
     */
    public static function decode(string $json, string $requestId, int $postId): ?array
    {
        $id = self::requestId($requestId);
        if ($postId < 1 || strlen($json) > BuilderContract::MAX_SNAPSHOT_BYTES) {
            return null;
        }
        try {
            $doc = json_decode($json, true, self::JSON_DEPTH, JSON_THROW_ON_ERROR);
        } catch (\JsonException $e) {
            return null;
        }
        if (!is_array($doc) || array_keys($doc) !== self::MEMBERS) {
            return null;
        }
        if ($doc['version'] !== self::VERSION || $doc['request_id'] !== $id || $doc['post_id'] !== $postId || !is_int($doc['taken_at']) || $doc['taken_at'] < 0) {
            return null;
        }

        if (!is_array($doc['post']) || array_keys($doc['post']) !== self::RESTORE_POST_COLUMNS) {
            return null;
        }
        $post = [];
        foreach (self::RESTORE_POST_COLUMNS as $column) {
            $bytes = self::bytesOf($doc['post'][$column]);
            if ($bytes === null || (in_array($column, self::INT_COLUMNS, true) && self::storedInt($bytes) === null)) {
                return null;
            }
            $post[$column] = $bytes;
        }

        if (!is_array($doc['meta']) || !ArrayShape::isList($doc['meta'])) {
            return null;
        }
        $meta = [];
        foreach ($doc['meta'] as $pair) {
            if (!is_array($pair) || array_keys($pair) !== [0, 1] || !is_string($pair[0]) || $pair[0] === self::EDIT_LOCK_KEY) {
                return null;
            }
            $value = $pair[1] === null ? null : self::bytesOf($pair[1]);
            if ($pair[1] !== null && $value === null) {
                return null;
            }
            $meta[] = [$pair[0], $value];
        }

        if (!is_array($doc['revisions']) || !ArrayShape::isList($doc['revisions'])) {
            return null;
        }
        $revisions = [];
        $previous  = 0;
        foreach ($doc['revisions'] as $revisionId) {
            if (!is_int($revisionId) || $revisionId <= $previous) {
                return null;
            }
            $revisions[] = $revisionId;
            $previous    = $revisionId;
        }

        return [
            'request_id' => $id,
            'post_id'    => $postId,
            'taken_at'   => $doc['taken_at'],
            'post'       => $post,
            'meta'       => $meta,
            'revisions'  => $revisions,
        ];
    }

    /**
     * Deletes stored snapshots older than RETENTION_SECONDS, oldest first, at
     * most $max of them. Of the rows under OPTION_PREFIX it examines the
     * SWEEP_WINDOW oldest, and ages only a row whose name is a snapshot name
     * byte for byte and whose text begins as a version 1 snapshot of that
     * request; any other row is left as it is. Never throws.
     *
     * @param int $max The most snapshots to delete in this call.
     * @return int The number deleted.
     */
    public static function sweep(int $max = 20): int
    {
        if ($max < 1) {
            return 0;
        }
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        /** @var \wpdb $wpdb */
        $cutoff = time() - self::RETENTION_SECONDS;
        try {
            $found = $wpdb->get_results($wpdb->prepare('SELECT option_id, option_name, SUBSTRING(option_value, 1, %d) AS head FROM %i WHERE option_name LIKE %s ORDER BY option_id ASC LIMIT %d', self::HEAD_BYTES, $wpdb->options, $wpdb->esc_like(self::OPTION_PREFIX) . '%', self::SWEEP_WINDOW), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- no API lists options by prefix, and only the head of each text is read; table via the %i identifier placeholder (WP 6.2+)
            self::assertQueryOk($wpdb);
        } catch (\RuntimeException $e) {
            return 0;
        }
        if (!is_array($found)) {
            return 0;
        }

        $deleted = 0;
        foreach ($found as $row) {
            if ($deleted >= $max) {
                break;
            }
            $row  = (array) $row;
            $name = $row['option_name'] ?? null;
            $head = $row['head'] ?? null;
            if (!is_string($name) || !is_string($head)) {
                continue;
            }
            $takenAt = self::takenAt($name, $head);
            if ($takenAt === null || $takenAt >= $cutoff) {
                continue;
            }
            if (delete_option($name)) {
                ++$deleted;
            }
        }

        return $deleted;
    }

    /**
     * The least length the stored text can have, from the bytes of the
     * post's meta values alone: base64 makes every three bytes four, and the
     * text holds more than the values. Read before the rows, so a post whose
     * meta could never fit is refused without loading it.
     *
     * @param object $wpdb   The database handle.
     * @param int    $postId Post ID.
     * @return int
     * @throws \RuntimeException When the database cannot answer.
     */
    private static function encodedLowerBound(object $wpdb, int $postId): int
    {
        /** @var \wpdb $wpdb */
        // The collation may skip rows whose key differs from the lock's only
        // in case or trailing spaces; they are kept in the snapshot, so the
        // sum stays a lower bound.
        $sum = $wpdb->get_var($wpdb->prepare('SELECT COALESCE(SUM(LENGTH(meta_value)), 0) FROM %i WHERE post_id = %d AND meta_key <> %s', $wpdb->postmeta, $postId, self::EDIT_LOCK_KEY)); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the stored length of the rows, before they are read; table via the %i identifier placeholder (WP 6.2+)
        self::assertQueryOk($wpdb);
        $bytes = self::storedInt($sum);
        if ($bytes === null || $bytes < 0) {
            throw new \RuntimeException('database read failed');
        }
        if ($bytes > intdiv(PHP_INT_MAX - 2, 4)) {
            return PHP_INT_MAX;
        }

        return intdiv(4 * $bytes + 2, 3);
    }

    /**
     * The posts row, the meta rows and the revision ids of a post, with every
     * stored value base64.
     *
     * @param object $wpdb   The database handle.
     * @param int    $postId Post ID.
     * @return array{post:array<string,string>,meta:list<array{0:string,1:string|null}>,revisions:list<int>}|null Null when there is no such post.
     * @throws \RuntimeException When the database cannot answer, or answers with something that is not stored bytes.
     */
    private static function read(object $wpdb, int $postId): ?array
    {
        /** @var \wpdb $wpdb */
        $row = $wpdb->get_row($wpdb->prepare('SELECT post_type, post_status, post_title, post_content, post_excerpt, post_name, post_password, post_modified_gmt, post_parent, menu_order, post_modified, post_content_filtered, post_author, post_date, post_date_gmt FROM %i WHERE ID = %d', $wpdb->posts, $postId), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the snapshot holds the stored row, so it is read uncached; table via the %i identifier placeholder (WP 6.2+)
        self::assertQueryOk($wpdb);
        if ($row === null) {
            return null;
        }
        if (!is_array($row)) {
            throw new \RuntimeException('database read failed');
        }
        $post = [];
        foreach (self::RESTORE_POST_COLUMNS as $column) {
            $value = $row[$column] ?? null;
            if (in_array($column, self::INT_COLUMNS, true)) {
                $int   = self::storedInt($value);
                $value = $int === null ? null : (string) $int;
            }
            if (!is_string($value)) {
                throw new \RuntimeException('database read failed');
            }
            $post[$column] = base64_encode($value);
        }

        $found = $wpdb->get_results($wpdb->prepare('SELECT meta_key, meta_value FROM %i WHERE post_id = %d ORDER BY meta_id ASC', $wpdb->postmeta, $postId), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the stored bytes of every row in meta_id order, never the meta cache; table via the %i identifier placeholder (WP 6.2+)
        self::assertQueryOk($wpdb);
        if (!is_array($found)) {
            throw new \RuntimeException('database read failed');
        }
        $meta = [];
        foreach ($found as $metaRow) {
            $metaRow = (array) $metaRow;
            $key     = $metaRow['meta_key'] ?? null;
            if (!is_string($key) || !array_key_exists('meta_value', $metaRow)) {
                throw new \RuntimeException('a postmeta row is not stored bytes');
            }
            $value = $metaRow['meta_value'];
            if ($value !== null && !is_string($value)) {
                throw new \RuntimeException('a postmeta row is not stored bytes');
            }
            // Only the exact key is the lock; a key the collation would
            // match is another row and is kept.
            if ($key === self::EDIT_LOCK_KEY) {
                continue;
            }
            $meta[] = [$key, $value === null ? null : base64_encode($value)];
        }

        $ids = $wpdb->get_col($wpdb->prepare('SELECT ID FROM %i WHERE post_parent = %d AND post_type = %s ORDER BY ID ASC', $wpdb->posts, $postId, 'revision')); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the revisions stored when the snapshot is taken, read uncached; table via the %i identifier placeholder (WP 6.2+)
        self::assertQueryOk($wpdb);
        if (!is_array($ids)) {
            throw new \RuntimeException('database read failed');
        }
        $revisions = [];
        foreach ($ids as $raw) {
            $revisionId = self::storedInt($raw);
            if ($revisionId === null || $revisionId < 1) {
                throw new \RuntimeException('database read failed');
            }
            $revisions[] = $revisionId;
        }

        return ['post' => $post, 'meta' => $meta, 'revisions' => $revisions];
    }

    /**
     * The stored text of an option, by its exact name, read with SQL.
     *
     * @param object $wpdb The database handle.
     * @param string $name Option name.
     * @return string|null Null when no row has exactly this name and a text value.
     * @throws \RuntimeException When the database cannot answer.
     */
    private static function storedText(object $wpdb, string $name): ?string
    {
        /** @var \wpdb $wpdb */
        $row = $wpdb->get_row($wpdb->prepare('SELECT option_name, option_value FROM %i WHERE option_name = %s', $wpdb->options, $name), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the stored bytes, never a cached or filtered option value; table via the %i identifier placeholder (WP 6.2+)
        self::assertQueryOk($wpdb);
        if ($row === null) {
            return null;
        }
        if (!is_array($row)) {
            throw new \RuntimeException('database read failed');
        }
        // The collation may match a name that differs in case or trailing
        // spaces; only the exact name is the snapshot's.
        if (($row['option_name'] ?? null) !== $name || !is_string($row['option_value'] ?? null)) {
            return null;
        }

        return $row['option_value'];
    }

    /**
     * When a sweep may age a row: the taken_at of a stored text whose name is
     * a snapshot name and whose head is a version 1 snapshot of that request.
     *
     * @param string $name Stored option name.
     * @param string $head The first HEAD_BYTES of its text.
     * @return int|null
     */
    private static function takenAt(string $name, string $head): ?int
    {
        if (!str_starts_with($name, self::OPTION_PREFIX)) {
            return null;
        }
        $id = substr($name, strlen(self::OPTION_PREFIX));
        if (preg_match(self::RE_REQUEST_ID, $id) !== 1) {
            return null;
        }
        if (preg_match(self::RE_HEAD, $head, $m) !== 1 || $m[1] !== $id) {
            return null;
        }

        return (int) $m[2];
    }

    /**
     * The bytes of a canonical base64 string, or null.
     *
     * @param mixed $encoded A value from a stored text.
     * @return string|null
     */
    private static function bytesOf(mixed $encoded): ?string
    {
        if (!is_string($encoded)) {
            return null;
        }
        $bytes = base64_decode($encoded, true);
        if ($bytes === false || base64_encode($bytes) !== $encoded) {
            return null;
        }

        return $bytes;
    }

    /**
     * An integer as the database answered it: its decimal text, as mysqli
     * answers every column, or an int from a driver that returns native
     * types. Anything that is not a plain integer is null.
     *
     * @param mixed $value The column.
     * @return int|null
     */
    private static function storedInt(mixed $value): ?int
    {
        if (is_int($value)) {
            return $value;
        }
        if (!is_string($value)) {
            return null;
        }
        $int = (int) $value;

        return (string) $int === $value ? $int : null;
    }

    /**
     * The request id in lowercase.
     *
     * @param string $requestId A request id.
     * @return string
     * @throws \InvalidArgumentException When it is not a UUID.
     */
    private static function requestId(string $requestId): string
    {
        $id = strtolower($requestId);
        if (preg_match(self::RE_REQUEST_ID, $id) !== 1) {
            throw new \InvalidArgumentException('the request id must be a UUID');
        }

        return $id;
    }

    /**
     * @param string $code Failure code.
     * @return array{ok:bool,code:string}
     */
    private static function fail(string $code): array
    {
        return ['ok' => false, 'code' => $code];
    }

    /**
     * A query that recorded an error fails the read; it is never read as "no rows".
     *
     * @param object $wpdb The database handle.
     * @throws \RuntimeException When the last query failed.
     */
    private static function assertQueryOk(object $wpdb): void
    {
        if (isset($wpdb->last_error) && (string) $wpdb->last_error !== '') {
            throw new \RuntimeException('database read failed');
        }
    }
}
