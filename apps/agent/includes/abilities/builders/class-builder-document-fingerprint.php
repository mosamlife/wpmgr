<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * builder_document_v1: the fingerprint of one builder page as it is stored.
 *
 *   sha256(json_encode([
 *     "wpmgr.builder_document.v1",
 *     post_type, post_status, sha256(post_title), sha256(post_content), post_modified_gmt,
 *     [[key, row_count, [sha256(row), ...]], ...]
 *   ]))
 *
 * The keys are the adapter's descriptor keys in byte order (strcmp). Each
 * key's rows are its postmeta rows in meta_id order, each hashed on its own
 * over the stored bytes; a key with no row is [key, 0, []], which is not the
 * same as a key with one empty row. json_encode uses its default flags and
 * every sha256 is lowercase hex.
 *
 * read() takes the posts row and the postmeta rows from SQL, never from
 * get_post_meta() or the object cache, so the fingerprint covers the bytes
 * in the database. A row counts for a key only when its meta_key is that key
 * byte for byte. The control plane replays the formula from the fixture
 * tests/fixtures/ability-run/builder-document-fp.json.
 */
final class BuilderDocumentFingerprint
{
    public const DOMAIN = 'wpmgr.builder_document.v1';

    /** The posts columns the fingerprint covers. */
    public const POST_FIELDS = ['post_type', 'post_status', 'post_title', 'post_content', 'post_modified_gmt'];

    /**
     * The fingerprint of a post and its rows under the given descriptor keys.
     *
     * @param array<string,mixed> $post      The POST_FIELDS, each a string.
     * @param array<mixed>        $rowsByKey Descriptor key => stored rows in meta_id order.
     * @param array<mixed>        $keys      The descriptor keys, in any order.
     * @return string Lowercase hex.
     * @throws \InvalidArgumentException When a post field, key or row is not a string, or rows name a key outside $keys.
     * @throws \JsonException            Never for valid strings.
     */
    public static function compute(array $post, array $rowsByKey, array $keys): string
    {
        $keys   = self::sortedKeys($keys);
        $fields = [];
        foreach (self::POST_FIELDS as $field) {
            if (!isset($post[$field]) || !is_string($post[$field])) {
                throw new \InvalidArgumentException('every post field must be a string');
            }
            $fields[$field] = $post[$field];
        }
        foreach ($rowsByKey as $key => $rows) {
            if (!in_array((string) $key, $keys, true)) {
                throw new \InvalidArgumentException('rows are given for a key outside the descriptor');
            }
            if (!is_array($rows) || !array_is_list($rows)) {
                throw new \InvalidArgumentException('the rows of a key must be a list');
            }
        }

        $meta = [];
        foreach ($keys as $key) {
            $hashes = [];
            foreach ($rowsByKey[$key] ?? [] as $row) {
                if (!is_string($row)) {
                    throw new \InvalidArgumentException('a row must be the stored string');
                }
                $hashes[] = hash('sha256', $row);
            }
            $meta[] = [$key, count($hashes), $hashes];
        }

        return hash('sha256', json_encode([
            self::DOMAIN,
            $fields['post_type'],
            $fields['post_status'],
            hash('sha256', $fields['post_title']),
            hash('sha256', $fields['post_content']),
            $fields['post_modified_gmt'],
            $meta,
        ], JSON_THROW_ON_ERROR));
    }

    /**
     * The stored post fields and descriptor rows of one post, read with SQL.
     *
     * @param int          $postId Post ID.
     * @param array<mixed> $keys   The descriptor keys.
     * @return array{post:array<string,string>,rows:array<string,list<string>>}|null Null when there is no such post.
     * @throws \InvalidArgumentException When the post ID or a key is not valid.
     * @throws \RuntimeException         When the database cannot answer, or answers with something that is not stored bytes.
     */
    public static function read(int $postId, array $keys): ?array
    {
        if ($postId < 1) {
            throw new \InvalidArgumentException('the post ID must be at least 1');
        }
        $keys = self::sortedKeys($keys);

        global $wpdb;
        if (!is_object($wpdb)) {
            throw new \RuntimeException('database handle unavailable');
        }
        /** @var \wpdb $wpdb */
        $post = $wpdb->get_row($wpdb->prepare('SELECT post_type, post_status, post_title, post_content, post_modified_gmt FROM %i WHERE ID = %d', $wpdb->posts, $postId), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the fingerprint covers the stored row, so it is read uncached; table from core via the %i identifier placeholder (WP 6.2+)
        self::assertQueryOk($wpdb);
        if ($post === null) {
            return null;
        }
        if (!is_array($post)) {
            throw new \RuntimeException('database read failed');
        }
        $fields = [];
        foreach (self::POST_FIELDS as $field) {
            if (!isset($post[$field]) || !is_string($post[$field])) {
                throw new \RuntimeException('database read failed');
            }
            $fields[$field] = $post[$field];
        }

        $rows = [];
        if ($keys !== []) {
            $in    = implode(', ', array_fill(0, count($keys), '%s'));
            $found = $wpdb->get_results($wpdb->prepare('SELECT meta_key, meta_value FROM %i WHERE post_id = %d AND meta_key IN (' . $in . ') ORDER BY meta_id ASC', array_merge([$wpdb->postmeta, $postId], $keys)), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.PreparedSQLPlaceholders.ReplacementsWrongNumber -- the stored bytes of each row, never the meta cache; table via %i, $in is one %s placeholder per key and every value goes through prepare() in the one replacements array
            self::assertQueryOk($wpdb);
            if (!is_array($found)) {
                throw new \RuntimeException('database read failed');
            }
            foreach ($found as $row) {
                $row   = (array) $row;
                $key   = $row['meta_key'] ?? null;
                $value = $row['meta_value'] ?? null;
                if (!is_string($key) || !is_string($value)) {
                    throw new \RuntimeException('a postmeta row is not stored bytes');
                }
                // The collation may match a key that differs in case or in
                // trailing spaces; only the exact key is the document's.
                if (!in_array($key, $keys, true)) {
                    continue;
                }
                $rows[$key][] = $value;
            }
        }

        return ['post' => $fields, 'rows' => $rows];
    }

    /**
     * The fingerprint of one stored post, or null when there is no such post.
     *
     * @param int          $postId Post ID.
     * @param array<mixed> $keys   The descriptor keys.
     * @return string|null
     * @throws \InvalidArgumentException See read().
     * @throws \RuntimeException         See read().
     */
    public static function ofPost(int $postId, array $keys): ?string
    {
        $stored = self::read($postId, $keys);
        if ($stored === null) {
            return null;
        }

        return self::compute($stored['post'], $stored['rows'], $keys);
    }

    /**
     * The keys in byte order.
     *
     * @param array<mixed> $keys Descriptor keys.
     * @return list<string>
     * @throws \InvalidArgumentException When a key is empty, not a string, or repeated.
     */
    private static function sortedKeys(array $keys): array
    {
        $out = [];
        foreach ($keys as $key) {
            if (!is_string($key) || $key === '' || in_array($key, $out, true)) {
                throw new \InvalidArgumentException('descriptor keys must be distinct non-empty strings');
            }
            $out[] = $key;
        }
        usort($out, 'strcmp');

        return $out;
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
