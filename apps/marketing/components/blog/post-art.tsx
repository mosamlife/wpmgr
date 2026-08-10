/**
 * Featured art for blog posts.
 *
 * WHY DRAWN IN CODE RATHER THAN COMMISSIONED OR PHOTOGRAPHED. Stock photography
 * of "a person at a laptop" says nothing about file integrity monitoring, and a
 * sketchy hand-drawn scene reads as amateurish next to a product that sells
 * operational seriousness. What is left is the thing good developer-tool blogs
 * actually do: systematic, geometric art that encodes the subject of the piece.
 * The scanner illustration IS a grid of plugins with one flagged. The vitals
 * illustration IS three thresholds with a marker on each.
 *
 * Drawing them in code buys three things a static asset cannot. They theme
 * themselves, because every colour is a design token and dark mode is already
 * solved. They cost nothing to ship, because there is no binary in the repo and
 * no image request on the page. And they cannot go stale against a rebrand,
 * because they read the same variables the rest of the site does.
 *
 * RULES FOR ADDING ONE.
 *
 *   No text. Labels would need translating, would overflow at card size, and
 *   would duplicate the title sitting directly beneath the art anyway.
 *
 *   No animation. These are featured images. Ten of them render at once on the
 *   index, and a reveal that has not fired is a card that looks broken. Every
 *   piece here is complete at first paint, with no script and no observer.
 *
 *   Decorative only. Each is aria-hidden: the post title carries the meaning,
 *   and announcing "diagram of a grid" to a screen reader adds noise, not
 *   information.
 *
 *   Geometry, not depiction. Rectangles, circles, lines and arcs on a 400x225
 *   field. If a piece needs a recognisable real-world object to work, it is the
 *   wrong idea for this system.
 */

import type { BlogCategory } from "@/lib/content/blog";

const W = 400;
const H = 225;

/**
 * Shared field: the tinted ground every piece is drawn on.
 *
 * Deliberately square-cornered and unbordered. Rounding and framing belong to
 * whatever is placing the art: a card clips it with overflow-hidden so the art
 * runs edge to edge under the top corners, and the post hero rounds and borders
 * its own wrapper. Drawing a rounded, stroked panel in here instead put a
 * rounded box inside a rounded card, which reads as a nested card.
 */
function Field() {
  return <rect x="0" y="0" width={W} height={H} fill="var(--primary-subtle)" />;
}

const ART_PROPS = {
  viewBox: `0 0 ${W} ${H}`,
  xmlns: "http://www.w3.org/2000/svg",
  "aria-hidden": true,
  focusable: "false",
  preserveAspectRatio: "xMidYMid meet",
  className: "h-full w-full",
} as const;

/* ------------------------------------------------------------------ *
 * Security                                                            *
 * ------------------------------------------------------------------ */

/** Hardened login: many attempts, three barriers, one key through. */
function GateArt() {
  const attempts = [40, 68, 96, 124, 152];
  return (
    <svg {...ART_PROPS}>
      <Field />
      {/* Attempts arriving from the left. All but one stop at the first wall. */}
      {attempts.map((y, i) => (
        <g key={y}>
          <circle cx={i % 2 === 0 ? 30 : 44} cy={y} r="4" fill="var(--muted-foreground)" opacity="0.45" />
          <line
            x1={i % 2 === 0 ? 38 : 52}
            y1={y}
            x2="104"
            y2={y}
            stroke="var(--muted-foreground)"
            strokeWidth="1.5"
            strokeDasharray="3 4"
            opacity="0.4"
          />
        </g>
      ))}

      {/* Three barriers, increasing in solidity left to right. */}
      {[
        { x: 112, o: 0.35 },
        { x: 152, o: 0.6 },
        { x: 192, o: 1 },
      ].map((b) => (
        <rect
          key={b.x}
          x={b.x}
          y="38"
          width="10"
          height={H - 76}
          rx="5"
          fill="var(--primary)"
          opacity={b.o}
        />
      ))}

      {/* The one credential that passes, drawn as a key travelling through. */}
      <line
        x1="104"
        y1="112"
        x2="286"
        y2="112"
        stroke="var(--primary)"
        strokeWidth="2.5"
        strokeLinecap="round"
      />
      <circle cx="300" cy="112" r="18" fill="none" stroke="var(--primary)" strokeWidth="2.5" />
      <circle cx="300" cy="112" r="6" fill="var(--primary)" />
      <rect x="316" y="108" width="30" height="8" rx="4" fill="var(--primary)" />
      <rect x="336" y="116" width="6" height="10" rx="3" fill="var(--primary)" />
    </svg>
  );
}

