// suppression_delete_refusal_test.go: a refused delete must not report success
// (GH #380).
//
// THE LIE THIS CLOSES.
//
// DELETE /sites/:siteId/email/suppression/:suppressionId resolves the entry by
// id with tenant scope and no site check, and deliberately so on the read side:
// a fleet-wide entry (site_id IS NULL) is visible to every site because the
// pre-send check matches it, and hiding it would let a collaborator's site mail
// an address the organisation stopped. m112 then refuses the DELETE of that row
// for a site-scoped principal.
//
// Postgres reports that refusal as zero rows affected, not as an error. So the
// endpoint answered 204: the fleet-wide entry was still in force and the caller
// had been told it was lifted. Nothing failed, nothing was logged, and an audit
// entry was written saying the suppression had been deleted.
//
// The OpenAPI spec already declared 403 and 404 on both delete routes. The code
// could not produce either. These tests pin that it now can.
package email

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// deleteSuppressionKind runs the service delete with a seeded repo outcome and
// returns the domain error kind the caller would be answered with.
func deleteSuppressionKind(t *testing.T, repoErr error) (*domain.Error, error) {
	t.Helper()
	repo := newFakeRepo()
	repo.suppressionDeleteErr = repoErr

	svc := NewService(&Repo{}, nil, nil)
	svc.repo = repo

	err := svc.DeleteSuppression(context.Background(), uuid.New(), uuid.New())
	var dErr *domain.Error
	if errors.As(err, &dErr) {
		return dErr, err
	}
	return nil, err
}

// TestRefusedSuppressionDeleteIsReportedAsForbidden is the case that used to
// answer 204. 403 rather than 404 because the entry is really there and the
// caller can really see it; telling them it does not exist would be a second,
// smaller lie, and it hides the one action that would work (asking an
// organisation member).
func TestRefusedSuppressionDeleteIsReportedAsForbidden(t *testing.T) {
	dErr, err := deleteSuppressionKind(t, ErrSuppressionRefused)
	if err == nil {
		t.Fatal("a delete that Postgres refused was reported as SUCCESS; the caller is told " +
			"a fleet-wide suppression entry is gone while it is still in force")
	}
	if dErr == nil {
		t.Fatalf("want a domain error the handler can map to a status, got %v", err)
	}
	if dErr.Kind != domain.KindForbidden {
		t.Fatalf("refused delete mapped to kind %v, want KindForbidden (403)", dErr.Kind)
	}
	if dErr.Code != "suppression_delete_forbidden" {
		t.Fatalf("refused delete code = %q", dErr.Code)
	}
	// The message has to be actionable: an operator who cannot remove the entry
	// needs to know it is organisation-wide and still applies.
	if dErr.Message == "" {
		t.Fatal("the refusal must explain itself; a bare 403 sends the operator back to " +
			"guessing, which is what the 204 did")
	}
}

// TestAbsentSuppressionDeleteIsReportedAsNotFound keeps the two failure modes
// apart. A row that is not there for this principal is a 404, and must not be
// dressed up as a permission problem.
func TestAbsentSuppressionDeleteIsReportedAsNotFound(t *testing.T) {
	dErr, err := deleteSuppressionKind(t, ErrNotFound)
	if err == nil {
		t.Fatal("deleting an entry that does not exist was reported as success")
	}
	if dErr == nil {
		t.Fatalf("want a domain error, got %v", err)
	}
	if dErr.Kind != domain.KindNotFound {
		t.Fatalf("absent delete mapped to kind %v, want KindNotFound (404)", dErr.Kind)
	}
}

// TestSuccessfulSuppressionDeleteStaysSilent is the regression guard. The whole
// change is about not lying on the refusal path; the ordinary path must go on
// answering 204 exactly as before, or every operator un-suppressing an address
// starts seeing errors.
func TestSuccessfulSuppressionDeleteStaysSilent(t *testing.T) {
	if _, err := deleteSuppressionKind(t, nil); err != nil {
		t.Fatalf("a delete that removed the row must still succeed: %v", err)
	}
}

// TestUnexpectedSuppressionDeleteErrorIsStillInternal pins that the two named
// outcomes did not swallow the third. A database that will not answer is not a
// permission problem and must not be reported as one: an operator told "you may
// not do this" stops retrying, which is precisely the wrong response to an
// outage.
func TestUnexpectedSuppressionDeleteErrorIsStillInternal(t *testing.T) {
	dErr, err := deleteSuppressionKind(t, errors.New("connection reset by peer"))
	if err == nil {
		t.Fatal("a database failure was reported as success")
	}
	if dErr == nil {
		t.Fatalf("want a domain error, got %v", err)
	}
	if dErr.Kind != domain.KindInternal {
		t.Fatalf("an unexpected repo error mapped to kind %v, want KindInternal (500)", dErr.Kind)
	}
}
