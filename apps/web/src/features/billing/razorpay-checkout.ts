// Razorpay Checkout.js loader for the in-app subscription checkout modal
// (M16 Phase B). Checkout.js is a third-party script Razorpay hosts itself
// (no first-party npm package ships this browser widget), so this loader
// fetches it ONCE, on demand, the instant the operator actually picks the
// Razorpay path — never a blocking <script> tag in index.html that would
// load a payment-provider script for every visitor regardless of which
// provider (or none) they end up choosing.
//
// CSP FLAG (frontend cannot fix this itself — see this feature's report):
// the app's Content-Security-Policy must allow Razorpay's own domains before
// this script can load or its modal can talk back to Razorpay's API.
// `loadRazorpayCheckout()` will reject with a script-load error under the
// current CSP until nginx.conf is updated.

const CHECKOUT_JS_SRC = "https://checkout.razorpay.com/v1/checkout.js";

/**
 * The exact payload Razorpay's Checkout.js `handler` option hands the
 * browser on a successful payment — field names verbatim, matching
 * POST /api/v1/billing/checkout/verify's request body (see use-billing.ts's
 * `RazorpayCheckoutSuccess`) so a caller can pass it straight through.
 */
export interface RazorpayHandlerResponse {
  razorpay_payment_id: string;
  razorpay_subscription_id: string;
  razorpay_signature: string;
}

export interface RazorpayCheckoutOptions {
  /** Razorpay's PUBLIC key id (Checkout.js's "key" option) — never the key secret. */
  key: string;
  subscription_id: string;
  /** Smallest-unit amount (paise for INR, cents for USD) — Razorpay's own Plan remains authoritative for what is actually charged. */
  amount?: number;
  currency?: string;
  name: string;
  description?: string;
  prefill?: { email?: string; name?: string };
  theme?: { color?: string };
  handler: (response: RazorpayHandlerResponse) => void;
  modal?: {
    /** Fires when the operator closes the modal without paying — a normal cancel, never an error. */
    ondismiss?: () => void;
  };
}

export interface RazorpayInstance {
  open: () => void;
  close?: () => void;
}

export interface RazorpayConstructor {
  new (options: RazorpayCheckoutOptions): RazorpayInstance;
}

declare global {
  interface Window {
    Razorpay?: RazorpayConstructor;
  }
}

let loadPromise: Promise<RazorpayConstructor> | null = null;

/**
 * Loads Razorpay's Checkout.js ONCE, on demand, and resolves with the
 * `window.Razorpay` constructor. Safe to call repeatedly — returns the same
 * in-flight/resolved promise rather than appending a second `<script>` tag.
 * Rejects (never throws synchronously) on a network error, CSP block, or ad
 * blocker, so a caller can show a fallback message instead of an uncaught
 * exception deep inside a click handler.
 */
export function loadRazorpayCheckout(): Promise<RazorpayConstructor> {
  if (typeof window !== "undefined" && window.Razorpay) {
    return Promise.resolve(window.Razorpay);
  }
  if (loadPromise) return loadPromise;

  loadPromise = new Promise<RazorpayConstructor>((resolve, reject) => {
    const script = document.createElement("script");
    script.src = CHECKOUT_JS_SRC;
    script.async = true;
    script.addEventListener(
      "load",
      () => {
        if (window.Razorpay) {
          resolve(window.Razorpay);
        } else {
          loadPromise = null;
          reject(
            new Error(
              "Razorpay's checkout script loaded but window.Razorpay is unavailable.",
            ),
          );
        }
      },
      { once: true },
    );
    script.addEventListener(
      "error",
      () => {
        loadPromise = null;
        reject(new Error("Could not load the Razorpay checkout script."));
      },
      { once: true },
    );
    document.head.appendChild(script);
  });

  return loadPromise;
}

/**
 * Test-only escape hatch: clears the module-level load cache so a test can
 * simulate "not yet loaded" again after a prior test populated it.
 */
export function __resetRazorpayCheckoutLoaderForTests(): void {
  loadPromise = null;
}
