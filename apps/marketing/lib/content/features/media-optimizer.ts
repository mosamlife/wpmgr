// Media Optimizer feature page content (FLAGSHIP).
// Seeded from apps/landing MEDIA + MEDIA_STEPS.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const MEDIA_OPTIMIZER_PAGE: FeaturePageData = {
  slug: "media-optimizer",
  title: "WordPress AVIF and WebP Image Optimization",
  metaTitle: "Convert Images to WebP and AVIF in WordPress | Media Optimizer",
  metaDescription:
    "WPMgr Media Optimizer converts your WordPress media library to AVIF and WebP in the cloud. Originals stay archived on the site, and every conversion is fully reversible.",
  hero: {
    eyebrow: "Media Optimizer",
    heading: "Convert WordPress images to AVIF and WebP, fully reversible",
    subhead:
      "Turn on Media Optimization and WPMgr re-encodes your library to AVIF and WebP in the cloud. Every browser gets the format it supports, your originals stay safely archived on the site, and you can revert any image with one click.",
    primaryCta: { label: "Optimize your media library free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "JPEG and PNG images are the biggest drag on page weight, and CDN resizing still serves the wrong format to older browsers",
    body: "Modern formats like AVIF and WebP deliver the same visual quality at a fraction of the file size, but converting a full WordPress media library manually is impractical. CDN-based resizing solves delivery but leaves the originals unchanged and often serves one format regardless of what the browser actually supports. WPMgr re-encodes the full image and every thumbnail WordPress generates, stores the originals safely, and lets each browser request the format it understands.",
  },
  steps: [
    {
      n: "1",
      icon: "Upload",
      title: "Upload or pick existing images",
      desc: "Flip on auto-optimize and every new upload gets queued the moment WordPress finishes generating its sizes. Already have a full library? Select existing images and send them in batches. No reformatting your workflow.",
    },
    {
      n: "2",
      icon: "Cpu",
      title: "A dedicated encoder re-encodes safely",
      desc: "A dedicated cloud encoder reads each image by its real bytes, not a guessed file extension, and re-encodes the full image plus every thumbnail to AVIF or WebP. Animated GIFs become animated WebP. Your originals are renamed and archived on the site, never deleted.",
    },
    {
      n: "3",
      icon: "Globe",
      title: "Browsers get the modern format automatically",
      desc: "A small .htaccess rule checks what each visitor's browser actually supports and serves the modern format only when it will display, falling back to the original everywhere else. Pages get lighter with no broken images and no front-end plugin bloat.",
    },
    {
      n: "4",
      icon: "RotateCcw",
      title: "Revert anytime, fully",
      desc: "Changed your mind, or a single image looks off? Restore puts every archived original back, full image and all thumbnails, and rewrites the URLs in your content. Nothing about optimization is a one-way door.",
    },
  ],
  subFeatures: [
    {
      icon: "ImageDown",
      title: "AVIF and WebP for every size",
      desc: "WPMgr totals bytes saved across every variant including all thumbnail sizes WordPress generates, plus a running count of images optimized, so the number on the dashboard reflects real files.",
    },
    {
      icon: "Undo2",
      title: "Fully reversible, image and thumbnails",
      desc: "Every archived original can be restored with one click. The restore rewrites all URLs in your content, including thumbnails. Nothing about the optimization process is permanent.",
    },
    {
      icon: "ShieldOff",
      title: "Zero image bytes on the control plane",
      desc: "Source files move from your site to your storage, and optimized files move from the encoder to your storage, using short-lived presigned URLs while WPMgr keeps only metadata.",
    },
    {
      icon: "ToggleLeft",
      title: "Opt-in and per-site",
      desc: "Media Optimization is off until you turn it on. Auto-optimize applies to new uploads; batch conversion handles the existing library. Each runs independently per site.",
    },
    {
      icon: "ImageOff",
      title: "Unused Image Cleaner",
      desc: "Finds media attachments that are not referenced anywhere, shows exactly where each image in use appears, and moves unwanted images to a reversible quarantine. Permanent deletion requires explicit confirmation.",
    },
    {
      icon: "RefreshCw",
      title: "Animated GIF to animated WebP",
      desc: "Animated GIFs are detected and transcoded to animated WebP automatically, significantly reducing file size while preserving motion content.",
    },
  ],
  faq: [
    {
      q: "Does Media Optimizer touch the original images?",
      a: "No. Originals are renamed and archived on the site before the optimized version is written. They are never deleted. You can restore any image or the entire library with one click.",
    },
    {
      q: "Does image data pass through WPMgr servers?",
      a: "No. Source files move from your site directly to your configured storage destination using short-lived presigned URLs. Optimized files move from the dedicated encoder to your storage. WPMgr keeps only metadata, never the image bytes.",
    },
    {
      q: "How does the browser get the right format?",
      a: "A small .htaccess rule (or nginx equivalent) checks the Accept header of each request and serves the AVIF or WebP version only when the browser declares support. Browsers that do not support the modern format receive the original automatically.",
    },
    {
      q: "Does it optimize thumbnails too?",
      a: "Yes. Every size WordPress generates for an image is included in the optimization batch. Byte savings are counted across the full image plus all thumbnail variants, not just the original upload.",
    },
    {
      q: "What about animated GIFs?",
      a: "Animated GIFs are detected automatically and transcoded to animated WebP. The motion is preserved and the file size is typically reduced significantly.",
    },
    {
      q: "Can I optimize images one at a time or do I have to run the whole library?",
      a: "Both. You can enable auto-optimize for new uploads, run a bulk batch on the existing library, or select individual images to optimize from the media screen. Each runs independently.",
    },
  ],
  siblingLinks: [
    { label: "Performance and page caching", href: "/features/performance" },
    { label: "Redis Object Cache", href: "/features/object-cache" },
  ],
  solutionLinks: [
    { label: "Speed up WordPress", href: "/solutions/wordpress-performance" },
  ],
};

// Media demo data for the visual widget (illustrative sample data only)
export const MEDIA_DEMO = {
  caption: "A sample upload, full image plus its thumbnail set",
  originalLabel: "Original JPEG",
  originalBytes: 2_480_000,
  optimizedLabel: "Optimized AVIF",
  optimizedBytes: 712_000,
  library: [
    { label: "Optimized", pct: 72, tone: "success" as const },
    { label: "Pending", pct: 18, tone: "warning" as const },
    { label: "Unsupported", pct: 10, tone: "muted" as const },
  ],
};
