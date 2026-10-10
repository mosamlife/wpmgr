package aipolicy

import "time"

// BudgetWindow is the rolling window every automatic-change count covers.
const BudgetWindow = 60 * time.Minute

// The fixed per-connection limits on changes approved by a site's setting.
// They are server constants, shown read-only; above one, a change waits for
// a person instead of running.
const (
	// DraftChangesPerConnection bounds automatic changes to drafts
	// (ai_draft, unpublished) per connection per window.
	DraftChangesPerConnection = 600
	// DraftSitesPerConnection bounds the distinct sites those changes are on.
	DraftSitesPerConnection = 30
)

// Bucket names a budget.
type Bucket string

// The budgets this build counts.
const (
	// BucketNone: the class has no budget this build counts, so it never
	// runs automatically.
	BucketNone Bucket = ""
	// BucketDraft: ai_draft and unpublished changes.
	BucketDraft Bucket = "draft"
)

// BucketOf is the budget a class's automatic changes count against.
func BucketOf(c Class) Bucket {
	switch c {
	case ClassAIDraft, ClassUnpublished:
		return BucketDraft
	}
	return BucketNone
}

// DraftClasses is the classes the draft budget counts, for the count query.
func DraftClasses() []Class { return []Class{ClassAIDraft, ClassUnpublished} }

// Usage is one connection's automatic changes in a bucket, counted inside
// the approving transaction under the tenant's policy lock.
type Usage struct {
	// Checked is true when the counts below were read. A bucket nobody read
	// asks with not_checked.
	Checked bool
	// Changes is the automatic changes in the window.
	Changes int64
	// Sites is the distinct sites those changes are on.
	Sites int64
	// SiteCounted is true when this request's site is already among Sites,
	// so this change adds no site.
	SiteCounted bool
	// OldestAt is when the oldest counted change was decided; the window
	// frees a change at OldestAt + BudgetWindow. Zero when nothing counted.
	OldestAt time.Time
}

// overBudget says which limit a change in bucket b would pass, if any.
func overBudget(b Bucket, u Usage) (AskReason, bool) {
	switch b {
	case BucketDraft:
		if u.Changes >= DraftChangesPerConnection {
			return AskOverChangeBudget, true
		}
		if !u.SiteCounted && u.Sites >= DraftSitesPerConnection {
			return AskOverSiteCap, true
		}
		return "", false
	}
	return AskNotChecked, true
}

// ResumesAt is when automatic changes in the bucket may resume after an
// over_change_budget Ask: the oldest counted change leaves the window. Zero
// when nothing was counted.
func (u Usage) ResumesAt() time.Time {
	if u.OldestAt.IsZero() {
		return time.Time{}
	}
	return u.OldestAt.Add(BudgetWindow)
}
