package catalog

import (
	"errors"
	"strings"
	"testing"
)

func ptr[T any](v T) *T {
	return &v
}

// fields returns "field:code" for every failed field of the error, or nil for no error.
func fields(t *testing.T, err error) []string {
	t.Helper()

	if err == nil {
		return nil
	}

	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("the error is %v, not a ValidationError", err)
	}

	out := make([]string, 0, len(invalid.Details))
	for _, d := range invalid.Details {
		out = append(out, d.Field+":"+d.Code)
	}

	return out
}

func sameFields(got, want []string) bool {
	return strings.Join(got, ",") == strings.Join(want, ",")
}

func TestNewModelCheck(t *testing.T) {
	good := func() NewModel {
		return NewModel{Name: "Example headset", CategoryID: ptr("00000000-0000-4000-8000-000000000001"), Prefix: "EXM"}
	}

	tests := []struct {
		name   string
		mutate func(*NewModel)
		want   []string
	}{
		{"a good body", func(*NewModel) {}, nil},
		{"a new category", func(m *NewModel) {
			m.CategoryID, m.NewCategory = nil, &CategoryInput{Name: "Example category", Tone: "blue"}
		}, nil},
		{"both categories", func(m *NewModel) { m.NewCategory = &CategoryInput{Name: "Example category", Tone: "blue"} }, []string{"categoryId:required"}},
		{"no category", func(m *NewModel) { m.CategoryID = nil }, []string{"categoryId:required"}},
		{"a category id that is no UUID", func(m *NewModel) { m.CategoryID = ptr("not-a-uuid") }, []string{"categoryId:invalid_format"}},
		{"a new category with a short name and a wrong tone", func(m *NewModel) {
			m.CategoryID, m.NewCategory = nil, &CategoryInput{Name: "x", Tone: "purple"}
		}, []string{"newCategory.name:too_short", "newCategory.tone:invalid_format"}},
		{"no name", func(m *NewModel) { m.Name = "  " }, []string{"name:required"}},
		{"a name of one character", func(m *NewModel) { m.Name = "x" }, []string{"name:too_short"}},
		{"a name of 101 characters", func(m *NewModel) { m.Name = strings.Repeat("a", 101) }, []string{"name:too_long"}},
		{"a name of 100 characters", func(m *NewModel) { m.Name = strings.Repeat("a", 100) }, nil},
		{"a description of 501 characters", func(m *NewModel) { m.Description = ptr(strings.Repeat("a", 501)) }, []string{"description:too_long"}},
		{"no prefix", func(m *NewModel) { m.Prefix = "" }, []string{"prefix:required"}},
		{"a prefix in lower case", func(m *NewModel) { m.Prefix = "exm" }, []string{"prefix:invalid_format"}},
		{"a prefix of one character", func(m *NewModel) { m.Prefix = "E" }, []string{"prefix:invalid_format"}},
		{"a prefix of seven characters", func(m *NewModel) { m.Prefix = "ABCDEFG" }, []string{"prefix:invalid_format"}},
		{"a prefix with a hyphen", func(m *NewModel) { m.Prefix = "EX-M" }, []string{"prefix:invalid_format"}},
	}

	for _, tc := range tests {
		m := good()
		tc.mutate(&m)

		if _, err := m.check(); !sameFields(fields(t, err), tc.want) {
			t.Fatalf("%s gives %v, want %v", tc.name, fields(t, err), tc.want)
		}
	}
}

func TestNewModelCheckTrimsTheText(t *testing.T) {
	got, err := NewModel{Name: "  Example headset ", CategoryID: ptr("00000000-0000-4000-8000-000000000001"), Description: ptr(" Text "), Prefix: "EXM"}.check()
	if err != nil || got.Name != "Example headset" || *got.Description != "Text" {
		t.Fatalf("the clean input is %+v, %v", got, err)
	}
}

