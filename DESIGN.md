---
version: alpha
name: WPMgr: The Ops Console
description: Calm, dense, professional surface for agencies operating dozens to thousands of WordPress sites. Product register. Design serves the task.
colors:
  background: "#FBFCFC"
  foreground: "#1B1F20"
  card: "#FFFFFF"
  muted: "#F1F4F4"
  muted-foreground: "#6C7678"
  border: "#E1E6E6"
  primary: "#1B6F77"
  primary-foreground: "#FBFCFC"
  success: "#27905F"
  warning: "#C58A1E"
  destructive: "#C13B2B"
  info: "#3D6FB8"
  severity-critical: "#8C2418"
  severity-high: "#C13B2B"
  severity-medium: "#C58A1E"
  severity-low: "#3D6FB8"
  chart-1: "#1B6F77"
  chart-2: "#27905F"
  chart-3: "#C58A1E"
  chart-4: "#A04C8A"
  chart-5: "#3D6FB8"
typography:
  display:
    fontFamily: "IBM Plex Sans"
    fontSize: 36px
    fontWeight: 600
    lineHeight: 1.1
    letterSpacing: -0.01em
  h1:
    fontFamily: "IBM Plex Sans"
    fontSize: 28px
    fontWeight: 600
    lineHeight: 1.2
  h2:
    fontFamily: "IBM Plex Sans"
    fontSize: 22px
    fontWeight: 600
    lineHeight: 1.3
  body:
    fontFamily: "IBM Plex Sans"
    fontSize: 16px
    fontWeight: 400
    lineHeight: 1.5
  body-sm:
    fontFamily: "IBM Plex Sans"
    fontSize: 14px
    fontWeight: 400
    lineHeight: 1.4
    fontFeature: '"tnum" 1'
  caption:
    fontFamily: "IBM Plex Sans"
    fontSize: 12px
    fontWeight: 500
    lineHeight: 1.3
    letterSpacing: 0.02em
  mono:
    fontFamily: "IBM Plex Mono"
    fontSize: 14px
    fontWeight: 400
    lineHeight: 1.4
    fontFeature: '"zero" 1'
rounded:
  none: 0px
  sm: 4px
  md: 6px
  lg: 8px
  xl: 12px
  full: 9999px
spacing:
  xs: 4px
  sm: 8px
  md: 12px
  lg: 16px
  xl: 24px
  2xl: 32px
  3xl: 48px
  4xl: 64px
  5xl: 96px
components:
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.primary-foreground}"
    typography: "{typography.body-sm}"
    rounded: "{rounded.md}"
    padding: "8px 14px"
  button-secondary:
    backgroundColor: "transparent"
    textColor: "{colors.foreground}"
    typography: "{typography.body-sm}"
    rounded: "{rounded.md}"
    padding: "8px 14px"
  button-destructive:
    backgroundColor: "{colors.destructive}"
    textColor: "#FBFCFC"
    typography: "{typography.body-sm}"
    rounded: "{rounded.md}"
    padding: "8px 14px"
  input:
    backgroundColor: "{colors.background}"
    textColor: "{colors.foreground}"
    typography: "{typography.body-sm}"
    rounded: "{rounded.md}"
    padding: "8px 12px"
  card:
    backgroundColor: "{colors.card}"
    textColor: "{colors.foreground}"
    rounded: "{rounded.lg}"
    padding: "24px"
  row:
    backgroundColor: "{colors.background}"
    textColor: "{colors.foreground}"
    typography: "{typography.body-sm}"
    padding: "10px 16px"
  row-hover:
    backgroundColor: "{colors.muted}"
  badge-success:
    backgroundColor: "#E6F4ED"
    textColor: "#1A6E48"
    typography: "{typography.caption}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  badge-warning:
    backgroundColor: "#FBF1DC"
    textColor: "#7A5612"
    typography: "{typography.caption}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  badge-destructive:
    backgroundColor: "#F7E1DD"
    textColor: "#8B2A1F"
    typography: "{typography.caption}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  badge-info:
    backgroundColor: "#E2EAF6"
    textColor: "#274A7B"
    typography: "{typography.caption}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
---

## Overview

The Ops Console. A calm, dense, professional surface for agencies operating dozens to thousands of WordPress sites. Reads like Linear, Sentry, and Plausible, not like Vercel, Stripe, or a SaaS landing page. Design serves the operator, who stares at this for hours. Nothing competes with their data for attention.

Hex values in YAML front matter are sRGB equivalents; the authoritative source is the OKLCH definitions in globals.css. DTCG export tools should round-trip from OKLCH for color-managed displays.

## Colors

A restrained strategy: tinted neutrals + one committed primary + four semantic colors. No purple gradients, no cyan-on-dark glows, no second non-semantic accent.

- Primary, Deep Restrained Teal `oklch(58% 0.12 195)`. Distinct from SaaS blue (250) and from WordPress blue (245). Used only for the single most important action per surface.
- Neutrals tinted toward 195 at chroma 0.008. Subconscious cohesion; no pure gray.
- Semantic at matched lightness 58 to 70 percent, distinct hues:
  - Success (sites up, backups OK) `oklch(58% 0.14 155)`
  - Warning (updates, recoverable issues) `oklch(70% 0.14 75)`
  - Destructive (down, vulnerabilities) `oklch(58% 0.20 25)`
  - Info (neutral notifications) `oklch(58% 0.10 235)`
