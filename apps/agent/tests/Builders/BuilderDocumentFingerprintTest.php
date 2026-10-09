<?php
/**
 * builder_document_v1: the formula, the shared fixture the control plane
 * replays, and the raw SQL read it is computed over.
 *
 * Fixture (regenerate with WPMGR_WRITE_FIXTURES=1, never hand-edit):
 *   builder-document-fp.json  posts, stored rows (base64) and fingerprints
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint
 */
final class BuilderDocumentFingerprintTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/builder-document-fp.json';

    /** The Elementor descriptor keys, in byte order. */
    private const ELEMENTOR_KEYS = ['_elementor_data', '_elementor_edit_mode', '_elementor_page_settings', '_elementor_template_type'];

    private const POST_ID = 418;

    private FakeBuilderWpdb $db;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->db        = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->db;
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Shared fixture
    // -------------------------------------------------------------------------

    public function test_replays_every_fixture_case(): void
    {
        $raw = file_get_contents(self::FIXTURE);
        $this->assertIsString($raw, 'builder-document-fp.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $doc = json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
        $this->assertSame(BuilderDocumentFingerprint::DOMAIN, $doc['domain']);
        $this->assertSame(self::ELEMENTOR_KEYS, $doc['descriptors']['elementor']);
        $this->assertNotEmpty($doc['cases']);

        foreach ($doc['cases'] as $case) {
            $rows = [];
            foreach ($case['rows_b64'] as $key => $encoded) {
                foreach ($encoded as $b64) {
                    $bytes = base64_decode($b64, true);
                    $this->assertIsString($bytes, $case['name'] . ': ' . $key . ' is not base64');
                    $rows[$key][] = $bytes;
                }
            }
            $this->assertSame(
                $case['fingerprint'],
                BuilderDocumentFingerprint::compute($case['post'], $rows, $case['keys']),
                $case['name']
            );
        }
    }

    public function test_fixture_is_what_the_code_produces(): void
    {
        $want = self::fixtureText();
        if (getenv('WPMGR_WRITE_FIXTURES') === '1') {
            $this->assertNotFalse(file_put_contents(self::FIXTURE, $want), 'could not write builder-document-fp.json');
        }
        $got = file_get_contents(self::FIXTURE);
        $this->assertIsString($got, 'builder-document-fp.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $this->assertSame($want, $got, 'builder-document-fp.json differs from what the agent produces now; regenerate with WPMGR_WRITE_FIXTURES=1');

        $byName = [];
        foreach (json_decode($want, true, 512, JSON_THROW_ON_ERROR)['cases'] as $case) {
            $byName[$case['name']] = $case['fingerprint'];
        }
        $this->assertSame($byName['serialized-page-settings'], $byName['keys-out-of-order'], 'the key order given does not matter');
        $this->assertNotSame($byName['all-keys-absent'], $byName['empty-row-is-not-absent']);
        $this->assertCount(count($byName) - 1, array_unique(array_values($byName)), 'every case but keys-out-of-order has its own fingerprint');
    }

    // -------------------------------------------------------------------------
    // Formula
    // -------------------------------------------------------------------------

    public function test_keys_hash_in_byte_order(): void
    {
        $post = self::post();
        // Byte order differs from numeric order ("10" < "9"), from natural
        // order ("_a10" < "_a9") and from case-folded order ("_B" < "_a").
        $byteOrder = ['10', '9', 'Zed', '_B', '_a10', '_a9', '_b'];
        $rows      = [];
        foreach ($byteOrder as $key) {
            $rows[$key] = ['row of ' . $key];
        }
        $want = self::formula($post, array_map(
            static fn (string $key): array => [$key, 1, [hash('sha256', 'row of ' . $key)]],
            $byteOrder
        ));

        $given = ['_a9', '9', '_b', 'Zed', '10', '_B', '_a10'];
        $this->assertSame($want, BuilderDocumentFingerprint::compute($post, $rows, $given));
        $this->assertSame($want, BuilderDocumentFingerprint::compute($post, $rows, array_reverse($given)));
        $this->assertSame($want, BuilderDocumentFingerprint::compute($post, $rows, $byteOrder));

        $natural = $byteOrder;
        natcasesort($natural);
        $this->assertNotSame($byteOrder, array_values($natural), 'the keys tell byte order from natural order');
        $this->assertNotSame($want, self::formula($post, array_map(
            static fn (string $key): array => [$key, 1, [hash('sha256', 'row of ' . $key)]],
            array_values($natural)
        )));
    }

    public function test_absent_key_differs_from_empty_row(): void
    {
        $post   = self::post();
        $absent = BuilderDocumentFingerprint::compute($post, [], self::ELEMENTOR_KEYS);
        $empty  = BuilderDocumentFingerprint::compute($post, ['_elementor_edit_mode' => ['']], self::ELEMENTOR_KEYS);

        $this->assertSame(self::formula($post, [
            ['_elementor_data', 0, []],
            ['_elementor_edit_mode', 0, []],
            ['_elementor_page_settings', 0, []],
            ['_elementor_template_type', 0, []],
        ]), $absent);
        $this->assertSame(self::formula($post, [
            ['_elementor_data', 0, []],
            ['_elementor_edit_mode', 1, [hash('sha256', '')]],
            ['_elementor_page_settings', 0, []],
            ['_elementor_template_type', 0, []],
        ]), $empty);
        $this->assertNotSame($absent, $empty);
        $this->assertSame($absent, BuilderDocumentFingerprint::compute($post, ['_elementor_edit_mode' => []], self::ELEMENTOR_KEYS), 'no rows is the absent key');
    }

    public function test_multi_row_key_hashes_each_row_in_meta_id_order(): void
    {
        $post   = self::post();
        $first  = '[{"id":"1a2b3c4","elType":"section"}]';
        $second = '[{"id":"7a8b9c0","elType":"section"}]';
        $want   = self::formula($post, [
            ['_elementor_data', 2, [hash('sha256', $first), hash('sha256', $second)]],
            ['_elementor_edit_mode', 1, [hash('sha256', 'builder')]],
            ['_elementor_page_settings', 0, []],
            ['_elementor_template_type', 0, []],
        ]);
        $rows = ['_elementor_data' => [$first, $second], '_elementor_edit_mode' => ['builder']];

        $this->assertSame($want, BuilderDocumentFingerprint::compute($post, $rows, self::ELEMENTOR_KEYS));
        $this->assertNotSame($want, BuilderDocumentFingerprint::compute($post, ['_elementor_data' => [$second, $first], '_elementor_edit_mode' => ['builder']], self::ELEMENTOR_KEYS));
        $this->assertNotSame($want, BuilderDocumentFingerprint::compute($post, ['_elementor_data' => [$first . $second], '_elementor_edit_mode' => ['builder']], self::ELEMENTOR_KEYS));

        // Stored out of meta_id order: the read puts them back in meta_id order.
        $this->db->addPost(self::POST_ID, $post);
        $this->db->addMeta(31, self::POST_ID, '_elementor_data', $second);
        $this->db->addMeta(7, self::POST_ID, '_elementor_edit_mode', 'builder');
        $this->db->addMeta(12, self::POST_ID, '_elementor_data', $first);

        $stored = BuilderDocumentFingerprint::read(self::POST_ID, self::ELEMENTOR_KEYS);
        $this->assertNotNull($stored);
        $this->assertSame([$first, $second], $stored['rows']['_elementor_data']);
        $this->assertSame($want, BuilderDocumentFingerprint::ofPost(self::POST_ID, self::ELEMENTOR_KEYS));
    }

    // -------------------------------------------------------------------------
    // Read
    // -------------------------------------------------------------------------

    public function test_reads_raw_rows_not_the_meta_cache(): void
    {
        $post     = self::post();
        $settings = 'a:2:{s:10:"hide_title";s:3:"yes";s:16:"background_color";s:7:"#F4EFE6";}';
        $data     = '[{"id":"5e6f7a8","settings":{"title":"Say \"bonjour\" at the café","image":{"url":"https:\/\/example.com\/wp-content\/uploads\/team.jpg"}}}]';
        $this->db->addPost(self::POST_ID, $post);
        $this->db->addMeta(40, self::POST_ID, '_elementor_page_settings', $settings);
        $this->db->addMeta(41, self::POST_ID, '_elementor_data', $data);
        $this->db->addMeta(42, self::POST_ID, '_elementor_edit_mode', 'builder');
        $this->db->addMeta(43, self::POST_ID, '_edit_lock', '1760000000:1');
        $this->db->addMeta(44, self::POST_ID, '_Elementor_Data', '[{"id":"ffffff0"}]');
        $this->db->addMeta(45, self::POST_ID, '_elementor_data ', '[{"id":"ffffff1"}]');
        $this->db->addMeta(46, self::POST_ID + 1, '_elementor_data', '[{"id":"ffffff2"}]');

        // What the meta cache would answer: values already unserialized and
        // unslashed. The read must not consult it.
        foreach (['get_post_meta', 'get_metadata', 'get_metadata_raw', 'get_post_custom', 'wp_cache_get', 'get_post'] as $cached) {
            Functions\expect($cached)->never();
        }

        $stored = BuilderDocumentFingerprint::read(self::POST_ID, self::ELEMENTOR_KEYS);

        $this->assertSame([
            'post' => $post,
            'rows' => [
                '_elementor_page_settings' => [$settings],
                '_elementor_data'          => [$data],
                '_elementor_edit_mode'     => ['builder'],
            ],
        ], $stored);
        $this->assertSame(
            BuilderDocumentFingerprint::compute($post, $stored['rows'], self::ELEMENTOR_KEYS),
            BuilderDocumentFingerprint::ofPost(self::POST_ID, self::ELEMENTOR_KEYS)
        );
        $this->assertSame('SELECT meta_key, meta_value FROM %i WHERE post_id = %d AND meta_key IN (%s, %s, %s, %s) ORDER BY meta_id ASC', $this->db->queries[1]['sql']);
        $this->assertSame(array_merge(['wp_postmeta', self::POST_ID], self::ELEMENTOR_KEYS), $this->db->queries[1]['args']);
    }

    public function test_a_missing_post_is_null_and_a_failed_read_throws(): void
    {
        $this->assertNull(BuilderDocumentFingerprint::read(self::POST_ID, self::ELEMENTOR_KEYS));
        $this->assertNull(BuilderDocumentFingerprint::ofPost(self::POST_ID, self::ELEMENTOR_KEYS));

        $this->db->addPost(self::POST_ID, self::post());
        foreach (['posts', 'postmeta'] as $table) {
            $this->db->failOn = $table;
            try {
                BuilderDocumentFingerprint::read(self::POST_ID, self::ELEMENTOR_KEYS);
                $this->fail('a failed ' . $table . ' read must throw, never read as absent');
            } catch (\RuntimeException $e) {
                $this->assertSame('database read failed', $e->getMessage());
            }
        }

        $this->db->addMeta(50, self::POST_ID, '_elementor_data', null);
        $this->expectException(\RuntimeException::class);
        BuilderDocumentFingerprint::read(self::POST_ID, self::ELEMENTOR_KEYS);
    }

    public function test_refuses_input_it_cannot_fingerprint(): void
    {
        $post = self::post();
        $bad  = [
            'repeated key'     => fn () => BuilderDocumentFingerprint::compute($post, [], ['_elementor_data', '_elementor_data']),
            'empty key'        => fn () => BuilderDocumentFingerprint::compute($post, [], ['']),
            'key not a string' => fn () => BuilderDocumentFingerprint::compute($post, [], [7]),
            'rows off the descriptor' => fn () => BuilderDocumentFingerprint::compute($post, ['_edit_lock' => ['1']], self::ELEMENTOR_KEYS),
            'row not a string' => fn () => BuilderDocumentFingerprint::compute($post, ['_elementor_data' => [['id' => 1]]], self::ELEMENTOR_KEYS),
            'rows not a list'  => fn () => BuilderDocumentFingerprint::compute($post, ['_elementor_data' => ['a' => 'x']], self::ELEMENTOR_KEYS),
            'post field missing' => fn () => BuilderDocumentFingerprint::compute(['post_type' => 'page'], [], self::ELEMENTOR_KEYS),
            'post id zero'     => fn () => BuilderDocumentFingerprint::read(0, self::ELEMENTOR_KEYS),
        ];
        foreach ($bad as $what => $call) {
            try {
                $call();
                $this->fail($what . ' must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage(), $what);
            }
        }
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * @return array<string,string>
     */
    private static function post(string $title = 'Spring sale', string $content = '<h2>Spring sale</h2>'): array
    {
        return [
            'post_type'         => 'page',
            'post_status'       => 'draft',
            'post_title'        => $title,
            'post_content'      => $content,
            'post_modified_gmt' => '2026-10-09 05:20:00',
        ];
    }

    /**
     * The formula written out by hand, with the per-key entries in the order given.
     *
     * @param array<string,string> $post    Post fields.
     * @param list<array{0:string,1:int,2:list<string>}> $entries Per-key entries.
     */
    private static function formula(array $post, array $entries): string
    {
        return hash('sha256', (string) json_encode([
            'wpmgr.builder_document.v1',
            $post['post_type'],
            $post['post_status'],
            hash('sha256', $post['post_title']),
            hash('sha256', $post['post_content']),
            $post['post_modified_gmt'],
            $entries,
        ]));
    }

    /**
     * The cases of the shared fixture.
     *
     * @return list<array{name:string,keys:list<string>,post:array<string,string>,rows:array<string,list<string>>}>
     */
    private static function cases(): array
    {
        $settings = serialize(['hide_title' => 'yes', 'background_background' => 'classic', 'background_color' => '#F4EFE6']);
        $simple   = '[{"id":"1a2b3c4","elType":"section","settings":[],"elements":[{"id":"2b3c4d5","elType":"column","settings":{"_column_size":100},"elements":[]}],"isInner":false}]';
        $document = [
            '_elementor_data'          => [$simple],
            '_elementor_edit_mode'     => ['builder'],
            '_elementor_page_settings' => [$settings],
            '_elementor_template_type' => ['wp-page'],
        ];
        $content = '<h2>Spring sale</h2><p>Fresh bread every morning.</p>';

        return [
            ['name' => 'all-keys-absent', 'keys' => self::ELEMENTOR_KEYS, 'post' => self::post(), 'rows' => []],
            ['name' => 'serialized-page-settings', 'keys' => self::ELEMENTOR_KEYS, 'post' => self::post('Spring sale', $content), 'rows' => $document],
            [
                'name' => 'json-with-escaped-quote-and-e-acute',
                'keys' => self::ELEMENTOR_KEYS,
                'post' => self::post('Spring sale', $content),
                'rows' => ['_elementor_data' => ['[{"id":"3c4d5e6","elType":"widget","settings":{"title":"Say \"bonjour\" at the café"},"elements":[],"widgetType":"heading"}]']] + $document,
            ],
            [
                'name' => 'escaped-slash-url',
                'keys' => self::ELEMENTOR_KEYS,
                'post' => self::post('Spring sale', $content),
                'rows' => ['_elementor_data' => ['[{"id":"5e6f7a8","elType":"widget","settings":{"image":{"url":"https:\/\/example.com\/wp-content\/uploads\/2026\/10\/team.jpg","id":42}},"elements":[],"widgetType":"image"}]']] + $document,
            ],
            [
                'name' => 'multi-row-key',
                'keys' => self::ELEMENTOR_KEYS,
                'post' => self::post('Spring sale', $content),
                'rows' => ['_elementor_data' => [$simple, '[{"id":"7a8b9c0","elType":"section","settings":[],"elements":[],"isInner":false}]']] + $document,
            ],
            ['name' => 'empty-row-is-not-absent', 'keys' => self::ELEMENTOR_KEYS, 'post' => self::post(), 'rows' => ['_elementor_edit_mode' => ['']]],
            [
                'name' => 'non-ascii-title',
                'keys' => self::ELEMENTOR_KEYS,
                'post' => self::post('Café Ünï 🚀 日本語', '<p>Prix: 5€ — naïve</p>'),
                'rows' => $document,
            ],
            [
                'name' => 'keys-out-of-order',
                'keys' => ['_elementor_template_type', '_elementor_data', '_elementor_page_settings', '_elementor_edit_mode'],
                'post' => self::post('Spring sale', $content),
                'rows' => $document,
            ],
        ];
    }

    private static function fixtureText(): string
    {
        $cases = [];
        foreach (self::cases() as $case) {
            $b64 = [];
            foreach ($case['rows'] as $key => $rows) {
                $b64[$key] = array_map('base64_encode', $rows);
            }
            $cases[] = [
                'name'        => $case['name'],
                'keys'        => $case['keys'],
                'post'        => $case['post'],
                'rows_b64'    => (object) $b64,
                'fingerprint' => BuilderDocumentFingerprint::compute($case['post'], $case['rows'], $case['keys']),
            ];
        }
        $doc = [
            'note'        => 'Generated by the agent (tests/Builders/BuilderDocumentFingerprintTest.php). Regenerate with WPMGR_WRITE_FIXTURES=1. '
                . 'fingerprint = sha256(json_encode([domain, post_type, post_status, sha256(post_title), sha256(post_content), post_modified_gmt, '
                . '[[key, row_count, [sha256(row), ...]], ...]])) with one entry per key of "keys", taken in byte order (strcmp) whatever order "keys" '
                . 'lists them in; a key\'s rows are its postmeta rows in meta_id order, and a key with no row is [key, 0, []]. '
                . 'json_encode with default flags; every sha256 is lowercase hex over the bytes. '
                . 'rows_b64 holds each stored meta_value, base64, in meta_id order; a key missing from rows_b64 has no row. '
                . 'descriptors.elementor is the Elementor document\'s key list.',
            'domain'      => BuilderDocumentFingerprint::DOMAIN,
            'descriptors' => ['elementor' => self::ELEMENTOR_KEYS],
            'cases'       => $cases,
        ];

        return json_encode($doc, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR) . "\n";
    }
}
