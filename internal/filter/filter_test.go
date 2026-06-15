package filter_test

import (
	"testing"

	"domains.lst/sub-preprocessor/internal/filter"
)

func TestParseAllowed_CountriesOnly(t *testing.T) {
	t.Parallel()

	set := filter.ParseAllowed("DE,US,  nl  ", "", nil)
	if !set.Has("DE") {
		t.Fatal("expected DE")
	}
	if !set.Has("US") {
		t.Fatal("expected US")
	}
	if !set.Has("NL") {
		t.Fatal("expected NL")
	}
	if set.Has("GB") {
		t.Fatal("unexpected GB")
	}
}

func TestParseAllowed_GroupsOnly(t *testing.T) {
	t.Parallel()

	groups := map[string][]string{
		"nordics": {"FI", "SE", "NO", "DK"},
		"baltics": {"EE", "LV", "LT"},
	}

	set := filter.ParseAllowed("", "nordics", groups)
	if !set.Has("FI") {
		t.Fatal("expected FI from group")
	}
	if !set.Has("SE") {
		t.Fatal("expected SE from group")
	}
	if !set.Has("NO") {
		t.Fatal("expected NO from group")
	}
	if !set.Has("DK") {
		t.Fatal("expected DK from group")
	}
	if set.Has("EE") {
		t.Fatal("unexpected EE (not in group)")
	}
	if set.Has("DE") {
		t.Fatal("unexpected DE")
	}
}

func TestParseAllowed_CountriesAndGroups(t *testing.T) {
	t.Parallel()

	groups := map[string][]string{
		"nordics": {"FI", "SE"},
	}

	set := filter.ParseAllowed("DE", "nordics", groups)
	if !set.Has("DE") {
		t.Fatal("expected DE from countries")
	}
	if !set.Has("FI") {
		t.Fatal("expected FI from group")
	}
	if !set.Has("SE") {
		t.Fatal("expected SE from group")
	}
}

func TestParseAllowed_UnknownGroupIgnored(t *testing.T) {
	t.Parallel()

	groups := map[string][]string{
		"nordics": {"FI"},
	}

	set := filter.ParseAllowed("DE", "unknown_group", groups)
	if !set.Has("DE") {
		t.Fatal("expected DE")
	}
	if set.Has("FI") {
		t.Fatal("unexpected FI (unknown group)")
	}
}

func TestParseAllowed_EmptyCountriesAndGroups(t *testing.T) {
	t.Parallel()

	set := filter.ParseAllowed("", "", nil)
	// Should produce empty set (no error expected)
	for _, v := range set {
		if v != 0 {
			t.Fatal("expected empty set")
		}
	}
}

func TestParseAllowed_MultipleGroups(t *testing.T) {
	t.Parallel()

	groups := map[string][]string{
		"nordics": {"FI", "SE"},
		"euro5":   {"DE", "FR", "GB", "IT", "ES"},
	}

	set := filter.ParseAllowed("", "nordics,euro5", groups)
	if !set.Has("FI") || !set.Has("SE") {
		t.Fatal("expected nordics countries")
	}
	if !set.Has("DE") || !set.Has("FR") || !set.Has("GB") || !set.Has("IT") || !set.Has("ES") {
		t.Fatal("expected euro5 countries")
	}
}

func TestParseAllowed_GroupsWithSpaces(t *testing.T) {
	t.Parallel()

	groups := map[string][]string{
		"nordics": {"FI", "SE"},
	}

	set := filter.ParseAllowed("", " nordics ", groups)
	if !set.Has("FI") {
		t.Fatal("expected FI from group with spaces")
	}
}
