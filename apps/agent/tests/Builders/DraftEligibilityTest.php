<?php
/**
 * DraftEligibility: a draft is WPMgr's only when the control plane names it
 * in the signed parameters, it carries exactly one created-by-request marker,
 * and that request's completed page-create ledger row created this very post
 * and was not undone.
 *
 * Post and meta rows live in FakeBuilderWpdb and are read through the SQL
 * read production uses; ledger rows are options read through get_option(),
 * as AbilityLedger reads them.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\DraftEligibility;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Commands\AbilityRunCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\DraftEligibility
 */
final class DraftEligibilityTest extends TestCase
{
    /** The draft WPMgr created. */
    private const DRAFT = 42;

    /** A copy of it that carries its marker. */
    private const COPY = 43;

    /** A draft a person made. */
    private const PERSON_DRAFT = 44;

    /** The page-create request that created DRAFT. */
    private const REQ = '11111111-2222-4333-8444-777777777777';

    /** Another request id, with no ledger row. */
    private const OTHER_REQ = '99999999-2222-4333-8444-555555555555';

    private FakeBuilderWpdb $wpdb;

    /** @var array<string,mixed> */
    private array $options = [];

    private int $metaId = 10;

    private mixed $savedWpdb = null;

    private bool $hadWpdb = false;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('get_option')->alias(fn ($name, $default = false) => array_key_exists($name, $this->options) ? $this->options[$name] : $default);
        $this->hadWpdb   = array_key_exists('wpdb', $GLOBALS);
        $this->savedWpdb = $GLOBALS['wpdb'] ?? null;
        $this->seed();
    }

    protected function tear_down(): void
    {
        if ($this->hadWpdb) {
            $GLOBALS['wpdb'] = $this->savedWpdb;
        } else {
            unset($GLOBALS['wpdb']);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_marker_and_ledger_and_signed_list_all_required(): void
    {
        $this->assertSame(['eligible' => true, 'reason' => ''], DraftEligibility::check(self::DRAFT, [self::DRAFT]), 'all three hold: eligible');

        // Each condition broken alone refuses, with its own reason.
        $cases = [
            'the list names another post'   => [fn () => null, [self::DRAFT + 100], 'not_in_signed_list'],
            'no marker row'                 => [fn () => $this->seed(markers: []), [self::DRAFT], 'no_marker'],
            'a marker that is no request'   => [fn () => $this->seed(markers: ['not-a-request']), [self::DRAFT], 'marker_not_a_request'],
            'a marker with a newline'       => [fn () => $this->seed(markers: [self::REQ . "\n"]), [self::DRAFT], 'marker_not_a_request'],
            'no ledger row for the marker'  => [fn () => $this->seed(markers: [self::OTHER_REQ]), [self::DRAFT], 'ledger_missing'],
            'a ledger row of another ability' => [fn () => $this->seed(ledger: ['ability' => OwnAbilities::NAME_REST_WRITE]), [self::DRAFT], 'ledger_missing'],
            'a creation that did not complete' => [fn () => $this->seed(ledger: ['phase' => 'post_created']), [self::DRAFT], 'ledger_not_completed'],
            'a ledger row naming another post' => [fn () => $this->seed(ledger: ['created_post_id' => self::DRAFT + 1]), [self::DRAFT], 'ledger_other_post'],
            'a ledger id held as text'      => [fn () => $this->seed(ledger: ['created_post_id' => (string) self::DRAFT]), [self::DRAFT], 'ledger_other_post'],
            'the draft was published'       => [fn () => $this->seed(post: ['post_status' => 'publish']), [self::DRAFT], 'not_draft'],
            'the draft is pending review'   => [fn () => $this->seed(post: ['post_status' => 'pending']), [self::DRAFT], 'not_draft'],
            'the post is gone'              => [fn () => $this->wpdb = $GLOBALS['wpdb'] = new FakeBuilderWpdb(), [self::DRAFT], 'missing'],
            'the rows cannot be read'       => [fn () => $this->wpdb->failOn = 'posts', [self::DRAFT], 'unreadable'],
        ];
        foreach ($cases as $why => [$break, $allowed, $reason]) {
            $this->seed();
            $break();
            $this->assertSame(['eligible' => false, 'reason' => $reason], DraftEligibility::check(self::DRAFT, $allowed), $why);
            $this->assertContains($reason, DraftEligibility::REASONS, $why);
        }

        // Put back, the draft is eligible again.
        $this->seed();
        $this->assertTrue(DraftEligibility::check(self::DRAFT, [self::DRAFT])['eligible']);
    }

    public function test_duplicate_with_copied_marker_is_not_eligible(): void
    {
        // Someone copied the draft with its meta: the copy carries the
        // original's marker, so the marker names a ledger row whose created
        // post is the original.
        $this->wpdb->addPost(self::COPY, $this->postFields());
        $this->wpdb->addMeta(++$this->metaId, self::COPY, DraftEligibility::MARKER_KEY, self::REQ);

        $this->assertSame(['eligible' => false, 'reason' => 'ledger_other_post'], DraftEligibility::check(self::COPY, [self::COPY]), 'even when the list names the copy');
        $this->assertSame(['eligible' => true, 'reason' => ''], DraftEligibility::check(self::DRAFT, [self::DRAFT]), 'the original stays eligible');
    }

    public function test_trashed_creation_not_eligible(): void
    {
        $this->seed(ledger: ['undo_state' => 'trashed']);
        $this->assertSame(['eligible' => false, 'reason' => 'trashed'], DraftEligibility::check(self::DRAFT, [self::DRAFT]), 'undone by trash, then restored from the trash as a draft');

        foreach (['available', 'none'] as $state) {
            $this->seed(ledger: ['undo_state' => $state]);
            $this->assertTrue(DraftEligibility::check(self::DRAFT, [self::DRAFT])['eligible'], $state);
        }
    }

    public function test_not_in_signed_list_not_eligible(): void
    {
        // The agent's own records say the draft is WPMgr's; without the
        // control plane naming it, it is not eligible.
        foreach (['no list' => [], 'another post' => [self::DRAFT + 1], 'the id as text' => [(string) self::DRAFT], 'the id as a float' => [(float) self::DRAFT]] as $why => $allowed) {
            $this->assertSame(['eligible' => false, 'reason' => 'not_in_signed_list'], DraftEligibility::check(self::DRAFT, $allowed), $why);
        }

        // A person's draft is refused before anything about it is read.
        $this->wpdb->addPost(self::PERSON_DRAFT, $this->postFields());
        $before = count($this->wpdb->queries);
        $this->assertSame(['eligible' => false, 'reason' => 'not_in_signed_list'], DraftEligibility::check(self::PERSON_DRAFT, [self::DRAFT]));
        $this->assertCount($before, $this->wpdb->queries, 'no row was read for a post the list does not name');
        $this->assertSame(['eligible' => false, 'reason' => 'no_marker'], DraftEligibility::check(self::PERSON_DRAFT, [self::PERSON_DRAFT]), 'named by mistake, it still has no marker');

        // The signed list: absent is empty; otherwise at most one post id,
        // each a JSON integer of at least 1.
        $this->assertSame([], DraftEligibility::signedIds((object) ['mode' => 'read']));
        foreach (['[]' => [], '[42]' => [42]] as $json => $want) {
            $this->assertSame($want, DraftEligibility::signedIds($this->p($json)), $json);
        }
        foreach (['[42,43]', '["42"]', '[42.0]', '[0]', '[-4]', '{}', '{"0":42}', 'null', '42', '"42"', '[[42]]', '[true]'] as $json) {
            $this->assertNull(DraftEligibility::signedIds($this->p($json)), $json);
        }
        $this->assertSame(1, DraftEligibility::MAX_SIGNED_IDS);
    }

    public function test_two_marker_rows_not_eligible(): void
    {
        foreach (['the same request twice' => [self::REQ, self::REQ], 'another request first' => [self::OTHER_REQ, self::REQ], 'another request second' => [self::REQ, self::OTHER_REQ]] as $why => $markers) {
            $this->seed(markers: $markers);
            $this->assertSame(['eligible' => false, 'reason' => 'marker_not_a_request'], DraftEligibility::check(self::DRAFT, [self::DRAFT]), $why);
        }

        // A key that only matches under the collation is not the marker.
        $this->seed(markers: []);
        $this->wpdb->addMeta(++$this->metaId, self::DRAFT, strtoupper(DraftEligibility::MARKER_KEY), self::REQ);
        $this->assertSame(['eligible' => false, 'reason' => 'no_marker'], DraftEligibility::check(self::DRAFT, [self::DRAFT]));
    }

    public function test_marker_key_is_the_one_page_create_writes(): void
    {
        $this->assertSame(AbilityRunCommand::META_CREATED_BY, DraftEligibility::MARKER_KEY);
        $this->assertSame(ElementorDocument::MARKER_KEY, DraftEligibility::MARKER_KEY);
    }

    // ---- helpers -----------------------------------------------------------

    /**
     * Rebuild the rows: DRAFT as WPMgr's completed page-create made it, with
     * the given changes.
     *
     * @param array<string,string> $post    Posts columns to change.
     * @param array<string,mixed>  $ledger  Ledger row fields to change.
     * @param list<string>|null    $markers The marker rows; null for the one REQ row.
     */
    private function seed(array $post = [], array $ledger = [], ?array $markers = null): void
    {
        $this->wpdb      = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;
        $this->wpdb->addPost(self::DRAFT, array_merge($this->postFields(), $post));
        $this->wpdb->addMeta(++$this->metaId, self::DRAFT, ElementorDocument::KEY_EDIT_MODE, 'builder');
        foreach ($markers ?? [self::REQ] as $marker) {
            $this->wpdb->addMeta(++$this->metaId, self::DRAFT, DraftEligibility::MARKER_KEY, $marker);
        }
        $this->options = [
            'wpmgr_ability_ledger_' . self::REQ => array_merge([
                'request_id'       => self::REQ,
                'ability'          => OwnAbilities::NAME_PAGE_CREATE,
                'phase'            => 'completed',
                'created_post_id'  => self::DRAFT,
                'after_fp'         => str_repeat('a', 64),
                'undo_state'       => 'available',
                'builder'          => 'elementor',
                'own_revision_ids' => [],
            ], $ledger),
        ];
    }

    /**
     * @return array<string,string>
     */
    private function postFields(): array
    {
        return [
            'post_type'         => 'page',
            'post_status'       => 'draft',
            'post_title'        => 'Our services',
            'post_content'      => '',
            'post_modified_gmt' => '2026-10-09 07:00:05',
        ];
    }

    /**
     * Signed parameters carrying allowed_draft_ids as the given JSON text,
     * decoded with objects as the command decodes p.
     */
    private function p(string $idsJson): object
    {
        $p = json_decode('{"mode":"read","allowed_draft_ids":' . $idsJson . '}', false, 32);
        $this->assertIsObject($p, $idsJson);

        return $p;
    }
}
