# NOTICE — WPMgr WordPress agent

The WPMgr agent is MIT-licensed (see the repository `LICENSE`).

## Third-party attribution

### FlyingPress (orchestration patterns — inspiration only)

The Media Optimizer's image-optimization **orchestration patterns** are inspired
by patterns observed in **FlyingPress 5.4.5** (FlyingWeb). Specifically:

- the **original-file rename pattern** (archiving a re-compressed original to a
  double-extension name and reversing it on restore),
- the **postmeta blob shape** (a per-attachment record holding optimization
  status, per-size optimized/unoptimized maps, and a verbatim pre-optimization
  `_wp_attachment_metadata` snapshot as the restore anchor), and
- the **Accept-header `.htaccess` fallback** (serving a legacy twin when the
  client does not advertise AVIF/WebP support and the twin exists on disk).

**No FlyingPress source code is included or copied** into WPMgr. These are
re-implementations of the *patterns* under WPMgr's own naming and architecture.

The actual image optimization runs on **WPMgr's own Go control plane** using
Discord's **`lilliput`** (MIT) — not a third-party or managed optimization API.

### lilliput (Discord)

`github.com/discord/lilliput` is MIT-licensed and used by the control-plane
`media-encoder` service for image decoding/encoding. It is not bundled with this
agent plugin (the agent performs no encoding).
