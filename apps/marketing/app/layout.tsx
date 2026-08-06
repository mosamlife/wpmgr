import type { Metadata, Viewport } from "next";
import { IBM_Plex_Sans, IBM_Plex_Mono } from "next/font/google";
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

const ibmPlexSans = IBM_Plex_Sans({
  weight: ["400", "500", "600", "700"],
  subsets: ["latin"],
  display: "swap",
  variable: "--font-ibm-plex-sans",
});

const ibmPlexMono = IBM_Plex_Mono({
  weight: ["400", "500"],
  subsets: ["latin"],
  display: "swap",
  variable: "--font-ibm-plex-mono",
});

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
        className={`${ibmPlexSans.variable} ${ibmPlexMono.variable} antialiased`}
        style={{ fontFamily: "var(--font-ibm-plex-sans, var(--font-sans))", fontFeatureSettings: '"cv11","ss01","ss03"' }}
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