- Vulnerability severity is a discrete 4-step scale, never a continuous gradient.
- Chart palette: 5 hues at matched lightness so no series visually dominates.
- Dark mode: lighter surfaces, not shadows, create depth. Background 15 to Card 19 to Popover 22 percent lightness.

## Typography

- IBM Plex Sans for UI body and headings. Weights 400, 500, 600, 700. Chosen because not on the flagged monoculture, excellent tabular numerals, ClearType hinting, ships matching mono.
- IBM Plex Mono for versions, hostnames, paths, CVE IDs, hashes.
- Fixed type scale at ratio 1.25: 11/12/14/16/18/22/28/36px. No fluid clamp.
- Body text 16px minimum. Table rows 14px is the workhorse.
- `font-variant-numeric: tabular-nums` on every column-bound number.
- Headings get `text-wrap: balance`; prose gets `text-wrap: pretty`.
- All-caps labels (column headers, captions) take +0.02em letter-spacing.

## Layout

4pt base spacing scale: 4 / 8 / 12 / 16 / 24 / 32 / 48 / 64 / 96 / 128. Never invent off-scale values.

- App shell: 240px sidebar + 48px topbar + content area (24px x 32px padding).
- Content: max-width unset; inside, 12-column grid at >=1024px.
- Density modes: Comfortable / Compact / Dense, persisted per user. Compact default.
- Tables virtualized (TanStack + react-virtuoso) at >100 rows. Sticky header, sticky checkbox + URL column.
- Breakpoints: 640 / 768 / 1024. Adapt, do not amputate.
- 65 to 75ch max measure for any prose block.

## Elevation & Depth

Borders over shadows. Default separator is 1px `--border`. Shadows reserved for things that literally float.

- `xs`: 0 1px 0 border, sticky table header on scroll
- `sm`: soft 1px row-hover lift, light mode only
- `md`: popover, dropdown, command palette
- `lg`: modal, drawer
- Dark mode: depth from lighter surfaces, shadows nearly invisible
- Never nest cards

## Shapes

- Default radius 6px for inputs, buttons, badges
- Cards 8px. Modals 12px. Avatars full.
- No pill buttons. Buttons are rectangles with 6px radius.
- Icons from lucide-react at 16/20/24px. Never emoji.

## Components

- Button: 8px vertical, 14px horizontal, 6px radius, weight 600, no shadow, no gradient. Hover darkens 4%L; active 8%L. Loading replaces label with inline spinner.
- Input: 36px height, 1px border, 6px radius, focus-visible ring 2px + 2px offset.
- Badge: 12px caption, 0.02em tracking, 2/8 padding, 4px radius.
- Card: card background, 1px border, 8px radius, 24px padding. Never nested.
- Table row: 10/16 padding in Compact; hover muted; selected muted + 2px left ring primary. No striping.
- Sidebar item: 8/12 padding, 6px radius, active = accent + 2px left ring primary. Counts mono right-aligned.
- Toast (Sonner): popover bg, border, shadow-md, 5s/8s auto-dismiss, always a verb action link.
- Modal: popover, 12px radius, shadow-lg, max 480px. Title is the action. Destructive requires typing the resource name.
- Status dot: 8px filled circle in the semantic color. Always paired with a label or time.
- Chip: 4px radius, 2/8 padding, never pill-shaped.
- Sparkline: chart-1 stroke 1.5px, no fill, only bound to real series.

## Do's and Don'ts

Do:
- Use OKLCH for every color value. Tint every neutral toward the primary hue at chroma 0.008.
- Use mono for every version string, hostname, path, hash, and CVE ID.
- Use `tabular-nums` on every column-bound number.
- Show counts in every bulk action ("Update plugins on 47 sites").
- Use verb-first button labels. Never OK or Submit.
- Provide a reduced-motion fallback for every animation.
- Show status as dot + label + time, never as a colored dot alone.
- Use borders for separation; reserve shadows for floating elements.
- Maintain WCAG AA on every text element including styled `<button>` and `<a>`.

Don't:
- Don't use Inter, Roboto, Fraunces, Geist, Plus Jakarta Sans, Space Grotesk, Recoleta, Instrument Sans, Mona Sans.
- Don't use purple-to-blue or cyan-on-dark gradients.
- Don't use gradient text. Don't use gradient anything for emphasis.
- Don't nest cards. Don't wrap every section in a card.
- Don't use side-tab accent borders on rounded cards.
- Don't use pill buttons.
- Don't use bounce or elastic easing.
- Don't animate width, height, padding, margin, top, or left.
- Don't use pure #000 or pure #FFF.
- Don't use modals as a reflex, use side panels or pages when content scrolls.
- Don't use em dashes in UI strings.
- Don't write "Welcome to WPMgr" or "Introducing" anywhere.
- Don't hide functionality on mobile. Adapt, don't amputate.
- Don't write generic errors. Always: what, why, how.
