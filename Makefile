.DEFAULT_GOAL := help
SHELL := /bin/bash

# node@22 is keg-only on this host; ensure it is on PATH for pnpm
export PATH := /opt/homebrew/opt/node@22/bin:$(PATH)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: bootstrap
bootstrap: ## First-time dev setup
	./scripts/bootstrap.sh

# Separate from bootstrap because bootstrap installs a toolchain and this does
# not: an up-to-date clone with the hook uninstalled needs this one command and
# nothing else. `git config core.hooksPath` is repo-local and repo-local config
# is never committed, so without it a clone carries .githooks/pre-push and git
# never runs it - the push to main is permitted exactly as it was on 2026-08-12.
#
# A checkout that PREDATES this target prints "No rule to make target" and
# installs nothing. That clone needs `git pull` first.
#
# TWO THINGS HERE ARE NOT STYLE, both measured wrong before:
#   1. core.hooksPath is set to an ABSOLUTE path derived from --git-common-dir.
#      A relative one is resolved by git against whichever working tree runs the
#      hook, so a linked worktree - or a checkout of a commit that predates the
#      hook - silently runs no hook while config keeps reading "installed".
#   2. The hook is COPIED into the common hooks directory rather than config
#      pointing at the tracked .githooks, because checking out an older commit
#      deletes that directory out from under a config still naming it.
# So re-run this after editing .githooks/pre-push; `make hooks-status` says
# whether the installed copy is still current.
.PHONY: hooks
hooks: ## Install the committed git hooks (pre-push refuses a push that lands on main)
	@set -eu; \
	hooks_dir="$$(cd "$$(git rev-parse --git-common-dir)" && pwd)/hooks"; \
	mkdir -p "$$hooks_dir"; \
	if [ -e "$$hooks_dir/pre-push" ] && ! cmp -s .githooks/pre-push "$$hooks_dir/pre-push"; then \
		aside="$$hooks_dir/pre-push.replaced-$$(date +%Y%m%d%H%M%S)"; \
		cp "$$hooks_dir/pre-push" "$$aside"; \
		echo "NOTE: a DIFFERENT pre-push hook was already installed. Copied aside to:"; \
		echo "      $$aside"; \
		echo "      Merge anything you need from it back into .githooks/pre-push."; \
	fi; \
	cp .githooks/pre-push "$$hooks_dir/pre-push"; \
	chmod +x "$$hooks_dir/pre-push"; \
	git config core.hooksPath "$$hooks_dir"; \
	echo "pre-push INSTALLED: $$hooks_dir/pre-push"

