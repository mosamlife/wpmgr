<?php
/**
 * AgeIdentity: lifecycle manager for the site's age (X25519) backup-encryption
 * identity, layered over the encrypted Keystore and the AgeCrypto primitive.
 *
 * Trust model (mirrors backup_contract.go):
 *   - The PRIVATE identity (raw X25519 secret) is generated locally on first
 *     backup and stored AES-256-GCM-encrypted in the Keystore. It NEVER leaves
 *     the site and is NEVER included in any control-plane-bound payload.
 *   - The PUBLIC recipient ("age1...") is derived from the secret on demand. The
 *     control plane holds only this recipient (sites.age_recipient) and echoes
 *     it back in the `backup` command's age_recipient field.
 *   - Before encrypting, the backup command asserts the CP-supplied recipient
 *     equals THIS site's own recipient (recipientMatches), so a substituted
 *     recipient (which the operator could never decrypt) is refused.
 *
 * The secret is loaded into memory only for the duration of an encrypt/decrypt
 * call and zeroed immediately after.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

use WPMgr\Agent\Keystore;

/**
 * Generates, stores, and uses the site's age identity for backup crypto.
 */
class AgeIdentity
{
    private Keystore $keystore;

    private AgeCrypto $age;

    /**
     * @param Keystore       $keystore Encrypted-at-rest key store.
     * @param AgeCrypto|null $age      age primitive (defaults to the real one).
     */
    public function __construct(Keystore $keystore, ?AgeCrypto $age = null)
    {
        $this->keystore = $keystore;
        $this->age      = $age ?? new AgeCrypto();
    }

    /**
     * Read-only health check of the keystore this identity lives in. See
     * Keystore::probe(): it never writes, never generates and never throws.
     *
     * @return array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string}
     */
    public function probeKeystore(): array
    {
        return $this->keystore->probe();
    }

    /**
     * Ensure an age identity exists, generating + storing one if absent.
     * Returns this site's PUBLIC recipient ("age1...").
     *
     * This is the only place a backup key is created. It creates one only
     * when none is stored and a keystore probe taken just before shows the
     * current key is the one this site uses now (KeystoreHealth::backupKeyUsable):
     * every stored key opens, or the site keypair opens. Otherwise it throws
     * and stores nothing.
     *
     * @return string The site's age recipient.
     * @throws \RuntimeException When no backup key is stored and none may be created.
     */
    public function ensureRecipient(): string
    {
        $secret = $this->keystore->getAgeIdentity();
        if ($secret === null) {
            if (!KeystoreHealth::backupKeyUsable($this->keystore->probe())) {
                throw new \RuntimeException(
                    'WPMgr Agent: no backup key is stored, and none is created while the saved keys do not open.'
                );
            }
            $pair   = $this->age->generateIdentity();
            $this->keystore->storeAgeIdentity($pair['secret']);
            $recipient = $pair['recipient'];
            SecureMemory::wipe($pair['secret']);
            SecureMemory::wipe($pair['identity']);

            return $recipient;
        }

        $recipient = $this->age->recipientForSecret($secret);
        SecureMemory::wipe($secret);

        return $recipient;
    }

    /**
     * This site's PUBLIC recipient, or '' if no identity is provisioned (does
     * NOT generate one).
     *
     * @return string
     */
    public function recipient(): string
    {
        $secret = $this->keystore->getAgeIdentity();
        if ($secret === null) {
            return '';
        }
        $recipient = $this->age->recipientForSecret($secret);
        SecureMemory::wipe($secret);

        return $recipient;
    }

    /**
     * Constant-time check that a CP-supplied recipient matches this site's own.
     * Generates the identity if none exists yet so a first backup can proceed,
     * under the same rule as ensureRecipient(), which throws when it may not.
     *
     * @param string $candidate Recipient from the CP command.
     * @return bool True when the candidate equals this site's recipient.
     */
    public function recipientMatches(string $candidate): bool
    {
        if ($candidate === '') {
            return false;
        }
        $own = $this->ensureRecipient();

        return hash_equals($own, $candidate);
    }

    /**
     * Encrypt a plaintext chunk to this site's recipient.
     *
     * @param string $plaintext Chunk plaintext.
     * @return string age ciphertext.
     */
    public function encryptChunk(string $plaintext): string
    {
        $recipient = $this->ensureRecipient();

        return $this->age->encrypt($plaintext, $recipient);
    }

    /**
     * Decrypt an age ciphertext chunk with this site's stored identity.
     *
     * @param string $ciphertext age ciphertext.
     * @return string Recovered plaintext.
     * @throws \RuntimeException When no identity is provisioned or decryption fails.
     */
    public function decryptChunk(string $ciphertext): string
    {
        $secret = $this->keystore->getAgeIdentity();
        if ($secret === null) {
            throw new \RuntimeException('WPMgr Agent: no age identity provisioned.');
        }
        try {
            return $this->age->decrypt($ciphertext, $secret);
        } finally {
            SecureMemory::wipe($secret);
        }
    }
}
