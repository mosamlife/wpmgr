// Placeholder. Tailwind + the chosen component library (ADR-014) are wired in
// Phase 4 by the frontend-architect. Kept here to mark the intended location.
import type { Config } from "tailwindcss";

export default {
  darkMode: "class",
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: { extend: {} },
  plugins: [],
} satisfies Config;
