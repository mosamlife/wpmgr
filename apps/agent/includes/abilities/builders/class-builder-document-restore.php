<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Puts a builder page back from its BuilderDocumentSnapshot, at the SQL layer.
 *
 * Every write is a $wpdb statement on the posts and postmeta tables, never
 * the meta API: no hook fires for a row, and no value is slashed, unslashed or
 * serialized, so a restored row holds the bytes it held when the snapshot
 * was taken, and a NULL meta_value is NULL again. A row belongs to a key only
 * when its meta_key is that key byte for byte, and rows are deleted by
 * meta_id, so a key the collation would match is never touched by mistake.
 * The edit lock (BuilderDocumentSnapshot::EDIT_LOCK_KEY, byte for byte) is
 * never deleted, written or guarded.
 *
 * full() is the automatic restore inside the call that wrote. The post's meta
 * rows, the edit lock and the derived keys aside, become exactly the
 * snapshot's rows in the snapshot's order (the meta_ids are new, the order is
 * kept): a key the write added is deleted, a key it removed comes back with
 * its row count. Every RESTORE_POST_COLUMNS column goes back to the
 * snapshot's bytes. Revisions the write made are left as they are. The
 * page's fingerprint must then equal the one it had before the write.
 *
 * scoped() is a person's undo of one change. Only the meta keys and the posts
 * columns the change wrote go back, and only when each still holds exactly
 * the bytes the change left; otherwise nothing is written. Every other key,
 * such as a featured image set after the change, is kept. Each key and
 * column is then read back and must hold the bytes it held before the
 * change.
 *
 * In both, the reads that decide the writes and the writes themselves run in
 * one transaction, rolled back on any database error. The descriptor's
 * derived keys (caches the builder rebuilds from the page) are deleted, never
 * restored or guarded. After the commit the post's object caches are
 * dropped and the adapter's afterRestore() drops what the builder caches
 * outside these rows. Must not be called inside an open transaction: starting
 * one commits it.
 *
 * Hashes, as the ledger records them for a change:
 *
 * - a key's rows: rowsSha256(), the sha256 of rowsDigest(), over the key's
 *   rows in meta_id order; a key with no row is rowsDigest([]);
 * - a posts column: the sha256 of its stored bytes, an integer column's
 *   decimal text (as a snapshot keeps it).
 */
final class BuilderDocumentRestore
{
    /** The page could not be put back as the snapshot holds it. */
    public const CODE_MISMATCH = 'restore_mismatch';

    /** A person's undo found something the change wrote changed since. */
    public const CODE_CONFLICT = 'conflict';

    /** scoped(): a key or column the change wrote no longer holds what the change left. */
    public const DETAIL_CHANGED = 'changed_after_this_change';

    /** scoped(): a database statement failed; the transaction was rolled back, nothing changed. */
    public const DETAIL_WRITE_FAILED = 'write_failed';

    /** scoped(): the snapshot does not hold the bytes the change replaced; nothing was written. */
    public const DETAIL_SNAPSHOT = 'snapshot_disagrees';

    /** scoped(): read back after the restore, a key or column does not hold its bytes from before the change. */
    public const DETAIL_READ_BACK = 'read_back_differs';

    /** scoped(): the post is gone; nothing was written. */
    public const DETAIL_POST_MISSING = 'post_missing';

    private const RE_SHA256 = '/^[0-9a-f]{64}$/D';

    /** The members of a changed key, in any order. */
    private const KEY_MEMBERS = ['after_sha256', 'before_sha256', 'key'];

    /** The members of a changed field, in any order. */
    private const FIELD_MEMBERS = ['after_sha256', 'before_sha256', 'field'];

