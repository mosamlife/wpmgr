<?php
/**
 * HtaccessInstaller: writes the Accept-header AVIF/WebP fallback block into the
 * site root .htaccess, between idempotent `# BEGIN WPMgr Media` /
 * `# END WPMgr Media` markers.
 *
 * Adapts reference/flying-press/assets/htaccess.txt:69-106 (the AVIF/WebP
 * Accept-fallback + `Vary: Accept`) under WPMgr markers. Pattern doc §3.
 *
 * Why this exists: in DIFFERENT-EXT mode the DB references the modern URL
 * (banner.avif), and the legacy twin (banner.jpg) is left on disk. Apache then
 * downgrades to the twin ONLY when the client did not advertise the modern
 * format AND the twin exists on disk (the `-f` guard ensures a missing twin
 * never 404s). `Vary: Accept` keeps shared caches/CDNs from poisoning a
 * no-AVIF client with an AVIF response.
 *
 * Idempotent: writing twice yields exactly ONE marked block (we replace the
 * existing block rather than append). On nginx (no .htaccess) we do NOT edit
 * server config — we surface an admin notice with the equivalent snippet.
 *
 * @package WPMgr\Agent\Media
 */

declare(strict_types=1);

namespace WPMgr\Agent\Media;

/**
 * Idempotent .htaccess Accept-fallback installer (+ nginx detection notice).
 */
final class HtaccessInstaller
{
    /** Opening marker (WP managed-block convention). */
    public const BEGIN = '# BEGIN WPMgr Media';

    /** Closing marker. */
    public const END = '# END WPMgr Media';

    /** Option flag: nginx detected, surface the manual-snippet admin notice. */
    public const OPTION_NGINX_NOTICE = 'wpmgr_agent_media_nginx_notice';

    /**
     * Ensure the WPMgr Media block is present in the site-root .htaccess.
     * On Apache: insert/replace the marked block (idempotent). On nginx: skip
     * the file edit and set the admin-notice flag.
     *
     * @return bool True when the block is present (or nginx notice set).
     */
    public function install(): bool
    {
        if ($this->isNginx()) {
            if (function_exists('update_option')) {
                update_option(self::OPTION_NGINX_NOTICE, 1, false);
            }

            return false;
        }

        $path = $this->htaccessPath();
        if ($path === '') {
            return false;
        }

        $existing = '';
        if (@is_file($path) && @is_readable($path)) {
            $existing = (string) @file_get_contents($path);
        }

        $block   = self::BEGIN . "\n" . $this->blockBody() . "\n" . self::END;
        $updated = $this->spliceBlock($existing, $block);

        if ($updated === $existing) {
            return true; // Already current; nothing to write.
        }

        // Only write if the directory is writable.
        $dir = dirname($path);
        if (!@is_writable($dir) && @is_file($path) && !@is_writable($path)) {
            if (function_exists('update_option')) {
                update_option(self::OPTION_NGINX_NOTICE, 0, false);
            }

            return false;
        }

        return @file_put_contents($path, $updated, LOCK_EX) !== false;
    }

    /**
     * Remove the WPMgr Media block from .htaccess (uninstall/cleanup).
     *
     * @return bool True on success or when nothing to remove.
     */
    public function uninstall(): bool
    {
        $path = $this->htaccessPath();
        if ($path === '' || !@is_file($path)) {
            return true;
        }
        $existing = (string) @file_get_contents($path);
        $stripped = $this->stripBlock($existing);
        if ($stripped === $existing) {
            return true;
        }

        return @file_put_contents($path, $stripped, LOCK_EX) !== false;
    }

    /**
     * Compute the .htaccess content that WOULD be written for a given input,
     * without touching disk. Pure — used by the idempotency unit test.
     *
     * @param string $existing Current .htaccess content.
     * @return string The content with exactly one WPMgr Media block.
     */
    public function renderInto(string $existing): string
    {
        $block = self::BEGIN . "\n" . $this->blockBody() . "\n" . self::END;

        return $this->spliceBlock($existing, $block);
    }

    /**
     * The equivalent nginx snippet (shown in the admin notice; never auto-applied).
     *
     * @return string
     */
    public function nginxSnippet(): string
    {
        return <<<NGINX
# WPMgr Media: serve AVIF/WebP when the client advertises support, else the legacy twin.
location ~* ^(?<wpmgr_base>.+)\.(?<wpmgr_ext>avif|webp)$ {
    add_header Vary Accept;
    set \$wpmgr_modern "";
    if (\$http_accept ~* "image/\$wpmgr_ext") { set \$wpmgr_modern "A"; }
    if (-f \$request_filename) { set \$wpmgr_modern "\${wpmgr_modern}B"; }
    if (\$wpmgr_modern = "AB") { break; }
    # Fall back to a legacy twin if one exists (try jpg, then png).
    try_files \$wpmgr_base.jpg \$wpmgr_base.png \$uri =404;
}
NGINX;
    }

