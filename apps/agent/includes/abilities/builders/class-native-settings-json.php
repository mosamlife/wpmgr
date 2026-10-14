<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

if (!defined('ABSPATH')) {
    exit;
}

/** One raw JSON parse, retaining duplicate-key detection before constructing maps. */
final class NativeSettingsJson
{
    private int $offset = 0;
    private ?array $failure = null;

    private function __construct(private string $text, private int $maxDepth = 16)
    {
    }

    /** @return array<string,mixed> Parsed settings or a concrete parse failure. */
    public static function parse(string $text): array
    {
        $parser = new self($text);
        return $parser->parseObject();
    }

    /** Check the complete page-edit input before json_decode can discard duplicate keys. */
    public static function validateDocument(string $text): array
    {
        return (new self($text, 32))->parseObject();
    }

    /** @return array<string,mixed> */
    private function parseObject(): array
    {
        $parser = $this;
        $text = $this->text;
        try {
            if (strlen($text) > 65536) {
                throw new \InvalidArgumentException('settings JSON exceeds the input limit');
            }
            $value = $parser->value('settings', 0);
            $parser->space();
            if ($parser->offset !== strlen($text) || !is_array($value)
                || ($value !== [] && ($text[strspn($text, " \t\r\n")] ?? '') !== '{')) {
                throw new \InvalidArgumentException('settings JSON must be one object or an empty list');
            }
            return ['ok' => true, 'settings' => $value];
        } catch (\InvalidArgumentException | \JsonException $e) {
            return ['ok' => false, 'failure' => $parser->failure ?? [
                'path' => 'input', 'key' => 'settings', 'reason' => 'write valid bounded settings JSON',
            ]];
        }
    }

    /** @return mixed */
    private function value(string $path, int $depth)
    {
        if ($depth > $this->maxDepth) {
            throw new \InvalidArgumentException('JSON nesting exceeds the limit');
        }
        $this->space();
        $first = $this->text[$this->offset] ?? '';
        if ($first === '"') {
            return $this->string();
        }
        if ($first === '{' || $first === '[') {
            return $this->collection($path, $depth, $first === '{');
        }
        foreach (['null' => null, 'true' => true, 'false' => false] as $token => $value) {
            if (substr($this->text, $this->offset, strlen($token)) === $token) {
                $this->offset += strlen($token);
                return $value;
            }
        }
        if (preg_match('/\G-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/', $this->text, $match, 0, $this->offset) === 1) {
            $this->offset += strlen($match[0]);
            return (float) $match[0];
        }

        throw new \InvalidArgumentException('invalid JSON value');
    }

    /** @return array<mixed> */
    private function collection(string $path, int $depth, bool $object): array
    {
        $this->offset++;
        $close = $object ? '}' : ']';
        $this->space();
        if (($this->text[$this->offset] ?? '') === $close) {
            $this->offset++;
            return [];
        }
        $result = [];
        $seen = [];
        do {
            $this->space();
            $key = $object ? $this->string() : count($result);
            if ($object) {
                if (isset($seen["\0" . $key])) {
                    $this->failure = ['path' => $path, 'key' => $key, 'reason' => 'object keys must be unique, including escaped spellings'];
                    throw new \InvalidArgumentException('duplicate JSON object key');
                }
                $seen["\0" . $key] = true;
                $this->space();
                $this->take(':');
            }
            $result[$key] = $this->value($path . '.' . $key, $depth + 1);
            $this->space();
            if (($this->text[$this->offset] ?? '') === $close) {
                $this->offset++;
                return $result;
            }
            $this->take(',');
        } while ($this->offset < strlen($this->text));

        throw new \InvalidArgumentException('unterminated JSON collection');
    }

    /** Decode a JSON string token once, including escaped property names. */
    private function string(): string
    {
        $start = $this->offset;
        if (($this->text[$start] ?? '') !== '"') {
            throw new \InvalidArgumentException('invalid JSON string');
        }
        $length = strlen($this->text);
        for ($end = $start + 1; $end < $length; $end++) {
            if ($this->text[$end] === '\\') {
                $end++;
            } elseif ($this->text[$end] === '"') {
                $value = json_decode(substr($this->text, $start, $end - $start + 1), true, 2, JSON_THROW_ON_ERROR);
                $this->offset = $end + 1;
                return $value;
            }
        }
        throw new \InvalidArgumentException('unterminated JSON string');
    }

    private function space(): void
    {
        $this->offset += strspn($this->text, " \t\r\n", $this->offset);
    }

    private function take(string $token): void
    {
        if (($this->text[$this->offset] ?? '') !== $token) {
            throw new \InvalidArgumentException('invalid JSON punctuation');
        }
        $this->offset++;
    }
}
