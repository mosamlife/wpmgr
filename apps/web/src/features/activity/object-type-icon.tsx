import {
  Plug,
  Brush,
  User,
  FileText,
  RefreshCw,
  Tag,
  Settings,
  ShoppingCart,
  Activity as ActivityIcon,
  type LucideIcon,
} from "lucide-react";

// Object-type -> lucide icon map for the activity feed (ADR-037 redesign).
//
// Each WordPress activity event carries an `object_type` describing what the
// event acted on. We render a small object-type icon at the head of every feed
// row so an operator can scan by *kind* (a plugin change vs. a login vs. a core
// update) without reading the sentence. Keys are lowercased before lookup so
// agent-side casing variance ("Plugin" vs "plugin") still resolves. Unknown
// types fall back to a neutral Activity glyph rather than rendering nothing,
// keeping the row's left edge visually stable.

const ICONS: Record<string, LucideIcon> = {
  plugin: Plug,
  theme: Brush,
  user: User,
  post: FileText,
  page: FileText,
  core: RefreshCw,
  term: Tag,
  taxonomy: Tag,
  option: Settings,
  setting: Settings,
  settings: Settings,
  woocommerce: ShoppingCart,
  order: ShoppingCart,
  product: ShoppingCart,
};

/** Resolve the lucide icon for an object_type, falling back to a neutral glyph. */
export function objectTypeIcon(objectType: string): LucideIcon {
  return ICONS[objectType.toLowerCase()] ?? ActivityIcon;
}
