---
paths:
  - "apps/agent/**/*.php"
---

# WordPress agent plugin

This plugin runs on strangers' servers, on hosts you cannot inspect, under
SAPIs you did not choose. Assume every environment assumption is wrong.

## Check what core does before guarding what core does not guard

A SAPI gate added here refused an operation WordPress core performs daily, and
broke self-update for the whole fleet. Read core's implementation first.

**A fix to the apply path can never be delivered by that apply path.** If the
bug is in the updater, the updater cannot ship its own fix.

## Never fall back to an empty base path

`WP_CONTENT_DIR ?? ''` writes at the filesystem root. This shipped once. Any
`?? ''` on a path is a bug; fail loudly instead.

## The autoloader must self-resolve every symbol

Source and clone installs have no Composer `vendor/`. Odd-filename classes must
resolve with `vendor/` absent.

## Do not depend on the shared request environment

- A third-party plugin's global JWT filter will hijack `Authorization: Bearer`
  and fatal the command token. Relocate off the shared header at include time.
- Roots/Bedrock `wp-config` raw defines fatal on naive parsing.
- `fastcgi_finish_request` is absent on OpenLiteSpeed; branch through
  `litespeed_finish_request` to a fallback.
- Host-detection filesystem probes are `@`-suppressed. Always.

## Tests

WP stubs go in `tests/wp-stubs.php` so Patchwork can transform them, **never**
in `bootstrap.php`. The `ABSPATH` define in bootstrap is load-bearing; its
absence silently killed CI at test #87 with exit 0.

Patchwork redefinitions leak across tests. Keep fakes self-contained.

## Exclusion matching is anchored

`strpos` on a path is not an exclusion check. An unanchored `db.php` exclude
silently dropped every `*db.php` plugin file during restore and reported the
fatal site as Completed. Match exact path, path segment, or anchored prefix.
