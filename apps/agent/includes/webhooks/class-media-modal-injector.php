<?php
/**
 * MediaModalInjector: surfaces the per-attachment optimization stats on two WP
 * surfaces — the Media Library modal panel and the attachment edit-screen meta
 * box — driven by one shared renderer (StatsRenderer).
 *
 * Implements the §4 modal injection pattern (analysis doc lines 26-28,647-674,
 * 740-757):
 *   - wp_prepare_attachment_for_js  -> stuff a `wpmgr_media_optimizer` HTML
 *     attribute into each attachment's Backbone model (only when optimizable).
 *   - admin_enqueue_scripts          -> (screen-guarded to upload.php) enqueue
 *     a JS monkey-patch of wp.media.view.Attachment.prototype.render that
 *     mounts the stats panel `beforebegin` of `.settings` (null-safe via ?.).
 *   - add_meta_boxes_attachment      -> a `side` meta box echoing the SAME HTML.
 *
 * SECURITY: the injected HTML is built by StatsRenderer (fully escaped). The JS
 * reads the string straight off the model attribute and inserts it — so the
 * escaping done in StatsRenderer is what makes this XSS-safe. The meta box echo
 * is the same escaped string. No nonce needed (read-only render; no actions).
 *
 * 2026-07 wp.org review fix: the modal-patch script used to be hand-printed
 * via a raw `echo '<script>...'` bound to `admin_footer-upload.php`. It is
 * now registered as a real (inline-only, no external src) script handle via
 * wp_enqueue_script()/wp_add_inline_script() on `admin_enqueue_scripts`, still
 * screen-guarded so it loads ONLY on the Media Library (upload.php) screen.
 *
 * @package WPMgr\Agent\Webhooks
 */

declare(strict_types=1);

namespace WPMgr\Agent\Webhooks;

use WPMgr\Agent\Media\StatsRenderer;

/**
 * Injects the optimization stats panel into the media modal + edit meta box.
 */
final class MediaModalInjector
{
    /** The Backbone model attribute carrying the pre-rendered stats HTML. */
    public const MODEL_ATTR = 'wpmgr_media_optimizer';

    /** wp_enqueue_script()/wp_add_inline_script() handle for the modal patch. */
    public const SCRIPT_HANDLE = 'wpmgr-media-stats';

    private StatsRenderer $renderer;

    public function __construct(?StatsRenderer $renderer = null)
    {
        $this->renderer = $renderer ?? new StatsRenderer();
    }

    /**
     * Register the three hooks. Admin-only; safe to call once per boot.
     *
     * @return void
     */
    public function registerHooks(): void
    {
        if (!function_exists('add_filter') || !function_exists('add_action')) {
            return;
        }
        add_filter('wp_prepare_attachment_for_js', [$this, 'injectModelAttribute'], 10, 2);
        add_action('add_meta_boxes_attachment', [$this, 'registerMetaBox']);
        add_action('admin_enqueue_scripts', [$this, 'enqueueModalScript']);
    }

    /**
     * wp_prepare_attachment_for_js filter: attach the stats HTML to the JS model
     * for optimizable attachments only.
     *
     * @param array<string,mixed> $response   The JS-serialized attachment.
     * @param mixed               $attachment The attachment post object.
     * @return array<string,mixed>
     */
    public function injectModelAttribute($response, $attachment): array
    {
        if (!is_array($response)) {
            $response = [];
        }
        $id   = is_object($attachment) && isset($attachment->ID) ? (int) $attachment->ID : 0;
        $mime = is_array($response) && isset($response['mime']) && is_string($response['mime']) ? $response['mime'] : '';
        if ($id <= 0) {
            return $response;
        }

        $html = $this->renderer->renderForAttachment($id, $mime);
        if ($html !== '') {
            $response[self::MODEL_ATTR] = $html;
        }

        return $response;
    }