/** File integrity: rows of hashes, one of them diverged. */
function IntegrityArt() {
  const rows = [50, 82, 114, 146, 178];
  const drifted = 114;
  return (
    <svg {...ART_PROPS}>
      <Field />
      {rows.map((y) => {
        const bad = y === drifted;
        const tint = bad ? "var(--warning-subtle-fg)" : "var(--primary)";
        return (
          <g key={y}>
            {/* file glyph */}
            <rect
              x="34"
              y={y - 11}
              width="18"
              height="22"
              rx="3"
              fill="none"
              stroke={bad ? tint : "var(--muted-foreground)"}
              strokeWidth="1.6"
              opacity={bad ? 1 : 0.65}
            />
            {/* hash segments */}
            {[0, 1, 2, 3, 4, 5, 6, 7].map((i) => (
              <rect
                key={i}
                x={68 + i * 26}
                y={y - 5}
                width={18}
                height="10"
                rx="2"
                fill={bad && i === 5 ? tint : "var(--primary)"}
                opacity={bad ? (i === 5 ? 1 : 0.25) : 0.55}
              />
            ))}
            {/* verdict */}
            {bad ? (
              <g stroke={tint} strokeWidth="2.2" strokeLinecap="round">
                <line x1="288" y1={y - 5} x2="298" y2={y + 5} />
                <line x1="298" y1={y - 5} x2="288" y2={y + 5} />
              </g>
            ) : (
              <path
                d={`M287 ${y} l4 5 l8 -10`}
                fill="none"
                stroke="var(--success)"
                strokeWidth="2.2"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            )}
          </g>
        );
      })}
    </svg>
  );
}

/** Vulnerability scanning: a grid of installed things, one matched. */
function ScannerArt() {
  const cols = 6;
  const rows = 4;
  const flagged = 15;
  return (
    <svg {...ART_PROPS}>
      <Field />
      {Array.from({ length: cols * rows }, (_, i) => {
        const cx = 46 + (i % cols) * 52;
        const cy = 44 + Math.floor(i / cols) * 46;
        const hit = i === flagged;
        return (
          <rect
            key={i}
            x={cx}
            y={cy}
            width="38"
            height="32"
            rx="5"
            fill={hit ? "var(--warning-subtle)" : "var(--card)"}
            stroke={hit ? "var(--warning-subtle-fg)" : "var(--border)"}
            strokeWidth={hit ? 2 : 1.2}
          />
        );
      })}
      {/* the match, marked rather than animated */}
      <circle
        cx={46 + (flagged % cols) * 52 + 19}
        cy={44 + Math.floor(flagged / cols) * 46 + 16}
        r="21"
        fill="none"
        stroke="var(--warning-subtle-fg)"
        strokeWidth="1.5"
        opacity="0.55"
      />
      {/* sweep, drawn at rest partway across */}
      <line
        x1="228"
        y1="26"
        x2="228"
        y2={H - 26}
        stroke="var(--primary)"
        strokeWidth="2"
        opacity="0.75"
      />
      <circle cx="228" cy="26" r="3.5" fill="var(--primary)" />
    </svg>
  );
}

/* ------------------------------------------------------------------ *
 * Performance                                                         *
 * ------------------------------------------------------------------ */

/** Core Web Vitals: three metrics against their thresholds. */
function VitalsArt() {
  const tracks = [
    { y: 62, pos: 0.3 },
    { y: 112, pos: 0.52 },
    { y: 162, pos: 0.78 },
  ];
  const x0 = 46;
  const x1 = 354;
  const span = x1 - x0;
  return (
    <svg {...ART_PROPS}>
      <Field />
      {tracks.map((t) => {
        const marker = x0 + span * t.pos;
        return (
          <g key={t.y}>
            {/* good / needs work / poor bands */}
            <rect x={x0} y={t.y - 6} width={span * 0.5} height="12" rx="6" fill="var(--success)" opacity="0.32" />
            <rect x={x0 + span * 0.5} y={t.y - 6} width={span * 0.25} height="12" fill="var(--warning-subtle-fg)" opacity="0.28" />
            <rect x={x0 + span * 0.75} y={t.y - 6} width={span * 0.25} height="12" rx="6" fill="var(--muted-foreground)" opacity="0.25" />
            {/* threshold ticks */}
            <line x1={x0 + span * 0.5} y1={t.y - 14} x2={x0 + span * 0.5} y2={t.y + 14} stroke="var(--border)" strokeWidth="1.5" />
            <line x1={x0 + span * 0.75} y1={t.y - 12} x2={x0 + span * 0.75} y2={t.y + 12} stroke="var(--border)" strokeWidth="1.5" />
            {/* measured value */}
            <circle cx={marker} cy={t.y} r="8" fill="var(--card)" stroke="var(--primary)" strokeWidth="3" />
          </g>
        );
      })}
    </svg>
  );
}

