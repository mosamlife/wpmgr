import { z } from "zod";
import type { AbilityRequest } from "@wpmgr/api";

import type { AbilityStatus } from "./ability-card-model";

export const templateRulesSchema = z.array(z.string().regex(/^(include|exclude)(\/[a-z0-9_-]{1,64}){1,2}(\/[1-9][0-9]{0,19})?$/)).max(20);
const titleSchema = z.strictObject({ id: z.number().int().positive().safe(), title: z.string() });
export const editTemplateSchema = z.strictObject({
  type: z.enum(["header", "footer"]),
  shows_on: z.strictObject({ before: templateRulesSchema, after: templateRulesSchema }),
  rule_titles: z.array(titleSchema), refusal_message: z.string().optional(),
});
const createTemplateSchema = z.strictObject({
  type: z.enum(["header", "footer"]), shows_on: templateRulesSchema,
  rule_titles: z.array(titleSchema), refusal_message: z.string().nullable(),
  also_live: z.array(z.strictObject({
    id: z.number().int().positive().safe(), title: z.string(),
    shows_on: templateRulesSchema.nullable(),
    shows_on_problem: z.enum(["unreadable", "too_many"]).nullable(),
  })).nullable(),
});
export type EditTemplate = z.infer<typeof editTemplateSchema>;
export type CreateTemplate = z.infer<typeof createTemplateSchema>;

export const TEMPLATE_EFFECT = "Draft only. Visitors see it only after a person publishes it in Elementor.";
export const TEMPLATE_CHECKS = "Widgets and settings checked against this site's Elementor and Pro.";
export const TEMPLATE_EDIT_UNDO = "Undo puts back the content and where it shows.";

const PLACE_WORDS: Record<string, string> = {
  general: "Entire site", "singular/front_page": "Front page", "singular/page": "All pages",
  "singular/post": "All posts", "singular/not_found404": "404 page", archive: "All archives",
};
const lowerFirst = (s: string) => s.charAt(0).toLowerCase() + s.slice(1);

function placeWords(rule: string, titles: readonly { id: number; title: string }[]): string {
  const place = rule.slice(rule.indexOf("/") + 1);
  if (Object.hasOwn(PLACE_WORDS, place)) return PLACE_WORDS[place]!;
  const match = /^singular\/(page|post)\/([1-9][0-9]*)$/.exec(place);
  if (match) {
    const title = titles.find((t) => String(t.id) === match[2])?.title;
    const noun = match[1] === "page" ? "Page" : "Post";
    return title ? `${noun} “${title}” (#${match[2]})` : `${noun.toLowerCase()} #${match[2]}`;
  }
  return `another place (${rule})`;
}

/** Every stored rule has visible words, including places outside the AI's list. */
export function showsOnWords(rules: readonly string[], titles: readonly { id: number; title: string }[]): string {
  if (rules.length === 0) return "Nowhere yet. A person chooses where it shows in Elementor.";
  const includes = rules.filter((r) => r.startsWith("include/")).map((r) => placeWords(r, titles));
  const excludes = rules.filter((r) => r.startsWith("exclude/")).map((r) => lowerFirst(placeWords(r, titles)));
  const included = includes.map((s, i) => i === 0 ? s : lowerFirst(s)).join(" and ");
  return `${included}${excludes.length ? `${included ? ", except " : "Except "}${excludes.join(" and ")}` : ""}`;
}

export function sentence(s: string): string {
  return s.endsWith(".") ? s : `${s}.`;
}

export function liveTemplateLine(live: NonNullable<CreateTemplate["also_live"]>[number], template: CreateTemplate): string {
  const target = `“${live.title}” (#${live.id})`;
  if (live.shows_on === null) {
    return `${target}: ${live.shows_on_problem === "too_many" ? "There are too many places to show here." : "WPMgr could not read where it shows."}`;
  }
  if (live.shows_on.length === 0) return `${target} shows nowhere yet.`;
  const words = lowerFirst(showsOnWords(live.shows_on, template.rule_titles));
  return sentence(`${target} shows on ${words === "entire site" || words === "front page" || words === "404 page" ? "the " : ""}${words}`);
}

export function readCreateTemplate(r: AbilityRequest): CreateTemplate | null {
  const parsed = createTemplateSchema.safeParse(r.page_template);
  return parsed.success ? parsed.data : null;
}

/** Reuses the page outline reader only after template input and site facts agree. */
export function templatePreviewInput(r: AbilityRequest, template: CreateTemplate): string | null {
  try {
    const raw: unknown = JSON.parse(r.input_json);
    if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
    const input = raw as Record<string, unknown>;
    if (input.template !== template.type || "post_type" in input || "page_frame" in input
      || input.editor !== "builder:elementor" || input.elementor_format !== "classic") return null;
    const rules = templateRulesSchema.safeParse("shows_on" in input ? input.shows_on : []);
    if (!rules.success || JSON.stringify(rules.data) !== JSON.stringify(template.shows_on)) return null;
    const { template: _type, shows_on: _rules, ...page } = input;
    return JSON.stringify({ ...page, post_type: "page" });
  } catch {
    return null;
  }
}

export function templatePublished(r: AbilityRequest): boolean {
  return r.undo_state === "refused_published" || r.outcome_code === "refused_published";
}

/** Template refusals and undo states use the same request state as page cards. */
export function templateStatus<S extends AbilityStatus>(r: AbilityRequest, template: CreateTemplate | EditTemplate, base: S): S {
  if (templatePublished(r)) return { ...base, kind: "undo_refused", text:
    `A person published this ${template.type} in Elementor. WPMgr no longer changes it, and undo is closed.` };
  if (r.undo_state === "refused_conflict") return { ...base, kind: "undo_refused", text:
    `This ${template.type} changed in Elementor after the AI's change. Undo would overwrite that work, so nothing was undone.` };
  if (template.refusal_message) return { ...base, text: template.refusal_message };
  return base;
}