    /**
     * The automatic restore after a failed write: the post goes back to the
     * snapshot, then its fingerprint is read again.
     *
     * @param int                $postId   Post ID; the snapshot must be of this post.
     * @param array<mixed>       $snapshot What BuilderDocumentSnapshot::decode() returned.
     * @param DocumentDescriptor $d        The adapter's descriptor.
     * @param BuilderAdapter     $a        The adapter.
     * @param string             $beforeFp The page's builder_document_v1 fingerprint before the write.
     * @return string|null Null when the post is back and its fingerprint equals
     *                     $beforeFp; otherwise CODE_MISMATCH. A database error
     *                     rolls the restore back, so the post keeps the rows the
     *                     failed write left.
     * @throws \InvalidArgumentException When the snapshot is not one of this post, or $beforeFp is not a sha256.
     */
    public static function full(int $postId, array $snapshot, DocumentDescriptor $d, BuilderAdapter $a, string $beforeFp): ?string
    {
        $snap = self::snapshot($postId, $snapshot);
        if (preg_match(self::RE_SHA256, $beforeFp) !== 1) {
            throw new \InvalidArgumentException('the fingerprint must be a lowercase sha256');
        }
        $derived = $d->derivedKeys;

        $failed = self::apply(
            $postId,
            static function (array $rows) use ($snap, $derived): array {
                $delete = [];
                foreach ($rows as $row) {
                    if ($row['key'] !== BuilderDocumentSnapshot::EDIT_LOCK_KEY) {
                        $delete[] = $row['id'];
                    }
                }
                $insert = [];
                foreach ($snap['meta'] as $pair) {
                    if (!in_array($pair[0], $derived, true)) {
                        $insert[] = $pair;
                    }
                }

                return ['delete' => $delete, 'insert' => $insert, 'post' => $snap['post']];
            }
        );
        if ($failed !== null) {
            return self::CODE_MISMATCH;
        }

        self::afterWrite($postId, $a);
        try {
            $now = BuilderDocumentFingerprint::ofPost($postId, $d->exactKeys);
        } catch (\Throwable $e) {
            return self::CODE_MISMATCH;
        }

        return $now !== null && hash_equals($beforeFp, $now) ? null : self::CODE_MISMATCH;
    }