# Exits NON-ZERO when the hook is not actually live, so it is usable as a check
# and cannot report success over its own absence. It compares the installed copy
# against the tracked one: "core.hooksPath is set" alone is not installed.
.PHONY: hooks-status
hooks-status: ## Report whether the pre-push hook is actually running in this checkout
	@hp="$$(git config --get core.hooksPath || true)"; \
	if [ -z "$$hp" ]; then \
		echo "pre-push NOT INSTALLED: core.hooksPath is unset, so git ignores .githooks/. Run 'make hooks'."; \
		exit 1; \
	fi; \
	case "$$hp" in /*) ;; *) hp="$$(pwd)/$$hp";; esac; \
	if [ ! -x "$$hp/pre-push" ]; then \
		echo "pre-push NOT INSTALLED: core.hooksPath=$$hp has no executable pre-push. Run 'make hooks'."; \
		exit 1; \
	fi; \
	if cmp -s .githooks/pre-push "$$hp/pre-push"; then \
		echo "pre-push INSTALLED and current: $$hp/pre-push"; \
	else \
		echo "pre-push INSTALLED but STALE: $$hp/pre-push differs from .githooks/pre-push. Run 'make hooks'."; \
		exit 1; \
	fi

.PHONY: quickstart
quickstart: ## One-command self-host bootstrap: write .env + generate secrets
	./scripts/init-env.sh

COMPOSE := docker compose -f infra/docker-compose.yml
COMPOSE_DEV := $(COMPOSE) -f infra/docker-compose.dev.yml

.PHONY: dev
dev: ## Run full stack for local development
	$(COMPOSE_DEV) up

.PHONY: up
up: ## Run the production-style stack (built images)
	$(COMPOSE) up -d

.PHONY: down
down: ## Stop the local stack
	$(COMPOSE_DEV) down

.PHONY: observability
observability: ## Run the stack with the observability profile (otel-lgtm)
	$(COMPOSE) --profile observability up -d

.PHONY: docker-build
docker-build: ## Build the api + web container images
	docker build -f infra/Dockerfile.api -t wpmgr-api:dev .
	docker build -f infra/Dockerfile.web -t wpmgr-web:dev .

.PHONY: build
build: build-api build-web ## Build everything

.PHONY: build-api
build-api: ## Build the Go API binary
	cd apps/api && go build -o bin/wpmgr ./cmd/wpmgr

.PHONY: build-web
build-web: ## Build the web SPA
	pnpm --filter @wpmgr/web build

.PHONY: test
test: test-api test-web ## Run all tests

.PHONY: test-api
# -timeout 45m, not the default. apps/api/tests stands up an ephemeral Postgres
# per test and sits right on Go's 10 minute default: measured 522s for that
# package alone and 601s under the parallel load of `go test ./...`, which
# panicked with "test timed out after 10m0s". The tests were fine; the limit
# was not. Without this flag the full run flakes on a busy machine.
test-api: ## Run Go tests (unit plus integration; needs Docker for the integration package)
	cd apps/api && go test -timeout 45m ./...

.PHONY: test-integration
# The integration package on its own, which is where the security proofs live:
# the m112 RLS tests that stop a collaborator invited to one site reaching the
# whole organisation's email credentials.
#
# THIS IS NOT RUN BY CI. .github/workflows/ci.yml excludes the package and
# .github/workflows/api-integration.yml is manual-only, because it takes about
# 18 minutes on a standard runner and that toll on every PR is not worth paying
# yet. So this command is the only thing standing in front of a regression in
# it. Run it before merging anything that touches RLS, the email domain or
# tenant scoping. Needs a working Docker daemon (testcontainers).
test-integration: ## Run the Go integration tests (Docker required, about 9 minutes)
	cd apps/api && go test -timeout 45m ./tests/...

.PHONY: test-web
test-web: ## Run frontend tests
	pnpm run test

.PHONY: lint
lint: ## Lint everything
	cd apps/api && go vet ./...
	pnpm run lint

# The same check CI runs, so a drifted install pin, hero badge or agent version
# is a five second answer here instead of a red build later. check-versions-test
# is the guard's own regression suite; run it after editing the guard.
.PHONY: check-versions
check-versions: ## Check every version-naming surface (docs, marketing, agent)
	scripts/check-version-surfaces.sh

.PHONY: check-versions-test
check-versions-test: ## Run the version surface guard's regression suite
	scripts/check-version-surfaces_test.sh

# ---- Agent harness (.claude) ------------------------------------------------
# The shell guards that used to live in scripts/claude/ are gone: deciding what
# a shell command will write by parsing its text is undecidable (eval, bash -c,
# a path built at run time), and four rounds of fixes closed real bypasses while
# each following round found more. What actually caught this project's real
# defects - a privilege escalation, a check-then-act race, an unclamped API-key
# role - was the review process in CLAUDE.md and review.md, which is kept.
#
# .githooks/pre-push is kept and is different in kind: git hands it
# already-resolved refs, so eval, quoting and exotic refspecs are gone before it
# runs. It is not guessing. `make hooks` installs it; `make hooks-status` says
# whether it is live. Those two are above, next to bootstrap.

# reproducible_zip: package $2 (a directory) inside $1 into $3, byte for byte
# identically every time the same tree is packaged.
#
# WHY THIS EXISTS (GitHub issue #322). A plain `zip -r` records each entry's
# mtime and walks the tree in filesystem order, so the same source produced a
# DIFFERENT archive on every build. agent-vendor deletes and reinstalls vendor/
# each time, which restamps thousands of files, so the drift was guaranteed
# rather than occasional.
#
# That mattered because the agent version deliberately does NOT track the repo
# version (it only changes when the agent changes), so a web-only release
# republished the SAME version string with different bytes. A self-hoster
# mirroring our releases correctly refused it: "upstream republished the same
# version with different bytes". Four artifacts on GitHub claimed 0.61.114,
# each with a different sha256, and every mirror kept refusing until the
# version moved.
#
# The mirror's assumption, that one version string means one set of bytes
# forever, is right. This makes it true.
#
#   touch  pins every mtime to a fixed instant. It must be >= 1980 because the
#          zip format cannot store anything earlier, and on an even second
#          because DOS timestamps have two-second granularity.
#   sort   fixes entry order independent of how the filesystem enumerates.
#   -X     drops platform extra fields (uid, gid, and the macOS-only ones), so
#          a Linux CI runner and a macOS workstation agree.
#
# $(1) working dir, $(2) directory to package, $(3) output archive name.
define reproducible_zip
	find $(1)/$(2) -exec touch -h -t 200001010000.00 {} +
	cd $(1) && find $(2) -print | LC_ALL=C sort | zip -X -q -@ $(3)
endef

.PHONY: agent-vendor
agent-vendor: ## Build a clean prod-only vendor/ for the agent (no-dev, stripped)
	# ADR-033 / M5.6: agent has ZERO production composer deps (we dropped phpbu
	# and ifsnop/mysqldump-php after the mysqli rewrite). composer install is
	# still needed to generate vendor/autoload.php's classmap for includes/.
	# Composer 2 in a container (no host PHP requirement). --no-dev drops dev
	# tooling; --ignore-platform-reqs skips ext-* runtime checks (the build
	# container doesn't ship ext-mysqli/zip/zlib; those checks happen on the
	# actual WP host at runtime, where the extensions are present).
	cd apps/agent && rm -rf vendor composer.lock
	# Run composer as the HOST user (not the container's root) so every extracted
	# vendor file is owned by the invoking user. Otherwise, on a Linux CI runner,
	# the bind-mounted files are created root-owned and the host-side strip step
	# below fails with "Permission denied" on read-only package files (e.g. a
	# dependency's README.md / phpstan.neon). COMPOSER_HOME points at a writable
	# cache dir since the mapped UID has no entry in the container's /etc/passwd.
	docker run --rm --user "$$(id -u):$$(id -g)" -e COMPOSER_HOME=/tmp/composer -v "$(PWD)/apps/agent:/app" -w /app composer:2 install --no-dev --optimize-autoloader --classmap-authoritative --ignore-platform-reqs
	# Strip non-runtime files from the runtime vendors. Be conservative: only
	# drop directories named exactly tests/Tests/doc/docs/examples/.git, plus
	# CHANGELOG/UPGRADING/README .md files. Never touch *.php.
	cd apps/agent/vendor && find . -type d \( -name tests -o -name Tests -o -name doc -o -name docs -o -name examples -o -name .git -o -name .github \) -prune -exec rm -rf {} +
	cd apps/agent/vendor && find . -type f \( -name 'CHANGELOG*.md' -o -name 'UPGRADING*.md' -o -name 'README*.md' -o -name 'CONTRIBUTING*.md' -o -name '.gitignore' -o -name 'phpunit.xml*' -o -name 'phpstan.neon*' -o -name '.editorconfig' \) -delete
	@echo "agent vendor size: $$(du -sh apps/agent/vendor | cut -f1)"

.PHONY: agent-zip
agent-zip: agent-vendor ## Package the WordPress agent plugin as a zip (with ifsnop vendor/)
	mkdir -p release
	# Rebuild (not update) the zip — without this, `zip -r` appends to the
	# existing archive, leaving stale entries from prior plugin versions (e.g.
	# old phpbu/ vendor tree, deleted files). Removing the target file forces
	# a clean rebuild every run.
	rm -f release/wpmgr-agent.zip
	rm -rf release/wpmgr-agent
	# Sweep dev-only files (tests, caches, macOS resource forks, nested
	# archives someone may have unzipped here for debugging) before packaging.
	cd apps/agent && rm -f Archive.zip .DS_Store .phpunit.result.cache && find . -name ".DS_Store" -delete
	# Stage the plugin under a STABLE top-level folder (wpmgr-agent/) before
	# zipping. WordPress derives a plugin's install folder (its slug) from the
	# archive's top-level directory — or, when files sit at the archive root,
	# from the .zip FILENAME. Packaging the bare contents (the old `zip -r . `)
	# meant a versioned filename like wpmgr-agent-0.10.5.zip extracted to
	# plugins/wpmgr-agent-0.10.5/ — a DIFFERENT slug from plugins/wpmgr-agent/,
	# so WordPress saw each release as a brand-new plugin instead of an update
	# (forcing a deactivate/delete that wipes the agent's wp-cron events).
	# Staging under wpmgr-agent/ pins the slug regardless of the .zip filename,
	# so every upload is recognised as an in-place update of the same plugin.
	rsync -a --delete \
		--exclude 'tests/' --exclude 'tests-e2e/' --exclude 'tools/' --exclude '*.dist' --exclude '.phpunit.cache/' \
		--exclude '.phpunit.result.cache' --exclude 'composer.lock' \
		--exclude '.DS_Store' --exclude '*.zip' \
		--exclude 'patchwork.json' \
		apps/agent/ release/wpmgr-agent/
	# VERSION override: when VERSION is provided (e.g. from the release tag),
	# strip any leading 'v' and stamp ONLY the staged copy — the source file is
	# never modified. Two precise in-place sed replacements target exactly the
	# plugin header "Version:" line and the WPMGR_AGENT_VERSION constant, leaving
	# all other lines unchanged. When VERSION is unset the staged copy carries the
	# source baseline unchanged, making this step a no-op.
	@if [ -n "$(VERSION)" ]; then \
		_v=$$(echo "$(VERSION)" | sed 's/^v//'); \
		case "$$_v" in \
			[0-9]*.[0-9]*.[0-9]*) \
				case "$$_v" in \
					*[!0-9A-Za-z.+\-]*) \
						echo "agent-zip: refusing unsafe VERSION '$(VERSION)' — only digits, letters, dots, hyphens, and plus signs are allowed" >&2; exit 1 ;; \
				esac ;; \
			*) echo "agent-zip: refusing VERSION '$(VERSION)' — must be MAJOR.MINOR.PATCH (with optional leading v)" >&2; exit 1 ;; \
		esac; \
		_v_esc=$$(printf '%s' "$$_v" | sed -e 's/[\/&|]/\\&/g'); \
		echo "agent-zip: stamping staged copy with version $$_v"; \
		sed -i.bak -E "s/^( \* Version:[ \t]+)[0-9]+\.[0-9]+\.[0-9].*/\1$$_v_esc/" release/wpmgr-agent/wpmgr-agent.php; \
		sed -i.bak -E "s/^(define\('WPMGR_AGENT_VERSION', *')[^']+(')/\1$$_v_esc\2/" release/wpmgr-agent/wpmgr-agent.php; \
		rm -f release/wpmgr-agent/wpmgr-agent.php.bak; \
	fi
	$(call reproducible_zip,release,wpmgr-agent,wpmgr-agent.zip)
	rm -rf release/wpmgr-agent
	@echo "agent zip: $$(du -sh release/wpmgr-agent.zip | cut -f1)"

.PHONY: agent-zip-wporg
agent-zip-wporg: agent-vendor ## Package the wp.org-distributable plugin zip (fleet-agent-site-manager identity; self-hosted identity untouched)
	mkdir -p release
	rm -f release/fleet-agent-site-manager.zip
	rm -rf release/fleet-agent-site-manager
	# Stage under fleet-agent-site-manager/ — the permanent wp.org slug. The self-updater
	# (class-update-checker.php) is physically excluded so PCP cannot match the
	# site_transient_update_plugins hook (B2 / G8). NOTICE.md and README.md are
	# excluded because wp.org rejects unexpected Markdown files (B4 / C8).
	# Dev-only files mirror the existing agent-zip excludes.
	# vendor/bin/ and vendor/*/bin/ are excluded because wp.org does not permit
	# CLI entrypoints (minifyjs, minifycss) — the matthiasmullie/minify library is
	# used via its PHP API only (no shell-out to the bin scripts anywhere in the
	# agent). vendor/*/LICENSE files are excluded because wp.org flags them as
	# unexpected non-code files; the library licences are already referenced in the
	# plugin's own LICENSE / readme.txt.
	# vendor/**/data-scripts/ and *.py files are excluded because wp.org does not
	# permit build tooling scripts (e.g. bjeavons/zxcvbn-php/data-scripts/build_*.py).
	# composer.json ships with vendor/ so Plugin Check does not warn about a vendored
	# directory without a composer.json. composer.lock is excluded (not needed at runtime).
	rsync -a --delete \
		--exclude 'tests/' --exclude 'tests-e2e/' --exclude 'tools/' --exclude '*.dist' --exclude '.phpunit.cache/' \
		--exclude '.phpunit.result.cache' --exclude 'composer.lock' \
		--exclude '.DS_Store' --exclude '*.zip' \
		--exclude '.distignore' --exclude '.gitignore' --exclude '.gitattributes' \
		--exclude 'phpstan.neon' --exclude 'phpstan-baseline.neon' \
		--exclude 'NOTICE.md' --exclude 'README.md' \
		--exclude 'patchwork.json' \
		--exclude 'includes/support/class-update-checker.php' \
		apps/agent/ release/fleet-agent-site-manager/
	# Remove CLI entrypoints, LICENSE files, data-scripts dirs, and Python files
	# that wp.org does not permit. rsync --exclude patterns for vendor/*/bin/ do not
	# recurse into arbitrary depth (e.g. vendor/matthiasmullie/minify/bin/ is 3 levels
	# deep), so a post-stage find+delete is more reliable than path-based excludes.
	find release/fleet-agent-site-manager/vendor/bin -mindepth 0 -maxdepth 0 -type d -exec rm -rf {} + 2>/dev/null || true
	find release/fleet-agent-site-manager/vendor -mindepth 2 -type d -name bin -exec rm -rf {} + 2>/dev/null || true
	find release/fleet-agent-site-manager/vendor -mindepth 2 -type f -name LICENSE -delete 2>/dev/null || true
	find release/fleet-agent-site-manager/vendor -type d -name data-scripts -exec rm -rf {} + 2>/dev/null || true
	find release/fleet-agent-site-manager/vendor -type f -name "*.py" -delete 2>/dev/null || true
	# Rename the main plugin file to match the wp.org slug. WordPress derives the
	# plugin's displayed name, slug, and update identity from the top-level .php
	# filename inside the archive folder — renaming is mandatory for the wp.org slug.
	mv release/fleet-agent-site-manager/wpmgr-agent.php \
		release/fleet-agent-site-manager/fleet-agent-site-manager.php
	# VERSION override: same mechanism as agent-zip — stamp ONLY the staged copy.
	# Two lines: the plugin header "Version:" and the WPMGR_AGENT_VERSION constant.
	@if [ -n "$(VERSION)" ]; then \
		_v=$$(echo "$(VERSION)" | sed 's/^v//'); \
		case "$$_v" in \
			[0-9]*.[0-9]*.[0-9]*) \
				case "$$_v" in \
					*[!0-9A-Za-z.+\-]*) \
						echo "agent-zip-wporg: refusing unsafe VERSION '$(VERSION)' — only digits, letters, dots, hyphens, and plus signs are allowed" >&2; exit 1 ;; \
				esac ;; \
			*) echo "agent-zip-wporg: refusing VERSION '$(VERSION)' — must be MAJOR.MINOR.PATCH (with optional leading v)" >&2; exit 1 ;; \
		esac; \
		_v_esc=$$(printf '%s' "$$_v" | sed -e 's/[\/&|]/\\&/g'); \
		echo "agent-zip-wporg: stamping staged copy with version $$_v"; \
		sed -i.bak -E "s/^( \* Version:[ \t]+)[0-9]+\.[0-9]+\.[0-9].*/\1$$_v_esc/" release/fleet-agent-site-manager/fleet-agent-site-manager.php; \
		sed -i.bak -E "s/^(define\('WPMGR_AGENT_VERSION', *')[^']+(')/\1$$_v_esc\2/" release/fleet-agent-site-manager/fleet-agent-site-manager.php; \
		rm -f release/fleet-agent-site-manager/fleet-agent-site-manager.php.bak; \
	fi
	# Stamp readme.txt Stable tag to match the plugin header Version. Mirrors the
	# VERSION block above; reads the stamped Version from the staged main file so
	# the two values always agree regardless of the VERSION variable.
	@_stamped_v=$$(grep -E '^ \* Version:' release/fleet-agent-site-manager/fleet-agent-site-manager.php | sed -E 's/.*Version:[ \t]+//'); \
	echo "agent-zip-wporg: stamping readme.txt Stable tag: $$_stamped_v"; \
	sed -i.bak -E "s/^(Stable tag:[ \t]+).*/\1$$_stamped_v/" release/fleet-agent-site-manager/readme.txt; \
	rm -f release/fleet-agent-site-manager/readme.txt.bak
	# Rewrite plugin-identity header fields in the staged main file:
	#   Plugin Name  -> Fleet Agent Site Manager  (reviewer-accepted display name; no "WP" prefix)
	#   License      -> GPLv2 or later            (§3 recommended posture)
	#   License URI  -> gnu.org GPL-2.0 URL       (§3)
	#   Text Domain  -> fleet-agent-site-manager  (matches new slug)
	#   WPMGR_AGENT_DISPLAY_NAME -> Fleet Agent Site Manager (admin menu/page
	#   title constant; keeps the wp.org admin screen name consistent with the
	#   listing identity above instead of showing the self-hosted "WPMgr Agent")
	sed -i.bak \
		-e "s|^ \* Plugin Name:.*| * Plugin Name:       Fleet Agent Site Manager|" \
		-e "s|^ \* License:.*| * License:           GPLv2 or later|" \
		-e "s|^ \* License URI:.*| * License URI:       https://www.gnu.org/licenses/gpl-2.0.html|" \
		-e "s|^ \* Text Domain:.*| * Text Domain:       fleet-agent-site-manager|" \
		-e "s|define('WPMGR_AGENT_DISPLAY_NAME', 'WPMgr Agent');|define('WPMGR_AGENT_DISPLAY_NAME', 'Fleet Agent Site Manager');|" \
		release/fleet-agent-site-manager/fleet-agent-site-manager.php
	rm -f release/fleet-agent-site-manager/fleet-agent-site-manager.php.bak
	# Inject the WPMGR_WPORG_BUILD constant immediately after the WPMGR_AGENT_VERSION
	# define line. This guards the self-updater boot hook (class-plugin.php:522) so
	# it never binds in the wp.org build, satisfying G8 / B2 (the file exclusion
	# above satisfies PCP static-analysis; the constant satisfies the runtime guard).
	# Use awk to insert the line immediately after the WPMGR_AGENT_VERSION define,
	# avoiding the multi-line sed /a\ syntax which is non-portable across BSD/GNU sed.
	awk "/^define\('WPMGR_AGENT_VERSION',/{print; print \"define('WPMGR_WPORG_BUILD', true);\"; next}1" \
		release/fleet-agent-site-manager/fleet-agent-site-manager.php \
		> release/fleet-agent-site-manager/fleet-agent-site-manager.php.tmp
	mv release/fleet-agent-site-manager/fleet-agent-site-manager.php.tmp \
		release/fleet-agent-site-manager/fleet-agent-site-manager.php
	# Rewrite the text-domain literal 'wpmgr-agent' -> 'fleet-agent-site-manager' across
	# all staged PHP files. This covers both __()/__e() text-domain args AND the
	# plugin-identity constants (PAGE_SLUG, exclude-dir lists) that reference the
	# plugin folder name — in the wp.org install the folder IS fleet-agent-site-manager,
	# so all references must agree. class-update-checker.php is already excluded above
	# and is never present in the staged tree.
	# GREP FIRST — surface every occurrence so the caller can audit non-text-domain hits.
	@echo "--- grep of 'wpmgr-agent' in staged tree (before rewrite) ---"; \
	grep -rn "'wpmgr-agent'" release/fleet-agent-site-manager/ --include="*.php" || true; \
	echo "--- end grep ---"
	find release/fleet-agent-site-manager -name "*.php" -print0 | \
		xargs -0 sed -i.bak "s/'wpmgr-agent'/'fleet-agent-site-manager'/g"
	find release/fleet-agent-site-manager -name "*.php.bak" -delete
	# ASSERT the rewrite above reached the self-target guard's own constant.
	# tests/ is excluded from the staged tree, so nothing else checks that the
	# wp.org build recognises its OWN slug as the agent; a refactor of that
	# constant to double quotes or string concatenation would silently leave the
	# wp.org build guarding the self-hosted slug only.
	@if ! grep -q "SELF_PLUGIN_FOLDER = 'fleet-agent-site-manager';" \
		release/fleet-agent-site-manager/includes/commands/class-update-command.php; then \
		echo "agent-zip-wporg: FAILED. SELF_PLUGIN_FOLDER in the staged tree is not 'fleet-agent-site-manager', so the self-target guard would not recognise the wp.org slug. Check the 'wpmgr-agent' rewrite above and the constant in apps/agent/includes/commands/class-update-command.php" >&2; \
		exit 1; \
	fi
	@echo "agent-zip-wporg: self-target guard constant OK (SELF_PLUGIN_FOLDER = 'fleet-agent-site-manager')"
	# ASSERT no staged file resolves the (now absent) self-updater class outside a
	# guard. includes/class-plugin.php SURVIVES the rsync above and holds two hard
	# class fetches, `new UpdateChecker(...)` and `UpdateChecker::HOOK_APPLY`, each
	# safe only because it sits inside a guard. Both sit among a dozen visually
	# identical unconditional calls, so dedenting one out is a plausible refactor,
	# and it would raise
	#   Fatal error: Uncaught Error: Class "WPMgr\Agent\Support\UpdateChecker" not found
	# on every request of every wp.org install. `wp plugin check` is static analysis
	# and never executes the plugin, so it cannot catch that. This assert runs
	# against the artifact that actually ships. Mirrors the SELF_PLUGIN_FOLDER
	# assertion above. tools/ is excluded from the staged tree, so the checker is
	# invoked from the source tree.
	php apps/agent/tools/assert-wporg-updatechecker-guard.php release/fleet-agent-site-manager
	$(call reproducible_zip,release,fleet-agent-site-manager,fleet-agent-site-manager.zip)
	rm -rf release/fleet-agent-site-manager
	@echo "agent wporg zip: $$(du -sh release/fleet-agent-site-manager.zip | cut -f1)"

