<?php
/**
 * IFrame — replace heavy YouTube iframes with a lightweight click-to-load facade.
 *
 * Each YouTube <iframe> is swapped for a small placeholder: a wrapper with the
 * video's thumbnail (loaded from i.ytimg.com, no YouTube JS) and a play glyph.
 * The real iframe is injected by a tiny inline runtime on click, with autoplay.
 * This removes ~hundreds of KB of YouTube JS + multiple third-party requests
 * from the initial page load (a large LCP/TBT win for video-heavy pages).
 *
 * The facade CSS + the one-function runtime are injected once each. Excluded
 * iframes (PerfConfig::$lazyLoadExclusions substrings) are left as-is.
 *
 * The placeholder is built from the public i.ytimg.com thumbnail URL (no
 * third-party thumbnail service).
 *
 * @package WPMgr\Agent\Optimizer
 */

declare(strict_types=1);

namespace WPMgr\Agent\Optimizer;

/**
 * Builds YouTube facades.
 */
final class IFrame
{
    private PerfConfig $config;

    /**
     * @param PerfConfig|null $config Optimization config.
     */
    public function __construct(?PerfConfig $config = null)
    {
        $this->config = $config ?? PerfConfig::load();
    }

    /**
     * Replace YouTube iframes with facades.
     *
     * @param string $html Full page HTML.
     * @return string
     */
    public function process(string $html): string
    {
        if (!$this->config->youtubePlaceholder) {
            return $html;
        }
        $pattern = '/<iframe\b[^>]*\bsrc=["\'](?:https?:)?\/\/(?:www\.)?(?:youtube(?:-nocookie)?\.com\/embed\/|youtu\.be\/)([A-Za-z0-9_-]{6,})[^"\']*["\'][^>]*>\s*<\/iframe>/i';
        if (!preg_match_all($pattern, $html, $matches, PREG_SET_ORDER)) {
            return $html;
        }

        $replaced = false;
        foreach ($matches as $set) {
            $tag = $set[0];
            if (TagHelper::matchesAny($this->config->lazyLoadExclusions, $tag)) {
                continue;
            }
            $videoId = $set[1];
            if (!preg_match('/^[A-Za-z0-9_-]{6,}$/', $videoId)) {
                continue;
            }
            $src   = TagHelper::attr($tag, 'src') ?? '';
            $title = TagHelper::attr($tag, 'title') ?? 'YouTube video';
            $facade = $this->facade($videoId, $src, $title);
            $html = str_replace($tag, $facade, $html);
            $replaced = true;
        }

        if ($replaced) {
            $html = $this->injectAssets($html);
        }
        return $html;
    }

    /**
     * Build the facade markup for one video.
     *
     * @param string $videoId YouTube video id.
     * @param string $src      Original iframe src (carries query params).
     * @param string $title    Accessible title.
     * @return string
     */
    private function facade(string $videoId, string $src, string $title): string
    {
        if (str_starts_with($src, '//')) {
            $src = 'https:' . $src;
        }
        $src .= (strpos($src, '?') !== false ? '&' : '?') . 'autoplay=1';
        $thumb = 'https://i.ytimg.com/vi/' . $videoId . '/hqdefault.jpg';

        $escSrc   = htmlspecialchars($src, ENT_QUOTES);
        $escThumb = htmlspecialchars($thumb, ENT_QUOTES);
        $escTitle = htmlspecialchars($title, ENT_QUOTES);

        return '<div class="wpmgr-yt" data-wpmgr-yt-src="' . $escSrc . '" onclick="wpmgrLoadYt(this)">'
            . '<img src="' . $escThumb . '" loading="lazy" width="1280" height="720" alt="' . $escTitle . '">'
            . '<button type="button" class="wpmgr-yt-play" aria-label="' . $escTitle . '"></button>'
            . '</div>';
    }

    /**
     * Inject the facade CSS + runtime once each.
     *
     * @param string $html Page HTML.
     * @return string
     */
    private function injectAssets(string $html): string
    {
        if (strpos($html, 'data-wpmgr-yt-assets') !== false) {
            return $html;
        }
        $css = '.wpmgr-yt{position:relative;display:block;width:100%;max-width:100%;aspect-ratio:16/9;'
            . 'cursor:pointer;overflow:hidden;background:#000}'
            . '.wpmgr-yt img{width:100%;height:100%;object-fit:cover;display:block}'
            . '.wpmgr-yt-play{position:absolute;inset:0;margin:auto;width:68px;height:48px;border:0;'
            . 'background:rgba(0,0,0,.55);border-radius:12px;cursor:pointer}'
            . '.wpmgr-yt-play::after{content:"";position:absolute;top:50%;left:54%;transform:translate(-50%,-50%);'
            . 'border-style:solid;border-width:11px 0 11px 18px;border-color:transparent transparent transparent #fff}';
        $js = 'function wpmgrLoadYt(e){var s=e.getAttribute("data-wpmgr-yt-src");if(!s)return;'
            . 'var f=document.createElement("iframe");f.setAttribute("src",s);f.setAttribute("frameborder","0");'
            . 'f.setAttribute("allow","accelerometer;autoplay;clipboard-write;encrypted-media;gyroscope;picture-in-picture");'
            . 'f.setAttribute("allowfullscreen","1");f.style.position="absolute";f.style.inset="0";'
            . 'f.style.width="100%";f.style.height="100%";e.innerHTML="";e.appendChild(f);}';

        // phpcs:ignore WordPress.WP.EnqueuedResources.NonEnqueuedStylesheet,WordPress.WP.EnqueuedResources.NonEnqueuedScript -- this class only runs from Optimizer::run(), called by CacheWriter's ob_start() callback (class-cache-writer.php) on the fully-rendered HTML string of a cacheable MISS, well after wp_head has already printed; the transform rewrites body <iframe> markup already in the buffered string, so it can only operate as a post-hoc string rewrite -- WP's enqueue API has no queue left to append to at this point
        $assets = '<style data-wpmgr-yt-assets>' . $css . '</style><script data-wpmgr-yt-assets>' . $js . '</script>';
        if (stripos($html, '</head>') !== false) {
            return (string) preg_replace('/<\/head>/i', $assets . '</head>', $html, 1);
        }
        return $assets . $html;
    }
}