    /**
     * A person's undo of one change: the keys and columns it wrote go back to
     * the snapshot, guarded all-or-nothing, and are read back.
     *
     * A changed key that is a derived key or the edit lock is neither guarded
     * nor restored. A key the snapshot does not hold is deleted.
     *
     * @param int                $postId        Post ID; the snapshot must be of this post.
     * @param array<mixed>       $snapshot      What BuilderDocumentSnapshot::decode() returned.
     * @param array<mixed>       $changedKeys   The change's keys: list of {key, before_sha256, after_sha256}.
     * @param array<mixed>       $changedFields The change's posts columns: list of {field, before_sha256, after_sha256}.
     * @param DocumentDescriptor $d             The adapter's descriptor.
     * @param BuilderAdapter     $a             The adapter.
     * @return array{code:string,detail:string}|null Null when every key and
     *         column is back. CODE_CONFLICT with DETAIL_CHANGED when one no
     *         longer holds what the change left, and CODE_MISMATCH with a DETAIL_
     *         token otherwise. Neither names a key, a column or anything
     *         read from the site. Every refusal but DETAIL_READ_BACK has
     *         written nothing.
     * @throws \InvalidArgumentException When the snapshot is not one of this post, or a changed key or field is not well formed.
     */
    public static function scoped(int $postId, array $snapshot, array $changedKeys, array $changedFields, DocumentDescriptor $d, BuilderAdapter $a): ?array
    {
        $snap    = self::snapshot($postId, $snapshot);
        $keys    = self::changedKeys($changedKeys);
        $fields  = self::changedFields($changedFields);
        $derived = $d->derivedKeys;

        $restore = [];
        foreach ($keys as $k) {
            if ($k['key'] !== BuilderDocumentSnapshot::EDIT_LOCK_KEY && !in_array($k['key'], $derived, true)) {
                $restore[] = $k;
            }
        }

        // The snapshot must hold what the change replaced, or the restore
        // could never read back as it was.
        foreach ($restore as $k) {
            if (!hash_equals($k['before'], self::rowsSha256(self::snapshotValues($snap['meta'], $k['key'])))) {
                return self::refusal(self::CODE_MISMATCH, self::DETAIL_SNAPSHOT);
            }
        }
        foreach ($fields as $f) {
            if (!hash_equals($f['before'], hash('sha256', $snap['post'][$f['field']]))) {
                return self::refusal(self::CODE_MISMATCH, self::DETAIL_SNAPSHOT);
            }
        }

        $names  = array_column($restore, 'key');
        $failed = self::apply(
            $postId,
            static function (array $rows, array $post) use ($snap, $restore, $fields, $names, $derived): array {
                foreach ($restore as $k) {
                    if (!hash_equals($k['after'], self::rowsSha256(self::currentValues($rows, $k['key'])))) {
                        return ['refuse' => self::refusal(self::CODE_CONFLICT, self::DETAIL_CHANGED)];
                    }
                }
                foreach ($fields as $f) {
                    if (!hash_equals($f['after'], hash('sha256', $post[$f['field']]))) {
                        return ['refuse' => self::refusal(self::CODE_CONFLICT, self::DETAIL_CHANGED)];
                    }
                }

                $delete = [];
                foreach ($rows as $row) {
                    if (in_array($row['key'], $names, true) || in_array($row['key'], $derived, true)) {
                        $delete[] = $row['id'];
                    }
                }
                $insert = [];
                foreach ($snap['meta'] as $pair) {
                    if (in_array($pair[0], $names, true)) {
                        $insert[] = $pair;
                    }
                }
                $columns = [];
                foreach ($fields as $f) {
                    $columns[$f['field']] = $snap['post'][$f['field']];
                }

                return ['delete' => $delete, 'insert' => $insert, 'post' => $columns];
            }
        );
        if ($failed !== null) {
            return $failed;
        }

        self::afterWrite($postId, $a);
        try {
            $now = self::read($postId, false);
        } catch (\Throwable $e) {
            $now = null;
        }
        if ($now === null) {
            return self::refusal(self::CODE_MISMATCH, self::DETAIL_READ_BACK);
        }
        foreach ($restore as $k) {
            if (!hash_equals($k['before'], self::rowsSha256(self::currentValues($now['rows'], $k['key'])))) {
                return self::refusal(self::CODE_MISMATCH, self::DETAIL_READ_BACK);
            }
        }
        foreach ($fields as $f) {
            if (!hash_equals($f['before'], hash('sha256', $now['post'][$f['field']]))) {
                return self::refusal(self::CODE_MISMATCH, self::DETAIL_READ_BACK);
            }
        }

        return null;
    }

    /**
     * The digest of one key's rows: json_encode, default flags, of
     * [row_count, [sha256(row), ...]] with the rows in meta_id order, each
     * sha256 lowercase hex over the stored bytes, and null for a row the
     * database holds as NULL. A key with no row is "[0,[]]", which is not the
     * digest of one empty row.
     *
     * @param array<mixed> $rows The key's stored rows, a list of strings and nulls.
     * @return string
     * @throws \InvalidArgumentException When $rows is not a list of strings and nulls.
     */
    public static function rowsDigest(array $rows): string
    {
        if (!ArrayShape::isList($rows)) {
            throw new \InvalidArgumentException('the rows must be a list');
        }
        $hashes = [];
        foreach ($rows as $row) {
            if ($row !== null && !is_string($row)) {
                throw new \InvalidArgumentException('a row must be the stored string or null');
            }
            $hashes[] = $row === null ? null : hash('sha256', $row);
        }

        return (string) json_encode([count($hashes), $hashes]);
    }

