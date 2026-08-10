<?php
/**
 * A minimal, self-contained double of \PHPMailer\PHPMailer\PHPMailer, used
 * only by SmtpHandlerTest.php.
 *
 * PHPMailer is not a Composer dependency of this plugin. WordPress bundles
 * it at runtime under ABSPATH/wp-includes/PHPMailer/, which does not exist in
 * the unit-test process. SmtpHandler's own guard
 * (`class_exists('PHPMailer\PHPMailer\PHPMailer')`) is what lets this double
 * stand in: once it is declared, SmtpHandler skips trying to require the real
 * bundled files and constructs this class instead.
 *
 * Kept in its own file (rather than declared inline in the test class) since
 * it lives under a different namespace (PHPMailer\PHPMailer) than the test
 * itself (WPMgr\Agent\Tests\Email); PHP does not allow mixing a bracketed
 * and an unbracketed namespace declaration in one file. The class
 * declarations here are unconditional at the top level and guarded by
 * class_exists(), matching the project's existing WP_Upgrader / Plugin_Upgrader
 * doubles in tests/bootstrap.php, and are self-contained (no Patchwork
 * function redefinition involved, so nothing here can leak state into an
 * unrelated test the way a Functions\when() stub can).
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace PHPMailer\PHPMailer;

if (!class_exists('PHPMailer\PHPMailer\Exception')) {
    /**
     * Stand-in for \PHPMailer\PHPMailer\Exception.
     */
    class Exception extends \Exception
    {
    }
}

if (!class_exists('PHPMailer\PHPMailer\FakePhpMailerRegistry')) {
    /**
     * Records the most recently constructed FakePhpMailer instance so a test
     * can inspect what SmtpHandler did with it after send() returns.
     */
    class FakePhpMailerRegistry
    {
        public static ?PHPMailer $last = null;
    }
}

if (!class_exists('PHPMailer\PHPMailer\PHPMailer')) {
    /**
     * Minimal double of \PHPMailer\PHPMailer\PHPMailer sufficient to exercise
     * SmtpHandler's address-handling logic without the real bundled library.
     *
     * Mirrors the real class's default address validator (FILTER_VALIDATE_EMAIL)
     * and its exact "Invalid address:  (%s): %s" exception message (note the
     * double space: the real 'invalid_address' language string itself ends in
     * a trailing space) closely enough to reproduce GH #312's reported failure
     * faithfully when exercised against the UNFIXED handler code.
     */
    class PHPMailer
    {
        public const ENCRYPTION_SMTPS = 'ssl';
        public const ENCRYPTION_STARTTLS = 'tls';

        public string $CharSet = 'UTF-8';
        public string $Host = '';
        public int $Port = 587;
        public string $SMTPSecure = '';
        public bool $SMTPAutoTLS = true;
        public bool $SMTPAuth = false;
        public string $Username = '';
        public string $Password = '';
        public string $Subject = '';
        public string $Body = '';
        public string $AltBody = '';
        public string $Sender = '';

        /** @var array<int,array{address:string,name:string}> */
        public array $recordedTo = [];
        /** @var array<int,array{address:string,name:string}> */
        public array $recordedCc = [];
        /** @var array<int,array{address:string,name:string}> */
        public array $recordedBcc = [];
        /** @var array<int,array{address:string,name:string}> */
        public array $recordedReplyTo = [];
        /** @var array{address:string,name:string}|null */
        public ?array $recordedFrom = null;

        private bool $exceptionsEnabled;

        public function __construct(bool $exceptions = false)
        {
            $this->exceptionsEnabled = $exceptions;
            FakePhpMailerRegistry::$last = $this;
        }

        public function isSMTP(): void
        {
        }

        public function isHTML(bool $isHtml = true): void
        {
        }

        public function setFrom(string $address, string $name = ''): bool
        {
            $this->recordedFrom = ['address' => $address, 'name' => $name];
            return true;
        }

        public function addAddress(string $address, string $name = ''): bool
        {
            return $this->addAnAddress('To', $address, $name, $this->recordedTo);
        }

        public function addCC(string $address, string $name = ''): bool
        {
            return $this->addAnAddress('Cc', $address, $name, $this->recordedCc);
        }

        public function addBCC(string $address, string $name = ''): bool
        {
            return $this->addAnAddress('Bcc', $address, $name, $this->recordedBcc);
        }

        public function addReplyTo(string $address, string $name = ''): bool
        {
            return $this->addAnAddress('Reply-To', $address, $name, $this->recordedReplyTo);
        }

        /**
         * @param array<int,array{address:string,name:string}> $bucket
         */
        private function addAnAddress(string $kind, string $address, string $name, array &$bucket): bool
        {
            if (filter_var($address, FILTER_VALIDATE_EMAIL) === false) {
                $message = sprintf('Invalid address:  (%s): %s', $kind, $address);
                if ($this->exceptionsEnabled) {
                    throw new Exception($message);
                }
                return false;
            }
            $bucket[] = ['address' => $address, 'name' => $name];
            return true;
        }

        public function addCustomHeader(string $name, ?string $value = null): bool
        {
            return true;
        }

        public function addAttachment(string $path, string $name = '', string $encoding = 'base64', string $type = ''): bool
        {
            return true;
        }

        public function send(): bool
        {
            if ($this->recordedTo === [] && $this->recordedCc === [] && $this->recordedBcc === []) {
                if ($this->exceptionsEnabled) {
                    throw new Exception('You must provide at least one recipient email address.');
                }
                return false;
            }
            // A real server answers an AUTH exchange carrying an empty password
            // with a rejection, which the bundled PHPMailer surfaces as this
            // exact string (wp-includes/PHPMailer/PHPMailer.php). Modelling it
            // here is what makes an empty credential distinguishable from a
            // wrong one in a unit test: to the sender they look identical.
            if ($this->SMTPAuth && $this->Password === '') {
                if ($this->exceptionsEnabled) {
                    throw new Exception('SMTP Error: Could not authenticate.');
                }
                return false;
            }
            return true;
        }

        public function getLastMessageID(): string
        {
            return 'fake-message-id';
        }
    }
}