/** Image optimization: the same picture, three encodings, descending weight. */
function CompressArt() {
  const bars = [
    { x: 60, h: 118, o: 0.3 },
    { x: 158, h: 74, o: 0.6 },
    { x: 256, h: 42, o: 1 },
  ];
  const base = 182;
  return (
    <svg {...ART_PROPS}>
      <Field />
      <line x1="40" y1={base} x2="360" y2={base} stroke="var(--border)" strokeWidth="1.5" />
      {bars.map((b) => (
        <g key={b.x}>
          <rect
            x={b.x}
            y={base - b.h}
            width="84"
            height={b.h}
            rx="6"
            fill="var(--primary)"
            opacity={b.o}
          />
          {/* a picture glyph riding the top of each bar */}
          <rect
            x={b.x + 22}
            y={base - b.h - 26}
            width="40"
            height="20"
            rx="3"
            fill="none"
            stroke="var(--primary)"
            strokeWidth="1.6"
            opacity="0.8"
          />
          <circle cx={b.x + 33} cy={base - b.h - 18} r="3" fill="var(--primary)" opacity="0.8" />
          <path
            d={`M${b.x + 27} ${base - b.h - 10} l9 -8 l7 6 l5 -4 l6 6 z`}
            fill="var(--primary)"
            opacity="0.8"
          />
        </g>
      ))}
      {/* the saving, as a descending step line */}
      <path
        d={`M60 ${base - 118 - 34} H144 V${base - 74 - 34} H242 V${base - 42 - 34} H340`}
        fill="none"
        stroke="var(--success)"
        strokeWidth="2.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        strokeDasharray="1 0"
        opacity="0.9"
      />
    </svg>
  );
}

/** Object cache: the short path to memory beside the long one to the database. */
function CacheArt() {
  return (
    <svg {...ART_PROPS}>
      <Field />
      {/* request origin */}
      <rect x="30" y="94" width="46" height="38" rx="6" fill="var(--card)" stroke="var(--border)" strokeWidth="1.4" />
      <line x1="40" y1="106" x2="66" y2="106" stroke="var(--muted-foreground)" strokeWidth="2" strokeLinecap="round" />
      <line x1="40" y1="113" x2="60" y2="113" stroke="var(--muted-foreground)" strokeWidth="2" strokeLinecap="round" opacity="0.6" />
      <line x1="40" y1="120" x2="63" y2="120" stroke="var(--muted-foreground)" strokeWidth="2" strokeLinecap="round" opacity="0.6" />

      {/* hit: short lane up to memory */}
      <path d="M76 106 C 120 106, 130 62, 176 62" fill="none" stroke="var(--primary)" strokeWidth="2.5" strokeLinecap="round" />
      <rect x="182" y="42" width="74" height="40" rx="7" fill="var(--primary)" opacity="0.16" />
      <rect x="182.75" y="42.75" width="72.5" height="38.5" rx="6.25" fill="none" stroke="var(--primary)" strokeWidth="1.5" />
      {[0, 1, 2, 3].map((i) => (
        <rect key={i} x={192 + i * 16} y="54" width="9" height="17" rx="2" fill="var(--primary)" opacity="0.85" />
      ))}
      <path d="M256 62 C 300 62, 306 100, 340 100" fill="none" stroke="var(--primary)" strokeWidth="2.5" strokeLinecap="round" />
      <path d="M334 94 l8 6 l-8 6" fill="none" stroke="var(--primary)" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" />

      {/* miss: long lane down to the database */}
      <path
        d="M76 122 C 120 122, 130 172, 176 172"
        fill="none"
        stroke="var(--muted-foreground)"
        strokeWidth="2"
        strokeLinecap="round"
        strokeDasharray="5 5"
        opacity="0.55"
      />
      <g opacity="0.55" stroke="var(--muted-foreground)" strokeWidth="1.6" fill="none">
        <ellipse cx="219" cy="154" rx="34" ry="10" />
        <path d="M185 154 v30 c0 5.5 15.2 10 34 10 s34 -4.5 34 -10 v-30" />
        <path d="M185 170 c0 5.5 15.2 10 34 10 s34 -4.5 34 -10" />
      </g>
      <path
        d="M253 172 C 300 172, 306 130, 340 130"
        fill="none"
        stroke="var(--muted-foreground)"
        strokeWidth="2"
        strokeLinecap="round"
        strokeDasharray="5 5"
        opacity="0.55"
      />
    </svg>
  );
}