func TestUnitInputCheck(t *testing.T) {
	good := func() UnitInput {
		return UnitInput{Count: ptr(1), Serial: ptr("SN-0001"), Condition: "Good", Place: "Example shelf"}
	}

	tests := []struct {
		name   string
		mutate func(*UnitInput)
		want   []string
	}{
		{"a good body", func(*UnitInput) {}, nil},
		{"twenty units without a serial", func(u *UnitInput) { u.Count, u.Serial = ptr(20), nil }, nil},
		{"no count", func(u *UnitInput) { u.Count = nil }, []string{"count:required"}},
		{"a count of 0", func(u *UnitInput) { u.Count = ptr(0) }, []string{"count:out_of_range"}},
		{"a count of 21", func(u *UnitInput) { u.Count, u.Serial = ptr(21), nil }, []string{"count:out_of_range"}},
		{"a serial with two units", func(u *UnitInput) { u.Count = ptr(2) }, []string{"serial:invalid_format"}},
		{"an empty serial with two units", func(u *UnitInput) { u.Count, u.Serial = ptr(2), ptr("") }, nil},
		{"a serial of two characters", func(u *UnitInput) { u.Serial = ptr("ab") }, []string{"serial:too_short"}},
		{"a serial of 101 characters", func(u *UnitInput) { u.Serial = ptr(strings.Repeat("a", 101)) }, []string{"serial:too_long"}},
		{"a serial that is spaces", func(u *UnitInput) { u.Serial = ptr("   ") }, nil},
		{"no condition", func(u *UnitInput) { u.Condition = "" }, []string{"condition:required"}},
		{"a place of one character", func(u *UnitInput) { u.Place = "x" }, []string{"place:too_short"}},
		{"a place of 201 characters", func(u *UnitInput) { u.Place = strings.Repeat("a", 201) }, []string{"place:too_long"}},
	}

	for _, tc := range tests {
		u := good()
		tc.mutate(&u)

		if _, err := u.check(); !sameFields(fields(t, err), tc.want) {
			t.Fatalf("%s gives %v, want %v", tc.name, fields(t, err), tc.want)
		}
	}
}

func TestAnEmptySerialMeansNoSerial(t *testing.T) {
	got, err := UnitInput{Count: ptr(1), Serial: ptr("  "), Condition: "Good", Place: "Example shelf"}.check()
	if err != nil || got.Serial != nil {
		t.Fatalf("the serial is %v, %v", got.Serial, err)
	}

	patch, err := UnitPatch{SerialSet: true, Serial: ptr("")}.check()
	if err != nil || !patch.SerialSet || patch.Serial != nil {
		t.Fatalf("the patch is %+v, %v", patch, err)
	}
}

func TestUnitPatchCheck(t *testing.T) {
	tests := []struct {
		name string
		in   UnitPatch
		want []string
	}{
		{"nothing", UnitPatch{}, []string{"body:required"}},
		{"a serial set to null", UnitPatch{SerialSet: true}, nil},
		{"a condition", UnitPatch{Condition: ptr("A small scratch")}, nil},
		{"a short place", UnitPatch{Place: ptr("x")}, []string{"place:too_short"}},
		{"an empty condition", UnitPatch{Condition: ptr(" ")}, []string{"condition:required"}},
		{"a short serial", UnitPatch{SerialSet: true, Serial: ptr("ab")}, []string{"serial:too_short"}},
	}

	for _, tc := range tests {
		if _, err := tc.in.check(); !sameFields(fields(t, err), tc.want) {
			t.Fatalf("%s gives %v, want %v", tc.name, fields(t, err), tc.want)
		}
	}
}

func TestModelPatchCheck(t *testing.T) {
	tests := []struct {
		name string
		in   ModelPatch
		want []string
	}{
		{"nothing", ModelPatch{}, []string{"body:required"}},
		{"retired only", ModelPatch{Retired: ptr(true)}, nil},
		{"a description that is empty", ModelPatch{Description: ptr("")}, nil},
		{"a category id that is no UUID", ModelPatch{CategoryID: ptr("x")}, []string{"categoryId:invalid_format"}},
		{"a name of one character", ModelPatch{Name: ptr("x")}, []string{"name:too_short"}},
	}

	for _, tc := range tests {
		if _, err := tc.in.check(); !sameFields(fields(t, err), tc.want) {
			t.Fatalf("%s gives %v, want %v", tc.name, fields(t, err), tc.want)
		}
	}
}

func TestLifecycleChangeCheck(t *testing.T) {
	tests := []struct {
		name string
		in   LifecycleChange
		want []string
	}{
		{"maintenance without a note", LifecycleChange{Lifecycle: "maintenance"}, nil},
		{"active with a note", LifecycleChange{Lifecycle: "active", Note: ptr("Repaired.")}, nil},
		{"lost without a note", LifecycleChange{Lifecycle: "lost"}, []string{"note:required"}},
		{"lost with a note of spaces", LifecycleChange{Lifecycle: "lost", Note: ptr("   ")}, []string{"note:required"}},
		{"retired without a note", LifecycleChange{Lifecycle: "retired"}, []string{"note:required"}},
		{"retired with a note", LifecycleChange{Lifecycle: "retired", Note: ptr("Broken for good.")}, nil},
		{"a note of 501 characters", LifecycleChange{Lifecycle: "lost", Note: ptr(strings.Repeat("a", 501))}, []string{"note:too_long"}},
		{"no lifecycle", LifecycleChange{}, []string{"lifecycle:required"}},
		{"another lifecycle", LifecycleChange{Lifecycle: "borrowed"}, []string{"lifecycle:invalid_format"}},
	}

	for _, tc := range tests {
		if _, err := tc.in.check(); !sameFields(fields(t, err), tc.want) {
			t.Fatalf("%s gives %v, want %v", tc.name, fields(t, err), tc.want)
		}
	}
}

