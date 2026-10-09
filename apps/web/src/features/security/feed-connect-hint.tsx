import { Link } from "@tanstack/react-router";

import { useMe, canManageInstanceEmail } from "@/features/auth/use-auth";

/**
 * Who connects the vulnerability feed and where, for the "Vulnerability feed
 * not configured yet" state on the site panel and on the fleet page.
 *
 * The feed key is instance-wide, so connecting it takes the same authority as
 * the instance email settings: `me.can_manage_instance_email`, the decision the
 * server makes. Whoever it admits gets a link to Settings > Vulnerability feed,
 * a page they can always open. Everyone else is told the instance administrator
 * does it, and no page is named: the Admin area opens for superadmins only, so
 * naming it sends everyone else to a page that bounces them.
 */
export function FeedConnectHint() {
  const { data: me } = useMe();

  if (canManageInstanceEmail(me)) {
    return (
      <>
        Connect the Wordfence Intelligence feed in{" "}
        <Link
          to="/settings/vuln-feed"
          className="font-medium text-[var(--color-primary)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
        >
          Vulnerability feed settings
        </Link>
        . Vulnerability scanning begins automatically once the feed is
        connected.
      </>
    );
  }

  return (
    <>
      The instance administrator needs to connect the Wordfence Intelligence
      feed. Vulnerability scanning begins automatically once the feed is
      connected.
    </>
  );
}