/* ------------------------------------------------------------------ *
 * Backups                                                             *
 * ------------------------------------------------------------------ */

/** Backup strategy: full plus increments, receding across a retention window. */
function SnapshotsArt() {
  const slabs = [0, 1, 2, 3, 4, 5];
  const base = 158;
  return (
    <svg {...ART_PROPS}>
      <Field />
      {slabs.map((i) => {
        const full = i === 0 || i === 3;
        const x = 44 + i * 52;
        const h = full ? 74 : 34;
        return (
          <g key={i}>
            <rect
              x={x}
              y={base - h}
              width="38"
              height={h}
              rx="5"
              fill="var(--primary)"
              opacity={full ? 0.9 : 0.4}
            />
            {full && (
              <rect x={x} y={base - h} width="38" height="8" rx="4" fill="var(--primary)" />
            )}
            {/* time tick */}
            <line x1={x + 19} y1={base + 8} x2={x + 19} y2={base + 16} stroke="var(--border)" strokeWidth="1.5" />
          </g>
        );
      })}
      {/* retention axis */}
      <line x1="34" y1={base + 8} x2="366" y2={base + 8} stroke="var(--border)" strokeWidth="1.5" />
      {/* the window that is kept */}
      <rect
        x="36"
        y="36"
        width={330}
        height={base - 22}
        rx="8"
        fill="none"
        stroke="var(--primary)"
        strokeWidth="1.5"
        strokeDasharray="6 6"
        opacity="0.45"
      />
    </svg>
  );
}

/** Restore: a timeline, and the point you go back to. */
function RestoreArt() {
  const pts = [64, 116, 168, 220, 272, 324];
  const target = 168;
  const y = 138;
  return (
    <svg {...ART_PROPS}>
      <Field />
      <line x1="40" y1={y} x2="360" y2={y} stroke="var(--border)" strokeWidth="2" />
      {pts.map((x) => {
        const isTarget = x === target;
        const after = x > target;
        return (
          <circle
            key={x}
            cx={x}
            cy={y}
            r={isTarget ? 10 : 6}
            fill={isTarget ? "var(--primary)" : after ? "var(--card)" : "var(--primary)"}
            stroke={isTarget ? "var(--primary)" : after ? "var(--muted-foreground)" : "var(--primary)"}
            strokeWidth={isTarget ? 3 : 1.6}
            opacity={after ? 0.5 : 1}
          />
        );
      })}
      {/* the rewind */}
      <path
        d={`M324 ${y - 22} C 300 ${y - 74}, 210 ${y - 74}, ${target + 2} ${y - 20}`}
        fill="none"
        stroke="var(--primary)"
        strokeWidth="2.5"
        strokeLinecap="round"
      />
      <path
        d={`M${target - 6} ${y - 30} l6 12 l13 -4`}
        fill="none"
        stroke="var(--primary)"
        strokeWidth="2.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      {/* the state that gets replaced */}
      <rect x={target + 24} y={y + 18} width={132} height="10" rx="5" fill="var(--muted-foreground)" opacity="0.25" />
      <rect x="40" y={y + 18} width={target - 46} height="10" rx="5" fill="var(--primary)" opacity="0.7" />
    </svg>
  );
}

/* ------------------------------------------------------------------ *
 * Agency operations                                                   *
 * ------------------------------------------------------------------ */

/** A fleet: many sites, one pane, status at a glance. */
function FleetArt() {
  const cells = Array.from({ length: 12 }, (_, i) => i);
  // Deterministic status: most fine, two attention, one down. No randomness,
  // so the art is byte-identical between server and client render.
  const attention = [4, 9];
  const down = [7];
  return (
    <svg {...ART_PROPS}>
      <Field />
      {/* the pane */}
      <rect x="30" y="30" width={W - 60} height={H - 60} rx="10" fill="var(--card)" stroke="var(--border)" strokeWidth="1.4" />
      <line x1="30" y1="58" x2={W - 30} y2="58" stroke="var(--border)" strokeWidth="1.2" />
      <circle cx="46" cy="44" r="3.5" fill="var(--primary)" />
      <rect x="58" y="40" width="54" height="8" rx="4" fill="var(--muted-foreground)" opacity="0.35" />

      {cells.map((i) => {
        const cx = 46 + (i % 4) * 80;
        const cy = 74 + Math.floor(i / 4) * 42;
        const tone = down.includes(i)
          ? "var(--warning-subtle-fg)"
          : attention.includes(i)
            ? "var(--info)"
            : "var(--success)";
        return (
          <g key={i}>
            <rect x={cx} y={cy} width="66" height="30" rx="5" fill="var(--muted)" opacity="0.5" />
            <circle cx={cx + 12} cy={cy + 15} r="4.5" fill={tone} />
            <rect x={cx + 23} y={cy + 11} width="32" height="4" rx="2" fill="var(--muted-foreground)" opacity="0.5" />
            <rect x={cx + 23} y={cy + 19} width="20" height="4" rx="2" fill="var(--muted-foreground)" opacity="0.3" />
          </g>
        );
      })}
    </svg>
  );
}

