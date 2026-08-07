"use client";

// Mobile accordion drawer. Activated by a hamburger button; full-height
// modal-ish overlay with focus trap. Features and Solutions expand as
// accordions. Pricing / Resources / Docs are plain links.
//
// A11y: same W3C APG Disclosure pattern as the desktop megamenu (NOT
// role=menu). Focus is trapped inside the open drawer; Escape / close button
// returns focus to the hamburger. Tap targets are >=48px.

import { useState, useEffect, useRef, useCallback } from "react";
import { createPortal } from "react-dom";
import { AnimatePresence, motion } from "motion/react";
import { cn } from "@/lib/utils";
import { Icon } from "@/components/ui/icon";
import { signupHref } from "@/lib/site";
import {
  NAV_ITEMS,
  FEATURES_COLUMNS,
  SOLUTIONS_COLUMNS,
  type PanelId,
} from "./nav-data";

// ---------------------------------------------------------------------------
// Focus trap utility
// ---------------------------------------------------------------------------

const FOCUSABLE_SELECTORS = [
  "a[href]",
  "button:not([disabled])",
  "input:not([disabled])",
  "select:not([disabled])",
  "textarea:not([disabled])",
  '[tabindex]:not([tabindex="-1"])',
].join(",");

function trapFocus(drawerEl: HTMLElement, e: KeyboardEvent) {
  if (e.key !== "Tab") return;
  const nodes = Array.from(
    drawerEl.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTORS),
  ).filter((el) => !el.closest("[hidden]") && el.offsetParent !== null);
  if (nodes.length === 0) return;
  const first = nodes[0] as HTMLElement;
  const last = nodes[nodes.length - 1] as HTMLElement;
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first.focus();
  }
}

// ---------------------------------------------------------------------------
// Motion constants
// ---------------------------------------------------------------------------

const DRAWER_MOTION = {
  initial: { opacity: 0, x: "100%" },
  animate: { opacity: 1, x: 0 },
  exit: { opacity: 0, x: "100%" },
  transition: {
    duration: 0.24,
    ease: [0.22, 1, 0.36, 1] as [number, number, number, number],
  },
};

const ACCORDION_MOTION = {
  initial: { opacity: 0, height: 0 },
  animate: { opacity: 1, height: "auto" },
  exit: { opacity: 0, height: 0 },
  transition: { duration: 0.2, ease: [0.22, 1, 0.36, 1] as [number, number, number, number] },
};

// ---------------------------------------------------------------------------
// MobileNav
// ---------------------------------------------------------------------------

