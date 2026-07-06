import type { Metadata } from "next";
import { buildMetadata, buildBreadcrumbLd, buildFAQPageLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { LegalPage } from "@/components/templates/legal-page";
import { COMPANY, LEGAL_EFFECTIVE_DATE, LEGAL_CONTACT_HREF, PADDLE } from "@/lib/content/legal";

export const metadata: Metadata = buildMetadata({
  title: "Refund Policy | WPMgr",
  description:
    "WPMgr refund policy: cancel a monthly subscription anytime, plus a 14-day money-back guarantee on your first paid payment, processed through Paddle.",
  canonical: "/refunds/",
});

const mail = (
  <a
    href={LEGAL_CONTACT_HREF}
    className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
  >
    {COMPANY.supportEmail}
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

const REFUND_FAQ = [
  {
    q: "Do I need to cancel my Free plan?",
    a: "No. The Free plan has no charge and no billing period, so there is nothing to cancel or refund. Simply stop using the Service or delete your account whenever you like.",
  },
  {
    q: "Can I get a refund for a partial month?",
    a: "No. Monthly subscriptions are billed in full at the start of each period and remain active through the end of that period once cancelled. We do not issue partial-month or prorated refunds for cancellation, outside of the 14-day money-back guarantee described above.",
  },
  {
    q: "What if I subscribed by mistake or the plan doesn't fit?",
    a: "If this is your first paid payment on your account, you are covered by the 14-day money-back guarantee: contact us within 14 days of that payment and we will arrange a full refund through Paddle.",
  },
];

export default function RefundsPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Legal", href: "/legal/" },
    { name: "Refund Policy", href: "/refunds/" },
  ]);
  const faqLd = buildFAQPageLd(REFUND_FAQ);

  return (
    <>
      <JsonLd data={breadcrumb} />
      <JsonLd data={faqLd} />
      <LegalPage
        eyebrow="Legal"
        title="Refund Policy"
        breadcrumbLabel="Refund Policy"
        effectiveDateLabel={`Effective ${LEGAL_EFFECTIVE_DATE}`}
        intro={
          <>
            This Refund Policy applies to paid WPMgr subscriptions on the hosted service at
            manage.wpmgr.app. All payments are processed by {paddleLink}, our merchant of record, and
            refunds are issued through Paddle to your original payment method.
          </>
        }
        sections={[
          {
            heading: "1. The Free plan needs no cancellation",
            body: (
              <p>
                The Free plan is not billed, so there is nothing to cancel and nothing to refund.
                You can stop using the Service, disconnect your sites, or delete your account at any
                time with no billing consequence.
              </p>
            ),
          },
          {
            heading: "2. Cancelling a monthly subscription",
            body: (
              <p>
                You can cancel a paid monthly subscription at any time from the dashboard billing
                page. Cancellation stops all future billing, and your plan stays active through the
                end of the period you already paid for. We do not issue refunds or credits for the
                unused portion of a billing period; this keeps our pricing simple and matches
                standard practice for monthly subscription software.
              </p>
            ),
          },
          {
            heading: "3. 14-day money-back guarantee",
            body: (
              <p>
                If you are new to paid WPMgr plans, your first paid subscription payment is covered
                by a 14-day money-back guarantee. If the Service is not right for you, contact us
                within 14 days of that first payment and we will arrange a full refund through
                Paddle, no detailed justification required. This guarantee applies once per customer
                and covers only the first paid payment; it does not apply to subsequent renewal
                payments or to a later resubscription after a previous refund.
              </p>
            ),
          },
          {
            heading: "4. How to request a refund",
            body: (
              <p>
                Email {mail} with your account email address and the date of the payment you would
                like refunded. You can also reach out through Paddle&apos;s own support channels,
                since Paddle processed the original transaction and appears on your statement.
                We aim to respond to every refund request within two business days.
              </p>
            ),
          },
          {
            heading: "5. How refunds are processed",
            body: (
              <p>
                Approved refunds are processed by Paddle back to the original payment method used
                for the purchase. Depending on your bank or card issuer, a refund can take several
                business days to appear on your statement after Paddle issues it. We are not able to
                refund to a different payment method or account than the one that made the original
                payment.
              </p>
            ),
          },
          {
            heading: "6. Exceptions",
            body: (
              <p>
                We reserve the right to decline a refund request that we reasonably believe is
                fraudulent, abusive, or made after extensive use of a paid plan well beyond the
                14-day guarantee window. We will always explain our reasoning if we decline a
                request.
              </p>
            ),
          },
          {
            heading: "7. Changes to this policy",
            body: (
              <p>
                We may update this Refund Policy as our billing practices evolve. We will post the
                updated policy here with a new effective date.
              </p>
            ),
          },
          {
            heading: "8. Contact",
            body: <p>Questions about a charge or a refund: {mail}.</p>,
          },
        ]}
      />
    </>
  );
}
