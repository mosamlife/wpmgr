package diagnostics

import "testing"

// TestAllCategoriesAreValid: every entry in AllCategories() must pass
// ValidCategory. Otherwise the operator GET handler would emit a card with a
// category string that the IngestDiagnostics path would reject — a silent
// "stuck on placeholder" bug.
func TestAllCategoriesAreValid(t *testing.T) {
	for _, c := range AllCategories() {
		if !ValidCategory(c) {
			t.Errorf("AllCategories includes %q but ValidCategory rejects it", c)
		}
	}
}

// TestValidCategoryRejectsUnknown sanity-checks the negative path.
func TestValidCategoryRejectsUnknown(t *testing.T) {
	if ValidCategory(Category("not-a-category")) {
		t.Error("ValidCategory should reject unknown categories")
	}
	if ValidCategory(Category("")) {
		t.Error("ValidCategory should reject empty string")
	}
}

// TestAllCategoriesHas15 — Site-Health-Full (v0.9.14) lifted the count from
// 14 to 15 with the addition of `wp_native` (the verbatim WP_Debug_Data
// dump). A miscount from a future refactor would silently drop the headline
// Directory-Sizes / Media / Constants / Permissions cards.
func TestAllCategoriesHas15(t *testing.T) {
	if got, want := len(AllCategories()), 15; got != want {
		t.Errorf("AllCategories length = %d, want %d", got, want)
	}
}

// TestWPNativeIsValid — defence-in-depth: the wp_native ingest must not be
// rejected by ValidCategory.
func TestWPNativeIsValid(t *testing.T) {
	if !ValidCategory(CategoryWPNative) {
		t.Error("CategoryWPNative must pass ValidCategory")
	}
}