    // ------------------------------------------------------------------
    // internals
    // ------------------------------------------------------------------

    /**
     * The Apache rewrite block body (between the markers). Adapted from
     * flying-press htaccess.txt:69-106 (markers + cache rules dropped — we only
     * own the Accept fallback + Vary).
     *
     * @return string
     */
    private function blockBody(): string
    {
        return <<<HTACCESS
<IfModule mod_rewrite.c>
	RewriteEngine On

	# AVIF fallback: no AVIF support -> serve png/jpg/jpeg twin if it exists.
	RewriteCond %{HTTP_ACCEPT} !image/avif [NC]
	RewriteCond %{DOCUMENT_ROOT}/\$1.png -f
	RewriteRule ^(.+)\.avif\$ \$1.png [L]

	RewriteCond %{HTTP_ACCEPT} !image/avif [NC]
	RewriteCond %{DOCUMENT_ROOT}/\$1.jpg -f
	RewriteRule ^(.+)\.avif\$ \$1.jpg [L]

	RewriteCond %{HTTP_ACCEPT} !image/avif [NC]
	RewriteCond %{DOCUMENT_ROOT}/\$1.jpeg -f
	RewriteRule ^(.+)\.avif\$ \$1.jpeg [L]

	# WebP fallback: no WebP support -> serve png/jpg/jpeg twin if it exists.
	RewriteCond %{HTTP_ACCEPT} !image/webp [NC]
	RewriteCond %{DOCUMENT_ROOT}/\$1.png -f
	RewriteRule ^(.+)\.webp\$ \$1.png [L]

	RewriteCond %{HTTP_ACCEPT} !image/webp [NC]
	RewriteCond %{DOCUMENT_ROOT}/\$1.jpg -f
	RewriteRule ^(.+)\.webp\$ \$1.jpg [L]

	RewriteCond %{HTTP_ACCEPT} !image/webp [NC]
	RewriteCond %{DOCUMENT_ROOT}/\$1.jpeg -f
	RewriteRule ^(.+)\.webp\$ \$1.jpeg [L]
</IfModule>

<IfModule mod_headers.c>
	<FilesMatch "\.(avif|webp)\$">
		Header merge Vary Accept
	</FilesMatch>
</IfModule>
HTACCESS;
    }

    /**
     * Insert or replace the marked block in $content. If a block already exists
     * (between BEGIN/END) it is REPLACED in place; otherwise the block is
     * prepended (so the Accept-rewrite runs before WP's own front-controller
     * rewrites). Always exactly one block — idempotent.
     *
     * @param string $content Existing file content.
     * @param string $block   The full block (markers included).
     * @return string
     */
    private function spliceBlock(string $content, string $block): string
    {
        $stripped = $this->stripBlock($content);

        if (trim($stripped) === '') {
            return $block . "\n";
        }

        // Prepend so our Accept fallback is evaluated ahead of WP's rules.
        return $block . "\n\n" . ltrim($stripped, "\n");
    }

    /**
     * Remove any existing WPMgr Media marked block from $content.
     *
     * @param string $content
     * @return string
     */
    private function stripBlock(string $content): string
    {
        $pattern = '/' . preg_quote(self::BEGIN, '/') . '.*?' . preg_quote(self::END, '/') . "\n?/s";
        $out     = preg_replace($pattern, '', $content);

        return is_string($out) ? $out : $content;
    }

    /**
     * Resolve the site-root .htaccess path. Uses get_home_path() when available
     * (admin context), else ABSPATH.
     *
     * @return string
     */
    private function htaccessPath(): string
    {
        $root = '';
        if (function_exists('get_home_path')) {
            $root = (string) get_home_path();
        }
        if ($root === '' && defined('ABSPATH')) {
            $root = (string) ABSPATH;
        }
        if ($root === '') {
            return '';
        }

        return rtrim($root, '/\\') . DIRECTORY_SEPARATOR . '.htaccess';
    }

    /**
     * Whether the server is nginx (which has no .htaccess). Detected via
     * SERVER_SOFTWARE — we never auto-edit nginx config.
     *
     * @return bool
     */
    public function isNginx(): bool
    {
        $software = isset($_SERVER['SERVER_SOFTWARE']) && is_string($_SERVER['SERVER_SOFTWARE'])
            ? strtolower($_SERVER['SERVER_SOFTWARE'])
            : '';

        return $software !== '' && strpos($software, 'nginx') !== false;
    }
}
