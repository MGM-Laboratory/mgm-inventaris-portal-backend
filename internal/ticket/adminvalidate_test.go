package ticket

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
)

func strp(v string) *string {
	return &v
}

func approve(items ...ApprovedInput) DecisionRequest {
	return DecisionRequest{Version: intp(1), Decision: DecisionApprove, Items: items}
}

func TestADecisionNeedsTheVersionAndAKnownKind(t *testing.T) {
	for name, tc := range map[string]struct {
		req  DecisionRequest
		want []string
	}{
		"no version":     {DecisionRequest{Decision: DecisionReject, Note: strp("No stock.")}, []string{"version required"}},
		"version zero":   {DecisionRequest{Version: intp(0), Decision: DecisionReject, Note: strp("No stock.")}, []string{"version out_of_range"}},
		"no decision":    {DecisionRequest{Version: intp(1)}, []string{"decision required"}},
		"unknown":        {DecisionRequest{Version: intp(1), Decision: "maybe"}, []string{"decision invalid_format"}},
		"reject no note": {DecisionRequest{Version: intp(1), Decision: DecisionReject}, []string{"note required"}},
		"reject blank":   {DecisionRequest{Version: intp(1), Decision: DecisionReject, Note: strp("   ")}, []string{"note required"}},
		"reject long":    {DecisionRequest{Version: intp(1), Decision: DecisionReject, Note: strp(strings.Repeat("a", 501))}, []string{"note too_long"}},
		"reject NUL":     {DecisionRequest{Version: intp(1), Decision: DecisionReject, Note: strp("a\x00b")}, []string{"note invalid_format"}},
		"every field":    {DecisionRequest{Decision: "x"}, []string{"version required", "decision invalid_format"}},
	} {
		_, err := tc.req.check(testLimits)
		if got := details(t, err); !slices.Equal(got, tc.want) {
			t.Errorf("%s fails with %v, want %v", name, got, tc.want)
		}
	}

	got, err := DecisionRequest{Version: intp(4), Decision: DecisionReject, Note: strp("  No stock.  ")}.check(testLimits)
	if err != nil || got.approve || got.version != 4 || got.note == nil || *got.note != "No stock." {
		t.Fatalf("a rejection gives %+v, %v", got, err)
	}
}

func TestAnApprovalChecksItsItemsAndDays(t *testing.T) {
	two, zero, minus := intp(2), intp(0), intp(-1)

	for name, tc := range map[string]struct {
		req  DecisionRequest
		want []string
	}{
		"no items":         {approve(), []string{"items required"}},
		"all zero":         {approve(ApprovedInput{modelA, zero}, ApprovedInput{modelB, zero}), []string{"items out_of_range"}},
		"missing quantity": {approve(ApprovedInput{modelA, nil}), []string{"items[0].approvedQty required"}},
		"negative":         {approve(ApprovedInput{modelA, minus}), []string{"items[0].approvedQty out_of_range"}},
		"bad id":           {approve(ApprovedInput{"nope", two}), []string{"items[0].modelId invalid_format"}},
		"no id":            {approve(ApprovedInput{"", two}), []string{"items[0].modelId required"}},
		"twice":            {approve(ApprovedInput{modelA, two}, ApprovedInput{strings.ToUpper(modelA), two}), []string{"items[1].modelId duplicate"}},
		"too many days": {DecisionRequest{Version: intp(1), Decision: DecisionApprove, Items: []ApprovedInput{{modelA, two}}, DurationDays: intp(31)},
			[]string{"durationDays out_of_range"}},
		"zero days": {DecisionRequest{Version: intp(1), Decision: DecisionApprove, Items: []ApprovedInput{{modelA, two}}, DurationDays: zero},
			[]string{"durationDays out_of_range"}},
		"long note": {DecisionRequest{Version: intp(1), Decision: DecisionApprove, Items: []ApprovedInput{{modelA, two}}, Note: strp(strings.Repeat("a", 501))},
			[]string{"note too_long"}},
	} {
		_, err := tc.req.check(testLimits)
		if got := details(t, err); !slices.Equal(got, tc.want) {
			t.Errorf("%s fails with %v, want %v", name, got, tc.want)
		}
	}

	req := DecisionRequest{
		Version: intp(3), Decision: DecisionApprove, DurationDays: intp(30), Note: strp("  Take them on Monday.  "),
		Items: []ApprovedInput{{strings.ToUpper(modelA), two}, {modelB, zero}},
	}

	got, err := req.check(testLimits)
	if err != nil || !got.approve || got.version != 3 || *got.days != 30 || *got.note != "Take them on Monday." {
		t.Fatalf("an approval gives %+v, %v", got, err)
	}

	if len(got.items) != 2 || got.items[0] != (approved{index: 0, modelID: modelA, qty: 2}) || got.items[1] != (approved{index: 1, modelID: modelB, qty: 0}) {
		t.Fatalf("the items are %+v", got.items)
	}

	blank, err := DecisionRequest{Version: intp(1), Decision: DecisionApprove, Items: []ApprovedInput{{modelA, two}}, Note: strp("  ")}.check(testLimits)
	if err != nil || blank.note != nil {
		t.Fatalf("a blank note of an approval gives %+v, %v", blank, err)
	}
}

