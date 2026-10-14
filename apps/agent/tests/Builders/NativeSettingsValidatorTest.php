<?php

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use PHPUnit\Framework\Attributes\DataProvider;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Abilities\Builders\NativeControlSchema;
use WPMgr\Agent\Abilities\Builders\NativeSettingsValidator;
use WPMgr\Agent\Abilities\Builders\Refusal;

/** Shared native settings contract, exercised without WordPress calls. */
final class NativeSettingsValidatorTest extends TestCase
{
    /** @return iterable<string,array{array<string,mixed>}> */
    public static function cases(): iterable
    {
        $raw = file_get_contents(dirname(__DIR__) . '/fixtures/ability-run/native-settings-cases.json');
        if ($raw === false) {
            throw new \RuntimeException('native settings cases missing');
        }
        $cases = json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
        if (!is_array($cases) || $cases === []) {
            throw new \RuntimeException('native settings cases empty');
        }
        foreach ($cases as $case) {
            yield $case['name'] => [$case];
        }
    }

    /** @param array<string,mixed> $case Shared case. */
    #[DataProvider('cases')]
    public function testSharedCases(array $case): void
    {
        $schema = new NativeControlSchema($case['schema'], $case['library_images'] ?? []);
        $before = serialize([$schema, $case['settings'], $case['breakpoints']]);
        $operation = $case['operation'] ?? 'stored';
        if ($operation === 'written') {
            $old = $case['before'] ?? [];
            $merged = $case['merged'] ?? NativeSettingsValidator::mergeSettings($old, $case['settings']);
            $result = NativeSettingsValidator::validateWritten($schema, $old, $case['settings'], $merged, $case['breakpoints']);
        } elseif ($operation === 'raw') {
            $result = NativeSettingsValidator::validateRaw($schema, $case['raw_json'], $case['breakpoints']);
        } elseif ($operation === 'node') {
            $result = NativeSettingsValidator::checkResultNode($schema, $case['node'], $case['breakpoints']);
        } else {
            $result = NativeSettingsValidator::validate($schema, $case['settings'], $case['breakpoints']);
        }
        self::assertSame($before, serialize([$schema, $case['settings'], $case['breakpoints']]), 'validation is read only');
        if ($case['expected'] === 'ok') {
            self::assertSame(['ok' => true], $result);
            return;
        }
        self::assertFalse($result['ok']);
        self::assertSame('refused', $result['outcome']);
        self::assertFalse($result['retryable']);
        self::assertSame($case['expected']['code'], $result['code']);
        self::assertSame($case['expected']['keys'], array_column($result['failures'], 'key'));
        if (isset($case['expected']['paths'])) {
            self::assertSame($case['expected']['paths'], array_column($result['failures'], 'path'));
        }
        foreach (array_slice($result['failures'], 0, Refusal::INLINE_FAILURES) as $failure) {
            self::assertStringContainsString($failure['path'] . '.' . $failure['key'], $result['detail']);
            self::assertNotEmpty($failure['reason']);
        }
        if (isset($case['expected']['allowed'])) {
            self::assertSame($case['expected']['allowed'], $result['failures'][0]['allowed']);
        }
        self::assertLessThanOrEqual(Refusal::MAX_FAILURES, count($result['failures']));
        self::assertArrayHasKey('schema', $result);
        self::assertLessThanOrEqual(32768, strlen(json_encode($result['schema'], JSON_THROW_ON_ERROR)));
        if (isset($case['expected']['schema_keys'])) {
            self::assertSame($case['expected']['schema_keys'], array_keys($result['schema']['controls']));
        }
    }

    public function testMalformedActivatorRefusesWithoutThrowing(): void
    {
        $schema = new NativeControlSchema([
            'widget' => 'heading',
            'atomic' => false,
            'controls' => ['typography_font_size' => [
                'type' => 'slider', 'units' => ['px'], 'group' => 'typography',
                'activator' => new \stdClass(),
            ]],
        ]);
        $result = NativeSettingsValidator::validate($schema, ['typography_font_size' => ['unit' => 'px', 'size' => '24']], ['mobile']);
        self::assertFalse($result['ok']);
        self::assertSame('native_settings_invalid', $result['code']);
        self::assertSame(['typography_font_size'], array_column($result['failures'], 'key'));
    }

    public function testFailureEnvelopeIsBounded(): void
    {
        $schema = new NativeControlSchema(['widget' => 'heading', 'atomic' => false, 'controls' => []]);
        $settings = [];
        for ($i = 0; $i <= Refusal::MAX_FAILURES; $i++) {
            $settings['unknown_' . $i] = 'Content';
        }
        $result = NativeSettingsValidator::validate($schema, $settings, []);
        self::assertFalse($result['ok']);
        self::assertCount(Refusal::MAX_FAILURES, $result['failures']);
        for ($i = 0; $i < Refusal::INLINE_FAILURES; $i++) {
            self::assertStringContainsString('settings.unknown_' . $i, $result['detail']);
        }
        self::assertStringNotContainsString('settings.unknown_' . Refusal::INLINE_FAILURES, $result['detail']);
        self::assertLessThanOrEqual(Refusal::MAX_DETAIL_CHARS, strlen($result['detail']));
    }

    public function testMergeOrderAndEmptyEncoding(): void
    {
        self::assertSame(['b' => 'New', 'c' => 'Added'], NativeSettingsValidator::mergeSettings(['a' => 'Old', 'b' => 'Before'], ['b' => 'New', 'a' => null, 'c' => 'Added']));
        self::assertSame('[]', NativeSettingsValidator::encodeSettings([]));
        self::assertSame('{"b":"New","c":"Added"}', NativeSettingsValidator::encodeSettings(['b' => 'New', 'c' => 'Added']));
    }

    public function testBuildResultNode(): void
    {
        $schema = new NativeControlSchema(['widget' => 'heading', 'atomic' => false, 'controls' => ['title' => ['type' => 'text']]]);
        $result = NativeSettingsValidator::buildResultNode($schema, 'abc1234', false, ['title' => 'Hello'], []);
        self::assertTrue($result['ok']);
        self::assertSame(['id' => 'abc1234', 'elType' => 'widget', 'widgetType' => 'heading', 'isInner' => false, 'settings' => ['title' => 'Hello'], 'elements' => []], $result['node']);
        self::assertSame(['ok' => true], NativeSettingsValidator::checkResultNode($schema, $result['node'], []));
        self::assertFalse(NativeSettingsValidator::buildResultNode($schema, 'not-hex', false, ['title' => 'Hello'], [])['ok']);
    }

    public function testWrittenMalformedControlRefuses(): void
    {
        $schema = new NativeControlSchema(['widget' => 'heading', 'atomic' => false, 'controls' => ['title' => new \stdClass()]]);
        $result = NativeSettingsValidator::validateWritten($schema, [], ['title' => 'Hello'], ['title' => 'Hello'], []);
        self::assertFalse($result['ok']);
        self::assertSame(['title'], array_column($result['failures'], 'key'));
    }
}
