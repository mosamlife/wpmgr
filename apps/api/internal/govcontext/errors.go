package govcontext

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// errVersionConflict is returned by Repo.CreateOrgVersion / CreateSiteVersion
// when the insert lost a race against a concurrent writer for the same
// version number — org_context_versions_version_key / site_context_versions_
// version_key (m122) turn that race into SQLSTATE 23505 instead of a silent
// lost update. service.go maps this to the same 409 context_version_conflict
// response as an application-level base_version mismatch (ADR-064 open
// question 2 — see service.go's doc comment for the full concurrency design).
var errVersionConflict = errors.New("govcontext: version conflict")

// isUniqueViolation reports whether err is Postgres' 23505, the same helper
// shape as internal/sitetag/repo.go and internal/update/dispatch_repo.go.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
