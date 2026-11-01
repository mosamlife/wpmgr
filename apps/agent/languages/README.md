# Translations

The agent loads its translations from this folder on every request, on the
`init` action, for the self-hosted and managed builds. The text domain is
`wpmgr-agent`.

Put compiled catalogues here, named `wpmgr-agent-<locale>.mo`, for example
`wpmgr-agent-de_DE.mo` and `wpmgr-agent-de_DE_formal.mo`. The matching
`wpmgr-agent-<locale>.po` source files and a `wpmgr-agent.pot` template belong
next to them.

A catalogue under `wp-content/languages/plugins/` takes precedence over one in
this folder, so a translation that survives plugin updates can live there
instead.

The wordpress.org build does not read this folder. WordPress downloads the
language packs for the plugin's wordpress.org slug and loads them itself.