func TestAnApprovalMustMatchTheModelsOfTheTicket(t *testing.T) {
	models := []ticketModel{{id: modelA, name: "Example headset", qty: 2}, {id: modelB, name: "Example cable", qty: 1}}

	for name, tc := range map[string]struct {
		items []ApprovedInput
		want  []string
	}{
		"all listed":   {[]ApprovedInput{{modelA, intp(2)}, {modelB, intp(0)}}, nil},
		"fewer":        {[]ApprovedInput{{modelA, intp(1)}, {modelB, intp(1)}}, nil},
		"above asked":  {[]ApprovedInput{{modelA, intp(3)}, {modelB, intp(1)}}, []string{"items[0].approvedQty out_of_range"}},
		"model absent": {[]ApprovedInput{{modelA, intp(2)}}, []string{"items required"}},
		"not on it":    {[]ApprovedInput{{modelA, intp(2)}, {modelB, intp(1)}, {"00000000-0000-4000-8000-0000000000c3", intp(1)}}, []string{"items[2].modelId unknown_reference"}},
	} {
		in, err := approve(tc.items...).check(testLimits)
		if err != nil {
			t.Fatalf("%s: the body is refused %v", name, err)
		}

		err = in.matchItems(models)
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s is refused %v", name, err)
			}

			continue
		}

		if got := details(t, err); !slices.Equal(got, tc.want) {
			t.Errorf("%s fails with %v, want %v", name, got, tc.want)
		}
	}
}

func TestAHandoverNeedsTheVersionAndDistinctTags(t *testing.T) {
	for name, tc := range map[string]struct {
		req  HandoverRequest
		want []string
	}{
		"no version": {HandoverRequest{Tags: []string{"MGM-EXM-001"}}, []string{"version required"}},
		"no tags":    {HandoverRequest{Version: intp(1)}, []string{"tags required"}},
		"bad shape":  {HandoverRequest{Version: intp(1), Tags: []string{"MGM-EXM-001", "exm-1"}}, []string{"tags[1] invalid_format"}},
		"empty":      {HandoverRequest{Version: intp(1), Tags: []string{"  "}}, []string{"tags[0] required"}},
		"twice":      {HandoverRequest{Version: intp(1), Tags: []string{"MGM-EXM-001", " MGM-EXM-001"}}, []string{"tags[1] duplicate"}},
	} {
		_, err := tc.req.check()
		if got := details(t, err); !slices.Equal(got, tc.want) {
			t.Errorf("%s fails with %v, want %v", name, got, tc.want)
		}
	}

	got, err := HandoverRequest{Version: intp(3), Tags: []string{" MGM-EXM-001", "MGM-EXM-002"}}.check()
	if err != nil || got.version != 3 || !slices.Equal(got.tags, []string{"MGM-EXM-001", "MGM-EXM-002"}) {
		t.Fatalf("a handover gives %+v, %v", got, err)
	}
}

