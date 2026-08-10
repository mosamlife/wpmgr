// Comparison page: ManageWP vs MainWP vs WPMgr.
//
// TWO RULES, AND THEY ARE NOT THE SAME RULE. See lib/content/types.ts.
//   What a CELL says must be true and sourced. Every claim below carries the
//   URL it came from and the date it was checked, and the page renders both.
//   Which ROWS appear is positioning and is ours to choose.
//
// A ROW IS A CLAIM WE ARE MAKING, so a row only exists where BOTH competitors
// are sourced on that axis. Page and object caching is a real WPMgr strength
// and is deliberately ABSENT from the matrix: we hold no sourced statement
// about ManageWP on caching, and "not stated" in a competitor cell reads as
// "they do not have it", which we cannot support. It is argued in the
// consolidation section instead, where the claim is only about us.
//
// The claims came from four passes: gather, adversarial audit, correction,
// publication gate. The audit caught a fabricated quotation in the first
// draft. Do not add a claim here without a source you fetched yourself, and
// re-check verifiedOn when prices move.
//
// A `cites` id IS the footnote the reader opens, and getting it wrong is
// invisible: the link still resolves, to a real claim about something else.
// Fourteen cells shipped that way. scripts/check-copy.mjs now rejects an id
// that does not exist and an id borrowed from the other product's column, but
// it cannot know that mw-20 is the wrong ManageWP claim for a cell about Safe
// Updates. Read the claim you are citing.

import type { ComparisonPageData } from "@/lib/content/types";

