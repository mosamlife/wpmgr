# apps/agent — WPMgr WordPress agent (PHP 8.0+)

MIT-licensed WordPress plugin installed on managed sites. Communicates with the
control plane over Ed25519-signed REST requests under `wpmgr/v1`. Entrypoint:
`wpmgr-agent.php`. Classes autoload from `includes/` (PSR-4 `WPMgr\Agent\`).

```bash
composer install
composer test
```

## Option namespace contract

Version 1. In force from the first agent release whose changelog names it.

The agent keeps everything that connects a WordPress install to a WPMgr
dashboard in one namespace in the database: rows whose names begin with
`wpmgr_agent_`. That covers the keys the agent signs requests and decrypts
backups with, its enrollment (site, tenant and dashboard address), and its
encrypted email-delivery secrets. Scripts can rely on the following.

| Table | Row names in the namespace |
|---|---|
| Options table (`wp_options`, or the site's own options table) | `wpmgr_agent_*`, `_transient_wpmgr_agent_*`, `_transient_timeout_wpmgr_agent_*`, `_site_transient_wpmgr_agent_*`, `_site_transient_timeout_wpmgr_agent_*` |
| Network table on multisite (`wp_sitemeta`, column `meta_key`, for the network's `site_id`) | `wpmgr_agent_*`, `_site_transient_wpmgr_agent_*`, `_site_transient_timeout_wpmgr_agent_*` |

- **Uninstall removes every row in the namespace**, including names this page
  does not list and rows written by other agent releases.
- **To reset an install to never connected, remove every row in the namespace,
  in every form above, in one operation.** Never remove a subset: a copied
  site keeps the original site's keys until all of them are gone.
- **Disconnect and Re-enroll in wp-admin are not a reset.** Both keep the
  site's backup key so that older backups stay restorable.
- **Match each form as an anchored prefix, and escape `_`**, which SQL `LIKE`
  treats as "any one character". A substring match such as
  `LIKE '%wpmgr_agent_%'` also catches other rows that merely contain the
  text. For a single site with the default table prefix:

  ```sql
  SELECT option_name FROM wp_options
  WHERE option_name LIKE 'wpmgr\_agent\_%'
     OR option_name LIKE '\_transient\_wpmgr\_agent\_%'
     OR option_name LIKE '\_transient\_timeout\_wpmgr\_agent\_%'
     OR option_name LIKE '\_site\_transient\_wpmgr\_agent\_%'
     OR option_name LIKE '\_site\_transient\_timeout\_wpmgr\_agent\_%';
  ```

- With a persistent object cache, transients live in the cache rather than
  the database and expire on their own. None of them holds the site's identity.

Outside the contract: options and transients whose names begin with `wpmgr_`
but not `wpmgr_agent_` hold feature settings and caches, and their names can
change between releases. Database tables and files the agent creates are not
covered either.

Changing the prefix, the forms above, or what the namespace holds is a
breaking change: it bumps the contract version and is announced in the
changelog.
