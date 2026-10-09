<?php
/**
 * In-memory $wpdb double for the media URL rewriter's database passes.
 *
 * The rewriter only uses $GLOBALS['wpdb'] when it is a \wpdb, so this file
 * declares an empty global wpdb class when none exists and the double extends
 * it. The double answers the two prefilter SELECTs and records every UPDATE.
 *
 * It does not evaluate the prefilter's LIKE and REGEXP clauses: the rows a test
 * seeds stand for the rows MySQL selected. MySQL compares those clauses without
 * regard to case under WordPress's default collations, so a seeded row may hold
 * a URL that differs from the mapped one only in case, as a real selection can.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace {
    if (!class_exists('wpdb', false)) {
        /**
         * Empty stand-in for WordPress's database class, so a test double can
         * pass an instanceof \wpdb check.
         */
        class wpdb
        {
        }
    }
}

namespace WPMgr\Agent\Tests {
    /**
     * Serves seeded post_content and postmeta rows and records updates.
     */
    final class FakeDbRewriterWpdb extends \wpdb
    {
        public string $posts = 'wp_posts';

        public string $postmeta = 'wp_postmeta';

        /** @var list<object> Rows the post_content SELECT returns (ID, post_content). */
        public array $postRows = [];

        /** @var list<array<string,mixed>> Rows the postmeta SELECT returns (meta_id, meta_value). */
        public array $metaRows = [];

        /** @var list<array{table:string,data:array<string,mixed>,where:array<string,mixed>}> */
        public array $updates = [];

        /**
         * @param string $text Text to escape for LIKE.
         * @return string
         */
        public function esc_like(string $text): string
        {
            return addcslashes($text, '_%\\');
        }

        /**
         * Carries the SQL through unchanged; the arguments are not needed.
         *
         * @param string $query SQL with placeholders.
         * @param mixed  ...$args Bound arguments.
         * @return string
         */
        public function prepare(string $query, ...$args): string
        {
            return $query;
        }

        /**
         * @param string $query  Output of prepare().
         * @param string $output Result format.
         * @return array<int,mixed>
         */
        public function get_results(string $query, string $output = 'OBJECT'): array
        {
            if (strpos($query, $this->postmeta) !== false) {
                return $this->metaRows;
            }
            return $this->postRows;
        }

        /**
         * @param string              $table Table name.
         * @param array<string,mixed> $data  Column values.
         * @param array<string,mixed> $where WHERE column values.
         * @return int
         */
        public function update(string $table, array $data, array $where): int
        {
            $this->updates[] = ['table' => $table, 'data' => $data, 'where' => $where];
            return 1;
        }
    }
}