export function MobileNav() {
  const [isOpen, setIsOpen] = useState(false);
  const [openAccordion, setOpenAccordion] = useState<PanelId | null>(null);
  const [mounted, setMounted] = useState(false);
  const hamburgerRef = useRef<HTMLButtonElement>(null);
  const drawerRef = useRef<HTMLDivElement>(null);

  // The drawer is portaled to document.body so its `position: fixed` is
  // relative to the viewport. The header sets `backdrop-filter`, which makes it
  // a containing block for fixed descendants; rendering the drawer inside it
  // would clip the drawer to the 64px header box.
  useEffect(() => {
    setMounted(true);
  }, []);

  const close = useCallback(() => {
    setIsOpen(false);
    setOpenAccordion(null);
  }, []);

  // Focus trap and Escape
  useEffect(() => {
    if (!isOpen) return;

    // Move focus into the drawer on open
    const firstFocusable = drawerRef.current?.querySelector<HTMLElement>(
      FOCUSABLE_SELECTORS,
    );
    firstFocusable?.focus();

    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") {
        close();
        hamburgerRef.current?.focus();
        return;
      }
      if (drawerRef.current) trapFocus(drawerRef.current, e);
    }
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [isOpen, close]);

  // Prevent body scroll when open
  useEffect(() => {
    if (isOpen) {
      document.body.style.overflow = "hidden";
    } else {
      document.body.style.overflow = "";
    }
    return () => {
      document.body.style.overflow = "";
    };
  }, [isOpen]);

  function toggleAccordion(id: PanelId) {
    setOpenAccordion((prev) => (prev === id ? null : id));
  }

  return (
    <>
      {/* Hamburger button */}
      <button
        ref={hamburgerRef}
        type="button"
        aria-label={isOpen ? "Close navigation" : "Open navigation"}
        aria-expanded={isOpen}
        aria-controls="mobile-nav-drawer"
        onClick={() => setIsOpen((prev) => !prev)}
        className={cn(
          "flex md:hidden h-10 w-10 items-center justify-center",
          "rounded-[var(--radius)] border border-[var(--border)] bg-card",
          "text-[var(--muted-foreground)] transition-colors duration-[var(--duration-fast)]",
          "hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)]",
          "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
        )}
      >
        <Icon name={isOpen ? "X" : "Menu"} size={18} strokeWidth={1.75} />
      </button>

      {/* Drawer (portaled to body to escape the header backdrop-filter
          containing block that would otherwise clip the fixed drawer) */}
      {mounted &&
        createPortal(
          <AnimatePresence>
            {isOpen && (
              <>
                {/* Scrim */}
            <motion.div
              initial={{ opacity: 0 }}
              animate={{ opacity: 1 }}
              exit={{ opacity: 0 }}
              transition={{ duration: 0.2 }}
              className="fixed inset-0 z-40 bg-[var(--foreground)]/20 md:hidden"
              aria-hidden
              onClick={close}
            />

            {/* Slide-in drawer */}
            <motion.div
              ref={drawerRef}
              id="mobile-nav-drawer"
              {...DRAWER_MOTION}
              className={cn(
                "fixed right-0 top-0 z-50 flex h-full w-80 max-w-[calc(100vw-3rem)] flex-col",
                "border-l border-[var(--border)] bg-card shadow-[var(--shadow-xl)] md:hidden",
              )}
              role="dialog"
              aria-modal="true"
              aria-label="Navigation menu"
            >
              {/* Drawer header */}
              <div className="flex h-16 shrink-0 items-center justify-between border-b border-[var(--border)] px-5">
                <span className="text-sm font-semibold text-foreground">
                  Navigation
                </span>
                <button
                  type="button"
                  onClick={close}
                  aria-label="Close navigation"
                  className={cn(
                    "flex h-10 w-10 items-center justify-center",
                    "rounded-[var(--radius)] text-[var(--muted-foreground)]",
                    "hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)]",
                    "transition-colors duration-[var(--duration-fast)]",
                    "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                  )}
                >
                  <Icon name="X" size={18} strokeWidth={1.75} />
                </button>
              </div>

              {/* Scrollable nav */}
              <nav
                className="flex-1 overflow-y-auto py-3"
                aria-label="Mobile navigation"
              >
                {NAV_ITEMS.map((item) => {
                  if (item.kind === "panel") {
                    const isExpanded = openAccordion === item.id;
                    const accordionBodyId = `mobile-accordion-${item.id}`;
                    const columns =
                      item.id === "features"
                        ? FEATURES_COLUMNS
                        : SOLUTIONS_COLUMNS;

                    return (
                      <div key={item.id} className="border-b border-[var(--border)]">
                        {/* Accordion trigger */}
                        <button
                          type="button"
                          aria-expanded={isExpanded}
                          aria-controls={accordionBodyId}
                          onClick={() => toggleAccordion(item.id)}
                          className={cn(
                            "flex w-full items-center justify-between px-5 py-4",
                            "text-left text-sm font-medium cursor-pointer",
                            "transition-colors duration-[var(--duration-fast)]",
                            "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                            "min-h-[48px]",
                            isExpanded
                              ? "text-foreground"
                              : "text-[var(--muted-foreground)] hover:text-foreground",
                          )}
                        >
                          {item.label}
                          <Icon
                            name="ChevronDown"
                            size={16}
                            strokeWidth={2}
                            className={cn(
                              "transition-transform duration-[var(--duration-fast)] ease-[var(--ease-out-quint)]",
                              isExpanded && "rotate-180",
                            )}
                          />
                        </button>

                        {/* Accordion body */}
                        <AnimatePresence initial={false}>
                          {isExpanded && (
                            <motion.div
                              id={accordionBodyId}
                              {...ACCORDION_MOTION}
                              className="overflow-hidden"
                            >
                              <div className="pb-3">
                                {item.id === "features"
                                  ? (columns as typeof FEATURES_COLUMNS).map(
                                      (col) => (
                                        <div key={col.id} className="px-4 pt-3">
                                          <p className="mb-1.5 px-2 text-xs font-semibold uppercase tracking-[0.1em] text-[var(--muted-foreground)]">
                                            {col.name}
                                          </p>
                                          {col.rows.map((row) => (
                                            <a
                                              key={row.href}
                                              href={row.href}
                                              onClick={close}
                                              className={cn(
                                                "flex min-h-[48px] items-start gap-3 rounded-[var(--radius)] px-2 py-2.5",
                                                "text-sm transition-colors duration-[var(--duration-fast)]",
                                                "hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                                              )}
                                            >
                                              <span
                                                className="mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-md bg-[var(--primary-subtle)]"
                                                aria-hidden
                                              >
                                                <Icon
                                                  name={row.icon}
                                                  size={14}
                                                  className="text-[var(--primary)]"
                                                  strokeWidth={1.75}
                                                />
                                              </span>
                                              <span>
                                                <span className="block font-medium text-foreground leading-tight">
                                                  {row.title}
                                                </span>
                                                <span className="mt-0.5 block text-xs text-[var(--muted-foreground)]">
                                                  {row.summary}
                                                </span>
                                              </span>
                                            </a>
                                          ))}
                                        </div>
                                      ),
                                    )
                                  : (columns as typeof SOLUTIONS_COLUMNS).map(
                                      (col) => (
                                        <div key={col.label} className="px-4 pt-3">
                                          <p className="mb-1.5 px-2 text-xs font-semibold uppercase tracking-[0.1em] text-[var(--muted-foreground)]">
                                            {col.label}
                                          </p>
                                          {col.rows.map((row) => (
                                            <a
                                              key={row.href}
                                              href={row.href}
                                              onClick={close}
                                              className={cn(
                                                "flex min-h-[48px] items-start gap-3 rounded-[var(--radius)] px-2 py-2.5",
                                                "text-sm transition-colors duration-[var(--duration-fast)]",
                                                "hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                                              )}
                                            >
                                              <span
                                                className="mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-md bg-[var(--primary-subtle)]"
                                                aria-hidden
                                              >
                                                <Icon
                                                  name={row.icon}
                                                  size={14}
                                                  className="text-[var(--primary)]"
                                                  strokeWidth={1.75}
                                                />
                                              </span>
                                              <span>
                                                <span className="block font-medium text-foreground leading-tight">
                                                  {row.title}
                                                </span>
                                                <span className="mt-0.5 block text-xs text-[var(--muted-foreground)]">
                                                  {row.summary}
                                                </span>
                                              </span>
                                            </a>
                                          ))}
                                        </div>
                                      ),
                                    )}

                                {/* Panel footer */}
                                <div className="mt-2 px-6">
                                  <a
                                    href={item.href}
                                    onClick={close}
                                    className={cn(
                                      "inline-flex min-h-[44px] items-center gap-1.5",
                                      "text-sm font-medium text-[var(--primary)]",
                                      "hover:text-[var(--primary-hover)] transition-colors duration-[var(--duration-fast)]",
                                      "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm",
                                    )}
                                  >
                                    View all {item.label.toLowerCase()}
                                    <Icon
                                      name="ArrowRight"
                                      size={14}
                                      strokeWidth={2}
                                    />
                                  </a>
                                </div>
                              </div>
                            </motion.div>
                          )}
                        </AnimatePresence>
                      </div>
                    );
                  }

                  // Simple link
                  return (
                    <a
                      key={item.href}
                      href={item.href}
                      onClick={close}
                      {...(item.external
                        ? { target: "_blank", rel: "noreferrer noopener" }
                        : {})}
                      className={cn(
                        "flex min-h-[48px] items-center border-b border-[var(--border)] px-5",
                        "text-sm font-medium text-[var(--muted-foreground)]",
                        "transition-colors duration-[var(--duration-fast)] hover:text-foreground",
                        "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                      )}
                    >
                      {item.label}
                      {item.external && (
                        <Icon
                          name="ArrowRight"
                          size={12}
                          strokeWidth={2}
                          className="ml-1.5 rotate-[-45deg] text-[var(--muted-foreground)]"
                        />
                      )}
                    </a>
                  );
                })}
              </nav>

              {/* CTA footer (the conversion buttons live here on mobile, not
                  crammed into the header bar) */}
              <div className="shrink-0 border-t border-[var(--border)] p-4 flex flex-col gap-2.5">
                <a
                  href={signupHref("mobile-nav")}
                  onClick={close}
                  className={cn(
                    "flex min-h-[48px] items-center justify-center rounded-[var(--radius)]",
                    "bg-[var(--primary)] px-4 text-sm font-semibold text-[var(--primary-foreground)]",
                    "transition-colors duration-[var(--duration-fast)] hover:bg-[var(--primary-hover)]",
                    "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                  )}
                >
                  Get started free
                </a>
                <a
                  href="/self-host"
                  onClick={close}
                  className={cn(
                    "flex min-h-[48px] items-center justify-center gap-2 rounded-[var(--radius)]",
                    "border border-[var(--border)] bg-card px-4 text-sm font-medium text-foreground",
                    "transition-colors duration-[var(--duration-fast)] hover:bg-[var(--accent)]",
                    "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                  )}
                >
                  Self-host it
                </a>
              </div>
            </motion.div>
          </>
        )}
          </AnimatePresence>,
          document.body,
        )}
    </>
  );
}