func TestFilterChecks(t *testing.T) {
	if _, err := (ModelFilter{Query: "a"}).check(); !sameFields(fields(t, err), []string{"q:too_short"}) {
		t.Fatalf("a short search gives %v", fields(t, err))
	}

	if _, err := (ModelFilter{Query: strings.Repeat("a", 101), CategoryID: "x"}).check(); !sameFields(fields(t, err), []string{"q:too_long", "categoryId:invalid_format"}) {
		t.Fatalf("a long search gives %v", fields(t, err))
	}

	if got, err := (ModelFilter{Query: "  "}).check(); err != nil || got.Query != "" {
		t.Fatalf("a search of spaces gives %q, %v", got.Query, err)
	}

	if _, err := (UnitFilter{Status: "reserved", ModelID: "x"}).check(); !sameFields(fields(t, err), []string{"modelId:invalid_format", "status:invalid_format"}) {
		t.Fatalf("a bad status gives %v", fields(t, err))
	}

	for _, status := range []string{"", "available", "borrowed", "maintenance", "lost", "retired"} {
		if _, err := (UnitFilter{Status: status}).check(); err != nil {
			t.Fatalf("the status %q fails with %v", status, err)
		}
	}
}

func TestCategoryInputCheck(t *testing.T) {
	var c checker

	CategoryInput{Name: " a ", Tone: ""}.check("", &c)

	if got := fields(t, c.err()); !sameFields(got, []string{"name:too_short", "tone:required"}) {
		t.Fatalf("a bad category gives %v", got)
	}

	c = checker{}
	CategoryInput{Name: strings.Repeat("a", 61), Tone: "green"}.check("", &c)

	if got := fields(t, c.err()); !sameFields(got, []string{"name:too_long"}) {
		t.Fatalf("a long name gives %v", got)
	}
}

func TestTagShapeAndSteps(t *testing.T) {
	for tag, want := range map[string]bool{
		"MGM-EXM-001": true, "MGM-EXM-1000": true, "MGM-E1-123": true,
		"MGM-EXM-01": false, "MGM-exm-001": false, "MGM-E-001": false, "MGM-EXM-001 ": false, "": false, "MGM-EXM-00a": false,
	} {
		if got := ValidTag(tag); got != want {
			t.Fatalf("ValidTag(%q) is %v, want %v", tag, got, want)
		}
	}

	// the table of docs/data-model.md, step by step
	allowed := map[[2]string]bool{
		{"active", "maintenance"}: true, {"active", "lost"}: true, {"active", "retired"}: true,
		{"maintenance", "active"}: true, {"maintenance", "lost"}: true, {"maintenance", "retired"}: true,
		{"lost", "active"}: true, {"lost", "retired"}: true,
		{"retired", "active"}: true,
	}

	for _, from := range lifecycles {
		for _, to := range lifecycles {
			got := false
			for _, s := range steps[from] {
				got = got || s == to
			}

			if got != allowed[[2]string{from, to}] {
				t.Fatalf("the step %s to %s is %v", from, to, got)
			}
		}
	}
}

func TestTextPostgreSQLWouldRefuseIsInvalidFormat(t *testing.T) {
	for _, bad := range []string{"ab\x00cd", "ab\xff\xfecd"} {
		if _, err := (UnitInput{Count: ptr(1), Serial: ptr("SN-" + bad), Condition: bad, Place: bad}).check(); !sameFields(fields(t, err), []string{"serial:invalid_format", "condition:invalid_format", "place:invalid_format"}) {
			t.Fatalf("%q in a unit gives %v", bad, fields(t, err))
		}

		if _, err := (ModelFilter{Query: bad}).check(); !sameFields(fields(t, err), []string{"q:invalid_format"}) {
			t.Fatalf("%q in a search gives %v", bad, fields(t, err))
		}

		if _, err := (LifecycleChange{Lifecycle: LifecycleMaintenance, Note: ptr(bad)}).check(); !sameFields(fields(t, err), []string{"note:invalid_format"}) {
			t.Fatalf("%q in a note gives %v", bad, fields(t, err))
		}
	}
}
