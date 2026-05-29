# Product

## Register

product

## Users

Agency operators who manage between 5 and 1,000 WordPress sites per workspace. They are technical, fast readers, keyboard-driven, and spend hours per day inside the dashboard. Their context is operational, not exploratory: they arrive with a job to finish (push updates across a fleet, restore a downed site, audit a vulnerability sweep, log into wp-admin without hunting credentials) and they evaluate the UI by how little it slows them down. They are fluent in tools like Linear, Sentry, Cloudflare, and Datadog and bring those expectations with them.

## Product Purpose

WPMgr is the operating console for agencies running WordPress at scale. It exists to make fleet operations safe and fast: bulk updates, bulk backups, restoring a single site under pressure, one-click login into wp-admin, scanning vulnerability lists, and reading uptime charts. Success looks like an operator finishing a high-stakes task in fewer keystrokes than the alternatives, with zero ambiguity about state, and never being surprised by a UI that decorates instead of informs.

### Primary tasks

- Operating at scale: bulk updates across many sites at once.
- Bulk backups and verifying their integrity.
- Restoring a single site under pressure, with clear progress and rollback.
- One-click login into wp-admin without hunting credentials.
- Scanning vulnerability lists and triaging what matters.
- Reading uptime charts and spotting anomalies fast.

## Brand Personality

Calm, clinical, operator-grade. The voice is the voice of an instrument panel, not a brochure. No hype, no marketing tone, no "welcome." Information density is preferred over decoration. The interface should feel like it was built by people who use it every day, for people who use it every day. Trust comes from precision and restraint, not enthusiasm.

Three words: calm, clinical, operator-grade.

## References (good)

Specific things to learn from, not styles to copy wholesale:

- **Linear**: keyboard-first flow, dense lists that stay legible, restrained accent use, the way state lives in type weight and color rather than in chrome.
- **Sentry**: operational color semantics (error/warn/ok read instantly), the discipline of showing real data in primary views without decoration.
- **Plausible**: how little chrome a useful dashboard actually needs; numbers and time series first, no hero metric theatre.
- **Cloudflare dashboard**: scale-aware tables, status pills used sparingly, navigation that survives going six levels deep into a single site.
- **Datadog (the clean views)**: time-series rigor, well-tuned grid lines, the way related signals sit on one canvas without competing.
- **Resend**: type and spacing discipline in a product UI; an accent that earns its presence by appearing rarely.

## Anti-references

What this must explicitly NOT look like:

- **Vercel marketing**: hero-led, gradient-saturated, motion-as-decoration. WPMgr is not a marketing page.
- **Stripe homepage**: editorial brand surface dressed as product. We are the inverse.
- **Wix and Squarespace**: consumer-soft, persuasion-led, hand-holding tone.
- **"Welcome to our platform" SaaS dashboards**: greeting cards, empty hero metrics, onboarding theatre, encouragement copy.
- Anything featuring purple gradients, glassmorphism, hero metric layouts, italic-serif heroes, gradient text, or nested cards.
- Body type set in Inter or Geist; these are signatures of the aesthetic we are rejecting.

## Design Principles

1. **The instrument disappears.** Operators are mid-task. The UI's job is to remove friction, not to be noticed. Familiarity is a feature; surprise is a tax.
2. **Information density over decoration.** Every pixel of chrome competes with the data that actually matters. If a border, gradient, or animation does not carry meaning, it is removed.
3. **State is the design.** Hover, focus, active, selected, loading, error, warning, success, info, disabled: these are the visual vocabulary. The aesthetic emerges from getting state right, not from adding flourish on top.
4. **Earn the accent.** Color is restrained by default. The accent appears for primary actions, current selection, and state, never as decoration. Loud color in a calm interface is a signal, not a style.
5. **Keyboard-first, dark-first, calm-first.** Operators live here for hours. The default mode (dark) and the default input (keyboard) shape every decision. Reduced motion is the floor, not an accommodation.

## Accessibility & Inclusion

- **WCAG AA is mandatory** on every text element, including all styled `<button>` and `<a>` elements. No exceptions for "subtle" labels, secondary text, or disabled states that still need to be readable.
- **Keyboard-first**: every interactive surface must be reachable, operable, and have a visible focus indicator that meets contrast requirements. Tab order is designed, not inherited.
- **Dark mode is the default** for most operators. Light mode is supported and held to the same contrast and density bar; it is not a second-class theme.
- **Reduced motion is respected globally.** `prefers-reduced-motion` disables non-essential transitions everywhere, by default, without per-component opt-in.
- **Internationalization**: English first, but every UI string must tolerate German (+30%) and French (+20%) length expansion without truncation or layout collapse. No hard-coded widths on text containers. Buttons, table headers, nav items, and form labels are tested at expanded lengths.

---

## Non-negotiable WPMgr constraints

These are project-wide bans that override register defaults. They apply everywhere and cannot be relaxed per surface.

### Banned fonts (never used, never imported)

Inter, Roboto, Geist, Plus Jakarta Sans, Fraunces, Recoleta, Space Grotesk, Instrument Sans, Mona Sans, Newsreader, Playfair, Cormorant, Tiempos.

### Banned visual patterns

- Purple-to-blue or cyan-on-dark gradients.
- Cyan-glow box-shadows.
- Gradient text, anywhere.
- Side-tab accent borders on rounded cards.
- Nested cards.
- Pill buttons.
- Bounce or elastic easing.
- Animating `width`, `height`, `top`, `left`, `margin`, or `padding`.
- Pure `#000` or pure `#fff`.

### Banned copy

- Em dashes in any UI string.
- "Welcome to WPMgr".
- "Introducing".
- "powerful".
- "seamless".
- "blazing fast".
- "your one-stop".
- "Manage your WordPress sites".
- "OK", "Submit", or "Cancel" as button labels. Buttons are always verb-first and specific to the action (e.g. "Restore site", "Run backup", "Open wp-admin").