func TestTheUnitsOfAHandoverMustFitTheApprovedModels(t *testing.T) {
	models := []ticketModel{{id: modelA, name: "Example headset", qty: 2, approved: 2}, {id: modelB, name: "Example cable", qty: 1, approved: 0}}
	good := func(tag, model string) catalog.UnitRef {
		return catalog.UnitRef{ID: "id-" + tag, Tag: tag, ModelID: model, Lifecycle: catalog.LifecycleActive}
	}

	headset1, headset2 := good("MGM-EXM-001", modelA), good("MGM-EXM-002", modelA)
	cable := good("MGM-CBL-001", modelB)
	maintenance := headset2
	maintenance.Lifecycle = catalog.LifecycleMaintenance
	held := headset2
	held.Held = true

	t.Run("the exact quantity passes", func(t *testing.T) {
		ids, err := matchUnits([]string{"MGM-EXM-002", "MGM-EXM-001"}, []catalog.UnitRef{headset1, headset2}, models)
		if err != nil || !slices.Equal(ids, []string{"id-MGM-EXM-002", "id-MGM-EXM-001"}) {
			t.Fatalf("the ids are %v, %v", ids, err)
		}
	})

	for name, tc := range map[string]struct {
		tags []string
		refs []catalog.UnitRef
		want []string
	}{
		"no such unit": {[]string{"MGM-EXM-001", "MGM-EXM-009"}, []catalog.UnitRef{headset1}, []string{"tags[1] unknown_reference"}},
		"too few":      {[]string{"MGM-EXM-001"}, []catalog.UnitRef{headset1}, []string{"tags required"}},
		"too many":     {[]string{"MGM-EXM-001", "MGM-EXM-002", "MGM-EXM-003"}, []catalog.UnitRef{headset1, headset2, good("MGM-EXM-003", modelA)}, []string{"tags too_many"}},
	} {
		_, err := matchUnits(tc.tags, tc.refs, models)
		if got := details(t, err); !slices.Equal(got, tc.want) {
			t.Errorf("%s fails with %v, want %v", name, got, tc.want)
		}
	}

	for name, tc := range map[string]struct {
		tags []string
		refs []catalog.UnitRef
	}{
		"not active":   {[]string{"MGM-EXM-001", "MGM-EXM-002"}, []catalog.UnitRef{headset1, maintenance}},
		"held":         {[]string{"MGM-EXM-001", "MGM-EXM-002"}, []catalog.UnitRef{headset1, held}},
		"other model":  {[]string{"MGM-EXM-001", "MGM-CBL-001"}, []catalog.UnitRef{headset1, cable}},
		"not approved": {[]string{"MGM-CBL-001"}, []catalog.UnitRef{cable}},
	} {
		_, err := matchUnits(tc.tags, tc.refs, models)

		var rule *catalog.RuleError
		if !errors.As(err, &rule) || rule.Kind != catalog.RuleUnitUnavailable {
			t.Errorf("%s gives %v, want a unit that is unavailable", name, err)
		}
	}
}

func TestAReturnNeedsAnOutcomeForEveryUnitAndANoteForALostOne(t *testing.T) {
	line := func(tag, outcome string, note *string) ReturnLine {
		return ReturnLine{Tag: tag, Outcome: outcome, Note: note}
	}

	for name, tc := range map[string]struct {
		req  ReturnRequest
		want []string
	}{
		"no version":   {ReturnRequest{Units: []ReturnLine{line("MGM-EXM-001", "ok", nil)}}, []string{"version required"}},
		"no units":     {ReturnRequest{Version: intp(1)}, []string{"units required"}},
		"lost no note": {ReturnRequest{Version: intp(1), Units: []ReturnLine{line("MGM-EXM-001", "lost", nil)}}, []string{"units[0].note required"}},
		"lost blank":   {ReturnRequest{Version: intp(1), Units: []ReturnLine{line("MGM-EXM-001", "lost", strp(" "))}}, []string{"units[0].note required"}},
		"no outcome":   {ReturnRequest{Version: intp(1), Units: []ReturnLine{line("MGM-EXM-001", "", nil)}}, []string{"units[0].outcome required"}},
		"bad outcome":  {ReturnRequest{Version: intp(1), Units: []ReturnLine{line("MGM-EXM-001", "broken", nil)}}, []string{"units[0].outcome invalid_format"}},
		"bad tag":      {ReturnRequest{Version: intp(1), Units: []ReturnLine{line("x", "ok", nil)}}, []string{"units[0].tag invalid_format"}},
		"twice": {ReturnRequest{Version: intp(1), Units: []ReturnLine{line("MGM-EXM-001", "ok", nil), line("MGM-EXM-001", "ok", nil)}},
			[]string{"units[1].tag duplicate"}},
		"long note": {ReturnRequest{Version: intp(1), Units: []ReturnLine{line("MGM-EXM-001", "maintenance", strp(strings.Repeat("a", 501)))}},
			[]string{"units[0].note too_long"}},
	} {
		_, err := tc.req.check()
		if got := details(t, err); !slices.Equal(got, tc.want) {
			t.Errorf("%s fails with %v, want %v", name, got, tc.want)
		}
	}

	got, err := ReturnRequest{Version: intp(2), Units: []ReturnLine{line("MGM-EXM-001", "ok", nil), line("MGM-EXM-002", "lost", strp(" Not back. "))}}.check()
	if err != nil || len(got.units) != 2 || got.units[1].note == nil || *got.units[1].note != "Not back." {
		t.Fatalf("a return gives %+v, %v", got, err)
	}
}

