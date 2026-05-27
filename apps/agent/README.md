# apps/agent — WPMgr WordPress agent (PHP 8.0+)

MIT-licensed WordPress plugin installed on managed sites. Communicates with the
control plane over Ed25519-signed REST requests under `wpmgr/v1`. Entrypoint:
`wpmgr-agent.php`. Classes autoload from `includes/` (PSR-4 `WPMgr\Agent\`).

```bash
composer install
composer test
```
