import js from "@eslint/js";
import globals from "globals";
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";

// Flat ESLint config for @wpmgr/web. Generated files (TanStack route tree and
// the @wpmgr/api codegen output, which lives in another package) are excluded.
export default tseslint.config(
  {
    ignores: [
      "dist/**",
      "node_modules/**",
      // TanStack Router generated route tree — treated as generated output.
      "src/routeTree.gen.ts",
      "**/*.gen.ts",
      "playwright-report/**",
      "test-results/**",
    ],
  },
  // Type-aware linting for the application TypeScript sources only.
  {
    files: ["**/*.{ts,tsx}"],
    extends: [
      js.configs.recommended,
      ...tseslint.configs.recommendedTypeChecked,
    ],
    languageOptions: {
      ecmaVersion: 2023,
      globals: { ...globals.browser, ...globals.node },
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true },
      ],
      // Strict TS hygiene (ADR: no `any`/`unknown` without narrowing).
      "@typescript-eslint/no-explicit-any": "error",
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
    },
  },
  // TanStack Router conventions: route modules throw `redirect()` objects in
  // guards (not Error instances) and must export a `Route` alongside the
  // component — both are intentional, so relax the relevant rules here.
  {
    files: ["src/routes/**/*.{ts,tsx}"],
    rules: {
      "@typescript-eslint/only-throw-error": "off",
      "react-refresh/only-export-components": "off",
    },
  },
  // TanStack Table's useReactTable returns a stable instance; the react-hooks
  // v7 "incompatible-library" heuristic flags it as a false positive.
  {
    files: [
      "src/features/sites/sites-table.tsx",
      "src/features/media/AssetsTable.tsx",
    ],
    rules: {
      "react-hooks/incompatible-library": "off",
    },
  },
  // JS config files (this file) run in Node and aren't part of the TS program;
  // lint them without type information.
  {
    files: ["**/*.js"],
    extends: [js.configs.recommended],
    languageOptions: { globals: { ...globals.node } },
  },
);