func TestAReturnMustNameEveryHeldUnitAndNoOther(t *testing.T) {
	held := []heldUnit{{id: "u1", tag: "MGM-EXM-001", modelID: modelA}, {id: "u2", tag: "MGM-EXM-002", modelID: modelA}}

	build := func(lines ...ReturnLine) returned {
		in, err := ReturnRequest{Version: intp(1), Units: lines}.check()
		if err != nil {
			t.Fatalf("the body is refused %v", err)
		}

		return in
	}

	out, err := build(ReturnLine{Tag: "MGM-EXM-002", Outcome: "lost", Note: strp("Gone.")}, ReturnLine{Tag: "MGM-EXM-001", Outcome: "ok"}).matchHeld(held)
	if err != nil || len(out) != 2 || out[0].UnitID != "u2" || out[0].Outcome != "lost" || out[1].UnitID != "u1" {
		t.Fatalf("the units are %+v, %v", out, err)
	}

	_, err = build(ReturnLine{Tag: "MGM-EXM-001", Outcome: "ok"}).matchHeld(held)
	if got := details(t, err); !slices.Equal(got, []string{"units required"}) {
		t.Fatalf("a missing unit fails with %v", got)
	}

	_, err = build(ReturnLine{Tag: "MGM-EXM-001", Outcome: "ok"}, ReturnLine{Tag: "MGM-EXM-009", Outcome: "ok"}).matchHeld(held)
	if got := details(t, err); !slices.Equal(got, []string{"units[1].tag unknown_reference", "units required"}) {
		t.Fatalf("a stranger fails with %v", got)
	}
}

func TestTheHistoryNoteOfAReturnHoldsTheNotesOfItsLines(t *testing.T) {
	plain, err := ReturnRequest{Version: intp(1), Units: []ReturnLine{{Tag: "MGM-EXM-001", Outcome: "ok"}}}.check()
	if err != nil || plain.historyNote() != nil {
		t.Fatalf("a return with no note gives %v, %v", plain.historyNote(), err)
	}

	noted, err := ReturnRequest{Version: intp(1), Units: []ReturnLine{
		{Tag: "MGM-EXM-001", Outcome: "ok"},
		{Tag: "MGM-EXM-002", Outcome: "maintenance", Note: strp("Loose cable.")},
		{Tag: "MGM-EXM-003", Outcome: "lost", Note: strp("Not back.")},
	}}.check()
	if err != nil {
		t.Fatalf("the body is refused %v", err)
	}

	want := "MGM-EXM-002 (maintenance) - Loose cable.\nMGM-EXM-003 (lost) - Not back."
	if got := noted.historyNote(); got == nil || *got != want {
		t.Fatalf("the note is %v, want %q", got, want)
	}
}

