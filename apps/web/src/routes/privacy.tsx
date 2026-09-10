import { createFileRoute, Link } from "@tanstack/react-router";
import { FleetHubLogo, Wordmark } from "@/components/brand/logo";

export const Route = createFileRoute("/privacy")({
  component: PrivacyPage,
});

function PrivacyPage() {
  return (
    <div className="min-h-dvh bg-[var(--color-background)] text-[var(--color-foreground)]">
      <div className="mx-auto max-w-3xl px-6 py-14">
        {/* Brand lockup */}
        <div className="mb-10 flex items-center gap-2.5">
          <FleetHubLogo size={26} />
          <Wordmark className="text-base" />
        </div>

        {/* Page content */}
        <article>
          <h1 className="mb-1 text-3xl font-bold tracking-tight">Privacy Policy</h1>
          <p className="mb-8 text-sm text-[var(--color-muted-foreground)]">Last updated: 8 June 2026</p>

          <p className="mb-6 leading-relaxed text-[var(--color-muted-foreground)]">
            WPMgr is open-source, self-hostable software for managing a fleet of WordPress sites —
            backups, updates, performance, and security. This Privacy Policy explains what data the
            WPMgr agent plugin transmits, and what the optional hosted service at{" "}
            <strong className="text-[var(--color-foreground)]">manage.wpmgr.app</strong> collects when
            you choose to use it.
          </p>

          <p className="mb-8 leading-relaxed text-[var(--color-muted-foreground)]">
            The full source code is public at{" "}
            <a
              href="https://github.com/mosamlife/wpmgr"
              target="_blank"
              rel="noreferrer noopener"
              className="text-[#12B5BE] underline underline-offset-4 hover:opacity-80"
            >
              https://github.com/mosamlife/wpmgr
            </a>{" "}
            (the agent plugin is MIT-licensed; the control plane is AGPL-3.0). You can self-host the
            entire stack and keep 100% of your data on your own infrastructure.
          </p>

          <Section title="Private and self-hosted by default">
            <p className="leading-relaxed text-[var(--color-muted-foreground)]">
              The WPMgr agent plugin has{" "}
              <strong className="text-[var(--color-foreground)]">no default endpoint</strong> and sends{" "}
              <strong className="text-[var(--color-foreground)]">no data anywhere</strong> until you
              connect it to a control plane that you choose and configure. The plugin is inert on
              activation. If you point it at a control plane you self-host, your data never reaches us
              — you operate the receiving service and your own policies apply.
            </p>
          </Section>

          <Section title="What the agent sends, and only to your control plane">
            <p className="mb-3 leading-relaxed text-[var(--color-muted-foreground)]">
              Once you connect a site, the agent communicates{" "}
              <strong className="text-[var(--color-foreground)]">only</strong> with the control-plane
              URL you configured, and only for the management actions you (or your schedules) initiate:
            </p>
            <ul className="list-disc space-y-3 pl-5 text-[var(--color-muted-foreground)]">
              <li className="leading-relaxed">
                <strong className="text-[var(--color-foreground)]">Site and environment metadata</strong>{" "}
                — site URL, WordPress / PHP / server versions, active theme and plugins, and Site Health
                diagnostics. Used to show your site's status.
              </li>
              <li className="leading-relaxed">
                <strong className="text-[var(--color-foreground)]">Update inventory</strong> — the list
                of available core, plugin, and theme updates.
              </li>
              <li className="leading-relaxed">
                <strong className="text-[var(--color-foreground)]">Backup archives:</strong>{" "}
                when you run or schedule a backup, the agent creates an archive of your database
                and/or files and uploads it to the storage destination your control plane
                configures: a control-plane-managed bucket by default, or a customer-owned bucket or
                local folder you point it at instead. Archive contents may include your site's content
                and personal data; the agent does not encrypt them before upload, so protection comes
                from your chosen destination's own access controls and at-rest encryption, if any.
              </li>
              <li className="leading-relaxed">
                <strong className="text-[var(--color-foreground)]">Rendered HTML</strong> — for
                used-CSS optimization, the agent submits rendered HTML of selected pages so unused CSS
                can be computed.
              </li>
              <li className="leading-relaxed">
                <strong className="text-[var(--color-foreground)]">Diagnostics and activity logs</strong>{" "}
                — error logs, performance/cache statistics, and a record of management actions, so they
                can be surfaced in the dashboard.
              </li>
            </ul>
            <p className="mt-4 leading-relaxed text-[var(--color-muted-foreground)]">
              Every agent request is verified with an Ed25519 signature tied to the key established
              when you enroll the site. The agent does not execute arbitrary remote code; it accepts
              only a fixed, named allow-list of commands.
            </p>
          </Section>

          <Section title="The hosted service (manage.wpmgr.app)">
            <p className="mb-3 leading-relaxed text-[var(--color-muted-foreground)]">
              If you use the hosted WPMgr service rather than self-hosting, we also process:
            </p>
            <ul className="list-disc space-y-2 pl-5 text-[var(--color-muted-foreground)]">
              <li className="leading-relaxed">
                <strong className="text-[var(--color-foreground)]">Account information</strong> — your
                name and email address, used to operate your account and send transactional email
                (verification, password reset, alerts).
              </li>
              <li className="leading-relaxed">
                The site data described above, on your behalf, to provide the dashboard, backups, and
                management features you use.
              </li>
              <li className="leading-relaxed">
                <strong className="text-[var(--color-foreground)]">Backup archives:</strong> for sites
                using the control-plane-managed bucket (the default), stored in our cloud object storage
                with the provider's encryption at rest. If you configure your own S3-compatible bucket or
                a local folder on the WordPress host instead, backup data does not pass through our
                storage.
              </li>
              <li className="leading-relaxed">
                <strong className="text-[var(--color-foreground)]">Operational logs</strong> needed to
                run and secure the service.
              </li>
            </ul>
          </Section>

          <Section title="What we do not do">
            <p className="leading-relaxed text-[var(--color-muted-foreground)]">
              We do <strong className="text-[var(--color-foreground)]">not</strong> sell your data, and
              we do <strong className="text-[var(--color-foreground)]">not</strong> share it with third
              parties for advertising. The only sub-processors involved in the hosted service are our
              cloud infrastructure provider (Google Cloud Platform — hosting and encrypted storage) and
              a transactional email provider. Self-hosted deployments involve no sub-processors at all.
            </p>
          </Section>

          <Section title="Security">
            <ul className="list-disc space-y-2 pl-5 text-[var(--color-muted-foreground)]">
              <li className="leading-relaxed">
                Agent-to-control-plane requests are Ed25519-signed and replay-protected.
              </li>
              <li className="leading-relaxed">
                Backup archives are not encrypted client side before upload; protection comes from
                your chosen destination's access controls and, where the destination provides it,
                encryption at rest. Client-side encryption of backup archives is planned.
              </li>
              <li className="leading-relaxed">All network traffic uses TLS.</li>
            </ul>
          </Section>

          <Section title="Your data, your control">
            <ul className="list-disc space-y-2 pl-5 text-[var(--color-muted-foreground)]">
              <li className="leading-relaxed">
                <strong className="text-[var(--color-foreground)]">Self-host</strong> to keep all data
                on infrastructure you control.
              </li>
              <li className="leading-relaxed">
                <strong className="text-[var(--color-foreground)]">Disconnect</strong> the agent (or
                deactivate the plugin) at any time to stop all data transmission immediately.
              </li>
              <li className="leading-relaxed">
                On the hosted service you can request access to, export of, or deletion of your account
                data by contacting us.
              </li>
            </ul>
          </Section>

          <Section title="Contact">
            <p className="leading-relaxed text-[var(--color-muted-foreground)]">
              Questions about this policy or your data:{" "}
              <a
                href="mailto:mosam@mosamgor.com"
                className="text-[#12B5BE] underline underline-offset-4 hover:opacity-80"
              >
                mosam@mosamgor.com
              </a>
              .
            </p>
          </Section>

          <Section title="Changes">
            <p className="leading-relaxed text-[var(--color-muted-foreground)]">
              We may update this policy as the software evolves. Material changes will be reflected here
              with a new "Last updated" date.
            </p>
          </Section>
        </article>

        {/* Footer nav */}
        <footer className="mt-14 flex flex-wrap items-center gap-4 border-t border-[var(--color-border)] pt-6 text-sm text-[var(--color-muted-foreground)]">
          <a
            href="https://manage.wpmgr.app"
            className="hover:text-[var(--color-foreground)]"
          >
            manage.wpmgr.app
          </a>
          <span aria-hidden="true" className="text-[var(--color-border)]">|</span>
          <Link
            to="/terms"
            className="hover:text-[var(--color-foreground)]"
          >
            Terms of Service
          </Link>
        </footer>
      </div>
    </div>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="mb-8">
      <h2 className="mb-3 text-xl font-semibold tracking-tight text-[var(--color-foreground)]">
        {title}
      </h2>
      {children}
    </section>
  );
}