.PHONY: agent-check
agent-check: ## Fast phpcs pass over apps/agent (committed phpcs.xml.dist). NOT the authoritative gate.
	cd apps/agent && (composer install --no-interaction --quiet --ignore-platform-reqs 2>/dev/null \
		|| composer update --no-interaction --quiet --ignore-platform-reqs)
	cd apps/agent && vendor/bin/phpcs -d memory_limit=1G

.PHONY: agent-format
agent-format: ## phpcbf auto-fix over apps/agent, then re-lint
	cd apps/agent && vendor/bin/phpcbf -d memory_limit=1G --report-summary --report-source || true
	cd apps/agent && vendor/bin/phpcs -d memory_limit=1G

.PHONY: agent-plugincheck
agent-plugincheck: agent-zip-wporg ## AUTHORITATIVE: `wp plugin check` on real WordPress via Docker (mariadb + wordpress:cli)
	# Always tests the wp.org-identity build (fleet-agent-site-manager) so the META
	# trademark/updater/readme checks key off the right slug. Exits non-zero on any ERROR row.
	cd tools/plugincheck && PLUGIN_ZIP="$(PWD)/release/fleet-agent-site-manager.zip" ./run.sh

.PHONY: agent-e2e-objectcache
agent-e2e-objectcache: agent-zip ## Run the object-cache E2E harness (Docker; Redis + WordPress + phpredis)
	# Spins a full Docker environment: WordPress 6.8 + MariaDB 11 + Redis 7 + phpredis.
	# Exercises provision → assert-cli → cross-request persistence (FIX A net) →
	# freshness guard → cron-check → negative-check → disable.
	# Requires Docker. PLUGIN_ZIP can be overridden; defaults to the wporg build.
	chmod +x apps/agent/tests-e2e/run.sh
	PLUGIN_ZIP="$(PWD)/release/fleet-agent-site-manager.zip" apps/agent/tests-e2e/run.sh

