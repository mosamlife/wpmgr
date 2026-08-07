/**
 * The auth-page illustration: a fleet connected to one control plane.
 *
 * WHY THIS SUBJECT. The auth pages are the first thing a new operator sees,
 * and the single idea worth landing before they have used anything is that
 * many sites become one pane. An abstract flourish would decorate the page;
 * this states the product. It is also drawn from the same tokens as the app,
 * so signup and the dashboard behind it look like one product rather than a
 * marketing page bolted onto a tool.
 *
 * THE RESTING STATE IS THE FINISHED DIAGRAM. Every animation here ends where
 * the static render begins: spokes fully drawn, every node visible, nothing
 * transparent. That is not a stylistic preference, it is a correctness
 * requirement. globals.css collapses animation-duration to 0.01ms under
 * prefers-reduced-motion, so a reduced-motion visitor jumps straight to the
 * end state, and any element whose end state was "hidden" would simply never
 * appear. The same applies to a renderer that never runs the animation at all.
 *
 * Motion is therefore additive: spokes draw outward from the hub, nodes settle
 * in, and status dots pulse. Remove all of it and the picture is unchanged.
 *
 * Decorative, so aria-hidden. The heading beside it carries the meaning.
 */

const NODES = [
  { x: 180, y: 44, tone: "up" },
  { x: 296, y: 110, tone: "up" },
  { x: 296, y: 246, tone: "warn" },
  { x: 180, y: 312, tone: "up" },
  { x: 64, y: 246, tone: "up" },
  { x: 64, y: 110, tone: "info" },
] as const;

const TONE_FILL: Record<string, string> = {
  up: "var(--color-success)",
  warn: "var(--color-warning)",
  info: "var(--color-info)",
};

export function FleetIllustration({ className }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 360 360"
      className={className}
      xmlns="http://www.w3.org/2000/svg"
      aria-hidden="true"
      focusable="false"
    >
      {/* Orbit rings. Purely spatial, drawn first so everything sits on top. */}
      <circle
        cx="180"
        cy="180"
        r="136"
        fill="none"
        stroke="var(--color-border)"
        strokeWidth="1"
        strokeDasharray="3 7"
        opacity="0.7"
      />
      <circle cx="180" cy="180" r="92" fill="none" stroke="var(--color-border)" strokeWidth="1" opacity="0.5" />

      {/* Spokes. `pathLength` normalises every line to 1 unit regardless of its
          real length, so one dash rule animates them all at the same rate
          instead of needing a per-line dasharray. */}
      {NODES.map((n, i) => (
        <line
          key={`spoke-${n.x}-${n.y}`}
          x1="180"
          y1="180"
          x2={n.x}
          y2={n.y}
          stroke="var(--color-primary)"
          strokeWidth="1.5"
          opacity="0.45"
          pathLength={1}
          strokeDasharray="1 1"
          className="wpmgr-fleet-spoke"
          style={{ animationDelay: `${i * 90}ms` }}
        />
      ))}

      {/* Site nodes. */}
      {NODES.map((n, i) => (
        <g
          key={`node-${n.x}-${n.y}`}
          className="wpmgr-fleet-node"
          style={{ animationDelay: `${240 + i * 90}ms`, transformOrigin: `${n.x}px ${n.y}px` }}
        >
          <rect
            x={n.x - 26}
            y={n.y - 20}
            width="52"
            height="40"
            rx="9"
            fill="var(--color-card)"
            stroke="var(--color-border)"
            strokeWidth="1.25"
          />
          {/* window chrome, so the node reads as a site rather than a box */}
          <line
            x1={n.x - 26}
            y1={n.y - 8}
            x2={n.x + 26}
            y2={n.y - 8}
            stroke="var(--color-border)"
            strokeWidth="1"
          />
          <circle cx={n.x - 19} cy={n.y - 14} r="2" fill="var(--color-muted-foreground)" opacity="0.55" />
          <rect
            x={n.x - 13}
            y={n.y + 1}
            width="26"
            height="3.5"
            rx="1.75"
            fill="var(--color-muted-foreground)"
            opacity="0.35"
          />
          <rect
            x={n.x - 13}
            y={n.y + 9}
            width="16"
            height="3.5"
            rx="1.75"
            fill="var(--color-muted-foreground)"
            opacity="0.25"
          />
          {/* status */}
          <circle
            cx={n.x + 17}
            cy={n.y + 11}
            r="3.5"
            fill={TONE_FILL[n.tone]}
            className="wpmgr-fleet-status"
            style={{ animationDelay: `${i * 320}ms` }}
          />
        </g>
      ))}

      {/* The hub, drawn last so it sits above the spokes meeting it. */}
      <g className="wpmgr-fleet-hub">
        <circle cx="180" cy="180" r="46" fill="var(--color-primary)" opacity="0.1" />
        <rect
          x="146"
          y="146"
          width="68"
          height="68"
          rx="18"
          fill="var(--color-primary)"
          stroke="var(--color-primary)"
          strokeWidth="1.5"
        />
        {/* Three stacked layers: the same idea as the sidebar mark, simplified
            to read at this size. */}
        <g
          fill="none"
          stroke="var(--color-primary-foreground)"
          strokeWidth="2.25"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <path d="M180 164 l20 10 l-20 10 l-20 -10 z" />
          <path d="M160 186 l20 10 l20 -10" />
          <path d="M160 196 l20 10 l20 -10" />
        </g>
      </g>
    </svg>
  );
}
