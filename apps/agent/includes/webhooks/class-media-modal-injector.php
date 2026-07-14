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
 *   - admin_footer-upload.php        -> a JS monkey-patch of
 *     wp.media.view.Attachment.prototype.render that mounts the stats panel
 *     `beforebegin` of `.settings` (null-safe via ?.).
 *   - add_meta_boxes_attachment      -> a `side` meta box echoing the SAME HTML.
 *
 * SECURITY: the injected HTML is built by StatsRenderer (fully escaped). The JS
 * reads the string straight off the model attribute and inserts it — so the
 * escaping done in StatsRenderer is what makes this XSS-safe. The meta box echo
 * is the same escaped string. No nonce needed (read-only render; no actions).
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
        add_action('admin_footer-upload.php', [$this, 'printModalScript']);
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
     * admin_footer-upload.php: print the Backbone render monkey-patch that
     * mounts the stats panel beforebegin of `.settings`. The HTML is read from
     * the (already-escaped) model attribute.
     *
     * @return void
     */
    public function printModalScript(): void
    {
        $attr = self::MODEL_ATTR;
        // The attribute name is a constant; the HTML it carries was escaped in
        // StatsRenderer. textContent of the mount class is static.
        echo '<script id="wpmgr-media-stats-js">'
            . '((Attachment) => {'
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
            . '})(window.wp && wp.media && wp.media.view ? wp.media.view.Attachment : null);'
            . '</script>';
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
