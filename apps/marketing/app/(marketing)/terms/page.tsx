import type { Metadata } from "next";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { LegalPage } from "@/components/templates/legal-page";
import { SITE_CONFIG } from "@/lib/site";
import { COMPANY, LEGAL_EFFECTIVE_DATE, LEGAL_CONTACT_HREF, PADDLE } from "@/lib/content/legal";

export const metadata: Metadata = buildMetadata({
  title: "Terms of Service | WPMgr",
  description:
    "Terms of Service for the WPMgr hosted service at manage.wpmgr.app, including subscription billing through Paddle as merchant of record.",
  canonical: "/terms/",
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

const paddleLink = (
  <a
    href={PADDLE.website}
    target="_blank"
    rel="noreferrer noopener"
    className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
  >
    {PADDLE.legalName}
  </a>
);

export default function TermsPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Legal", href: "/legal/" },
    { name: "Terms of Service", href: "/terms/" },
  ]);

  return (
    <>
      <JsonLd data={breadcrumb} />
      <LegalPage
        eyebrow="Legal"
        title="Terms of Service"
        breadcrumbLabel="Terms of Service"
        effectiveDateLabel={`Effective ${LEGAL_EFFECTIVE_DATE}`}
        intro={
          <>
            These Terms of Service ("Terms") govern your access to and use of the WPMgr hosted
            service at manage.wpmgr.app (the "Service"), operated by {COMPANY.legalName}, located
            at {COMPANY.address} ("WPMgr," "we," "us," or "our"). By creating an account or using
            the Service, you agree to these Terms. If you do not agree, do not use the Service.
          </>
        }
        sections={[
          {
            heading: "1. The service",
            body: (
              <p>
                WPMgr is a cloud dashboard for managing WordPress sites: backups and restore,
                updates, uptime and SSL monitoring, performance tooling, and security tooling,
                delivered through a small agent plugin installed on each connected WordPress site
                and a control plane you access at manage.wpmgr.app. These Terms cover the hosted
                Service only. Self-hosting is addressed separately in section 7 below.
              </p>
            ),
          },
          {
            heading: "2. Accounts and eligibility",
            body: (
              <>
                <p>
                  You must be at least 18 years old and able to form a binding contract to create
                  an account. You are responsible for the accuracy of the information you provide,
                  for keeping your login credentials confidential, and for all activity that occurs
                  under your account. Notify us immediately if you suspect unauthorized use of your
                  account.
                </p>
                <p>
                  Where an account is used on behalf of an organization, you represent that you have
                  the authority to bind that organization to these Terms, and "you" refers to that
                  organization as well as the individual user.
                </p>
              </>
            ),
          },
          {
            heading: "3. Acceptable use",
            body: (
              <>
                <p>You agree not to use the Service to:</p>
                <ul className="list-disc space-y-2 pl-6">
                  <li>Connect a WordPress site that you do not own or are not authorized to manage.</li>
                  <li>Store, process, or distribute unlawful, infringing, or malicious content.</li>
                  <li>Interfere with or disrupt the Service, its infrastructure, or other customers.</li>
                  <li>
                    Attempt to gain unauthorized access to any account, system, or network connected
                    to the Service.
                  </li>
                  <li>
                    Reverse engineer the hosted Service in a way that would circumvent its access
                    controls (note that reading and modifying the underlying open-source software
                    itself is expressly permitted; see section 7).
                  </li>
                </ul>
              </>
            ),
          },
          {
            heading: "4. Subscription plans and billing through Paddle",
            body: (
              <>
                <p>
                  Paid plans are billed monthly. Our order process and payment collection are
                  conducted by our online reseller and merchant of record, {paddleLink}. Paddle sells
                  the WPMgr subscription to you as the authorized reseller, is responsible for
                  processing your payment, and appears as the merchant on your card or bank
                  statement. Paddle is also responsible for invoicing, calculating and remitting
                  applicable sales tax, VAT, or GST, and processing refunds on our behalf under our
                  Refund Policy.
                </p>
                <p>
                  By subscribing, you also agree to Paddle&apos;s buyer terms and privacy policy,
                  which govern the payment transaction itself. Your subscription renews
                  automatically each billing period until you cancel. We will provide reasonable
                  advance notice before any price change takes effect for your account.
                </p>
              </>
            ),
          },
          {
            heading: "5. Changing plans",
            body: (
              <p>
                You may upgrade or downgrade your plan at any time from the dashboard billing page.
                An upgrade takes effect immediately and is billed on a prorated basis for the
                remainder of the current billing period. A downgrade takes effect at the start of
                your next billing period. If a downgrade would put you over the new plan&apos;s site
                limit or managed storage allowance, you will need to reduce usage before the
                downgrade can take effect.
              </p>
            ),
          },
          {
            heading: "6. Cancellation",
            body: (
              <p>
                You may cancel a paid subscription at any time from the dashboard billing page.
                Cancellation stops future billing; your plan remains active through the end of the
                period you already paid for, after which your account reverts to the Free plan or is
                suspended if it exceeds the Free plan&apos;s limits. See our Refund Policy for
                details on refund eligibility.
              </p>
            ),
          },
          {
            heading: "7. Self-hosting is a separate, open-source option",
            body: (
              <p>
                The WPMgr control plane is separately available as open-source software under the
                AGPL-3.0 license, and the WordPress agent plugin is MIT-licensed, at {githubLink}.
                If you run the control plane yourself instead of using the hosted Service, these
                Terms do not apply to that self-hosted deployment; only the applicable open-source
                license does. Nothing in these Terms restricts your rights under those licenses.
              </p>
            ),
          },
          {
            heading: "8. Intellectual property",
            body: (
              <p>
                We own all rights, title, and interest in the hosted Service, including its
                software, design, and trademarks, except for the open-source components licensed as
                described in section 7. You retain all rights to the content and data of the
                WordPress sites you connect. You grant us a limited license to access, process, and
                store that content solely to provide the Service to you.
              </p>
            ),
          },
          {
            heading: "9. Your data and your WordPress sites",
            body: (
              <>
                <p>
                  You are solely responsible for the WordPress sites you connect to the Service,
                  including their content, their compliance with applicable law, and any
                  third-party plugins or themes installed on them. You authorize the agent to
                  perform the management actions you request or schedule, such as backups, updates,
                  performance changes, and security operations, on the sites you connect.
                </p>
                <p>
                  The Service provides backups on a commercially reasonable, best-effort basis. You
                  are responsible for periodically verifying that your backups restore correctly and
                  for retaining independent copies of business-critical data. We are not liable for
                  data that cannot be recovered.
                </p>
              </>
            ),
          },
          {
            heading: "10. Warranties and disclaimers",
            body: (
              <p>
                The Service is provided "as is" and "as available," without warranties of any kind,
                express or implied, including merchantability, fitness for a particular purpose, and
                non-infringement, to the maximum extent permitted by law. We do not warrant that the
                Service will be uninterrupted, error-free, or fully secure.
              </p>
            ),
          },
          {
            heading: "11. Limitation of liability",
            body: (
              <p>
                To the maximum extent permitted by law, WPMgr and {COMPANY.legalName} will not be
                liable for any indirect, incidental, special, consequential, or punitive damages, or
                for loss of data, profits, or business, arising from your use of the Service. Our
                total aggregate liability for any claim relating to the Service will not exceed the
                amount you paid us for the Service in the twelve months preceding the claim.
              </p>
            ),
          },
          {
            heading: "12. Indemnification",
            body: (
              <p>
                You agree to indemnify and hold WPMgr, {COMPANY.legalName}, and our contributors
                harmless from any claim, loss, or expense, including reasonable legal fees, arising
                from your use of the Service, the WordPress sites you connect, or your violation of
                these Terms or applicable law.
              </p>
            ),
          },
          {
            heading: "13. Suspension and termination",
            body: (
              <p>
                You may disconnect any site or close your account at any time. We may suspend or
                terminate your access if you materially breach these Terms, pose a security or
                operational risk to the Service or other customers, or if required by law. Where
                practical, we will provide notice and an opportunity to resolve the issue before
                suspension.
              </p>
            ),
          },
          {
            heading: "14. Changes to these terms",
            body: (
              <p>
                We may update these Terms as the Service evolves. We will post the updated Terms
                here with a new effective date, and for material changes we will provide reasonable
                advance notice by email or in the dashboard. Continued use of the Service after a
                change takes effect constitutes acceptance of the updated Terms.
              </p>
            ),
          },
          {
            heading: "15. Governing law",
            body: (
              <p>
                These Terms are governed by the laws of {COMPANY.jurisdiction}, without regard to
                its conflict-of-law principles. Any dispute arising from these Terms or the Service
                will be subject to the exclusive jurisdiction of the courts located in{" "}
                {COMPANY.jurisdiction}, unless applicable law requires otherwise.
              </p>
            ),
          },
          {
            heading: "16. Contact",
            body: <p>Questions about these Terms: {mail}.</p>,
          },
        ]}
      />
    </>
  );
}
