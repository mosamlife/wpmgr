FROM wordpress:6.8-php8.3-apache

# Install phpredis and enable it.
#
# NOT via `pecl install redis`. pecl.php.net no longer serves this package — its
# REST endpoint returns a non-XML body, so pecl reports
#   No releases available for package "pecl.php.net/redis"
# and `pecl channel-update pecl.php.net` fails first with
#   XML Error: 'Not well-formed (invalid token)' on line '1'
# That is an upstream retirement, nothing in this repo, and it bricks the image
# build before any WordPress or object-cache code runs. Building the extension
# from the project's own release tarball removes the dependency on that channel.
#
# The version is pinned: an unpinned build of a C extension is not reproducible,
# and this image is the substrate for a gate whose job is to notice change.
ARG PHPREDIS_VERSION=6.1.0
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends $PHPIZE_DEPS; \
    curl -fsSL "https://github.com/phpredis/phpredis/archive/refs/tags/${PHPREDIS_VERSION}.tar.gz" \
        -o /tmp/phpredis.tar.gz; \
    mkdir -p /usr/src/php/ext/redis; \
    tar -xzf /tmp/phpredis.tar.gz -C /usr/src/php/ext/redis --strip-components=1; \
    docker-php-ext-install -j"$(nproc)" redis; \
    rm -rf /tmp/phpredis.tar.gz /usr/src/php/ext/redis; \
    apt-get purge -y --auto-remove $PHPIZE_DEPS; \
    rm -rf /var/lib/apt/lists/*

# Assert the extension is actually loadable. `docker-php-ext-install` succeeding
# is not the same claim, and a silently absent phpredis would make the whole
# object-cache suite fall back to array mode and report a meaningless pass.
RUN php -m | grep -x redis

# Install WP-CLI.
RUN curl -sS -o /usr/local/bin/wp https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar \
    && chmod +x /usr/local/bin/wp

# WP-CLI needs to run as www-data inside containers.
RUN echo "#!/bin/sh\n/usr/local/bin/wp --allow-root \"\$@\"" > /usr/local/bin/wpcli \
    && chmod +x /usr/local/bin/wpcli

# assert.php is the staged CLI tool; copy it into the image.
COPY assert.php /usr/local/bin/wpmgr-assert.php
RUN chmod +x /usr/local/bin/wpmgr-assert.php