    /**
     * The hash a change records for one key: the sha256, lowercase hex, of
     * rowsDigest($rows).
     *
     * @param array<mixed> $rows The key's stored rows, a list of strings and nulls.
     * @return string
     * @throws \InvalidArgumentException See rowsDigest().
     */
    public static function rowsSha256(array $rows): string
    {
        return hash('sha256', self::rowsDigest($rows));
    }

    /**
     * Runs one restore: opens a transaction, reads the post's meta rows and
     * posts row with locking reads, asks $plan what to write, writes it and
     * commits. Rolls back on a refusal from $plan and on any failure.
     *
     * @param int                                                                                                        $postId Post ID.
     * @param \Closure(list<array{id:int,key:string,value:string|null}>, array<string,string>): array<string,mixed> $plan   (rows, post) => {delete: list<int>, insert: list<array{0:string,1:string|null}>, post: array<string,string>} or {refuse: {code, detail}}.
     * @return array{code:string,detail:string}|null Null when committed.
     */
    private static function apply(int $postId, \Closure $plan): ?array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return self::refusal(self::CODE_MISMATCH, self::DETAIL_WRITE_FAILED);
        }
        /** @var \wpdb $wpdb */
        if ($wpdb->query('START TRANSACTION') === false || self::lastError($wpdb)) { // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the restore's writes commit or roll back together
            return self::refusal(self::CODE_MISMATCH, self::DETAIL_WRITE_FAILED);
        }

        try {
            $current = self::read($postId, true);
            if ($current === null) {
                $wpdb->query('ROLLBACK'); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- ends the restore's transaction; nothing was written

                return self::refusal(self::CODE_MISMATCH, self::DETAIL_POST_MISSING);
            }
            $writes = $plan($current['rows'], $current['post']);
            if (isset($writes['refuse'])) {
                $wpdb->query('ROLLBACK'); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- ends the restore's transaction; nothing was written

                return $writes['refuse'];
            }

            foreach ($writes['delete'] as $metaId) {
                // By meta_id: the collation would match other keys' rows.
                $deleted = $wpdb->delete($wpdb->postmeta, ['meta_id' => $metaId, 'post_id' => $postId], ['%d', '%d']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the meta API deletes every row the collation matches and fires hooks; the caches are dropped after the commit
                self::assertWritten($wpdb, $deleted !== false);
            }
            foreach ($writes['insert'] as [$key, $value]) {
                $inserted = $wpdb->insert($wpdb->postmeta, ['post_id' => $postId, 'meta_key' => $key, 'meta_value' => $value], ['%d', '%s', '%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.SlowDBQuery.slow_db_query_meta_key,WordPress.DB.SlowDBQuery.slow_db_query_meta_value -- the stored bytes, never unslashed or serialized by the meta API; a null value is stored as NULL
                self::assertWritten($wpdb, $inserted === 1);
            }
            if ($writes['post'] !== []) {
                $data    = [];
                $formats = [];
                foreach ($writes['post'] as $column => $bytes) {
                    $int              = in_array($column, BuilderDocumentSnapshot::INT_COLUMNS, true);
                    $data[$column]    = $int ? (int) $bytes : $bytes;
                    $formats[]        = $int ? '%d' : '%s';
                }
                $updated = $wpdb->update($wpdb->posts, $data, ['ID' => $postId], $formats, ['%d']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the stored columns as the snapshot holds them, with no post hooks; the caches are dropped after the commit
                self::assertWritten($wpdb, $updated !== false);
            }

            $committed = $wpdb->query('COMMIT'); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the restore's writes land together
            self::assertWritten($wpdb, $committed !== false);
        } catch (\Throwable $e) {
            $wpdb->query('ROLLBACK'); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- a failed restore leaves the rows it found

            return self::refusal(self::CODE_MISMATCH, self::DETAIL_WRITE_FAILED);
        }

        return null;
    }

    /**
     * After the commit: the post's object caches, then what the builder
     * caches outside the rows.
     *
     * @param int            $postId Post ID.
     * @param BuilderAdapter $a      The adapter.
     */
    private static function afterWrite(int $postId, BuilderAdapter $a): void
    {
        wp_cache_delete($postId, 'post_meta');
        wp_cache_delete($postId, 'posts');
        clean_post_cache($postId);
        try {
            $a->afterRestore($postId);
        } catch (\Throwable $e) {
            // The rows are back; the builder rebuilds its caches from them.
            unset($e);
        }
    }

    /**
     * The post's meta rows in meta_id order and its RESTORE_POST_COLUMNS,
     * read with SQL; locking reads inside the restore's transaction.
     *
     * @param int  $postId Post ID.
     * @param bool $lock   Read FOR UPDATE.
     * @return array{rows:list<array{id:int,key:string,value:string|null}>,post:array<string,string>}|null Null when there is no such post.
     * @throws \RuntimeException When the database cannot answer, or answers with something that is not stored bytes.
     */
    private static function read(int $postId, bool $lock): ?array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            throw new \RuntimeException('database handle unavailable');
        }
        /** @var \wpdb $wpdb */
        if ($lock) {
            $row = $wpdb->get_row($wpdb->prepare('SELECT post_type, post_status, post_title, post_content, post_excerpt, post_name, post_password, post_modified_gmt, post_parent, menu_order, post_modified, post_content_filtered, post_author, post_date, post_date_gmt FROM %i WHERE ID = %d FOR UPDATE', $wpdb->posts, $postId), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- a locking read of the stored row inside the restore's transaction; table via the %i identifier placeholder (WP 6.2+)
        } else {
            $row = $wpdb->get_row($wpdb->prepare('SELECT post_type, post_status, post_title, post_content, post_excerpt, post_name, post_password, post_modified_gmt, post_parent, menu_order, post_modified, post_content_filtered, post_author, post_date, post_date_gmt FROM %i WHERE ID = %d', $wpdb->posts, $postId), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the stored row as the restore left it, never a cached post; table via the %i identifier placeholder (WP 6.2+)
        }
        self::assertRead($wpdb);
        if ($row === null) {
            return null;
        }
        if (!is_array($row)) {
            throw new \RuntimeException('database read failed');
        }
        $post = [];
        foreach (BuilderDocumentSnapshot::RESTORE_POST_COLUMNS as $column) {
            $value = $row[$column] ?? null;
            if (in_array($column, BuilderDocumentSnapshot::INT_COLUMNS, true)) {
                $int   = self::storedInt($value);
                $value = $int === null ? null : (string) $int;
            }
            if (!is_string($value)) {
                throw new \RuntimeException('database read failed');
            }
            $post[$column] = $value;
        }

        if ($lock) {
            $found = $wpdb->get_results($wpdb->prepare('SELECT meta_id, meta_key, meta_value FROM %i WHERE post_id = %d ORDER BY meta_id ASC FOR UPDATE', $wpdb->postmeta, $postId), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- a locking read of every stored row inside the restore's transaction, never the meta cache; table via the %i identifier placeholder (WP 6.2+)
        } else {
            $found = $wpdb->get_results($wpdb->prepare('SELECT meta_id, meta_key, meta_value FROM %i WHERE post_id = %d ORDER BY meta_id ASC', $wpdb->postmeta, $postId), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- every stored row as the restore left it, never the meta cache; table via the %i identifier placeholder (WP 6.2+)
        }
        self::assertRead($wpdb);
        if (!is_array($found)) {
            throw new \RuntimeException('database read failed');
        }
        $rows = [];
        foreach ($found as $metaRow) {
            $metaRow = (array) $metaRow;
            $id      = self::storedInt($metaRow['meta_id'] ?? null);
            $key     = $metaRow['meta_key'] ?? null;
            if ($id === null || $id < 1 || !is_string($key) || !array_key_exists('meta_value', $metaRow)) {
                throw new \RuntimeException('a postmeta row is not stored bytes');
            }
            $value = $metaRow['meta_value'];
            if ($value !== null && !is_string($value)) {
                throw new \RuntimeException('a postmeta row is not stored bytes');
            }
            $rows[] = ['id' => $id, 'key' => $key, 'value' => $value];
        }

        return ['rows' => $rows, 'post' => $post];
    }

    /**
     * The decoded snapshot's columns and meta rows, checked to be of this post.
     *
     * @param int          $postId   Post ID.
     * @param array<mixed> $snapshot What BuilderDocumentSnapshot::decode() returned.
     * @return array{post:array<string,string>,meta:list<array{0:string,1:string|null}>}
     * @throws \InvalidArgumentException When it is not a decoded snapshot of this post.
     */
    private static function snapshot(int $postId, array $snapshot): array
    {
        if ($postId < 1 || ($snapshot['post_id'] ?? null) !== $postId) {
            throw new \InvalidArgumentException('the snapshot is not of this post');
        }
        $columns = $snapshot['post'] ?? null;
        if (!is_array($columns) || count($columns) !== count(BuilderDocumentSnapshot::RESTORE_POST_COLUMNS)) {
            throw new \InvalidArgumentException('the snapshot must hold every restored column');
        }
        $post = [];
        foreach (BuilderDocumentSnapshot::RESTORE_POST_COLUMNS as $column) {
            $bytes = $columns[$column] ?? null;
            if (!is_string($bytes) || (in_array($column, BuilderDocumentSnapshot::INT_COLUMNS, true) && self::storedInt($bytes) === null)) {
                throw new \InvalidArgumentException('the snapshot must hold every restored column as stored bytes');
            }
            $post[$column] = $bytes;
        }

        $pairs = $snapshot['meta'] ?? null;
        if (!is_array($pairs) || !ArrayShape::isList($pairs)) {
            throw new \InvalidArgumentException('the snapshot meta must be a list');
        }
        $meta = [];
        foreach ($pairs as $pair) {
            if (!is_array($pair) || array_keys($pair) !== [0, 1] || !is_string($pair[0]) || $pair[0] === BuilderDocumentSnapshot::EDIT_LOCK_KEY || ($pair[1] !== null && !is_string($pair[1]))) {
                throw new \InvalidArgumentException('a snapshot meta row must be [key, stored bytes or null], never the edit lock');
            }
            $meta[] = [$pair[0], $pair[1]];
        }

        return ['post' => $post, 'meta' => $meta];
    }

    /**
     * @param array<mixed> $changed List of {key, before_sha256, after_sha256}.
     * @return list<array{key:string,before:string,after:string}>
     * @throws \InvalidArgumentException When an entry is not well formed or a key is named twice.
     */
    private static function changedKeys(array $changed): array
    {
        if (!ArrayShape::isList($changed)) {
            throw new \InvalidArgumentException('the changed keys must be a list');
        }
        $out  = [];
        $seen = [];
        foreach ($changed as $entry) {
            if (!is_array($entry) || !self::members($entry, self::KEY_MEMBERS) || !is_string($entry['key']) || !self::sha256s($entry)) {
                throw new \InvalidArgumentException('a changed key must be {key, before_sha256, after_sha256}');
            }
            if (in_array($entry['key'], $seen, true)) {
                throw new \InvalidArgumentException('a key is named twice');
            }
            $seen[] = $entry['key'];
            $out[]  = ['key' => $entry['key'], 'before' => $entry['before_sha256'], 'after' => $entry['after_sha256']];
        }

        return $out;
    }

    /**
     * @param array<mixed> $changed List of {field, before_sha256, after_sha256}.
     * @return list<array{field:string,before:string,after:string}>
     * @throws \InvalidArgumentException When an entry is not well formed, names a column a snapshot does not keep, or a column twice.
     */
    private static function changedFields(array $changed): array
    {
        if (!ArrayShape::isList($changed)) {
            throw new \InvalidArgumentException('the changed fields must be a list');
        }
        $out  = [];
        $seen = [];
        foreach ($changed as $entry) {
            if (!is_array($entry) || !self::members($entry, self::FIELD_MEMBERS) || !self::sha256s($entry) || !in_array($entry['field'], BuilderDocumentSnapshot::RESTORE_POST_COLUMNS, true)) {
                throw new \InvalidArgumentException('a changed field must be {field, before_sha256, after_sha256} naming a restored column');
            }
            if (in_array($entry['field'], $seen, true)) {
                throw new \InvalidArgumentException('a column is named twice');
            }
            $seen[] = $entry['field'];
            $out[]  = ['field' => $entry['field'], 'before' => $entry['before_sha256'], 'after' => $entry['after_sha256']];
        }

        return $out;
    }

    /**
     * Whether an entry has exactly these members.
     *
     * @param array<mixed> $entry   Entry.
     * @param list<string> $members Member names, sorted.
     * @return bool
     */
    private static function members(array $entry, array $members): bool
    {
        $names = array_map('strval', array_keys($entry));
        sort($names);

        return $names === $members;
    }

    /**
     * Whether before_sha256 and after_sha256 are lowercase sha256 hex.
     *
     * @param array<mixed> $entry Entry.
     * @return bool
     */
    private static function sha256s(array $entry): bool
    {
        foreach (['before_sha256', 'after_sha256'] as $member) {
            if (!is_string($entry[$member]) || preg_match(self::RE_SHA256, $entry[$member]) !== 1) {
                return false;
            }
        }

        return true;
    }

    /**
     * The snapshot's rows of one key, byte for byte, in snapshot order.
     *
     * @param list<array{0:string,1:string|null}> $meta Snapshot rows.
     * @param string                              $key  Meta key.
     * @return list<string|null>
     */
    private static function snapshotValues(array $meta, string $key): array
    {
        $values = [];
        foreach ($meta as $pair) {
            if ($pair[0] === $key) {
                $values[] = $pair[1];
            }
        }

        return $values;
    }

    /**
     * The stored rows of one key, byte for byte, in meta_id order.
     *
     * @param list<array{id:int,key:string,value:string|null}> $rows Stored rows.
     * @param string                                           $key  Meta key.
     * @return list<string|null>
     */
    private static function currentValues(array $rows, string $key): array
    {
        $values = [];
        foreach ($rows as $row) {
            if ($row['key'] === $key) {
                $values[] = $row['value'];
            }
        }

        return $values;
    }

    /**
     * An integer as the database or a snapshot holds it: its decimal text,
     * or an int from a driver that returns native types. Anything else is
     * null.
     *
     * @param mixed $value The value.
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
     * @param string $code   Code.
     * @param string $detail Detail token.
     * @return array{code:string,detail:string}
     */
    private static function refusal(string $code, string $detail): array
    {
        return ['code' => $code, 'detail' => $detail];
    }

    /**
     * Whether the last statement recorded an error.
     *
     * @param object $wpdb The database handle.
     * @return bool
     */
    private static function lastError(object $wpdb): bool
    {
        return isset($wpdb->last_error) && (string) $wpdb->last_error !== '';
    }

    /**
     * A read that recorded an error fails; it is never read as "no rows".
     *
     * @param object $wpdb The database handle.
     * @throws \RuntimeException When the last statement failed.
     */
    private static function assertRead(object $wpdb): void
    {
        if (self::lastError($wpdb)) {
            throw new \RuntimeException('database read failed');
        }
    }

    /**
     * A write that failed, or recorded an error, ends the restore.
     *
     * @param object $wpdb The database handle.
     * @param bool   $ok   Whether the statement's answer was a success.
     * @throws \RuntimeException When it did not succeed.
     */
    private static function assertWritten(object $wpdb, bool $ok): void
    {
        if (!$ok || self::lastError($wpdb)) {
            throw new \RuntimeException('database write failed');
        }
    }
}
