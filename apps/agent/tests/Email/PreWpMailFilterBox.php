<?php
/**
 * PreWpMailFilterBox: mutable, live handle for the `pre_wp_mail` filter
 * registrations captured by MailRouterTest::captureRegisteredPreWpMailFilters().
 *
 * Extracted out of an inline `new class () {...}` so PHPStan sees a normal,
 * writable class property across the method boundary. An anonymous class
 * annotated via the `@return object{filters: ...}` PHPDoc shape is inferred
 * as a READ-ONLY object shape once it crosses out of the declaring method's
 * scope, so every caller appending to `->filters` after calling that helper
 * (`$filters->filters[] = [...]`) trips `assign.propertyReadOnly` even
 * though the property itself is an ordinary mutable `public array`. A named
 * class has no such inference gap.
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

/**
 * Live handle for captured `pre_wp_mail` filter registrations.
 */
final class PreWpMailFilterBox
{
    /** @var list<array{priority:int,callback:callable}> */
    public array $filters = [];
}