.PHONY: agent-release
agent-release: agent-zip ## Publish the agent release (zip + latest.json) to object storage for CP-driven self-update (ADR-042)
	# Uploads the versioned package FIRST, then latest.json LAST, so the CP
	# manifest never points at a package that is not yet in place. Override the
	# bucket/prefix via WPMGR_RELEASE_BUCKET / WPMGR_RELEASE_PREFIX. Use
	# `make agent-release-dry-run` to preview latest.json without uploading.
	./scripts/release-agent.sh

.PHONY: agent-release-dry-run
agent-release-dry-run: agent-zip ## Preview the agent release (build zip + print latest.json) without uploading
	./scripts/release-agent.sh --dry-run

.PHONY: gen
gen: ## Regenerate OpenAPI clients (Go + TS)
	./scripts/gen-openapi.sh

.PHONY: gen-secrets
gen-secrets: ## Print the boot-critical self-host secrets as ready-to-paste env lines
	# Self-verifying generator: each secret is decoded back through the server's
	# own boot parsers before it is printed (see apps/api/cmd/wpmgr-cli).
	cd apps/api && go run ./cmd/wpmgr-cli gen-secrets

.PHONY: validate-env
validate-env: ## Check the environment config and list every problem at once
	cd apps/api && go run ./cmd/wpmgr-cli validate-env

.PHONY: init-env
init-env: ## Copy .env.example -> .env and inject fresh secrets (preserves existing .env)
	./scripts/init-env.sh
