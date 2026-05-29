// Phase 4 / Sprint 4 surface 4.14 - Forms.
//
// Field-level validation error renderer. Implements the "what, why, how"
// pattern from DESIGN.md "Do's and Don'ts" - "Don't write generic errors.
// Always: what, why, how." Example output:
//
//   Invalid URL · It must start with https:// · Edit the URL above
//
// Renders nothing if `what` is empty, so callers can pass through the
// zod/react-hook-form error message verbatim:
//
//   <FieldError what={errors.url?.message} why="..." how="..." />

interface FieldErrorProps {
  /** Primary error label (e.g. "Invalid URL"). Required, but tolerant of
   *  undefined so callers can pass `errors.field?.message` directly. */
  what?: string;
  /** Plain-language cause (e.g. "It must start with https://"). */
  why?: string;
  /** Actionable next step (e.g. "Edit the URL above"). */
  how?: string;
}

export function FieldError({ what, why, how }: FieldErrorProps) {
  if (!what) return null;

  return (
    <p
      role="alert"
      className="mt-1 text-sm text-destructive"
    >
      <span className="font-medium">{what}</span>
      {why ? (
        <>
          {" · "}
          <span>{why}</span>
        </>
      ) : null}
      {how ? (
        <>
          {" · "}
          <span>{how}</span>
        </>
      ) : null}
    </p>
  );
}
