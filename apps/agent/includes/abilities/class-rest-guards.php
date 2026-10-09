<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Interception guards for the one REST request the engine dispatches.
 *
 * Four REST server filters can short-circuit or rewrite a request:
 * rest_pre_dispatch, rest_request_before_callbacks, rest_dispatch_request and
 * rest_request_after_callbacks. While armed, each gets a recorder at the
 * earliest priority and a guard at the latest one, and every callback acts
 * only on the request object the engine built (an identity compare); any
 * other request passes through untouched. Each of our callbacks must hold its
 * edge (first in the lowest bucket, last in the highest) every time it runs.
 *
 * The before-callbacks guard also checks the matched handler: its callback
 * and its permission callback must both be WordPress core's own code, and it
 * must accept the row's method. Otherwise it returns a WP_Error, which stops
 * the permission check and the callback, and the call is refused.
 *
 * After the dispatch, verify() requires that each guard saw our request
 * exactly once and that the response reports the reviewed route pattern and
 * the handler the guard checked. A short-circuit before matching never
 * reaches the later guards, so it is refused even when a value was forged
 * after our pre-dispatch guard ran.
 *
 * Violation labels form a closed set: pre_dispatch, before_callbacks,
 * dispatch, after_callbacks.
 */
final class RestGuards
{
    public const FILTER_PRE      = 'rest_pre_dispatch';
    public const FILTER_BEFORE   = 'rest_request_before_callbacks';
    public const FILTER_DISPATCH = 'rest_dispatch_request';
    public const FILTER_AFTER    = 'rest_request_after_callbacks';

    public const LABELS = ['pre_dispatch', 'before_callbacks', 'dispatch', 'after_callbacks'];

    private bool $armed = false;

    private ?object $request = null;

    private string $pattern = '';

    private string $method = '';

    /** @var array<string,list<mixed>> Values recorded at the earliest priority, per label. */
    private array $recorded = [];

    /** @var array<string,list<mixed>> Response data recorded with an after-callbacks value. */
    private array $recordedData = [];

    /** @var array<string,int> Guard runs per label, for our request. */
    private array $runs = [];

    /** @var array<string,true> */
    private array $violations = [];

    private bool $handlerRefused = false;

    /** @var array<string,mixed>|null The handler the before-callbacks guard checked. */
    private ?array $handler = null;

    /** @var array<string,string>|null Canonical roots, injectable for tests. */
    private ?array $roots;

    /** @var list<array{0:string,1:callable,2:int}> */
    private array $hooks = [];

    /**
     * @param array<string,string>|null $roots Canonical roots; from AbilityOwnership::roots() when null.
     */
    public function __construct(?array $roots = null)
    {
        $this->roots = $roots;
    }

    /**
     * Install the guards for one request.
     *
     * @param object $request The request the engine built.
     * @param string $pattern The route pattern the matched handler must report.
     * @param string $method  The row's method.
     * @return void
     */
    public function arm(object $request, string $pattern, string $method): void
    {
        $this->disarm();
        $this->request        = $request;
        $this->pattern        = $pattern;
        $this->method         = strtoupper($method);
        $this->recorded       = [];
        $this->recordedData   = [];
        $this->runs           = [];
        $this->violations     = [];
        $this->handlerRefused = false;
        $this->handler        = null;
        $this->armed          = true;

        $pairs = [
            self::FILTER_PRE      => ['pre_dispatch', 2],
            self::FILTER_BEFORE   => ['before_callbacks', 2],
            self::FILTER_DISPATCH => ['dispatch', 1],
            self::FILTER_AFTER    => ['after_callbacks', 2],
        ];
        foreach ($pairs as $filter => [$label, $requestArg]) {
            $this->hook($filter, $this->recorder($filter, $label, $requestArg), PHP_INT_MIN);
            $this->hook($filter, $this->guard($filter, $label, $requestArg), PHP_INT_MAX);
        }
    }

