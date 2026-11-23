package ticket

import (
	"slices"
	"testing"
	"time"
)

func TestHoldingAndOpenStatusesFollowTheDesign(t *testing.T) {
	// the lists of docs/data-model.md
	wantHolding := []string{StatusApproved, StatusReady, StatusBorrowed}
	wantOpen := []string{StatusSubmitted, StatusApproved, StatusReady, StatusBorrowed}

	for _, status := range Statuses() {
		if IsHolding(status) != slices.Contains(wantHolding, status) {
			t.Errorf("IsHolding(%s) is %v", status, IsHolding(status))
		}

		if IsOpen(status) != slices.Contains(wantOpen, status) {
			t.Errorf("IsOpen(%s) is %v", status, IsOpen(status))
		}
	}

	if sqlList(holding) != "'disetujui', 'siap_diambil', 'dipinjam'" || sqlList(open) != "'diajukan', 'disetujui', 'siap_diambil', 'dipinjam'" {
		t.Fatalf("the SQL lists are %s and %s", sqlList(holding), sqlList(open))
	}
}

func TestEveryStepUsesAStoredStatusAndALeavingStatusIsNeverFinal(t *testing.T) {
	final := []string{StatusReturned, StatusCancelled, StatusRejected}

	for _, step := range steps {
		if !slices.Contains(Statuses(), step.From) || !slices.Contains(Statuses(), step.To) {
			t.Errorf("the step %+v names a status that does not exist", step)
		}

		if slices.Contains(final, step.From) {
			t.Errorf("the step %+v leaves a final status", step)
		}
	}
}

func TestAGuestMayOnlyCancelASubmittedTicket(t *testing.T) {
	for _, from := range Statuses() {
		for _, to := range Statuses() {
			want := from == StatusSubmitted && to == StatusCancelled

			if got := Allowed(from, to, ActorGuest); got != want {
				t.Errorf("a guest from %s to %s is %v", from, to, got)
			}
		}
	}

	if got := Sources(StatusCancelled, ActorGuest); !slices.Equal(got, []string{StatusSubmitted}) {
		t.Fatalf("the sources of a guest cancel are %v", got)
	}
}

func TestAnAdminFollowsTheFlowOfTheDesign(t *testing.T) {
	allowed := map[[2]string]bool{
		{StatusSubmitted, StatusApproved}: true, {StatusSubmitted, StatusRejected}: true,
		{StatusApproved, StatusReady}: true, {StatusApproved, StatusCancelled}: true,
		{StatusReady, StatusBorrowed}: true, {StatusReady, StatusCancelled}: true,
		{StatusBorrowed, StatusReturned}: true,
	}

	for _, from := range Statuses() {
		for _, to := range Statuses() {
			if got := Allowed(from, to, ActorAdmin); got != allowed[[2]string{from, to}] {
				t.Errorf("an admin from %s to %s is %v", from, to, got)
			}
		}
	}

	// an admin cancels a ticket that holds stock and rejects one that holds none
	if got := Sources(StatusCancelled, ActorAdmin); !slices.Equal(got, []string{StatusApproved, StatusReady}) {
		t.Fatalf("the sources of an admin cancel are %v", got)
	}
}

func TestEffectiveStatusIsLateOnlyAfterTheDueDate(t *testing.T) {
	pickup := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	approved := 3
	due := dueDate(pickup, 7, &approved)

	if due.Format(dateLayout) != "2026-11-23" || dueDate(pickup, 7, nil).Format(dateLayout) != "2026-11-27" {
		t.Fatalf("the due dates are %s and %s", due.Format(dateLayout), dueDate(pickup, 7, nil).Format(dateLayout))
	}

	tests := []struct {
		name   string
		status string
		now    time.Time
		want   string
	}{
		{"on the due date", StatusBorrowed, time.Date(2026, 11, 23, 16, 0, 0, 0, time.UTC), StatusBorrowed},
		// 17:00 UTC is midnight in Jakarta, so the next day has begun there
		{"after midnight in Jakarta", StatusBorrowed, time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC), StatusLate},
		{"long after", StatusBorrowed, time.Date(2026, 12, 30, 0, 0, 0, 0, time.UTC), StatusLate},
		{"another status is never late", StatusReady, time.Date(2026, 12, 30, 0, 0, 0, 0, time.UTC), StatusReady},
		{"a returned ticket is never late", StatusReturned, time.Date(2026, 12, 30, 0, 0, 0, 0, time.UTC), StatusReturned},
	}

	for _, tc := range tests {
		if got := effectiveStatus(tc.status, due, today(tc.now)); got != tc.want {
			t.Errorf("%s gives %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestTodayIsTheDateInJakarta(t *testing.T) {
	// 20:00 UTC on 17 November is 03:00 on 18 November in Jakarta
	got := today(time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC))
	if got.Format(dateLayout) != "2026-11-18" {
		t.Fatalf("today is %s", got.Format(dateLayout))
	}
}

func TestNumberSuffixUsesOnlyTheAlphabet(t *testing.T) {
	for range 200 {
		suffix, err := numberSuffix()
		if err != nil || len(suffix) != suffixLength {
			t.Fatalf("the suffix is %q, %v", suffix, err)
		}

		for _, r := range suffix {
			if !slices.Contains([]rune(numberAlphabet), r) {
				t.Fatalf("the suffix %q has the character %q", suffix, r)
			}
		}

		if !ValidNumber("MGM-2611-0001-" + suffix) {
			t.Fatalf("the number with the suffix %q has the wrong shape", suffix)
		}
	}

	if len(numberAlphabet) != 32 {
		t.Fatalf("the alphabet has %d characters", len(numberAlphabet))
	}
}
