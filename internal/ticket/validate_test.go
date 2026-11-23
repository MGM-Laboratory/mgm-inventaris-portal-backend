package ticket

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
)

const (
	modelA = "00000000-0000-4000-8000-0000000000a1"
	modelB = "00000000-0000-4000-8000-0000000000b2"
)

var (
	testLimits = Limits{MaxItems: 10, MinDays: 1, MaxDays: 30, MaxOpen: 3}
	// 20:00 UTC is 03:00 on 18 November in Jakarta
	testNow = time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC)
)

func intp(v int) *int {
	return &v
}

func validRequest() CreateRequest {
	return CreateRequest{
		Items:        []ItemInput{{ModelID: modelA, Qty: intp(2)}},
		Purpose:      "Example loan for a lab exercise.",
		PickupDate:   "2026-11-20",
		DurationDays: intp(7),
	}
}

// details returns "field code" for each failed field, in order.
func details(t *testing.T, err error) []string {
	t.Helper()

	var invalid *catalog.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("the error is %v", err)
	}

	out := make([]string, 0, len(invalid.Details))
	for _, d := range invalid.Details {
		if d.Message == "" {
			t.Fatalf("the detail %+v has no message", d)
		}

		out = append(out, d.Field+" "+d.Code)
	}

	return out
}

func TestAValidRequestIsCleaned(t *testing.T) {
	req := validRequest()
	req.Purpose = "  Example loan for a lab exercise.  "
	req.Items = []ItemInput{{ModelID: strings.ToUpper(modelA), Qty: intp(2)}, {ModelID: modelB, Qty: intp(1)}}

	in, err := req.check(testLimits, testNow)
	if err != nil {
		t.Fatalf("check failed %v", err)
	}

	if in.purpose != "Example loan for a lab exercise." || in.duration != 7 || in.pickup.Format(dateLayout) != "2026-11-20" {
		t.Fatalf("the input is %+v", in)
	}

	if len(in.items) != 2 || in.items[0] != (item{modelID: modelA, qty: 2}) || in.items[1] != (item{modelID: modelB, qty: 1}) {
		t.Fatalf("the items are %+v", in.items)
	}
}

