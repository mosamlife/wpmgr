# NOTICE — WPMgr

WPMgr's control plane is AGPL-3.0; the WordPress agent is MIT-licensed. See the
repository `LICENSE` files. This file records third-party attribution for the
open-source dependencies used by WPMgr. The agent also carries its own
attribution at [`apps/agent/NOTICE.md`](apps/agent/NOTICE.md).

## Performance Suite

### Page caching and database cleanup

WPMgr's page cache uses the standard WordPress disk-cache pattern: a `WP_CACHE`
advanced-cache drop-in serves pre-gzipped HTML from disk on a hit, with an
`.htaccess` / nginx fast-path and a managed-block marker convention. The drop-in,
the cacheability exclusion set, the disk purge, and the preloader are WPMgr's own
implementation under WPMgr's naming and architecture.

### matthiasmullie/minify (MIT)

CSS and JS minification uses
[`matthiasmullie/minify`](https://github.com/matthiasmullie/minify) (^1.3, MIT), a
small pure-PHP library bundled with the agent's Composer dependencies.

### matthiasmullie/path-converter (MIT)

[`matthiasmullie/path-converter`](https://github.com/matthiasmullie/path-converter)
(~1.1, MIT) is a Composer dependency of `matthiasmullie/minify` above (relative
path rewriting for minified CSS `url()` references), installed transitively into
the agent's vendor tree. Copyright (c) 2015 Matthias Mullie.

### Remove Unused CSS (RUCSS)

WPMgr's RUCSS engine is a pure-Go implementation on the control plane. There is no
headless browser and no third-party RUCSS service. It is built on these
open-source Go libraries:

- [`golang.org/x/net/html`](https://pkg.go.dev/golang.org/x/net/html) (BSD-3-Clause)
  — parse the page HTML into a DOM.
- [`github.com/tdewolff/parse`](https://github.com/tdewolff/parse) (MIT) — tokenize
  each stylesheet.
- [`github.com/andybalholm/cascadia`](https://github.com/andybalholm/cascadia)
  (BSD-2-Clause) — compile CSS selectors and test them against the parsed DOM.

### web-vitals (Google, Apache-2.0)

Real User Monitoring's Core Web Vitals collection bundles
[`web-vitals`](https://github.com/GoogleChrome/web-vitals) (^5.3.0,
Apache-2.0), Copyright 2020 Google LLC, via an esbuild build in
`apps/tracker`. The bundle ships inside the agent plugin zip as
`assets/wpmgr-rum.js` / `assets/wpmgr-rum.min.js`. Source and license:
https://github.com/GoogleChrome/web-vitals/blob/main/LICENSE

## Security

### bjeavons/zxcvbn-php (MIT)

The agent's password-strength policy uses
[`bjeavons/zxcvbn-php`](https://github.com/bjeavons/zxcvbn-php) (^1.4, MIT), a
pure-PHP port of zxcvbn, bundled with the agent's Composer dependencies.
Copyright (c) 2013-2014 Benjamin Jeavons.

### symfony/polyfill-mbstring (MIT)

[`symfony/polyfill-mbstring`](https://github.com/symfony/polyfill-mbstring)
(>=1.3.1, MIT) is a Composer dependency of `bjeavons/zxcvbn-php` above (mbstring
functions on PHP builds without the extension), installed transitively into the
agent's vendor tree. Copyright (c) 2015-present Fabien Potencier and
contributors.

## Media Optimizer

The Media Optimizer's attribution (including Discord's `lilliput`, MIT, for
encoding) is recorded in [`apps/agent/NOTICE.md`](apps/agent/NOTICE.md).