    /**
     * A recorder: remembers the value at the earliest priority.
     *
     * @param string $filter     Filter.
     * @param string $label      Label.
     * @param int    $requestArg Index of the request argument.
     * @return callable
     */
    private function recorder(string $filter, string $label, int $requestArg): callable
    {
        $self = null;
        $self = function (...$args) use ($filter, $label, $requestArg, &$self) {
            $value = $args[0] ?? null;
            if (!$this->armed || !$this->isOurs($args[$requestArg] ?? null)) {
                return $value;
            }
            if (!AbilityGuards::holdsEdge($filter, $self, true)) {
                $this->violations[$label] = true;
            }
            $this->recorded[$label][]     = $value;
            $this->recordedData[$label][] = self::dataOf($value);

            return $value;
        };

        return $self;
    }

    /**
     * A guard: compares at the latest priority and refuses.
     *
     * @param string $filter     Filter.
     * @param string $label      Label.
     * @param int    $requestArg Index of the request argument.
     * @return callable
     */
    private function guard(string $filter, string $label, int $requestArg): callable
    {
        $self = null;
        $self = function (...$args) use ($filter, $label, $requestArg, &$self) {
            $value = $args[0] ?? null;
            if (!$this->armed || !$this->isOurs($args[$requestArg] ?? null)) {
                return $value;
            }
            $this->runs[$label] = ($this->runs[$label] ?? 0) + 1;
            if (!AbilityGuards::holdsEdge($filter, $self, false)) {
                $this->violations[$label] = true;
            }
            $stack = $this->recorded[$label] ?? [];
            if ($stack === []) {
                $this->violations[$label] = true;

                return $this->stop($label);
            }
            $recorded     = array_pop($this->recorded[$label]);
            $recordedData = array_pop($this->recordedData[$label]);

            switch ($label) {
                case 'pre_dispatch':
                    if (!empty($value) || !empty($recorded)) {
                        $this->violations[$label] = true;
                    }
                    break;
                case 'before_callbacks':
                    if (!self::same($recorded, $value, $recordedData)) {
                        $this->violations[$label] = true;
                    }
                    $handler = is_array($args[1] ?? null) ? $args[1] : [];
                    $this->handler = $handler;
                    if (!$this->handlerIsCore($handler)) {
                        $this->handlerRefused = true;
                    }
                    break;
                case 'dispatch':
                    if (null !== $value || null !== $recorded || ($args[2] ?? null) !== $this->pattern) {
                        $this->violations[$label] = true;
                    }
                    break;
                case 'after_callbacks':
                    if (!self::same($recorded, $value, $recordedData)) {
                        $this->violations[$label] = true;
                    }
                    break;
            }

            if (isset($this->violations[$label]) || $this->handlerRefused) {
                // The after-callbacks value is the response; a refusal is
                // decided by verify(), so it is left as it is here.
                return $label === 'after_callbacks' ? $value : $this->stop($label);
            }

            return $value;
        };

        return $self;
    }

    /**
     * The value a refusing guard returns: a non-empty error stops the
     * dispatch at pre-dispatch and before the callbacks, and halts it at
     * the dispatch filter.
     *
     * @param string $label Label.
     * @return \WP_Error
     */
    private function stop(string $label): \WP_Error
    {
        if ($this->handlerRefused) {
            return new \WP_Error('rest_handler_not_core', 'rest_handler_not_core', ['status' => 403]);
        }

        return new \WP_Error('wpmgr_rest_intercepted', 'rest_intercepted/' . $label, ['status' => 409]);
    }

    /**
     * Both the callback and the permission callback are core's own code, and
     * the handler accepts the row's method.
     *
     * @param array<string,mixed> $handler Matched handler.
     * @return bool
     */
    public function handlerIsCore(array $handler): bool
    {
        $methods = $handler['methods'] ?? null;
        if (!is_array($methods) || empty($methods[$this->method])) {
            return false;
        }
        $roots = $this->roots ?? AbilityOwnership::roots();
        foreach (['callback', 'permission_callback'] as $key) {
            if (!array_key_exists($key, $handler)) {
                return false;
            }
            $files = AbilityOwnership::sourceFiles($handler[$key]);
            if ($files === null) {
                return false;
            }
            foreach ($files as $file) {
                if (AbilityOwnership::classifyPath($file, $roots)['kind'] !== AbilityOwnership::KIND_CORE) {
                    return false;
                }
            }
        }

        return true;
    }

