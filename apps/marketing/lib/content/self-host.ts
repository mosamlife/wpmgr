// /self-host page content.
//
// THIS PAGE EXISTS SO SELF-HOSTING CAN STOP BEING A BUTTON. Every feature and
// solution template used to close with "Self-host it" beside "Star on GitHub",
// which put two off-site exits next to the signup on 22 templates, at the
// moment of highest intent. None of the seven comparable projects does that.
//
// It is not a demotion. Self-hosting is the reason the AGPL posture is
// credible, and pretending it does not exist would be a betrayal of it. The
// difference is between not SELLING it and not MENTIONING it. It stays in the
// FAQs, the pricing note, the footer, the header, and here, on a page that
// treats it seriously.
//
// The page states the trade in both directions, which the site never did. As
// presented before, hosted Free was strictly dominated by self-host on every
// axis the site emphasised, which makes the hosted plan look like a worse
// version of free rather than a different operating model.

import { SITE_CONFIG } from "@/lib/site";

export const SELF_HOST = {
  metaTitle: "Self-host WPMgr: run the whole stack yourself",
  metaDescription:
    "WPMgr is AGPL-3.0 and self-hosting is free for any number of sites. What you run, what it costs in time, and how it differs from the hosted tier.",
  hero: {
    heading: "Self-host WPMgr",
    subhead:
      "Free, unlimited sites, every feature. The control plane is AGPL-3.0 and the agent is MIT, so you can read every line before you run it.",
    command: "curl -fsSL https://raw.githubusercontent.com/mosamlife/wpmgr/main/scripts/quickstart-selfhost.sh | bash",
    commandNote:
      "One command on a 64-bit Linux host with Docker 24 or newer. The script fetches every file the stack needs, generates the secrets, and prints the command that brings it up. 2 GB of RAM is enough to start.",
  },
  // The sentence the site was missing. Stated as a fact about who operates the
  // server, not as a feature difference, because it is not a feature
  // difference: the software is the same.
  tradeoff: {
    heading: "What you take on",
    body: "Self-hosting is free and unlimited. It means you run a Postgres database, a control plane and an encoder, you keep them patched, and you own your backup storage. Hosted means we run all of that and you connect a site in a minute. The software is the same either way, and so is the feature set. The difference is who operates the server.",
  },
  runs: {
    heading: "What you are running",
    items: [
      {
        icon: "Server",
        title: "Control plane",
        desc: "A Go binary and the React dashboard. Database migrations apply themselves on boot.",
      },
      {
        icon: "Database",
        title: "Postgres",
        desc: "The fleet's own database, with row level security enforced per tenant.",
      },
      {
        icon: "ImageDown",
        title: "Media encoder",
        desc: "A pull worker for image and font optimization. Optional if you do not use it.",
      },
      {
        icon: "Lock",
        title: "Object storage",
        desc: "Any S3-compatible bucket you control, holding backups you hold the keys to.",
      },
    ],
  },
  reality: {
    heading: "The honest version",
    items: [
      "Updates are a pull and a restart. Migrations run themselves when the control plane boots.",
      "You are the one paged when the database fills up, and the one who restores it.",
      "There is no site limit, no feature gate and no licence key. Nothing checks in with us.",
      "If we stopped tomorrow you would still have the source, the running system and your data.",
    ],
  },
  cta: {
    heading: "Read it before you run it",
    body: "The control plane, the dashboard and the agent are all public. So is every release.",
    primary: { label: "Quickstart on GitHub", href: `${SITE_CONFIG.github}#quickstart-self-host` },
    secondary: { label: "System requirements", href: `${SITE_CONFIG.github}/blob/main/docs/requirements.md` },
  },
} as const;
