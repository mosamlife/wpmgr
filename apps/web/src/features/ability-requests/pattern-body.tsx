import { iconWords, planButtonStyle } from "./design-model";
import { Label, OutlineNodeView, Txt, type SectionLeaf } from "./layout-preview";
import { imageAltLabel, quoted, type PageMediaFact, type PatternNode } from "./outline-model";

// What a pattern of a designed page holds, shown under the pattern's name: the
// words and the pictures the page will carry, so the person approving reads
// them. A hero and a call to action hold leaves, drawn as the outline of any
// page draws them (a hero's picture by its file name). A features item shows
// its icon in words, its title and its text; a counter its value, suffix and
// label; a logo its file name and alt text; a testimonial its quote, name, role
// and picture; a plan its name, price, period, features and button; a question
// its answer.
//
// Everything is a React text node. The AI's words, and the site's file names,
// sit in a bidi isolate beside our small labels, and none of them is ever made
// into markup, a style or an address.

type Of<K extends PatternNode["pattern"]> = Extract<PatternNode, { pattern: K }>;
type Media = ReadonlyMap<number, PageMediaFact> | null;

export interface PatternBodyProps {
  pattern: PatternNode;
  /** What the site said about each image the page names. */
  media: Media;
  /** The site's host as WPMgr recorded it (site_host), to tell its own links from others. */
  siteHost: string;
  /** The site's own address, for "View image in WordPress". Unknown is fine. */
  siteUrl: string | null;
}

export function PatternBody({ pattern, media, siteHost, siteUrl }: PatternBodyProps) {
  return (
    <div
      data-testid="pattern-body"
      data-pattern={pattern.pattern}
      className="ml-1 mt-1 space-y-2 border-l border-border pl-3"
    >
      <Body pattern={pattern} media={media} siteHost={siteHost} siteUrl={siteUrl} />
    </div>
  );
}

function Body({ pattern, media, siteHost, siteUrl }: PatternBodyProps) {
  switch (pattern.pattern) {
    case "hero":
    case "cta": {
      const children: readonly SectionLeaf[] = pattern.children;
      return (
        <>
          {children.map((child, i) => (
            <OutlineNodeView key={i} node={child} media={media} siteHost={siteHost} siteUrl={siteUrl} />
          ))}
        </>
      );
    }
    case "features":
      return <FeatureList features={pattern.features} />;
    case "stats":
      return <StatList stats={pattern.stats} />;
    case "logos":
      return <LogoList logos={pattern.logos} media={media} />;
    case "testimonials":
      return <TestimonialList testimonials={pattern.testimonials} media={media} />;
    case "pricing":
      return <PlanList plans={pattern.plans} media={media} siteHost={siteHost} siteUrl={siteUrl} />;
    case "faq":
      return <QuestionList faq={pattern.faq} />;
  }
}

/** An image by the file name the site reported, or by its media library number when it reported none. */
function ImageName({ id, media }: { id: number; media: Media }) {
  const fact = media?.get(id);
  return fact === undefined ? <>{`media library item ${id}`}</> : <Txt>{fact.filename}</Txt>;
}

function FeatureList({ features }: { features: Of<"features">["features"] }) {
  return (
    <ul aria-label="Features" className="space-y-2">
      {features.map((item, i) => (
        <li key={i} data-testid="feature-item" className="break-words">
          <p>
            <Label>Icon</Label>{" "}
            <span data-testid="feature-icon" className="text-muted-foreground">
              {iconWords(item.icon)}
            </span>
          </p>
          <p>
            <Label>Title</Label>{" "}
            <span data-testid="feature-title">
              <Txt className="font-semibold">{item.title}</Txt>
            </span>
          </p>
          <p>
            <Label>Text</Label>{" "}
            <span data-testid="feature-text">
              <Txt>{item.text}</Txt>
            </span>
          </p>
        </li>
      ))}
    </ul>
  );
}

function StatList({ stats }: { stats: Of<"stats">["stats"] }) {
  return (
    <ul aria-label="Stats" className="space-y-1.5">
      {stats.map((item, i) => (
        <li key={i} data-testid="stat-item" className="break-words">
          <span data-testid="stat-value" className="font-semibold">
            {item.value}
            {item.suffix === undefined ? null : <Txt>{item.suffix}</Txt>}
          </span>{" "}
          <span data-testid="stat-label">
            <Txt>{item.label}</Txt>
          </span>
        </li>
      ))}
    </ul>
  );
}

