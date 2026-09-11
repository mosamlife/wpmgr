<?php
/**
 * ObjectcacheApplyConfigCommand — persists the object-cache connection config
 * to the 0600 config file and reports the applied hash.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/objectcache.apply_config
 *   Authorization: Bearer <Ed25519 JWT, cmd="objectcache.apply_config", aud=<siteId>>
 *   Body: ObjectCacheConfigRequest (see objectcache_contract.go)
 *
 * Response: ObjectCacheApplyConfigResult { ok, detail }
 *
 * Security: the signed JWT body carries the DECRYPTED password; the agent
 * stores it in the 0600 config file and NEVER echoes it back.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\ObjectCache\ObjectCacheConfig;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Persists the object-cache connection config to the 0600 config file.
 */
final class ObjectcacheApplyConfigCommand implements CommandInterface
{
	/**
	 * {@inheritDoc}
	 */
	public function name(): string
	{
		return 'objectcache.apply_config';
	}

	/**
	 * Effect: persists the Redis object-cache config file with 0600 permissions. It does not activate
	 * anything.
	 *
	 * @return CommandEffect
	 */
	public function effect(): CommandEffect
	{
		return CommandEffect::Write;
	}

	/**
	 * Repeatability: writing the same config twice converges.
	 *
	 * @return CommandRepeatability
	 */
	public function repeatability(): CommandRepeatability
	{
		return CommandRepeatability::Idempotent;
	}

	/**
	 * {@inheritDoc}
	 *
	 * @param array<string,mixed> $claims Validated JWT claims (unused).
	 * @param array<string,mixed> $params ObjectCacheConfigRequest fields.
	 * @return array{ok:bool,detail:string}
	 */
	public function execute( array $claims, array $params ): array
	{
		try {
			$config = ObjectCacheConfig::fromParams( $params );
			$loader = new ObjectCacheConfig();

			if ( ! $loader->save( $config ) ) {
				return [ 'ok' => false, 'detail' => 'config file write failed' ];
			}

			// Hash is password-redacted; safe to return.
			$hash = $loader->computeHash( $config );

			return [
				'ok'          => true,
				'detail'      => 'config applied',
				'config_hash' => $hash,
			];

		} catch ( \Throwable $e ) {
			return [ 'ok' => false, 'detail' => 'apply_config failed' ];
		}
	}
}