/** White-label reporting: your header, their numbers. */
function ReportArt() {
  return (
    <svg {...ART_PROPS}>
      <Field />
      {/* page */}
      <rect x="96" y="24" width="208" height={H - 48} rx="9" fill="var(--card)" stroke="var(--border)" strokeWidth="1.4" />
      {/* the branded band, the whole point of the piece */}
      <path d="M96 33 a9 9 0 0 1 9 -9 h190 a9 9 0 0 1 9 9 v25 H96 z" fill="var(--primary)" />
      <circle cx="116" cy="41" r="7" fill="var(--primary-foreground)" opacity="0.9" />
      <rect x="130" y="37" width="58" height="8" rx="4" fill="var(--primary-foreground)" opacity="0.75" />

      {/* summary figures */}
      {[0, 1, 2].map((i) => (
        <g key={i}>
          <rect x={112 + i * 64} y="74" width="52" height="34" rx="5" fill="var(--primary)" opacity="0.12" />
          <rect x={120 + i * 64} y="82" width="26" height="9" rx="3" fill="var(--primary)" opacity="0.8" />
          <rect x={120 + i * 64} y="96" width="36" height="5" rx="2.5" fill="var(--muted-foreground)" opacity="0.45" />
        </g>
      ))}

      {/* a chart */}
      {[26, 40, 30, 48, 36, 54].map((h, i) => (
        <rect
          key={i}
          x={116 + i * 30}
          y={182 - h}
          width="18"
          height={h}
          rx="3"
          fill="var(--primary)"
          opacity={0.35 + i * 0.1}
        />
      ))}
      <line x1="110" y1="182" x2="292" y2="182" stroke="var(--border)" strokeWidth="1.4" />
    </svg>
  );
}

/* ------------------------------------------------------------------ *
 * Registry                                                            *
 * ------------------------------------------------------------------ */

export const POST_ART = {
  gate: GateArt,
  integrity: IntegrityArt,
  scanner: ScannerArt,
  vitals: VitalsArt,
  compress: CompressArt,
  cache: CacheArt,
  snapshots: SnapshotsArt,
  restore: RestoreArt,
  fleet: FleetArt,
  report: ReportArt,
} as const;

export type PostArtName = keyof typeof POST_ART;

// `hasOwn`, not `in`. The value arrives as MDX frontmatter, so the guard is
// asked about arbitrary author-supplied strings, and `in` also answers true for
// everything on Object.prototype. `art: __proto__` would have passed the guard,
// indexed to Object.prototype, and thrown "element type is invalid" during
// static generation instead of falling back to the category illustration.
export function isPostArtName(value: unknown): value is PostArtName {
  return typeof value === "string" && Object.hasOwn(POST_ART, value);
}

/**
 * Every category has a fallback, so a post that forgets the `art` key still
 * gets a real illustration rather than an empty box. A missing key is a missed
 * opportunity, never a broken card.
 */
const CATEGORY_FALLBACK: Record<BlogCategory, PostArtName> = {
  "wordpress-security": "gate",
  "wordpress-performance": "vitals",
  "wordpress-backups": "snapshots",
  "agency-operations": "fleet",
};

export function resolvePostArt(art: unknown, category: BlogCategory): PostArtName {
  return isPostArtName(art) ? art : CATEGORY_FALLBACK[category];
}

/**
 * Renders a post's art at a fixed 16:9. The aspect ratio is set on the wrapper
 * rather than the svg so the card reserves its space before paint and nothing
 * shifts, which matters on an index rendering ten of these.
 */
export function PostArt({
  art,
  category,
  className,
}: {
  art?: unknown;
  category: BlogCategory;
  className?: string;
}) {
  const Art = POST_ART[resolvePostArt(art, category)];
  return (
    <div className={className} style={{ aspectRatio: "400 / 225" }}>
      <Art />
    </div>
  );
}