func TestEachFieldFailsWithItsCode(t *testing.T) {
	long := strings.Repeat("a", 501)

	tests := []struct {
		name   string
		change func(*CreateRequest)
		want   []string
	}{
		{"no items", func(r *CreateRequest) { r.Items = nil }, []string{"items required"}},
		{"a missing model", func(r *CreateRequest) { r.Items[0].ModelID = "" }, []string{"items[0].modelId required"}},
		{"a model that is no identifier", func(r *CreateRequest) { r.Items[0].ModelID = "not-an-id" }, []string{"items[0].modelId invalid_format"}},
		{"an identifier in another form", func(r *CreateRequest) { r.Items[0].ModelID = "urn:uuid:" + modelA }, []string{"items[0].modelId invalid_format"}},
		{"a missing quantity", func(r *CreateRequest) { r.Items[0].Qty = nil }, []string{"items[0].qty required"}},
		{"a quantity of 0", func(r *CreateRequest) { r.Items[0].Qty = intp(0) }, []string{"items[0].qty out_of_range"}},
		{"a negative quantity", func(r *CreateRequest) { r.Items[0].Qty = intp(-3) }, []string{"items[0].qty out_of_range"}},
		{"quantities above the limit", func(r *CreateRequest) { r.Items[0].Qty = intp(11) }, []string{"items too_many_items"}},
		{"a huge quantity", func(r *CreateRequest) { r.Items[0].Qty = intp(int(^uint(0) >> 1)) }, []string{"items too_many_items"}},
		{"two huge quantities", func(r *CreateRequest) {
			r.Items = []ItemInput{{ModelID: modelA, Qty: intp(int(^uint(0) >> 1))}, {ModelID: modelB, Qty: intp(int(^uint(0) >> 1))}}
		}, []string{"items too_many_items"}},
		{"quantities that add up above the limit", func(r *CreateRequest) {
			r.Items = []ItemInput{{ModelID: modelA, Qty: intp(6)}, {ModelID: modelB, Qty: intp(5)}}
		}, []string{"items too_many_items"}},
		{"a model twice", func(r *CreateRequest) {
			r.Items = []ItemInput{{ModelID: modelA, Qty: intp(1)}, {ModelID: modelB, Qty: intp(1)}, {ModelID: strings.ToUpper(modelA), Qty: intp(1)}}
		}, []string{"items[2].modelId duplicate"}},
		{"no purpose", func(r *CreateRequest) { r.Purpose = "   " }, []string{"purpose required"}},
		{"a short purpose", func(r *CreateRequest) { r.Purpose = "abcd" }, []string{"purpose too_short"}},
		{"a long purpose", func(r *CreateRequest) { r.Purpose = long }, []string{"purpose too_long"}},
		{"a purpose with a NUL", func(r *CreateRequest) { r.Purpose = "Example\x00 purpose" }, []string{"purpose invalid_format"}},
		{"a purpose that is not UTF-8", func(r *CreateRequest) { r.Purpose = "Example \xff purpose" }, []string{"purpose invalid_format"}},
		{"no pickup date", func(r *CreateRequest) { r.PickupDate = "" }, []string{"pickupDate required"}},
		{"a pickup date in another shape", func(r *CreateRequest) { r.PickupDate = "20-11-2026" }, []string{"pickupDate invalid_format"}},
		{"a day that does not exist", func(r *CreateRequest) { r.PickupDate = "2026-02-30" }, []string{"pickupDate invalid_format"}},
		{"a pickup date with a time", func(r *CreateRequest) { r.PickupDate = "2026-11-20T10:00:00Z" }, []string{"pickupDate invalid_format"}},
		{"yesterday in Jakarta", func(r *CreateRequest) { r.PickupDate = "2026-11-17" }, []string{"pickupDate out_of_range"}},
		{"no duration", func(r *CreateRequest) { r.DurationDays = nil }, []string{"durationDays required"}},
		{"a duration of 0", func(r *CreateRequest) { r.DurationDays = intp(0) }, []string{"durationDays out_of_range"}},
		{"a duration above the limit", func(r *CreateRequest) { r.DurationDays = intp(31) }, []string{"durationDays out_of_range"}},
		{"three fields at once", func(r *CreateRequest) {
			r.Purpose, r.PickupDate, r.DurationDays = "", "", nil
		}, []string{"purpose required", "pickupDate required", "durationDays required"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := validRequest()
			tc.change(&req)

			_, err := req.check(testLimits, testNow)

			got := details(t, err)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("the details are %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTodayInJakartaIsAllowedAsPickupDate(t *testing.T) {
	req := validRequest()
	req.PickupDate = "2026-11-18"

	if _, err := req.check(testLimits, testNow); err != nil {
		t.Fatalf("today in Jakarta was refused %v", err)
	}

	// 16:00 UTC is 23:00 on 17 November in Jakarta, so 17 November is still today there
	req.PickupDate = "2026-11-17"

	if _, err := req.check(testLimits, time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("today in Jakarta was refused %v", err)
	}
}

func TestTheLimitsOfTheLabMove(t *testing.T) {
	req := validRequest()
	req.DurationDays = intp(5)

	if _, err := req.check(Limits{MaxItems: 10, MinDays: 1, MaxDays: 4, MaxOpen: 3}, testNow); err == nil {
		t.Fatal("5 days passed a limit of 4")
	}

	req = validRequest()
	req.Items[0].Qty = intp(3)

	if _, err := req.check(Limits{MaxItems: 2, MinDays: 1, MaxDays: 30, MaxOpen: 3}, testNow); err == nil {
		t.Fatal("3 items passed a limit of 2")
	}
}

func TestValidNumber(t *testing.T) {
	for number, want := range map[string]bool{
		"MGM-2611-0001-K7QX":  true,
		"MGM-2611-0001-k7qx":  false,
		"MGM-2611-0001-K7QI":  false,
		"MGM-2611-0001-K7Q0":  false,
		"MGM-2611-001-K7QX":   false,
		"MGM-2611-0001-K7QXX": false,
		"MGM-2611-0001":       false,
		"":                    false,
	} {
		if got := ValidNumber(number); got != want {
			t.Errorf("ValidNumber(%q) is %v", number, got)
		}
	}
}