function LogoList({ logos, media }: { logos: Of<"logos">["logos"]; media: Media }) {
  return (
    <ul aria-label="Logos" className="space-y-1.5">
      {logos.map((logo, i) => (
        <li key={i} data-testid="logo-item" className="break-words">
          <p>
            <Label>Image</Label>{" "}
            <span data-testid="logo-file" className="text-muted-foreground">
              <ImageName id={logo.attachment_id} media={media} />
            </span>
          </p>
          <p>
            <Label>{imageAltLabel(logo.alt, true)}</Label>
            {logo.alt === "" ? null : (
              <>
                {" "}
                <Txt>{quoted(logo.alt)}</Txt>
              </>
            )}
          </p>
        </li>
      ))}
    </ul>
  );
}

function TestimonialList({
  testimonials,
  media,
}: {
  testimonials: Of<"testimonials">["testimonials"];
  media: Media;
}) {
  return (
    <ul aria-label="Testimonials" className="space-y-2">
      {testimonials.map((item, i) => (
        <li key={i} data-testid="testimonial-item" className="break-words">
          <p className="italic" data-testid="testimonial-quote">
            <Txt>{quoted(item.quote)}</Txt>
          </p>
          <p>
            <Label>Name</Label>{" "}
            <span data-testid="testimonial-name">
              <Txt className="font-semibold">{item.name}</Txt>
            </span>
          </p>
          {item.role === undefined ? null : (
            <p>
              <Label>Role</Label>{" "}
              <span data-testid="testimonial-role">
                <Txt>{item.role}</Txt>
              </span>
            </p>
          )}
          {item.attachment_id === undefined ? null : (
            <p>
              <Label>Picture</Label>{" "}
              <span data-testid="testimonial-picture" className="text-muted-foreground">
                <ImageName id={item.attachment_id} media={media} />
              </span>
            </p>
          )}
        </li>
      ))}
    </ul>
  );
}

function PlanList({
  plans,
  media,
  siteHost,
  siteUrl,
}: {
  plans: Of<"pricing">["plans"];
  media: Media;
  siteHost: string;
  siteUrl: string | null;
}) {
  return (
    <ul aria-label="Plans" className="space-y-3">
      {plans.map((plan, i) => (
        <li key={i} data-testid="plan-item" className="space-y-1 break-words">
          <p>
            <Label>Plan</Label>{" "}
            <span data-testid="plan-name">
              <Txt className="font-semibold">{plan.name}</Txt>
            </span>
            {plan.highlighted === true ? (
              <>
                {" "}
                <span
                  data-testid="plan-highlighted"
                  className="inline-flex items-center rounded bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground"
                >
                  Highlighted
                </span>
              </>
            ) : null}
          </p>
          <p>
            <Label>Price</Label>{" "}
            <span data-testid="plan-price">
              <Txt>{plan.price}</Txt>
            </span>
          </p>
          {plan.period === undefined ? null : (
            <p>
              <Label>Period</Label>{" "}
              <span data-testid="plan-period">
                <Txt>{plan.period}</Txt>
              </span>
            </p>
          )}
          <div>
            <p>
              <Label>Includes</Label>
            </p>
            <ul className="mt-0.5 list-disc space-y-0.5 pl-5">
              {plan.features.map((feature, j) => (
                <li key={j} data-testid="plan-feature" className="break-words">
                  <Txt>{feature}</Txt>
                </li>
              ))}
            </ul>
          </div>
          {/* The plan's button names no style; the page gives it the one its plan has. */}
          <OutlineNodeView
            node={{
              type: "buttons",
              buttons: [{ text: plan.cta.text, url: plan.cta.url, style: planButtonStyle(plan) }],
            }}
            media={media}
            siteHost={siteHost}
            siteUrl={siteUrl}
          />
        </li>
      ))}
    </ul>
  );
}

function QuestionList({ faq }: { faq: Of<"faq">["faq"] }) {
  return (
    <ul aria-label="Questions" className="space-y-2">
      {faq.map((item, i) => (
        <li key={i} data-testid="faq-item" className="break-words">
          <p>
            <Label>Question</Label>{" "}
            <span data-testid="faq-question">
              <Txt className="font-semibold">{item.question}</Txt>
            </span>
          </p>
          <p>
            <Label>Answer</Label>{" "}
            <span data-testid="faq-answer">
              <Txt>{item.answer}</Txt>
            </span>
          </p>
        </li>
      ))}
    </ul>
  );
}
