"use client";

import { useState, useEffect } from "react";
import { Icon } from "@/components/ui/icon";
import { Logo } from "@/components/ui/logo";
import { Button } from "@/components/ui/button";
import { Container } from "@/components/ui/primitives";
import { cn } from "@/lib/utils";
import { SITE_CONFIG, signupHref } from "@/lib/site";
import { MegaMenu } from "@/components/nav/mega-menu";
import { MobileNav } from "@/components/nav/mobile-nav";

function ThemeToggle() {
  const [theme, setTheme] = useState<"light" | "dark">("light");

  useEffect(() => {
    const stored = localStorage.getItem(SITE_CONFIG.themeStorageKey);
    const initial = stored === "dark" ? "dark" : "light";
    setTheme(initial);
    document.documentElement.classList.toggle("dark", initial === "dark");
  }, []);

  function toggle() {
    const next = theme === "dark" ? "light" : "dark";
    setTheme(next);
    document.documentElement.classList.toggle("dark", next === "dark");
    try {
      localStorage.setItem(SITE_CONFIG.themeStorageKey, next);
    } catch {
      // storage blocked; toggle still works for this session.
    }
  }

  return (
    <button
      type="button"
      onClick={toggle}
      aria-label={`Switch to ${theme === "dark" ? "light" : "dark"} mode`}
      className="inline-flex h-9 w-9 cursor-pointer items-center justify-center rounded-[var(--radius)] border border-[var(--border)] bg-card text-[var(--muted-foreground)] transition-colors duration-[var(--duration-fast)] hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
    >
      <Icon name={theme === "dark" ? "Sun" : "Moon"} size={17} />
    </button>
  );
}

export function SiteHeader() {
  return (
    <header className="sticky top-0 z-40 border-b border-[var(--border)] bg-[var(--background)]/85 backdrop-blur-md">
      <Container className="flex h-16 items-center justify-between gap-4">
        {/* Logo */}
        <a
          href="/"
          className="flex items-center gap-2.5 cursor-pointer focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm"
          aria-label="WPMgr home"
        >
          <Logo />
        </a>

        {/* Desktop megamenu (hidden on mobile) */}
        <MegaMenu />

        {/* Actions */}
        <div className="flex items-center gap-2">
          <a
            href={SITE_CONFIG.github}
            target="_blank"
            rel="noreferrer noopener"
            aria-label="WPMgr on GitHub"
            className={cn(
              "hidden h-9 w-9 cursor-pointer items-center justify-center",
              "rounded-[var(--radius)] border border-[var(--border)] bg-card",
              "text-[var(--muted-foreground)] transition-colors duration-[var(--duration-fast)]",
              "hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)]",
              "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
              "md:inline-flex",
            )}
          >
            <Icon name="Github" size={17} />
          </a>
          <ThemeToggle />
          <Button href="/self-host" variant="secondary" className="hidden md:inline-flex">
            Self-host it
          </Button>
          <Button href={signupHref("header")} className="hidden md:inline-flex">
            Get started free
          </Button>

          {/* Mobile hamburger (rendered by MobileNav; visible only below md) */}
          <MobileNav />
        </div>
      </Container>
    </header>
  );
}
