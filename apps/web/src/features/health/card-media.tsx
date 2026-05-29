import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";

import { DiagnosticCard } from "./diagnostic-card";
import { asWpNative, fieldValue, section } from "./wp-native";

// Media Handling card — wp-media section of WP_Debug_Data. The fields operators
// care about for "can this site process the uploaded JPGs/PDFs?":
//   - image_editor (Imagick / GD), imagick / GD / ImageMagick / Ghostscript
//   - file_uploads, post_max_size, upload_max_filesize, max_effective_size

export function CardMedia({ card }: { card: SiteDiagnosticsCard | undefined }) {
  const payload = asWpNative(card);
  const sec = section(payload, "wp-media");

  const rows = [
    row(sec, "image_editor", "Image editor"),
    row(sec, "imagick_module_version", "Imagick module"),
    row(sec, "imagick_version", "Imagick"),
    row(sec, "imagemagick_version", "ImageMagick"),
    row(sec, "gd_version", "GD"),
    row(sec, "gd_formats", "GD formats"),
    row(sec, "ghostscript_version", "Ghostscript"),
    row(sec, "upload_max_filesize", "Upload max"),
    row(sec, "post_max_size", "POST max"),
    row(sec, "max_effective_size", "Effective max"),
    row(sec, "max_file_uploads", "Max file uploads"),
    row(sec, "file_uploads", "File uploads enabled"),
  ].filter(
    (r): r is { label: string; value: string; mono?: boolean } => r !== null,
  );

  return (
    <DiagnosticCard title="Media Handling" card={card}>
      {rows.length > 0 ? (
        <DefinitionList rows={rows} />
      ) : (
        <p className="text-sm text-muted-foreground">
          Awaiting first sync from the agent.
        </p>
      )}
    </DiagnosticCard>
  );
}

function row(
  sec: ReturnType<typeof section>,
  key: string,
  label: string,
): { label: string; value: string; mono?: boolean } | null {
  const v = fieldValue(sec, key);
  if (v === null) return null;
  return { label, value: v, mono: true };
}