func TestACancelAndAReadyNoticeNeedTheirFields(t *testing.T) {
	for name, tc := range map[string]struct {
		req  AdminCancelRequest
		want []string
	}{
		"nothing":    {AdminCancelRequest{}, []string{"version required", "note required"}},
		"no note":    {AdminCancelRequest{Version: intp(1)}, []string{"note required"}},
		"blank note": {AdminCancelRequest{Version: intp(1), Note: strp("  ")}, []string{"note required"}},
		"no version": {AdminCancelRequest{Note: strp("Nobody came.")}, []string{"version required"}},
	} {
		_, err := tc.req.check()
		if got := details(t, err); !slices.Equal(got, tc.want) {
			t.Errorf("%s fails with %v, want %v", name, got, tc.want)
		}
	}

	if _, err := (StepRequest{}).check(); err == nil {
		t.Fatal("a step with no version passes")
	}

	if v, err := (StepRequest{Version: intp(5)}).check(); err != nil || v != 5 {
		t.Fatalf("a step with a version gives %d, %v", v, err)
	}
}

func TestTheQueueFilterChecksItsFields(t *testing.T) {
	for name, tc := range map[string]struct {
		f    AdminFilter
		want []string
	}{
		"short text":  {AdminFilter{Query: "a"}, []string{"q too_short"}},
		"long text":   {AdminFilter{Query: strings.Repeat("a", 101)}, []string{"q too_long"}},
		"bad status":  {AdminFilter{Statuses: []string{"disetujui", "done"}}, []string{"status invalid_format"}},
		"empty value": {AdminFilter{Statuses: []string{""}}, []string{"status invalid_format"}},
		"bad date":    {AdminFilter{PickupFrom: "20-11-2026", DueTo: "2026-02-30"}, []string{"pickupFrom invalid_format", "dueTo invalid_format"}},
	} {
		_, err := tc.f.check()
		if got := details(t, err); !slices.Equal(got, tc.want) {
			t.Errorf("%s fails with %v, want %v", name, got, tc.want)
		}
	}

	got, err := AdminFilter{Query: " head ", Statuses: []string{"terlambat", "dipinjam", "terlambat"}, PickupFrom: "2026-11-01", DueTo: "2026-12-31"}.check()
	if err != nil || got.text != "head" || !slices.Equal(got.statuses, []string{"terlambat", "dipinjam"}) || got.pickupFrom == nil ||
		got.pickupFrom.Format(dateLayout) != "2026-11-01" || got.pickupTo != nil || got.dueTo == nil {
		t.Fatalf("the filter is %+v, %v", got, err)
	}

	none, err := AdminFilter{}.check()
	if err != nil || none.text != "" || none.statuses != nil {
		t.Fatalf("an empty filter gives %+v, %v", none, err)
	}
}

func TestAStepChecksTheStatusBeforeTheVersion(t *testing.T) {
	h := head{ID: "x", Status: StatusApproved, Version: 3}

	kind := func(err error) catalog.RuleKind {
		var rule *catalog.RuleError
		if !errors.As(err, &rule) {
			t.Fatalf("the error is %v", err)
		}

		return rule.Kind
	}

	if err := refuse(h, []string{StatusApproved}, 3); err != nil {
		t.Fatalf("a fitting ticket is refused %v", err)
	}

	if kind(refuse(h, []string{StatusApproved}, 2)) != catalog.RuleVersionConflict {
		t.Fatal("a stale version is not a version conflict")
	}

	if kind(refuse(h, []string{StatusReady}, 3)) != catalog.RuleStatusConflict {
		t.Fatal("a wrong status is not a status conflict")
	}

	if kind(refuse(h, []string{StatusReady}, 2)) != catalog.RuleStatusConflict {
		t.Fatal("a wrong status and a stale version are not a status conflict")
	}
}

func TestEveryStepOfTheAdminHasItsSourcesInTheTable(t *testing.T) {
	for _, tc := range []struct {
		to   string
		want []string
	}{
		{StatusApproved, []string{StatusSubmitted}},
		{StatusRejected, []string{StatusSubmitted}},
		{StatusReady, []string{StatusApproved}},
		{StatusBorrowed, []string{StatusReady}},
		{StatusReturned, []string{StatusBorrowed}},
		{StatusCancelled, []string{StatusApproved, StatusReady}},
	} {
		if got := Sources(tc.to, ActorAdmin); !slices.Equal(got, tc.want) {
			t.Errorf("an admin reaches %s from %v, want %v", tc.to, got, tc.want)
		}
	}
}
