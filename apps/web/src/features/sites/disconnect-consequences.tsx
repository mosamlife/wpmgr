/**
 * What the Disconnect confirmation tells the operator will happen. The site page
 * and the Sites list both show it, so the two cannot disagree.
 *
 * Disconnect only revokes the agent's access. The site stays in the default
 * list as disconnected with its history kept, and can be reconnected or
 * archived later. Archiving is a separate action with its own confirmation, so
 * this text must never say the site "is archived".
 */
export function DisconnectConsequences() {
  return (
    <div className="space-y-2">
      <p>
        We'll send a revoke to the agent on its next heartbeat (within ~60
        seconds). The agent stops accepting commands and clears its credentials.
      </p>
      <p>
        Backups and monitoring stop. The site stays in your sites list as
        disconnected, with its full history kept. You can reconnect it or
        archive it later.
      </p>
    </div>
  );
}
