<?php
/**
 * DropinInstaller — installs and removes the page-cache serving drop-in
 * (advanced-cache.php) in wp-content.
 *
 * The drop-in template lives at assets/wpmgr-advanced-cache.php with a
 * `CONFIG_TO_REPLACE` placeholder. On install we render the live config into
 * the placeholder (so the drop-in is self-contained and needs zero plugin/DB
 * load on the serving fast path) and write it to wp-content.
 *
 * WP Engine quirk: on Atomic / EverCache hosts (detected by the presence of the
 * `Atomic_Persistent_Data` class) WP Engine ships its OWN advanced-cache.php, so
 * we MUST NOT overwrite it. There we install under the alternate filename
 * `wpmgr-advanced-cache.php` and chain to it; on every other host we install the
 * canonical `advanced-cache.php`.
 *
 * Standard WordPress disk-cache drop-in technique.
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

/**
 * Installs / removes the advanced-cache.php serving drop-in.
 */
final class DropinInstaller
{
    /** Canonical WordPress drop-in filename. */
    public const CANONICAL = 'advanced-cache.php';

    /** Alternate filename used on WP Engine Atomic hosts. */
    public const WPENGINE = 'wpmgr-advanced-cache.php';

    /** Placeholder in the template replaced with the live config var_export. */
    public const CONFIG_PLACEHOLDER = 'CONFIG_TO_REPLACE';

    /** Signature line proving a drop-in on disk is ours (safe to overwrite/remove). */
    public const SIGNATURE = 'WPMgr Page Cache drop-in';

    /**
     * Constant name embedded in the template that carries the drop-in version.
     * DropinInstaller reads this from the on-disk copy and from the template to
     * detect a version mismatch and trigger a transparent reinstall.
     */
    public const VERSION_CONSTANT = 'WPMGR_PAGE_CACHE_DROPIN_VERSION';

    /** Absolute path to the wp-content directory. */
    private string $contentDir;

    /** Absolute path to the template source file. */
    private string $templatePath;

    /**
     * @param string|null $contentDir   wp-content path (defaults to WP_CONTENT_DIR).
     * @param string|null $templatePath Drop-in template path (defaults to the plugin asset).
     */
    public function __construct(?string $contentDir = null, ?string $templatePath = null)
    {
        if ($contentDir !== null) {
            $this->contentDir = rtrim($contentDir, '/\\');
        } elseif (defined('WP_CONTENT_DIR')) {
            $this->contentDir = rtrim((string) constant('WP_CONTENT_DIR'), '/\\');
        } else {
            $this->contentDir = '';
        }

        if ($templatePath !== null) {
            $this->templatePath = $templatePath;
        } elseif (defined('WPMGR_AGENT_DIR')) {
            $this->templatePath = rtrim((string) constant('WPMGR_AGENT_DIR'), '/\\')
                . '/assets/wpmgr-advanced-cache.php';
        } else {
            $this->templatePath = '';
        }
    }

    /**
     * Whether this host ships its own advanced-cache.php (WP Engine Atomic), in
     * which case we install under the alternate filename to avoid clobbering it.
     *
     * @return bool
     */
    public function isWpEngine(): bool
    {
        return class_exists('Atomic_Persistent_Data');
    }

    /**
     * The drop-in filename appropriate for this host.
     *
     * @return string
     */
    public function dropinFilename(): string
    {
        return $this->isWpEngine() ? self::WPENGINE : self::CANONICAL;
    }

    /**
     * Absolute path the drop-in is installed at on this host.
     *
     * @return string
     */
    public function dropinPath(): string
    {
        if ($this->contentDir === '') {
            return '';
        }
        return $this->contentDir . '/' . $this->dropinFilename();
    }

    /**
     * Extract the WPMGR_PAGE_CACHE_DROPIN_VERSION string from a PHP source blob.
     *
     * Looks for a define() call of the form:
     *   define('WPMGR_PAGE_CACHE_DROPIN_VERSION', '1.2.3');
     *
     * Returns an empty string when the constant is absent.
     *
     * @param string $source PHP source to search.
     * @return string Version string or ''.
     */
    public function extractVersion(string $source): string
    {
        if (preg_match(
            "/define\(\s*'" . preg_quote(self::VERSION_CONSTANT, '/') . "'\s*,\s*'([^']+)'\s*\)/",
            $source,
            $m
        ) === 1) {
            return $m[1];
        }
        return '';
    }

