// Shared flat ESLint config. Expanded in Phase 4 once the frontend stack is
// locked (React, TanStack, etc.). Kept minimal so the workspace lints today.
import js from "@eslint/js";

/** @type {import("eslint").Linter.Config[]} */
export default [
  js.configs.recommended,
  {
    ignores: ["dist/**", "**/*.gen.ts", "node_modules/**"],
  },
];