    /**
     * add_meta_boxes_attachment action: add a `side` meta box on the attachment
     * edit screen rendering the same stats HTML.
     *
     * @param mixed $post The attachment post object.
     * @return void
     */
    public function registerMetaBox($post): void
    {
        if (!function_exists('add_meta_box')) {
            return;
        }
        $id   = is_object($post) && isset($post->ID) ? (int) $post->ID : 0;
        $mime = is_object($post) && isset($post->post_mime_type) && is_string($post->post_mime_type)
            ? $post->post_mime_type
            : '';
        if ($id <= 0 || !$this->renderer->isOptimizable($id, $mime)) {
            return;
        }

        add_meta_box(
            'wpmgr_media_optimizer',
            $this->label('WPMgr Image Optimization'),
            [$this, 'renderMetaBox'],
            'attachment',
            'side'
        );
    }

    /**
     * Meta box callback: echo the escaped stats HTML.
     *
     * @param mixed $post The attachment post object.
     * @return void
     */
    public function renderMetaBox($post): void
    {
        $id   = is_object($post) && isset($post->ID) ? (int) $post->ID : 0;
        $mime = is_object($post) && isset($post->post_mime_type) && is_string($post->post_mime_type)
            ? $post->post_mime_type
            : '';
        if ($id <= 0) {
            return;
        }
        // StatsRenderer escapes every dynamic value with esc_html() at
        // assembly time (belt-and-suspenders); wp_kses() here is the visible
        // escape-at-output-boundary pass (2026-07 wp.org review fix), scoped
        // to exactly the tags/attributes StatsRenderer emits.
        echo wp_kses($this->renderer->renderForAttachment($id, $mime), self::allowedStatsKses());
    }

    /**
     * Explicit wp_kses() tag/attribute allowlist for StatsRenderer's output,
     * scoped to exactly the tags/attributes it emits (grep-verified): plain
     * <div class="...">/<strong>/<span class="...">/<code> wrapper markup,
     * no interactive elements at all -- unlike the 2FA module's forms, this
     * is a read-only display panel.
     *
     * @return array<string,array<string,bool>>
     */
    private static function allowedStatsKses(): array
    {
        return [
            'div'    => ['class' => true],
            'span'   => ['class' => true],
            'strong' => [],
            'code'   => [],
        ];
    }

    /**
     * admin_enqueue_scripts: enqueue (screen-guarded to the Media Library
     * upload.php screen) the Backbone render monkey-patch that mounts the
     * stats panel beforebegin of `.settings`. The HTML is read from the
     * (already-escaped) model attribute.
     *
     * The script has no external src -- its body is inline-only, so a
     * src-less handle is registered purely to carry the inline body via
     * wp_add_inline_script() (the standard WP idiom for inline-only script
     * content). Declaring the core `media-views` handle as a dependency
     * guarantees wp.media.view.Attachment exists by the time this callback
     * runs, regardless of exactly when admin_enqueue_scripts fired relative
     * to wp_enqueue_media().
     *
     * @param string $hook Current admin page hook suffix.
     * @return void
     */
    public function enqueueModalScript(string $hook): void
    {
        if ($hook !== 'upload.php') {
            return;
        }
        if (!function_exists('wp_enqueue_script') || !function_exists('wp_add_inline_script')) {
            return;
        }

        $ver = defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : false;

        wp_enqueue_script(self::SCRIPT_HANDLE, '', ['media-views'], $ver, true);

        $attr = self::MODEL_ATTR;
        // The attribute name is a constant; the HTML it carries was escaped in
        // StatsRenderer.
        $inline = '((Attachment) => {'
            . 'if (!Attachment || !Attachment.prototype) return;'
            . 'const originalRender = Attachment.prototype.render;'
            . 'Attachment.prototype.render = function () {'
            . 'originalRender.apply(this, arguments);'
            . 'const html = this.model && this.model.get ? this.model.get(' . wp_json_encode($attr) . ') : "";'
            . 'if (!html) return;'
            . 'const settings = this.el && this.el.querySelector ? this.el.querySelector(".settings") : null;'
            . 'if (settings && settings.insertAdjacentHTML) {'
            . 'settings.insertAdjacentHTML("beforebegin", "<div class=\"wpmgr-media-stats-panel details\">" + html + "</div>");'
            . '}'
            . '};'
            . '})(window.wp && wp.media && wp.media.view ? wp.media.view.Attachment : null);';

        wp_add_inline_script(self::SCRIPT_HANDLE, $inline);
    }

    /**
     * @param string $text
     * @return string
     */
    private function label(string $text): string
    {
        return $text;
    }
}
