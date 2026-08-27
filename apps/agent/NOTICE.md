# NOTICE — WPMgr WordPress agent

The WPMgr agent is MIT-licensed (see the repository `LICENSE`).

## Third-party attribution

### bjeavons/zxcvbn-php (MIT)

Password-strength scoring in the Security module's password policy uses
[`bjeavons/zxcvbn-php`](https://github.com/bjeavons/zxcvbn-php) (^1.4, MIT), a
pure-PHP port of zxcvbn, in the agent's Composer dependencies. Copyright (c)
2013-2014 Benjamin Jeavons.

### lilliput (Discord, MIT)

`github.com/discord/lilliput` is MIT-licensed and used by the control-plane
`media-encoder` service for image decoding/encoding. It is not bundled with this
agent plugin (the agent performs no encoding).

### matthiasmullie/minify (MIT)

CSS and JS minification uses
[`matthiasmullie/minify`](https://github.com/matthiasmullie/minify) (^1.3, MIT), a
small pure-PHP library in the agent's Composer dependencies.

### web-vitals (Google, Apache-2.0)

The Real User Monitoring collector bundles
[`web-vitals`](https://github.com/GoogleChrome/web-vitals) (^5.3.0,
Apache-2.0), Copyright 2020 Google LLC, via an esbuild build in
`apps/tracker`. The bundle ships inside the plugin zip as
`assets/wpmgr-rum.js` / `assets/wpmgr-rum.min.js`. Source and license:
https://github.com/GoogleChrome/web-vitals/blob/main/LICENSE

## Implementation notes

The Media Optimizer keeps a per-attachment postmeta record (optimization status,
per-size optimized maps, and a verbatim pre-optimization `_wp_attachment_metadata`
snapshot as the restore anchor), archives a re-compressed original under a
double-extension name and reverses it on restore, and serves a legacy image twin
via an Accept-header `.htaccess` fallback when the client does not advertise
AVIF/WebP. Image optimization itself runs on WPMgr's own Go control plane.

The page cache uses the standard WordPress disk-cache pattern: a `WP_CACHE`
advanced-cache drop-in serving pre-gzipped HTML from disk, an `.htaccess` / nginx
fast-path, a managed-block marker convention, a cacheability exclusion set, disk
purge, and a preloader. Remove Unused CSS is computed by WPMgr's pure-Go engine on
the control plane (no headless browser, no third-party service); the agent posts
the rendered page and inlines the result, degrading to full CSS on any failure.
See the repository-root [`NOTICE.md`](../../NOTICE.md).