    /**
     * Whether the installed drop-in version differs from the template version.
     *
     * Returns false (no refresh needed) in any of these cases:
     *   - The template is unreadable.
     *   - The template carries no version constant.
     *   - The installed drop-in is absent (will be written by install() anyway).
     *   - The installed drop-in is foreign (we never touch a foreign file).
     *   - The installed and template versions match.
     *
     * Returns true only when we can confirm our drop-in is on disk AND its
     * version constant is older than the template.
     *
     * @return bool
     */
    public function needsRefresh(): bool
    {
        $path = $this->dropinPath();
        if ($path === '' || !@is_file($path)) {
            return false; // nothing installed — install() handles first write
        }
        if ($this->templatePath === '' || !@is_file($this->templatePath)) {
            return false; // can't read template — can't compare
        }

        $installed = @file_get_contents($path);
        if ($installed === false || strpos($installed, self::SIGNATURE) === false) {
            return false; // absent or foreign — never touch
        }

        $template = @file_get_contents($this->templatePath);
        if ($template === false) {
            return false;
        }

        $templateVersion  = $this->extractVersion($template);
        $installedVersion = $this->extractVersion($installed);

        if ($templateVersion === '' || $installedVersion === '') {
            return false; // one side has no version — cannot compare safely
        }

        return $installedVersion !== $templateVersion;
    }

    /**
     * Whether our drop-in (any version) is on disk at this host's drop-in path.
     * A foreign or unreadable file is not ours.
     *
     * @return bool
     */
    public function isInstalled(): bool
    {
        $path = $this->dropinPath();
        if ($path === '' || !@is_file($path)) {
            return false;
        }
        $existing = @file_get_contents($path);
        return $existing !== false && strpos($existing, self::SIGNATURE) !== false;
    }

    /**
     * Render the drop-in source with the live config inlined.
     *
     * @param array<string,mixed> $config Serialisable cache config.
     * @return string The rendered PHP source (empty string when the template is unreadable).
     */
    public function render(array $config): string
    {
        if ($this->templatePath === '' || !@is_file($this->templatePath)) {
            return '';
        }
        $template = @file_get_contents($this->templatePath);
        if ($template === false) {
            return '';
        }

        $export = var_export($config, true); // phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_var_export -- generating PHP source for the advanced-cache.php drop-in, not debug output

        return str_replace(self::CONFIG_PLACEHOLDER, $export, $template);
    }

    /**
     * Install (or refresh) the drop-in with the given config. Idempotent: a
     * byte-identical existing drop-in is left untouched.
     *
     * Refuses to overwrite a foreign advanced-cache.php (one that is NOT ours and
     * NOT on a WP Engine host) — that signals another cache plugin owns it.
     *
     * @param array<string,mixed> $config Cache config to inline.
     * @return bool True on success or when already current.
     */
    public function install(array $config): bool
    {
        $path = $this->dropinPath();
        if ($path === '') {
            return false;
        }

        $rendered = $this->render($config);
        if ($rendered === '') {
            return false;
        }

        // On non-WP-Engine hosts, never clobber a foreign canonical drop-in.
        // Treat an unreadable existing file as foreign: we cannot confirm it is
        // ours, so we must not overwrite it.
        if (!$this->isWpEngine() && @is_file($path)) {
            $existing = @file_get_contents($path);
            if ($existing === false) {
                return false; // unreadable: treat as foreign, refuse to overwrite
            }
            if (strpos($existing, self::SIGNATURE) === false && trim($existing) !== '') {
                return false; // another cache plugin owns advanced-cache.php
            }
            if ($existing === $rendered) {
                return true; // already current
            }
        }

        if (!@is_writable($this->contentDir) && !(@is_file($path) && @is_writable($path))) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
            return false;
        }

        $result = @file_put_contents($path, $rendered, LOCK_EX);
        return $result !== false;
    }

    /**
     * Remove the drop-in if (and only if) it is ours. Idempotent.
     *
     * @return bool True when the drop-in is absent or successfully removed.
     */
    public function uninstall(): bool
    {
        $path = $this->dropinPath();
        if ($path === '' || !@is_file($path)) {
            return true;
        }

        $existing = @file_get_contents($path);
        if ($existing === false) {
            return true;
        }
        // Only delete a drop-in we recognise as ours.
        if (strpos($existing, self::SIGNATURE) === false) {
            return true;
        }

        wp_delete_file($path);
        return !@file_exists($path);
    }
}