export const MANAGEWP_VS_MAINWP: ComparisonPageData = {
  slug: "managewp-vs-mainwp-vs-wpmgr",
  title: "ManageWP vs MainWP vs WPMgr",
  metaTitle: "ManageWP vs MainWP vs WPMgr: One Dashboard, Compared",
  metaDescription:
    "ManageWP, MainWP and WPMgr compared on where the dashboard runs, where your backups land, and what the bill does as the fleet grows. Every figure sourced and dated.",
  targetQuery: "managewp vs mainwp",
  hero: {
    heading: "ManageWP vs MainWP vs WPMgr",
    subhead:
      "The feature lists overlap. What does not overlap is where the dashboard runs, where your backups land, and what the bill does when the fleet grows.",
    chips: [
      "AGPL-3.0 control plane",
      "MIT agent, listed on WordPress.org",
      "Self-hosted: unlimited sites, no licence fee",
    ],
  },
  products: [
    {
      key: "managewp",
      name: "ManageWP",
      url: "https://managewp.com/",
      claims: [
      {
        id: "mw-01",
        topic: "pricing",
        claim:
          "The pricing page presents the free plan under the heading \"Free on unlimited websites, forever\", introduced by the paragraph \"Our free tier is packed with awesomeness! No setup hassle, no hidden fees. 24/7 support. Lightning fast dashboard that performs well with hundreds of websites, free Monthly Backup, and more. We take care of everything, so you can take care of your websites.\" It then lists fifteen included items: Monthly Cloud Backup, Clone & Migrate, Safe Updates, Security Check, Performance Check, Client Reports, Google Analytics, Maintenance Mode, Code Snippets, 1-click Login, Manage Comments, Local Sync, \"Manage Updates, Plugins & Themes\", Collaborate with Teams & Clients, and Template Builder. Clone & Migrate and Safe Updates each carry the footnote \"*requires premium backup\".",
        sourceUrl: "https://managewp.com/pricing/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-02",
        topic: "pricing",
        claim:
          "Premium capabilities are sold as individual add-ons priced per website per month rather than as tiered plans. The pricing FAQ answer to \"How are premium add-ons billed?\" reads in full: \"Per-website, per add-on: You only pay for the add-ons you activate on each website. Each premium add-on is priced monthly, per website. You’re billed at the end of the month, based on your actual usage (no upfront payments).\"",
        sourceUrl: "https://managewp.com/pricing/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-03",
        topic: "pricing",
        claim:
          "The premium add-on section does not show two side-by-side price columns. It shows one price set at a time, controlled by a single switch whose two labels read \"Per website\" and \"Bundle\". The switch is served checked, which selects Bundle, and every one of the nine add-on cards is served carrying data-price=\"bundle\", with the stylesheet rule .managewp-price-card__price-tags-wrapper[data-price=bundle] .managewp-price-card__price-tag-single{display:none} hiding the per-website amount. A visitor therefore sees bundle pricing on arrival and must flip the switch to \"Per website\" to see the per-site amounts. Directly beneath the switch the page reads: \"For agencies with over 25 websites, a fixed monthly fee for up to 100 websites. Prices shown on a monthly basis, with each bundle covering up to 100 websites. Bundles can stack.\"",
        sourceUrl: "https://managewp.com/pricing/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-04",
        topic: "pricing",
        claim:
          "Eight add-ons carry both a bundle price and a per-website price. In the default Bundle view, in the order the page lists them, the flat monthly fee for up to 100 websites is: Backups $75, Uptime Monitor $25, Automated Security Check $25, Automated Performance Check $25, Advanced Client Reports $25, White Label $25, SEO Ranking $25, and Link Monitor $25. After switching to \"Per website\", the same eight cards show a monthly price per website: Backups $2, and $1 each for Uptime Monitor, Automated Security Check, Automated Performance Check, Advanced Client Reports, White Label, SEO Ranking, and Link Monitor. Amounts are printed with a bare \"$\" symbol.",
        sourceUrl: "https://managewp.com/pricing/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-05",
        topic: "pricing",
        claim:
          "Vulnerability Protection is the only one of the nine add-ons with no bundle price. It is not visible at all in the default Bundle view: the page carries the rule body.page-id-109 .post-101:has(.managewp-price-card__price-tags-wrapper[data-price=\"bundle\"]){display: none !important;} and post-101 is the Vulnerability Protection card. Flipping the switch to \"Per website\" reveals it at $2 per website per month, and its card carries only that one amount, with no bundle figure.",
        sourceUrl: "https://managewp.com/pricing/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-06",
        topic: "pricing",
        claim:
          "Below the nine add-on cards sits a separate card reading \"All-in-one package\" and \"(all bundles combined)\" with a price of $150. That card holds a single price element rather than the two-value structure the add-on cards use, and no stylesheet rule keys off its price state, so $150 is what a visitor sees whichever way the switch is set. No website count is printed on the card itself; the up-to-100-websites scope comes from the paragraph beneath the switch, which reads \"For agencies with over 25 websites, a fixed monthly fee for up to 100 websites. Prices shown on a monthly basis, with each bundle covering up to 100 websites. Bundles can stack.\"",
        sourceUrl: "https://managewp.com/pricing/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-07",
        topic: "pricing",
        claim:
          "All figures on the pricing page are monthly, and the page presents no annual price. It states \"Monthly payment cycle, no setup fee, no termination penalties or long term contracts.\" The FAQ adds, answering \"What if I manage many websites?\": \"If you manage a large number of websites, our Bundles offer a flat monthly fee that covers Premium tools for up to 100 websites a month, helping agencies and larger teams save even more.\"",
        sourceUrl: "https://managewp.com/pricing/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-08",
        topic: "pricing",
        claim:
          "The Uptime Monitor feature page repeats the add-on price in two labelled blocks. One is labelled \"Regular Price\" and \"Per website / Month\" and shows $1. The other is labelled \"Bundle Price\" and \"Up to 100 websites / Month\" and shows $25. (The labels and the amounts sit in separate page elements, so they are described here rather than quoted as one sentence.)",
        sourceUrl: "https://managewp.com/features/uptime-monitor/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-09",
        topic: "pricing",
        claim:
          "The Client Report feature page prices two versions on separate cards. The Free Client Report card ends with \"$0\" followed by \"/ per month\". The Premium Client Report card ends with \"$1\" followed by \"/ per month (or $25/month for up to 100 sites)\". Only the premium card carries the bundle qualifier; the free card does not. In both cases the amount and the qualifier sit in separate page elements, so the rows are described here rather than quoted as one string.",
        sourceUrl: "https://managewp.com/features/client-report/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-10",
        topic: "hosting",
        claim:
          "ManageWP is a vendor-hosted dashboard. The component required on each managed WordPress site is the ManageWP Worker connector plugin, which connects the site to the ManageWP service. The plugin's own installation instructions direct the user to \"Create an account on ManageWP.com\" and then \"Follow the steps to add your first website\", and its changelog entry for version 4.3.0 reads \"New: More secure and flexible communication between the Worker plugin and the ManageWP servers.\" The optional premium Vulnerability Protection add-on additionally installs a second plugin on the site (see the Vulnerability Protection claim below).",
        sourceUrl: "https://wordpress.org/plugins/worker/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-11",
        topic: "hosting",
        claim:
          "Backup data is held by the vendor. The backup user guide states: \"Backups are stored on our servers for 90 days (or 30 days if you deactivate the tool and 7 days if you deactivate the website).\"",
        sourceUrl: "https://managewp.com/guide/backup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-12",
        topic: "hosting",
        claim:
          "Customers can select the storage region for backups. The backup feature page states, under the heading \"GDPR-ready storage\": \"Choose US or EU data centers for every website, ensuring compliance and complete control over your data’s location.\" The same page lists among the free tier's features \"Off-site storage with the option to choose between US or EU data centers\".",
        sourceUrl: "https://managewp.com/features/backup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-13",
        topic: "hosting",
        claim:
          "ManageWP has been owned by GoDaddy since 2016. The company's about page states: \"On September 6, 2016, we joined forces with GoDaddy, a company that shares our enthusiasm of product quality and customer satisfaction.\"",
        sourceUrl: "https://managewp.com/about/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-14",
        topic: "install-count",
        claim:
          "The WordPress.org plugin API reports the ManageWP Worker plugin (slug \"worker\") with active_installs of 1,000,000, a rating of 92 out of 100, and 677 total ratings. The ratings breakdown is 602 five-star, 11 four-star, 7 three-star, 7 two-star, and 50 one-star.",
        sourceUrl: "https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&request%5Bslug%5D=worker",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-15",
        topic: "install-count",
        claim:
          "The plugin's WordPress.org listing page displays, in its Meta panel, \"Version 4.9.35\", \"Last updated 3 weeks ago\", \"Active installations 1+ million\", \"WordPress version 3.1 or higher\", and \"Tested up to 7.0.3\", and lists 36 available languages. Its ratings panel displays \"4.6 out of 5 stars.\" and \"602 5-star reviews\".",
        sourceUrl: "https://wordpress.org/plugins/worker/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-16",
        topic: "install-count",
        claim:
          "The WordPress.org plugin API reports the ManageWP Worker plugin at version 4.9.35, last updated 2026-07-16, tested up to WordPress 7.0.3, requiring WordPress 3.1 or higher, and first added to the directory on 2011-03-10. The API's requires_php field is false, meaning no minimum PHP version is declared. The API lists the plugin's homepage as https://managewp.com and its author as the WordPress.org profile managewp.",
        sourceUrl: "https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&request%5Bslug%5D=worker",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-17",
        topic: "install-count",
        claim:
          "ManageWP's about page displays the figures \"+2M Websites managed with ManageWP\", \"+150K Work hours saved each day\", and \"+65K Loyal customers who swear by ManageWP\". The page also states: \"By January 2012 ManageWP was officially released, and within a month 100,000 websites were being managed with our service.\"",
        sourceUrl: "https://managewp.com/about/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-18",
        topic: "licensing",
        claim:
          "The ManageWP Worker connector plugin is licensed under the GNU General Public License v3 or later. Its readme, as returned by the WordPress.org plugin API, states: \"ManageWP Worker is free software: you can redistribute it and/or modify it under the terms of the GNU General Public License as published by the Free Software Foundation, either version 3 of the License, or (at your option) any later version.\"",
        sourceUrl: "https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&request%5Bslug%5D=worker",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-19",
        topic: "features",
        claim:
          "Backups are incremental after the first run. The backup feature page states, under the heading \"Smart, incremental backups\": \"Only changed files and tables are processed after the first full run. This keeps your sites fast and your server load minimal.\"",
        sourceUrl: "https://managewp.com/features/backup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-20",
        topic: "features",
        claim:
          "Backups are encrypted. The backup feature page states, under the heading \"Total security\": \"All backups are encrypted in transit and at rest, so your data is protected at every step.\"",
        sourceUrl: "https://managewp.com/features/backup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-21",
        topic: "features",
        claim:
          "Free accounts get monthly scheduled backups, and higher frequencies require the premium add-on. The backup user guide states: \"With ManageWP, you can create free monthly backups of your WordPress site, or adjust the backup frequency to weekly, daily, every 12h, every 6h, or every 1h with the premium version.\" It adds: \"Premium users can also run on-demand backups and download backup files.\" and \"By default, only core WordPress files and database tables are backed up.\"",
        sourceUrl: "https://managewp.com/guide/backup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-22",
        topic: "features",
        claim:
          "The Free Backup card on the backup feature page ends its bullet list with \"Backups stored for 90 days\", followed by the price \"$0\" and \"/ per month\". The same free list contains \"Monthly scheduled backups\", \"Off-site storage with the option to choose between US or EU data centers\", \"One-click restore to recover your site quickly\", \"The ability to exclude specific files and folders from your backups\", and \"Notifications via Email or Slack if your site goes down\". So the documented 90 day retention applies to the free tier as well as the premium one.",
        sourceUrl: "https://managewp.com/features/backup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-23",
        topic: "features",
        claim:
          "Cloning and migration require the paid Backup add-on. The Clone user guide states: \"Please note that you must have premium Backups enabled to use the Clone tool.\" Describing cloning to an existing connected site, it says: \"This is an easy way to create staging and production environments.\"",
        sourceUrl: "https://managewp.com/guide/clone/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-24",
        topic: "features",
        claim:
          "Safe Updates provides update rollback and requires the paid Backup add-on. The guide states: \"Then we take a screenshot of your site before the updates, run the updates, check your site’s status code, and take another screenshot after the updates.\" It also states: \"Because we create an on-demand restore point, you must have premium Backups enabled to use Safe Updates.\"",
        sourceUrl: "https://managewp.com/guide/safe-updates/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-25",
        topic: "features",
        claim:
          "Uptime monitoring is a paid add-on. The feature page's bullets read, in full and with ManageWP's own em dashes preserved: \"Checks all your websites every 60 seconds to make sure they’re online and responsive.\", \"Choose to get notified by Email, SMS, or Slack—whatever fits your workflow.\", \"Double-checks any outage before notifying you—so you only get real alerts, never false alarms.\", and \"See uptime percentage, response times, and a detailed check history, all in one place.\"",
        sourceUrl: "https://managewp.com/features/uptime-monitor/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-26",
        topic: "features",
        claim:
          "Security Check reports rather than remediates. The feature page carries this note, quoted in full with ManageWP's own em dash preserved: \"Security Check is a messenger, not a cleaner—it alerts you, but does not remove malware.\" Its free bullets include \"Detect malware, blacklist status, and vulnerabilities\", \"Review detailed reports and scan history\", and \"Flag site errors and outdated software\" (ManageWP's wording). Scheduling is premium only: \"Schedule automatic Security Checks (daily or weekly)\" and \"Get real-time notifications via Email or Slack if issues are detected\".",
        sourceUrl: "https://managewp.com/features/security-check/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-27",
        topic: "features",
        claim:
          "Vulnerability Protection is a premium add-on that places a second plugin on the managed site. The guide states: \"If you upgrade to Vulnerability Protection (premium add-on), ManageWP will install the Patchstack Security plugin on your site and automatically activate three security modules: Rapid Mitigation (virtual patches), Advanced Hardening (attack prevention rules), and Community IP Blocklist (blocks malicious IPs), neutralizing threats at the application level even while you’re waiting for an official security update to become available.\" It also states \"Detection is automatically enabled on all sites in your ManageWP dashboard.\" and \"Protection requires activation per site.\"",
        sourceUrl: "https://managewp.com/guide/vulnerabilities/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-28",
        topic: "features",
        claim:
          "Performance Check uses two scoring engines. The feature page states, under the heading \"Dual-engine accuracy\": \"Scans your sites with both Google PageSpeed and Yahoo! YSlow rulesets for deeper, data-backed insights.\" The premium tier adds \"Schedule automatic Performance Checks (daily or weekly)\", \"Receive instant notifications via email or Slack if your site underperforms\", and \"Choose your preferred scan region for more precise results\", and is priced on that page at \"$1\" then \"/ per month (or $25/month for up to 100 sites)\".",
        sourceUrl: "https://managewp.com/features/performance-scan/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-29",
        topic: "features",
        claim:
          "Client reports are available free, including a customizable front page, logo branding, a \"Custom Work Section to highlight your unique value\", \"Ready-made templates for quick setup\", \"PDF download for easy sharing\", \"WooCommerce stats\", and \"Localization in 25+ languages\". Free reports carry a \"ManageWP watermark and sent from a ManageWP email address\". The premium tier adds \"Remove the ManageWP watermark for a fully white-labeled look\", \"Send reports from your own email address\", \"Automate report delivery (weekly, bi-weekly, or monthly scheduling)\", \"Bulk generate and send reports to multiple clients at once\", and \"Advanced customization: change colors, fonts, header/footer, and section order\".",
        sourceUrl: "https://managewp.com/features/client-report/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-30",
        topic: "features",
        claim:
          "The dashboard includes an Optimization widget for database housekeeping. The getting started guide states: \"Use the Optimization widget to tidy up your website’s post revisions and spam comments, as well as MB Overhead.\" It adds: \"You can choose what you wish to optimize and what websites to perform this action on or select Optimize All.\" and \"To change the number of post revisions to keep, adjust this option in Advanced Settings\". The guide makes no statement about whether the widget is free or paid.",
        sourceUrl: "https://managewp.com/guide/getting-started/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-31",
        topic: "features",
        claim:
          "SEO Ranking is a premium add-on covering keyword position tracking and competitor comparison. Its feature page states, in full: \"SEO Ranking brings all your keyword metrics and competitor insights into one clear, easy-to-read list, so you can make informed decisions without digging through spreadsheets or hunting for data.\" Its bullets include \"Tracks your keyword positions: Instantly see how your keywords are performing across all your sites, with clear up/down movement each week\".",
        sourceUrl: "https://managewp.com/features/seo-ranking/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-32",
        topic: "features",
        claim:
          "White Label is listed on the pricing page as its own add-on, separate from and in addition to Advanced Client Reports, and the two carry identical prices: $25 per month for up to 100 websites in the default Bundle view, and $1 per website per month after switching the price control to \"Per website\".",
        sourceUrl: "https://managewp.com/pricing/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-33",
        topic: "limits",
        claim:
          "There is no cap on the number of websites on the free tier. The pricing page heads the free plan \"Free on unlimited websites, forever\".",
        sourceUrl: "https://managewp.com/pricing/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-34",
        topic: "limits",
        claim:
          "Backup retention on ManageWP servers is 90 days by default, dropping to 30 days if the backup tool is deactivated and 7 days if the website is deactivated. The backup user guide states: \"Backups are stored on our servers for 90 days (or 30 days if you deactivate the tool and 7 days if you deactivate the website).\"",
        sourceUrl: "https://managewp.com/guide/backup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-35",
        topic: "limits",
        claim:
          "The ManageWP user guide index organises guides by feature and includes no guide for an API. The string \"API\" does not appear anywhere in the text of that index page, which runs to roughly 3,000 characters. This describes the index only and is not a statement about whether an API exists.",
        sourceUrl: "https://managewp.com/guide/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mw-36",
        topic: "support",
        claim:
          "Support is offered to free users. The plugin readme's FAQ, answering \"Do you offer support for free users?\", states: \"Yes. No matter if you’re free or premium user, we are here for you 24/7.\" The pricing page's free tier paragraph likewise contains the sentence \"24/7 support.\"",
        sourceUrl: "https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&request%5Bslug%5D=worker",
        verifiedOn: "2026-08-06",
      },
      ],
    },
    {
      key: "mainwp",
      name: "MainWP",
      url: "https://mainwp.com/",
      claims: [
      {
        id: "mn-01",
        topic: "pricing",
        claim:
          "MainWP's pricing page shows two paid products alongside a free tier, not three paid options. Both paid cards are titled \"MAINWP PRO\": one carries the subtitle \"Yearly\" at 199 USD with the meta line \"Billed Yearly\", or the subtitle \"Monthly \" at 29 USD with the meta line \"Billed Monthly\", and the other carries the subtitle \"Lifetime\" at 599 USD with the meta lines \"One-Time Payment\" and \"Always the Best Price - Never Discounted.\" The 29 and the 199 are the same tier under one Monthly/Yearly control, so only one of them is on screen at a time. The Yearly view is the default: the page ships both tables in its markup, its site-wide custom stylesheet (wp-content/uploads/bricks/css/global-custom-css.min.css) contains the rule #mainwp-pricing-table-monthly { display:none; }, and the inline script only swaps the two tables on click. A visitor therefore sees 199 USD unless they click Monthly. Both Pro cards list the bullet \"Manage Unlimited Websites\", so the price is flat and not per site.",
        sourceUrl: "https://mainwp.com/signup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-02",
        topic: "pricing",
        claim:
          "The 199 USD yearly figure is a promotional price. MainWP's own product page for the yearly subscription embeds Product structured data with two unit prices in USD: 199.00 as the offer price and 249.00 carrying priceType https://schema.org/ListPrice, both marked valueAddedTaxIncluded false, with priceValidUntil 2027-12-31. MainWP's public store endpoint for the same product (https://mainwp.com/wp-json/wc/store/v1/products/347661) reports on_sale true, regular_price 24900 and sale_price 19900 at currency_minor_unit 2, and renders the pair as \"Original price was: $249.00.\" and \"Current price is: $199.00.\" A visitor is quoted 199 USD per year by default; 249 USD is the list value behind it. The monthly product (2900) and the Lifetime product (59900) report on_sale false, so neither carries a separate list price.",
        sourceUrl: "https://mainwp.com/add-on/the-bundle-yearly-membership/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-03",
        topic: "pricing",
        claim:
          "The currency is US dollars, established from MainWP's own embedded configuration rather than from any label beside the prices. On the pricing page each amount renders as a bare dollar prefix span followed by the number, giving $29, $199 and $599 with no currency code next to them. The two USD tokens in the document are both first-party configuration: the WooCommerce analytics block reports \"store_currency\":\"USD\" and the AffiliateWP tracking block reports \"currency\":\"USD\". The Product structured data on the individual plan pages independently gives priceCurrency USD.",
        sourceUrl: "https://mainwp.com/signup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-04",
        topic: "pricing",
        claim:
          "MainWP's free tier is named \"ESSENTIALS\", is priced \"Free\", and is subtitled \"Everything you need to get started\". Its card lists five items: \"All Free Add-ons\", \"Any Future Free Add-ons\", \"Critical Security & Performance Updates\", \"Community Support\", and a fifth bullet that reads \"Manage Unlimited SItes\" [sic, capital I in the source]. The two Pro cards list \"All 30+ Existing Pro Add-ons\", \"All Future Add-ons\", \"Critical Security & Performance Updates\", \"Priority Ticket Support\" and \"Manage Unlimited Websites\".",
        sourceUrl: "https://mainwp.com/signup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-05",
        topic: "pricing",
        claim:
          "The Essentials versus Pro comparison table on the same pricing page lists the row \"Number of Websites to Manage\" as Unlimited in both the FREE and the PRO column. The table also words the add-on entitlement as \"Access to all 33+ Existing & New Premium Add-ons with Priority Support\" in its desktop layout and as \"Access to all 33+ Existing & New Premium Extensions with Priority Support\" in its stacked layout, while the Pro cards higher up the same page say \"All 30+ Existing Pro Add-ons\". Support is listed as \"Expert Support via Ticket, Community, & WP forum\" for both columns.",
        sourceUrl: "https://mainwp.com/signup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-06",
        topic: "pricing",
        claim:
          "MainWP frames its model against subscription platforms on the pricing page itself. The page heading reads \"WordPress ® Management without the SaaS Pricing\", with the registered trademark symbol pulled tight against the word by a negative margin so a visitor sees it as WordPress followed immediately by the symbol, a section is headed \"Why should you choose MainWP over a SaaS solution?\", and the first block under it is headed \"No SaaS-Style Pricing\" and reads \"We don’t charge like a SaaS because we aren’t one. Enjoy a simple, straightforward pricing model with our extension bundles.\"",
        sourceUrl: "https://mainwp.com/signup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-07",
        topic: "pricing",
        claim:
          "The pricing page FAQ states a refund and an upgrade policy. On refunds it says \"we offer a 30-day money-back guarantee\" and that a customer who is unhappy and makes contact within 30 days of the initial transaction is refunded. On upgrades it describes a \"90 Day Pro Upgrade Guarantee\": a Monthly subscriber who moves to the Yearly or Lifetime plan within the first three months can receive credit for up to three months of initial payments, the credit is not applied automatically, a coupon code must be requested from support, and the offer cannot be combined with other discounts. The same FAQ confirms the Lifetime price at 599 USD.",
        sourceUrl: "https://mainwp.com/signup/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-08",
        topic: "pricing",
        claim:
          "MainWP's individual Pro add-on pages present the add-on as \"Available in the Pro Bundle\" and show no price in the page copy, but the same pages embed Product structured data with an individual USD price and availability https://schema.org/InStock: 69.00 for the MainWP Pro Reports Extension and 39.00 for the MainWP Maintenance Extension. MainWP's public store endpoint reports both products as purchasable and in stock at those prices. We therefore do not claim that Pro add-ons are unavailable separately.",
        sourceUrl: "https://mainwp.com/add-on/pro-reports/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-09",
        topic: "pricing",
        claim:
          "MainWP states \"We accept PayPal and Stripe.\" as its payment types. Account creation runs through a checkout for the free bundle, of which MainWP says \"There is no charge for the free bundle, and your account is created as soon as the order is completed.\" On renewal after a lapse, MainWP states that a member who cancels and later rejoins pays the current price at that time, and the higher rate if prices have increased.",
        sourceUrl: "https://docs.mainwp.com/getting-started/pre-install-faq",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-10",
        topic: "hosting",
        claim:
          "MainWP is self-hosted. Its own FAQ describes the product as \"MainWP is a free and open-source WordPress management system that lets you manage multiple WordPress sites from one self-hosted Dashboard.\" and describes three parts: the Dashboard plugin installed on a dedicated WordPress site, the Child plugin installed on each managed site, and optional extensions. The same page answers the meaning of self-hosted with \"Your MainWP Dashboard is hosted on your own WordPress installation, not on our servers.\"",
        sourceUrl: "https://docs.mainwp.com/getting-started/pre-install-faq",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-11",
        topic: "hosting",
        claim:
          "MainWP states that site data stays in the customer's own database: \"Your Dashboard runs on your WordPress installation, your data stays in your database, and all communication happens directly between your servers.\" A bullet on the same page reads \"No data passes through MainWP servers at any point\". The page also states \"MainWP is released under the GPLv3 license, with all source code available on GitHub.\"",
        sourceUrl: "https://docs.mainwp.com/getting-started/why-does-self-hosted-and-open-source-matter",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-12",
        topic: "hosting",
        claim:
          "On telemetry MainWP states \"MainWP stable releases include no telemetry or phone-home functionality.\" and \"The free MainWP Dashboard and Child plugins include no telemetry or tracking by default. MainWP does not know who installs these plugins or where they are installed.\" Three optional third-party services (Usetiful interactive guides, Chatbase chat support, YouTube embeds) are listed as disabled by default and toggleable under MainWP Tools. The same page carries one carve-out: \"MainWP v6 Early Access builds include optional, limited telemetry to help improve the product before stable release.\", which it says can be disabled at any time.",
        sourceUrl: "https://docs.mainwp.com/getting-started/are-you-guys-watching-what-i-am-doing",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-13",
        topic: "hosting",
        claim:
          "For customers who buy add-ons, MainWP states \"If you purchase MainWP Add-ons, your MainWP Dashboard domain name and IP address are collected periodically to verify license ownership and API key validity. Child site information is never collected.\" It adds that this data is required for licence verification and is collected only from users who purchase add-ons.",
        sourceUrl: "https://docs.mainwp.com/getting-started/are-you-guys-watching-what-i-am-doing",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-14",
        topic: "hosting",
        claim:
          "MainWP's published system requirements page opens with \"MainWP runs on standard WordPress hosting that meets these requirements.\" and gives minimums of WordPress 6.2, WordPress memory limit 40MB, PHP 8.1, PHP safe mode disabled, PHP max execution time 30 seconds, PHP memory limit 256MB, cURL enabled with a 60 second timeout, and MySQL 5.0. Recommended values are latest WordPress, WordPress memory limit 256MB, PHP 8.3, 300 second execution time, PHP memory limit 1024MB, 300 second cURL timeout, and MySQL 5.0+. Only the separate \"Additional Requirements\" section is scoped \"For the Dashboard server\", covering the operating system open file limit, ignore_user_abort, and required PHP functions. Note that this page states a PHP 8.1 minimum while both plugin readme headers on WordPress.org state Requires PHP 7.4; both figures are MainWP's own, published on different surfaces.",
        sourceUrl: "https://docs.mainwp.com/advanced/miscellaneous/mainwp-system-requirements",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-15",
        topic: "hosting",
        claim:
          "MainWP states that the system is for self-hosted WordPress sites only: \"No, the MainWP system is for self-hosted WordPress sites only. WordPress.com sites cannot install custom plugins required for MainWP to function.\"",
        sourceUrl: "https://docs.mainwp.com/getting-started/pre-install-faq",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-16",
        topic: "install-count",
        claim:
          "The MainWP Child connector plugin (slug mainwp-child) reports 700,000 active installs on the WordPress.org plugin API, with a rating of 100 out of 100 across 70 ratings (69 five-star, 1 two-star), version 6.1.6, last updated 2026-08-05, tested up to WordPress 7.0.2, requires WordPress 6.2, requires PHP 7.4, and first added to the directory on 2013-08-27.",
        sourceUrl: "https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&request[slug]=mainwp-child",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-17",
        topic: "install-count",
        claim:
          "The MainWP Dashboard plugin (slug mainwp) reports 20,000 active installs on the WordPress.org plugin API, with a rating of 98 out of 100 across 2,344 ratings (2,248 five-star, 72 four-star, 9 three-star, 5 two-star, 10 one-star), version 6.1.6, last updated 2026-08-05, tested up to WordPress 7.0.2, requires WordPress 6.2, requires PHP 7.4, and first added to the directory on 2014-02-26.",
        sourceUrl: "https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&request[slug]=mainwp",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-18",
        topic: "install-count",
        claim:
          "MainWP publishes its own headline figure on its homepage, which a visitor reads as \"Trusted by 20000+ site owners managing 700000+ sites!\" with both numbers set in bold. That is MainWP counting site owners and sites under management, which is a different measure from the WordPress.org active install counts even though the two headline numbers coincide. For install counts we cite the WordPress.org plugin API, because active installs are measured there rather than stated by the vendor.",
        sourceUrl: "https://mainwp.com/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-19",
        topic: "licensing",
        claim:
          "The MainWP Dashboard plugin's WordPress.org readme header declares \"License: GPLv3 or later\" with \"License URI: https://www.gnu.org/licenses/gpl-3.0.html\", alongside Requires at least 6.2, Requires PHP 7.4 and Stable tag 6.1.6. Its title line reads \"MainWP Dashboard: Self-hosted WordPress Management for Agencies\".",
        sourceUrl: "https://plugins.svn.wordpress.org/mainwp/trunk/readme.txt",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-20",
        topic: "licensing",
        claim:
          "The MainWP Child plugin's WordPress.org readme header declares \"License: GPLv3 or later\" with \"License URI: https://www.gnu.org/licenses/gpl-3.0.html\", alongside Requires at least 6.2, Requires PHP 7.4 and Stable tag 6.1.6. Its title line reads \"MainWP Child - Securely Connects to the MainWP Dashboard to Manage Multiple Sites\".",
        sourceUrl: "https://plugins.svn.wordpress.org/mainwp-child/trunk/readme.txt",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-21",
        topic: "licensing",
        claim:
          "The MainWP Dashboard source repository on GitHub (mainwp/mainwp) is published under the GNU General Public License v3.0, reported as GPL-3.0 by the repository's own licence metadata.",
        sourceUrl: "https://github.com/mainwp/mainwp",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-22",
        topic: "licensing",
        claim:
          "The MainWP Child source repository on GitHub (mainwp/mainwp-child) is published under the GNU General Public License v3.0, reported as GPL-3.0 by the repository's own licence metadata.",
        sourceUrl: "https://github.com/mainwp/mainwp-child",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-23",
        topic: "licensing",
        claim:
          "MainWP states \"All extensions are licensed under the GPL, just like WordPress. Extensions can be used on unlimited sites.\" and that \"Support and update licenses are separate and valid for the duration of your MainWP membership (Monthly, Yearly, or Lifetime).\" On multiple Dashboards it states \"Yes. Extensions are installed on the MainWP Dashboard (not child sites), and you can install them on as many Dashboards as you have.\" and it states that extensions may be used for client work without violating the licence.",
        sourceUrl: "https://docs.mainwp.com/getting-started/pre-install-faq",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-24",
        topic: "licensing",
        claim:
          "On cancellation MainWP states: \"If you cancel, extensions continue working with the last version you had—you just lose access to updates and support.\" It also states \"The Lifetime membership does not expire.\" (The dash inside that first sentence is MainWP's own punctuation, reproduced as published.)",
        sourceUrl: "https://docs.mainwp.com/getting-started/pre-install-faq",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-25",
        topic: "features",
        claim:
          "MainWP ships no native backup engine and says so directly: \"MainWP focuses on site management rather than building a native backup system.\" The page gives two alternatives instead, backup extensions that connect established backup providers, and API Backups that trigger a backup at the hosting provider. On the second it carries the heading \"API Backups (Free)\" and states that API Backups \"trigger backups directly through your hosting provider at no additional cost\".",
        sourceUrl: "https://docs.mainwp.com/sites/backups/why-doesnt-mainwp-include-backups-in-the-dashboard",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-26",
        topic: "features",
        claim:
          "MainWP's backup extensions drive third-party backup plugins. The documentation lists five in a plain table with no tier labels: BackWPup, BackupBuddy, Time Capsule, UpdraftPlus and WPVivid. Incremental behaviour, storage destinations and encryption are properties of whichever third-party plugin is driven, not of MainWP.",
        sourceUrl: "https://docs.mainwp.com/sites/backups/manage-backups",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-27",
        topic: "features",
        claim:
          "The tier labels for those backup add-ons live on MainWP's add-ons listing, not in the documentation. As shown on the All Add-ons tab on 2026-08-06, \"BackWPup Integration\", \"MainWP Solid Backups Extension\", \"MainWP Time Capsule Extension\" and \"MainWP UpdraftPlus Extension\" each carry the label Essential, and \"WPvivid Backup for MainWP\" carries the labels Third-Party and Free. Note the naming difference on MainWP's own surfaces: the listing calls it \"MainWP Solid Backups Extension\" while the documentation table still says \"BackupBuddy\" for the same add-on at /add-on/mainwpbuddy/.",
        sourceUrl: "https://mainwp.com/mainwp-add-ons/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-28",
        topic: "features",
        claim:
          "MainWP API Backups run against the host rather than the site: \"API Backups lets you create backups directly through your hosting provider, cloud manager, or VPS without installing backup plugins on your sites.\" The supported provider table lists cPanel (\"Native and WP Toolkit backups\"), Plesk (\"WP Toolkit backups\"), Kinsta, Cloudways (\"Automatic site assignment\"), GridPane (\"Requires Owner Account with Developer Plan or higher\"), Vultr, Akamai (Linode) and DigitalOcean. The same page states that MainWP API Backups do not use custom storage locations and that backups are stored wherever the host or cloud manager has configured.",
        sourceUrl: "https://docs.mainwp.com/sites/backups/manage-backups",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-29",
        topic: "features",
        claim:
          "MainWP has automatic update rollback, introduced in MainWP 5.1 and integrating with the WordPress core rollback available in WordPress 6.3 and above. It is automatically active on all update pages, triggers when an update fails, restores the previous version automatically, and shows a Dashboard notice. MainWP states plainly: \"MainWP does not support on-demand rollback to previous versions.\" The suggested alternatives are downloading a previous version from wordpress.org and installing by upload, or storing premium plugin versions in the Favorites Extension.",
        sourceUrl: "https://docs.mainwp.com/sites/updates/does-mainwp-have-safe-updates",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-30",
        topic: "features",
        claim:
          "Uptime monitoring is built into the MainWP Dashboard. Documented defaults: Enable Uptime Monitoring is Disabled, Monitor Type is HTTP(s) with Ping and Keyword Monitoring as the alternatives, Method is HEAD, Monitor Interval is 60m on a slider running 5m, 10m, 15m, 30m, 45m and then 1h through 24h, Timeout is 60s, Down Confirmation Check is Enabled, and the up-status codes default to 200,201,202,203,204,205,206. A separate Site Health Monitoring section reads the WordPress core Site Health result from each child site. Scheduled checks require WP Cron or a server cron.",
        sourceUrl: "https://docs.mainwp.com/sites/management/uptime-monitoring",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-31",
        topic: "features",
        claim:
          "MainWP lists uptime monitoring among its free features, with the bullets \"Track uptime and downtime across all connected sites\", \"Choose from HTTP, Ping, and Keyword-based monitors\", \"Receive email alerts if a site goes down\" and \"No third-party service required\".",
        sourceUrl: "https://mainwp.com/mainwp-free-features/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-32",
        topic: "features",
        claim:
          "The separate MainWP Advanced Uptime Monitor Integration is a different design from the built-in monitor, and is free. Its page carries the labels Essential, \"Available in the Free Bundle\" and \"Available in the Pro Bundle\", and its FAQ answers \"Yes, the Advanced Uptime Monitor is completely free for all MainWP users.\" Rather than monitoring directly, it drives third-party services: \"This extension integrates with top monitoring platforms like Uptime Robot, NodePing, Site24x7, and Better Uptime.\" Its Additional Info states \"This extension requires API Key from one of the supported API integrations.\"",
        sourceUrl: "https://mainwp.com/add-on/advanced-uptime-monitor/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-33",
        topic: "features",
        claim:
          "Client reporting is a Pro add-on. The MainWP Pro Reports Extension page carries the label Pro and the line \"Available in the Pro Bundle\". Its Additional Info notes that the add-on requires the MainWP Child Reports plugin to be installed on child sites, that the Child Reports plugin needs PHP 7.4 or higher, and that it can be installed on sites directly from the WordPress.org plugin repository.",
        sourceUrl: "https://mainwp.com/add-on/pro-reports/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-34",
        topic: "features",
        claim:
          "Database cleanup is a Pro add-on. The MainWP Maintenance Extension page carries the label Pro and the line \"Available in the Pro Bundle\". It describes removing post revisions, auto drafts, trash posts, transients and spam comments across multiple sites, scheduled runs, and one-click database optimisation. Its Additional Info carries the caveat \"The Database optimization feature is not recommended for child sites that are running on MariaDB database\".",
        sourceUrl: "https://mainwp.com/add-on/maintenance/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-35",
        topic: "features",
        claim:
          "Staging is a Pro add-on. The MainWP Staging Extension page carries the label Pro and the line \"Available in the Pro Bundle\", and states that it integrates with the WP Staging plugin, described on the page as \"a free WordPress plugin essential for creating staging sites\". Pushing changes back to live is not available through the extension. Asked \"Can I use the MainWP Staging Extension to push changes from my staging site to my live site?\", MainWP answers: \"No. For pushing changes from Staging to Live sites, WP Staging Pro plugin is required, but MainWP Staging Extension works only with the free version.\" The same page states there is no limit to the number of staging sites that can be created.",
        sourceUrl: "https://mainwp.com/add-on/staging/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-36",
        topic: "features",
        claim:
          "Security scanning is split across free and paid add-ons on MainWP's own listing, as labelled on the All Add-ons tab on 2026-08-06. Labelled Essential: MainWP Vulnerability Checker Extension, Jetpack Protect Integration, MainWP Sucuri Extension, Patchstack Integration. Labelled Pro: Wordfence Integration, Solid Security Integration, Jetpack Scan Integration, Virusdie Integration. The performance and caching add-ons MainWP Cache Control Integration, WP Rocket Integration and Lighthouse Integration each carry the label Pro.",
        sourceUrl: "https://mainwp.com/mainwp-add-ons/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-37",
        topic: "features",
        claim:
          "MainWP's own performance category page lists four add-ons on 2026-08-06, and they are not uniformly paid. MainWP Cache Control Integration, MainWP Maintenance Extension and WP Rocket Integration each carry the label Pro, and WP Compress for MainWP carries the labels Third-Party and Free. Lighthouse Integration is not filed under this category, and MainWP Maintenance Extension is, so MainWP's category taxonomy and our own reading of the add-on labels do not line up exactly.",
        sourceUrl: "https://mainwp.com/mainwp-add-ons/add-on-category/performance/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-38",
        topic: "features",
        claim:
          "The MainWP Vulnerability Checker Extension depends on an external vulnerability database: it \"uses either the free MainWP NVD API or the paid WPScan Vulnerability Database API to bring you information about vulnerable plugins and themes on your Child Sites\". Its page carries the labels Essential, \"Available in the Free Bundle\" and \"Available in the Pro Bundle\", and it documents an \"Ignore\" function for dismissing false positives.",
        sourceUrl: "https://mainwp.com/add-on/vulnerability-checker/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-39",
        topic: "features",
        claim:
          "Counting the labels on MainWP's own add-ons listing on 2026-08-06, the All Add-ons tab holds 64 cards: 38 labelled Pro, 13 labelled Essential, and 13 labelled Third-Party, with every Third-Party card also carrying a Free tag. The page's only grouping control is a three tab set whose tabs read \"All Add-ons\", \"Extensions\" and \"Integrations\"; Essential, Pro and Third-Party are labels printed on each card, not filters. On the same date the Extensions tab held 23 cards (20 Pro, 3 Essential) and the Integrations tab held 40 (17 Pro, 10 Essential, 13 Third-Party).",
        sourceUrl: "https://mainwp.com/mainwp-add-ons/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-40",
        topic: "features",
        claim:
          "MainWP's free features page lists, at no cost: bulk or individual updates for WordPress core, plugins, themes and translations plus automatic updates and \"Check for abandoned plugins & themes\"; plugin and theme install, activate, deactivate and delete; \"1-Click access to sites (Password-less login)\"; site tags, notes, export and import, and a site health check; uptime monitoring; client profiles with tags, the ability to create your own client fields, and suspend or unsuspend; a cost tracker; user management including \"Update administrator passwords\"; post and page publishing, editing, unpublishing and deletion; and MainWP Browser Extension support connecting through the MainWP REST API.",
        sourceUrl: "https://mainwp.com/mainwp-free-features/",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-41",
        topic: "features",
        claim:
          "MainWP ships a REST API v2 at /wp-json/mainwp/v2/ with Bearer token authentication, key scopes of Read for GET and Write & Delete for POST, PUT, PATCH and DELETE, an optional v1 compatibility mode that issues a legacy Consumer Key and Secret, and a global /batch endpoint. Endpoint categories are sites, clients, tags, updates, costs, users, settings, monitoring, API keys, posts, pages and batch. A machine-readable OpenAPI 3.1.0 description of the full v2 API is published at https://raw.githubusercontent.com/mainwp/docs/main/api-reference/openapi.yaml, and MainWP directs integrators to \"Use the MainWP Postman collection as the source of truth for request and response schemas.\"",
        sourceUrl: "https://docs.mainwp.com/api-reference/rest-api/overview",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-42",
        topic: "features",
        claim:
          "MainWP publishes an MCP Server, described on its overview page as a way to \"Manage your WordPress network by talking to Claude, Cursor, or any MCP-capable AI assistant.\" The page states it is \"a small program that runs on your own computer, alongside your AI tool\" and that \"Nothing new is installed on your Dashboard or your child sites; it talks to the Dashboard the same way your browser does, over HTTPS with credentials you control and can revoke at any time.\" On permissions it states that the MCP server \"exposes only the tools you permit and enforces that policy on every call\", and on safety that \"Destructive operations stop at a confirmation gate before anything runs.\"",
        sourceUrl: "https://docs.mainwp.com/mcp-server/overview",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-43",
        topic: "features",
        claim:
          "MainWP publishes per-client configuration for the MCP server, described on the page itself as \"Configuration for Claude Desktop, Claude Code, Cursor, VS Code Copilot, OpenAI Codex, ZenCoder, and other MCP hosts.\" Each client runs the same command, npx -y @mainwp/mcp, with the Dashboard URL, a WordPress username and a WordPress Application Password supplied through the environment, and the page also documents pointing separate server entries at separate credential folders for multiple Dashboards.",
        sourceUrl: "https://docs.mainwp.com/mcp-server/clients",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-44",
        topic: "features",
        claim:
          "The MainWP MCP server is open source. The mainwp/mainwp-mcp repository on GitHub is published under the GNU General Public License v3.0 (GPL-3.0) and is described there as \"MCP Server for MainWP Dashboard - Exposes MainWP Abilities API as MCP tools\". The hyphen in that description is a plain hyphen in the repository metadata, reproduced as published.",
        sourceUrl: "https://github.com/mainwp/mainwp-mcp",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-45",
        topic: "features",
        claim:
          "The Security Measures section of MainWP's security overview page lists six items: \"MainWP Dashboard and child sites communicate using OpenSSL encryption. If OpenSSL is unavailable or misconfigured, MainWP uses PHPSecLib as a fallback.\"; a single Dashboard lock, \"A child site can only connect to one MainWP Dashboard at a time.\"; \"WordPress passwords are not stored on the MainWP Dashboard.\"; regular security testing, \"Penetration tests are performed through white hat security programs on PatchStack and HackerOne.\"; in-house development; and self-hosted architecture. A later section of the same page documents a further mechanism: \"MainWP Child requires at least one connection authentication method for the initial connection: Password Authentication, Unique Security ID, or both.\" Note that on this page PHPSecLib is named as a transport fallback for OpenSSL, not as the cipher for stored credentials.",
        sourceUrl: "https://docs.mainwp.com/getting-started/how-secure-is-the-mainwp-plugin",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-46",
        topic: "features",
        claim:
          "Request signing is documented separately. On first connection the Dashboard generates a 2048 bit public and private key pair with openssl_pkey_new(), the public key is saved on both the child site and the Dashboard, and the private key is encrypted and saved only on the Dashboard. Every later request is signed with openssl_sign() and verified with openssl_verify(), and \"The child site only processes requests with valid signatures that match its stored public key.\" Requests carry the username, the function name and a mainwpsignature parameter, and MainWP escapes all request parameter values before sending.",
        sourceUrl: "https://docs.mainwp.com/advanced/miscellaneous/mainwp-connection-security",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-47",
        topic: "features",
        claim:
          "Stored third-party credentials use a different cipher again: \"MainWP uses AES GCM encryption via PHPSecLib to securely store third-party API keys and login credentials in the Dashboard database.\" MainWP says this was introduced in version 4.5, covers extension API keys, third-party service logins and internal authentication tokens, uses a 32 character key and a 16 character initialisation vector generated with the PHPSecLib Random class, and stores the encryption key in a separate Key File outside the database. It states explicitly that this does not cover data created by third-party plugins on child sites.",
        sourceUrl: "https://docs.mainwp.com/advanced/security/how-mainwp-stores-3rd-party-api-keys-and-other-sensitive-data",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-48",
        topic: "features",
        claim:
          "Private key encryption at rest arrived later: \"MainWP Dashboard version 5.3 introduces encryption for private keys stored in your database.\" Existing connections are encrypted through a one-time \"Encrypt Keys Now\" prompt after upgrading, new child sites have their private keys encrypted automatically, and the mechanism reuses the AES GCM framework introduced in 4.5 with the key held in a separate Key File.",
        sourceUrl: "https://docs.mainwp.com/advanced/openssl-keys-encryption",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-49",
        topic: "limits",
        claim:
          "There is no site cap on any MainWP tier. MainWP states \"You can manage an unlimited number of child sites. However, how many sites a single Dashboard can control depends on your hosting quality.\"",
        sourceUrl: "https://docs.mainwp.com/getting-started/pre-install-faq",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-50",
        topic: "limits",
        claim:
          "MainWP publishes hosting guidance by network size rather than a tested site ceiling: up to 30 sites on shared hosting (\"Standard shared plans work well\"), 31 to 100 sites on reseller hosting with additional server memory and the \"Optimize data loading\" setting enabled, and 100+ sites on a VPS with at least 512MB PHP and WordPress memory and the same setting enabled.",
        sourceUrl: "https://docs.mainwp.com/advanced/miscellaneous/mainwp-system-requirements",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-51",
        topic: "limits",
        claim:
          "Built-in monitoring history retention defaults to 180 days. The documented options are keep forever with no automatic cleanup, 30 days, 90 days, 180 days and 365 days. The same page notes a \"Maximum simultaneous uptime monitoring requests\" setting under Advanced Settings.",
        sourceUrl: "https://docs.mainwp.com/sites/management/uptime-monitoring",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-52",
        topic: "limits",
        claim:
          "MainWP does not officially support WordPress Multisite: \"We have reports from users that it works, but we do not officially test on or support WordPress Multisite installations.\" The same FAQ states \"We do not recommend direct customization of MainWP core code, and we do not support modified installations.\", pointing developers to its own extension documentation instead.",
        sourceUrl: "https://docs.mainwp.com/getting-started/pre-install-faq",
        verifiedOn: "2026-08-06",
      },
      {
        id: "mn-53",
        topic: "limits",
        claim:
          "MainWP's REST API documentation warns about reachability: \"Your MainWP Dashboard URL must be reachable from the system sending API requests. A local-only Dashboard is typically not reachable by external API clients.\" It also notes that the v2 Bearer token is shown once and cannot be revealed later, and that API access stays active while at least one API key is enabled.",
        sourceUrl: "https://docs.mainwp.com/api-reference/rest-api/overview",
        verifiedOn: "2026-08-06",
      },
      ],
    },
  ],
  // GROUP ORDER IS LOAD BEARING. Parity first: it costs nothing, and it answers
  // "the new one must be missing things" before a reader forms the thought.
  matrix: [
    {
      label: "All three do this",
      rows: [
        {
          label: "Bulk updates across sites",
          cells: {
            wpmgr: { value: "Yes, with a pre-update snapshot and rollback", tone: "included" },
            managewp: { value: "Yes. Safe Updates adds a restore point and needs premium Backups", tone: "included", cites: ["mw-24"] },
            mainwp: { value: "Yes, free, including automatic updates", tone: "included", cites: ["mn-40"] },
          },
        },
        {
          label: "Uptime monitoring",
          cells: {
            wpmgr: { value: "Built in and free, with TLS expiry checks", tone: "included" },
            managewp: { value: "Add-on: $1 per site per month, or $25 for up to 100 sites", tone: "paid", cites: ["mw-08"] },
            mainwp: { value: "Built in and free, off until enabled", tone: "included", cites: ["mn-30", "mn-31"] },
          },
        },
        {
          label: "Client reports",
          cells: {
            wpmgr: { value: "Built in, white label, with a client portal", tone: "included" },
            managewp: { value: "Free with a ManageWP watermark. Advanced is $1 per site per month", tone: "paid", cites: ["mw-09"] },
            mainwp: { value: "Pro Reports add-on, needs a second plugin on each site", tone: "paid", cites: ["mn-33"] },
          },
        },
        {
          label: "One-click login to wp-admin",
          cells: {
            wpmgr: { value: "Yes, single use and audited", tone: "included" },
            managewp: { value: "Yes, in the free tier", tone: "included", cites: ["mw-01"] },
            mainwp: { value: "Yes, in the free tier", tone: "included", cites: ["mn-40"] },
          },
        },
      ],
    },
    {
      label: "Where the dashboard runs",
      rows: [
        {
          label: "Who hosts the dashboard",
          cells: {
            wpmgr: { value: "You do. A hosted tier exists if you prefer", tone: "included" },
            managewp: { value: "The vendor. You install a connector plugin on each site", tone: "neutral", cites: ["mw-10"] },
            mainwp: { value: "You do, as a plugin on a WordPress site you control", tone: "included", cites: ["mn-11"] },
          },
        },
        {
          label: "Where backups are stored",
          cells: {
            wpmgr: { value: "Storage you choose, encrypted before it leaves the site", tone: "included" },
            managewp: { value: "The vendor's servers, with a US or EU region choice", tone: "neutral", cites: ["mw-11", "mw-12"] },
            mainwp: { value: "Wherever the third-party plugin you configure sends them", tone: "neutral", cites: ["mn-25", "mn-26"] },
          },
        },
        {
          label: "Source you can read",
          cells: {
            wpmgr: { value: "All of it. AGPL-3.0 control plane, MIT agent", tone: "included" },
            managewp: { value: "The connector plugin, GPLv3. The dashboard is not published", tone: "partial", cites: ["mw-18"] },
            mainwp: { value: "Dashboard and child plugin, GPLv3, on GitHub", tone: "included", cites: ["mn-21", "mn-22"] },
          },
        },
      ],
    },
    {
      label: "What the bill does as the fleet grows",
      rows: [
        {
          label: "Pricing shape",
          cells: {
            wpmgr: { value: "Free and unlimited when self-hosted", tone: "included" },
            managewp: { value: "Per site, per add-on, per month, with bundle options", tone: "paid", cites: ["mw-02", "mw-04"] },
            mainwp: { value: "Flat, for unlimited sites", tone: "paid", cites: ["mn-01"] },
          },
        },
        {
          label: "Cost of a backup at 25 sites",
          cells: {
            wpmgr: { value: "$0", tone: "included" },
            managewp: { value: "$50 a month at $2 per site, or $75 for the bundle", tone: "paid", cites: ["mw-04"] },
            mainwp: { value: "Included, but you supply the backup plugin", tone: "partial", cites: ["mn-25"] },
          },
        },
      ],
    },
    {
      label: "Built in, or assembled",
      rows: [
        {
          label: "Backup engine",
          cells: {
            wpmgr: { value: "Built in. Incremental, client-side encrypted", tone: "included" },
            managewp: { value: "Built in. Incremental, encrypted in transit and at rest", tone: "included", cites: ["mw-19", "mw-20"] },
            mainwp: { value: "None of its own. It drives third-party backup plugins", tone: "partial", cites: ["mn-25", "mn-26"] },
          },
        },
        {
          label: "Security scanning",
          cells: {
            wpmgr: { value: "Built in: hardening, file integrity, vulnerability matching", tone: "included" },
            managewp: { value: "Free check reports only. Vulnerability Protection is $2 per site per month and installs a second plugin", tone: "paid", cites: ["mw-05", "mw-26", "mw-27"] },
            mainwp: { value: "Add-ons, some free and some paid", tone: "paid", cites: ["mn-36"] },
          },
        },
        {
          label: "Database cleaning",
          cells: {
            wpmgr: { value: "Built in, with orphan classification and a preview", tone: "included" },
            managewp: { value: "An optimization widget in the dashboard", tone: "included", cites: ["mw-30"] },
            mainwp: { value: "The Maintenance add-on, labelled Pro", tone: "paid", cites: ["mn-34"] },
          },
        },
      ],
    },
  ],
  replaces: {
    heading: "One tool, or eight line items",
    subhead:
      "Backups, security, caching, images, database, uptime, reports and updates. On WPMgr these are one system with one login and one bill. Self-hosted, that bill is nothing.",
    items: [
      { icon: "DatabaseBackup", label: "Backups and restore" },
      { icon: "ShieldCheck", label: "Security and hardening" },
      { icon: "Zap", label: "Page and object cache" },
      { icon: "ImageDown", label: "Image optimization" },
      { icon: "Database", label: "Database cleaning" },
      { icon: "Activity", label: "Uptime monitoring" },
      { icon: "FileText", label: "Client reports" },
      { icon: "RefreshCw", label: "Updates and rollback" },
    ],
  },
  cost: {
    heading: "Move the slider to your fleet size",
    subhead:
      "Computed from each vendor's own published prices. ManageWP takes the cheaper of per site or bundle, including their all-in-one package. MainWP is flat. Self-hosted WPMgr is free at every size.",
    models: [
      {
        productKey: "managewp",
        label: "ManageWP, backups plus uptime plus reports",
        perSite: 4,
        bundle: 125,
        bundleCovers: 100,
        // The all-in-one package, which covers every bundle for $150 up to 100
        // websites (mw-06). It is dearer than the $125 of the three bundles
        // this row models, so it never wins today, and it is listed anyway: if
        // one of those three prices rises past it, the widget must charge the
        // package rather than quietly print a figure no customer would pay.
        alternatives: [{ amount: 150, covers: 100 }],
        period: "month",
        note: "Backups $2, uptime $1, advanced reports $1 per site per month. Bundles are $75 plus $25 plus $25 for up to 100 sites, and their all-in-one package covering every bundle is $150.",
        cites: ["mw-04", "mw-06"],
      },
      {
        productKey: "mainwp",
        label: "MainWP Pro",
        perSite: 0,
        flat: 199,
        period: "year",
        note: "Flat for unlimited sites. You still supply and configure a backup plugin.",
        cites: ["mn-01", "mn-25"],
      },
      {
        productKey: "wpmgr",
        label: "WPMgr, self-hosted",
        perSite: 0,
        flat: 0,
        period: "year",
        note: "Unlimited sites, every feature, no licence fee. You run the control plane.",
      },
    ],
  },
  locality: {
    heading: "Where your fleet data actually lives",
    subhead:
      "This is the one difference nobody closes by shipping a feature next quarter.",
    lanes: [
      {
        productKey: "wpmgr",
        path: ["Your sites", "Your control plane", "Your storage"],
        note: "Backups are encrypted on the site before they leave it, so the control plane only ever holds ciphertext, even when you use the hosted tier.",
      },
      {
        productKey: "managewp",
        path: ["Your sites", "Vendor dashboard", "Vendor storage"],
        note: "The dashboard and the backups are the vendor's, with a US or EU region choice.",
        cites: ["mw-10", "mw-11", "mw-12"],
      },
      {
        productKey: "mainwp",
        path: ["Your sites", "Your WordPress dashboard", "Plugin decides"],
        note: "The dashboard is yours. Where backups land depends on which third-party plugin you configure.",
        cites: ["mn-11", "mn-25", "mn-26"],
      },
    ],
  },
  faq: [
    {
      q: "Do I have to move everything at once?",
      a: "No. WPMgr connects a site by installing one plugin and pasting a pairing code, and it does not ask you to remove anything. Connect one site, leave the rest where they are, and compare on your own fleet before you decide.",
    },
    {
      q: "What happens to my sites if WPMgr goes away?",
      a: "The control plane is AGPL-3.0 and the agent is MIT, both public on GitHub. If we stopped tomorrow you would still hold the source, the running system and your data, and could keep operating or fork it. That is a different answer from a vendor promising to stay in business.",
    },
    {
      q: "Is self-hosting a lot of work?",
      a: "One command brings up the whole stack on a 64-bit Linux host with Docker. It needs 2 GB of RAM to start. Updates are a pull and a restart, and migrations apply themselves on boot.",
    },
    {
      q: "Where do backups go, and can you read them?",
      a: "To storage you choose, and no. Each backup is encrypted on the site with a key held by the site, so the control plane stores ciphertext it cannot open. That holds on the hosted tier too.",
    },
    {
      q: "How current are the figures on this page?",
      a: "Every claim carries the page it came from and the date it was checked, and all of them are listed on the sources page. Prices change; if one of these is stale, the link is how you find out.",
    },
  ],
};