    /**
     * The post-hoc check, after the dispatch returned and while still armed.
     *
     * @param mixed $response What the dispatch returned.
     * @return void
     */
    public function verify($response): void
    {
        // A call that never reached the before-callbacks guard was answered
        // before matching: that is a pre-dispatch short-circuit.
        if (($this->runs['before_callbacks'] ?? 0) === 0) {
            $this->violations['pre_dispatch'] = true;
        }
        foreach (self::LABELS as $label) {
            if (($this->runs[$label] ?? 0) > 1 || ($this->recorded[$label] ?? []) !== []) {
                $this->violations[$label] = true;
            }
        }
        // The dispatch filter runs only when the permission check passed; a
        // permission refusal skips it and is reported as the route's error.
        foreach (['pre_dispatch', 'before_callbacks', 'after_callbacks'] as $label) {
            if (($this->runs[$label] ?? 0) === 0 && ($this->runs['before_callbacks'] ?? 0) > 0) {
                $this->violations[$label] = true;
            }
        }
        if (!is_object($response) || !method_exists($response, 'get_matched_route') || !method_exists($response, 'get_matched_handler')) {
            $this->violations['pre_dispatch'] = true;

            return;
        }
        if (($this->runs['dispatch'] ?? 0) === 0 && method_exists($response, 'get_status') && (int) $response->get_status() < 400) {
            $this->violations['dispatch'] = true;
        }
        if ($response->get_matched_route() !== $this->pattern) {
            $this->violations['pre_dispatch'] = true;
        }
        $handler = $response->get_matched_handler();
        if (!is_array($handler) || $this->handler === null
            || ($handler['callback'] ?? null) !== ($this->handler['callback'] ?? null)
            || ($handler['permission_callback'] ?? null) !== ($this->handler['permission_callback'] ?? null)) {
            $this->violations['pre_dispatch'] = true;
        }
    }

    /**
     * Violation labels, in the fixed order of LABELS.
     *
     * @return list<string>
     */
    public function violations(): array
    {
        return array_values(array_filter(self::LABELS, fn ($l) => isset($this->violations[$l])));
    }

    /**
     * Did the matched handler fail the core-ownership check?
     *
     * @return bool
     */
    public function handlerRefused(): bool
    {
        return $this->handlerRefused;
    }

    /**
     * Remove every hook and stop guarding.
     *
     * @return void
     */
    public function disarm(): void
    {
        $this->armed = false;
        foreach ($this->hooks as [$filter, $callback, $priority]) {
            remove_filter($filter, $callback, $priority);
        }
        $this->hooks = [];
    }

    /**
     * @param mixed $candidate A filter's request argument.
     * @return bool
     */
    private function isOurs($candidate): bool
    {
        return $this->request !== null && $candidate === $this->request;
    }

    /**
     * Unchanged: the same instance for an object (and equal data for a
     * response), equal for anything else.
     *
     * @param mixed $recorded     Value at the earliest priority.
     * @param mixed $value        Value at the latest priority.
     * @param mixed $recordedData Data recorded with it.
     * @return bool
     */
    private static function same($recorded, $value, $recordedData): bool
    {
        if (is_object($recorded) || is_object($value)) {
            return $recorded === $value && self::dataOf($value) == $recordedData; // phpcs:ignore Universal.Operators.StrictComparisons.LooseEqual -- response data is compared by value, as the guard contract states; identity is checked separately
        }

        return $recorded == $value; // phpcs:ignore Universal.Operators.StrictComparisons.LooseEqual -- a non-object value is compared by value
    }

    /**
     * The data of a response value, for comparison.
     *
     * @param mixed $value Value.
     * @return mixed
     */
    private static function dataOf($value)
    {
        if (is_object($value) && method_exists($value, 'get_data')) {
            return RestCall::plain($value->get_data());
        }
        if ($value instanceof \WP_Error) {
            return [$value->get_error_code(), $value->get_error_data()];
        }

        return null;
    }

    /**
     * @param string   $filter   Filter.
     * @param callable $callback Callback.
     * @param int      $priority Priority.
     * @return void
     */
    private function hook(string $filter, callable $callback, int $priority): void
    {
        add_filter($filter, $callback, $priority, 4);
        $this->hooks[] = [$filter, $callback, $priority];
    }
}
