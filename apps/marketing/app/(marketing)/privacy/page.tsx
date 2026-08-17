import type { Metadata } from "next";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { LegalPage } from "@/components/templates/legal-page";
import { SITE_CONFIG } from "@/lib/site";
import { COMPANY, LEGAL_EFFECTIVE_DATE, LEGAL_CONTACT_HREF, PADDLE, STRIPE, RAZORPAY } from "@/lib/content/legal";

export const metadata: Metadata = buildMetadata({
  title: "Privacy Policy",
  description:
    "How WPMgr collects, uses, and protects data for the hosted service at manage.wpmgr.app, including sub-processors Google Cloud, Stripe, Razorpay, and Paddle.",
  canonical: "/privacy",
});

const mail = (
  <a
    href={LEGAL_CONTACT_HREF}
    className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
  >
    {COMPANY.supportEmail}
  </a>
);

const githubLink = (
  <a
    href={SITE_CONFIG.github}
    target="_blank"
    rel="noreferrer noopener"
    className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
  >
    {SITE_CONFIG.github}
  </a>
);

export default function PrivacyPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Legal", href: "/legal" },
    { name: "Privacy Policy", href: "/privacy" },
  ]);

  return (
    <>
      <JsonLd data={breadcrumb} />
      <LegalPage
        eyebrow="Legal"
        title="Privacy Policy"
        breadcrumbLabel="Privacy Policy"
        effectiveDateLabel={`Effective ${LEGAL_EFFECTIVE_DATE}`}
        intro={
          <>
            This Privacy Policy explains what personal data {COMPANY.legalName} ("WPMgr," "we,"
            "us") collects when you use the WPMgr hosted service at manage.wpmgr.app (the
            "Service"), why we collect it, and the choices you have. If you self-host the WPMgr
            control plane instead, this policy does not apply; your data stays entirely on your own
            infrastructure and you are the data controller.
          </>
        }
        sections={[
          {
            heading: "1. Information we collect",
            body: (
              <>
                <p>
                  <strong className="font-semibold text-foreground">Account information.</strong>{" "}
                  Your name, email address, and password (stored as a salted hash, never in plain
                  text), used to operate your account and send transactional email such as
                  verification, password reset, and alerts.
                </p>
                <p>
                  <strong className="font-semibold text-foreground">Connected-site metadata.</strong>{" "}
                  For each WordPress site you connect, the agent plugin reports the site URL,
                  WordPress, PHP, and server versions, the active theme and plugin inventory, and
                  Site Health diagnostics. This is used to power the dashboard, uptime and update
                  checks, and security scanning.
                </p>
                <p>
                  <strong className="font-semibold text-foreground">Backups.</strong> Backup
                  archives of your database and/or files are encrypted before they leave your
                  server and stored either on our managed infrastructure or on storage you configure
                  and control, depending on your plan.
                </p>
                <p>
                  <strong className="font-semibold text-foreground">Aggregate performance telemetry.</strong>{" "}
                  If you enable Real User Monitoring on a site, it collects anonymous Core Web
                  Vitals and page-load timing from that site&apos;s visitors: no page content, no
                  visitor names, emails, or other personally identifying information, and no
                  cookies. Query strings are stripped from recorded page paths before storage.
                </p>
                <p>
                  <strong className="font-semibold text-foreground">Billing information.</strong>{" "}
                  We do not collect or store your card details. Payment information is collected and
                  processed by whichever payment provider you choose at checkout: Razorpay, Stripe,
                  or Paddle. See section 3.
                </p>
                <p>
                  <strong className="font-semibold text-foreground">Operational logs.</strong>{" "}
                  Standard request and error logs needed to run, secure, and troubleshoot the
                  Service.
                </p>
              </>
            ),
          },
          {
            heading: "2. How we use information",
            body: (
              <>
                <p>We use the information described above to:</p>
                <ul className="list-disc space-y-2 pl-6">
                  <li>Provide, operate, and maintain the dashboard and the features you enable.</li>
                  <li>Authenticate your account and secure it against unauthorized access.</li>
                  <li>Send transactional email: verification, password reset, and alerts you opt into.</li>
                  <li>Process payment, manage your subscription, and communicate billing changes.</li>
                  <li>Monitor, debug, and improve the reliability and security of the Service.</li>
                  <li>Comply with legal obligations and enforce our Terms of Service.</li>
                </ul>
                <p>
                  We do not sell your data, and we do not use your account or site data for
                  advertising.
                </p>
              </>
            ),
          },
          {
            heading: "3. Sub-processors",
            body: (
              <>
                <p>
                  We share data with a small number of sub-processors, each engaged solely to
                  operate the Service on our behalf:
                </p>
                <ul className="list-disc space-y-2 pl-6">
                  <li>
                    <strong className="font-semibold text-foreground">Google Cloud Platform</strong>{" "}
                    hosts the control plane, database, and managed backup storage.
                  </li>
                  <li>
                    <strong className="font-semibold text-foreground">{STRIPE.legalName}</strong>{" "}
                    and{" "}
                    <strong className="font-semibold text-foreground">{RAZORPAY.legalName}</strong>{" "}
                    are payment processors for payments made through those providers. For those
                    payments, {COMPANY.legalName} is the seller, and Stripe or Razorpay processes
                    the payment on our behalf. Each receives the billing and contact data needed to
                    process your payment and acts as an independent data controller for that
                    billing data under its own privacy policy.
                  </li>
                  <li>
                    <strong className="font-semibold text-foreground">{PADDLE.legalName}</strong>{" "}
                    is our payment processor and merchant of record for sales it processes. Paddle
                    receives the billing and contact data needed to process your payment, calculate
                    tax, issue invoices and receipts, and process refunds for Paddle-processed
                    sales. Paddle acts as an independent data controller for that billing data under
                    its own privacy policy.
                  </li>
                  <li>A transactional email provider, used solely to deliver account emails.</li>
                  <li>
                    <strong className="font-semibold text-foreground">Google Analytics</strong> and{" "}
                    <strong className="font-semibold text-foreground">PostHog</strong> receive
                    anonymous usage statistics from this marketing website only. They are not used
                    in the dashboard and they receive no data about the WordPress sites you manage.
                    See section 8.
                  </li>
                </ul>
                <p>Self-hosted deployments of the WPMgr control plane involve no sub-processors at all.</p>
              </>
            ),
          },
          {
            heading: "4. Data location and international transfers",
            body: (
              <p>
                Hosted-service data is stored on Google Cloud Platform infrastructure. Where data is
                transferred across borders, for example to Stripe, Razorpay, or Paddle for payment
                processing, we rely on the sub-processor&apos;s own safeguards, such as standard
                contractual clauses, for that transfer. Contact us at {mail} if you need details
                about a specific transfer.
              </p>
            ),
          },
          {
            heading: "5. Data retention",
            body: (
              <p>
                We retain account and site data for as long as your account is active. Backup
                archives are retained according to your plan&apos;s retention settings. If you
                close your account, we delete or anonymize account and site data within a
                commercially reasonable period, except where we are required to retain records
                (for example billing records) for legal or tax purposes.
              </p>
            ),
          },
          {
            heading: "6. Security",
            body: (
              <ul className="list-disc space-y-2 pl-6">
                <li>Backup archives are encrypted before they leave your server.</li>
                <li>All network traffic to and from the Service uses TLS.</li>
                <li>
                  Agent-to-control-plane requests are cryptographically signed and
                  replay-protected.
                </li>
                <li>Passwords are stored as salted hashes; we never store plain-text passwords.</li>
                <li>
                  The control plane and agent are open source at {githubLink}, so our security
                  design is auditable rather than taken on faith.
                </li>
              </ul>
            ),
          },
          {
            heading: "7. Your rights",
            body: (
              <p>
                You can access, correct, export, or delete your account data at any time by
                contacting {mail}. Depending on your jurisdiction, you may also have the right to
                object to or restrict certain processing, or to lodge a complaint with your local
                data protection authority. We will respond to verified requests within a reasonable
                time and in any event within the period required by applicable law.
              </p>
            ),
          },
          {
            heading: "8. Cookies and website analytics",
            body: (
              <>
                <p>
                  The hosted dashboard uses a minimal set of cookies: a session cookie required to
                  keep you signed in, and, where enabled, a cookie that remembers a trusted device
                  for two-factor authentication. We do not use advertising or cross-site tracking
                  cookies on the dashboard. Real User Monitoring, described above, does not use
                  cookies.
                </p>
                <p>
                  This marketing website, wpmgr.app, uses Google Analytics and PostHog to measure
                  which pages people find useful and which of them lead to someone starting an
                  account. Both set cookies in your browser. We use them to answer questions about
                  pages, not about people: no account is created or identified from this website,
                  we do not build advertising profiles, and we do not sell or share this data.
                </p>
                <p>
                  If your browser sends a Do Not Track signal, PostHog is disabled for your visit.
                  You can also block both with any content blocker, and the site works normally
                  without them.
                </p>
              </>
            ),
          },
          {
            heading: "9. Children's privacy",
            body: (
              <p>
                The Service is a business tool for managing WordPress sites and is not directed at
                children. We do not knowingly collect personal data from anyone under 18. If you
                believe a child has provided us with personal data, contact us at {mail} and we will
                delete it.
              </p>
            ),
          },
          {
            heading: "10. Changes to this policy",
            body: (
              <p>
                We may update this Privacy Policy as the Service evolves. We will post the updated
                policy here with a new effective date, and for material changes we will provide
                reasonable advance notice by email or in the dashboard.
              </p>
            ),
          },
          {
            heading: "11. Contact",
            body: (
              <p>
                Questions about this policy or a request regarding your data: {mail}. Our registered
                address is {COMPANY.address}.
              </p>
            ),
          },
        ]}
      />
    </>
  );
}
