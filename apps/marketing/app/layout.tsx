import type { Metadata, Viewport } from "next";
import localFont from "next/font/local";
import { MotionConfig } from "motion/react";
import "@/styles/globals.css";
import {
  buildOrganizationLd,
  buildWebSiteLd,
} from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { SITE_CONFIG } from "@/lib/site";
import { GoogleAnalytics } from "@next/third-parties/google";
import { PostHogPageViews } from "@/components/analytics/posthog-provider";
import { TrackSignupClicks } from "@/components/analytics/track-signup-clicks";
import { GA_MEASUREMENT_ID, GSC_VERIFICATION, analyticsEnabled } from "@/lib/analytics";

// IBM Plex is self-hosted, so a build never needs a network fetch. Fetching it
// from Google Fonts at build time made the build fail whenever that fetch did,
// on pull requests that had nothing to do with this site.
//
// The files in ./fonts are the Google Fonts subsets this site renders, under the
// SIL Open Font License 1.1 (./fonts/OFL.txt): latin for both families, plus
// latin-ext for Plex Sans, which holds the rupee sign on the pricing page's INR
// toggle. Plex Sans is one variable file per subset that every weight shares,
// which is why four entries point at one path.
//
// next/font/local names a family after its constant, but the design tokens
// (font-sans, font-mono and the base code rule in @wpmgr/tokens) look the
// families up as "IBM Plex Sans" and "IBM Plex Mono". So each call registers its
// faces under that name with a font-family declaration, and a unicode-range
// declaration keeps each file to the characters it was cut for. Next requires
// these options to be written out as literals, hence the repetition.
const ibmPlexSans = localFont({
  src: [
    { path: "./fonts/ibm-plex-sans-latin.woff2", weight: "400", style: "normal" },
    { path: "./fonts/ibm-plex-sans-latin.woff2", weight: "500", style: "normal" },
    { path: "./fonts/ibm-plex-sans-latin.woff2", weight: "600", style: "normal" },
    { path: "./fonts/ibm-plex-sans-latin.woff2", weight: "700", style: "normal" },
  ],
  display: "swap",
  declarations: [
    { prop: "font-family", value: "IBM Plex Sans" },
    {
      prop: "unicode-range",
      value:
        "U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD",
    },
  ],
  variable: "--font-ibm-plex-sans",
});

// Not preloaded and given no fallback face of its own: it is fetched only when a
// page shows a character in its range, and ibmPlexSans supplies the fallback.
const ibmPlexSansLatinExt = localFont({
  src: [
    { path: "./fonts/ibm-plex-sans-latin-ext.woff2", weight: "400", style: "normal" },
    { path: "./fonts/ibm-plex-sans-latin-ext.woff2", weight: "500", style: "normal" },
    { path: "./fonts/ibm-plex-sans-latin-ext.woff2", weight: "600", style: "normal" },
    { path: "./fonts/ibm-plex-sans-latin-ext.woff2", weight: "700", style: "normal" },
  ],
  display: "swap",
  preload: false,
  adjustFontFallback: false,
  declarations: [
    { prop: "font-family", value: "IBM Plex Sans" },
    {
      prop: "unicode-range",
      value:
        "U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C4, U+2113, U+2C60-2C7F, U+A720-A7FF",
    },
  ],
  variable: "--font-ibm-plex-sans-latin-ext",
});

const ibmPlexMono = localFont({
  src: [
    { path: "./fonts/ibm-plex-mono-latin-400.woff2", weight: "400", style: "normal" },
    { path: "./fonts/ibm-plex-mono-latin-500.woff2", weight: "500", style: "normal" },
  ],
  display: "swap",
  declarations: [
    { prop: "font-family", value: "IBM Plex Mono" },
    {
      prop: "unicode-range",
      value:
        "U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD",
    },
  ],
  variable: "--font-ibm-plex-mono",
});

// The generated --font-ibm-plex-* variables name the constants above rather than
// the registered families, so the body stack is written out. "ibmPlexSans
// Fallback" is the size-adjusted fallback face next/font generates for Plex Sans.
const BODY_FONT_STACK =
  '"IBM Plex Sans", "ibmPlexSans Fallback", ui-sans-serif, system-ui, sans-serif';

export const viewport: Viewport = {
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#1791A6" },
    { media: "(prefers-color-scheme: dark)", color: "#0E5E6B" },
  ],
};

export const metadata: Metadata = {
  metadataBase: new URL(SITE_CONFIG.baseUrl),
  title: {
    template: "%s · WPMgr",
    default: "WPMgr: Open-Source, Self-Hosted WordPress Fleet Management",
  },
  description: SITE_CONFIG.description,
  openGraph: {
    type: "website",
    siteName: SITE_CONFIG.name,
    title: "WPMgr: Open-Source, Self-Hosted WordPress Fleet Management",
    description: SITE_CONFIG.description,
    url: SITE_CONFIG.baseUrl,
    images: [
      {
        url: "/opengraph-image",
        width: 1200,
        height: 630,
        alt: "WPMgr - Open-source WordPress fleet management",
      },
    ],
  },
  twitter: {
    card: "summary_large_image",
    title: "WPMgr: Open-Source, Self-Hosted WordPress Fleet Management",
    description: SITE_CONFIG.description,
    images: ["/opengraph-image"],
  },
  robots: { index: true, follow: true },
  alternates: { canonical: SITE_CONFIG.baseUrl },
  // Search Console HTML-tag verification. Emitted only when the token is
  // configured, so a fork does not ship our verification tag. Removing it after
  // verification succeeds would un-verify the property, so it stays.
  ...(GSC_VERIFICATION ? { verification: { google: GSC_VERIFICATION } } : {}),
};

// Pre-paint theme script: runs synchronously before first paint to apply
// the stored theme class and avoid flash of wrong theme. Reads the same
// localStorage key as apps/landing for continuity across the LB cutover.
const THEME_SCRIPT = `(function(){try{var t=localStorage.getItem('wpmgr-landing-theme');if(t==='dark')document.documentElement.classList.add('dark');}catch(e){}})();`;

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        {/* Pre-paint theme: synchronous inline script avoids FOUC */}
        <script dangerouslySetInnerHTML={{ __html: THEME_SCRIPT }} />
      </head>
      <body
        className={`${ibmPlexSans.variable} ${ibmPlexSansLatinExt.variable} ${ibmPlexMono.variable} antialiased`}
        style={{ fontFamily: BODY_FONT_STACK, fontFeatureSettings: '"cv11","ss01","ss03"' }}
      >
        {/* Global reduced-motion gate: Motion honours user OS preference */}
        <MotionConfig reducedMotion="user">
          {children}
        </MotionConfig>
        <PostHogPageViews />
        <TrackSignupClicks />
        {/* Root JSON-LD: Organization + WebSite */}
        <JsonLd data={buildOrganizationLd()} />
        <JsonLd data={buildWebSiteLd()} />
        {/* Loads nothing when the measurement ID is absent, which is the case
            for every build except ours. next/third-parties handles App Router
            client-side navigation, which a raw gtag snippet does not. */}
        {analyticsEnabled.ga && <GoogleAnalytics gaId={GA_MEASUREMENT_ID} />}
      </body>
    </html>
  );
}
