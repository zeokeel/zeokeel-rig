package schedule_test

// DSH-compatible timing selector tests: typed selectors, their canonical
// string form, validation errors, and update behavior. The legacy free-form
// schedule string stays accepted and verbatim.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/schedule"
)

// toolMapOf indexes tools by name for the DSH selector tests.
func toolMapOf(t *testing.T, tools []rig.Tool) map[string]rig.Tool {
	t.Helper()
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

// scheduleOf returns the stored schedule string of one task.
func scheduleOf(t *testing.T, scheduler rig.Scheduler, id string) string {
	t.Helper()
	tasks, err := scheduler.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, task := range tasks {
		if task.ID == id {
			return task.Schedule
		}
	}
	t.Fatalf("task %q not found", id)
	return ""
}

// futureRFC3339 returns a strictly future RFC 3339 instant derived from now.
func futureRFC3339(offset time.Duration) string {
	return time.Now().Add(offset).UTC().Format("2006-01-02T15:04:05Z07:00")
}

// canonicalInstant formats a time like the schedule selector canonical form.
func canonicalInstant(instant time.Time) string {
	return instant.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// TestScheduleDSHCreateSelectors covers each typed selector's canonical stored
// form and DSH-compatible canonicalization.
func TestScheduleDSHCreateSelectors(t *testing.T) {
	scheduler := schedule.NewScheduler()
	tools := schedule.Tools(scheduler)
	create := toolByName(t, tools, schedule.ToolScheduleCreate)

	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	absolute := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	absoluteString := absolute.Format("2006-01-02T15:04:05Z07:00")
	local := time.Now().In(shanghai).Add(3 * time.Hour).Truncate(time.Millisecond)

	cases := []struct {
		name      string
		args      string
		canonical string
	}{
		{
			name:      "after_seconds",
			args:      `{"title":"after","prompt":"run","after_seconds":90}`,
			canonical: "after 90s",
		},
		{
			name:      "at string",
			args:      fmt.Sprintf(`{"title":"at","prompt":"run","at":%q}`, absoluteString),
			canonical: "at " + canonicalInstant(absolute),
		},
		{
			name: "at object",
			args: fmt.Sprintf(`{"title":"at object","prompt":"run","at":{"date":%q,"time":%q,"time_zone":"Asia/Shanghai"}}`,
				local.Format("2006-01-02"), local.Format("15:04:05.000")),
			canonical: "at " + canonicalInstant(local),
		},
		{
			name:      "every_seconds",
			args:      `{"title":"every","prompt":"run","every_seconds":300}`,
			canonical: "every 300s",
		},
		{
			name:      "daily",
			args:      `{"title":"daily","prompt":"run","daily":{"time":"23:00:00","time_zone":"Asia/Shanghai"}}`,
			canonical: "daily 23:00:00.000 Asia/Shanghai",
		},
		{
			name:      "daily fractional seconds",
			args:      `{"title":"daily fraction","prompt":"run","daily":{"time":"23:00:00.5","time_zone":"UTC"}}`,
			canonical: "daily 23:00:00.500 UTC",
		},
		{
			name:      "weekly",
			args:      `{"title":"weekly","prompt":"run","weekly":{"time":"09:00:00","time_zone":"UTC","weekdays":[5,1,3]}}`,
			canonical: "weekly 09:00:00.000 UTC 1,3,5",
		},
		{
			name:      "cron",
			args:      `{"title":"cron","prompt":"run","cron":{"expression":"*/15 9-17 * * 1-5","time_zone":"Asia/Shanghai"}}`,
			canonical: "cron */15 9-17 * * 1-5 Asia/Shanghai",
		},
		{
			name:      "cron canonicalizes a stepped range",
			args:      `{"title":"cron range","prompt":"run","cron":{"expression":"0-59/15 * * * *","time_zone":"UTC"}}`,
			canonical: "cron 0-45/15 * * * * UTC",
		},
		{
			name:      "cron canonicalizes a stepped list",
			args:      `{"title":"cron list","prompt":"run","cron":{"expression":"0 0 * * 1,3,5","time_zone":"UTC"}}`,
			canonical: "cron 0 0 * * 1-5/2 UTC",
		},
		{
			name:      "cron folds Sunday 7 onto 0",
			args:      `{"title":"cron sunday","prompt":"run","cron":{"expression":"0 0 * * 7","time_zone":"UTC"}}`,
			canonical: "cron 0 0 * * 0 UTC",
		},
		{
			name:      "cron collapses a full star walk",
			args:      `{"title":"cron star","prompt":"run","cron":{"expression":"*/1 * * * *","time_zone":"UTC"}}`,
			canonical: "cron * * * * * UTC",
		},
		{
			name:      "cron collapses neighbouring values",
			args:      `{"title":"cron runs","prompt":"run","cron":{"expression":"1,2,3,10,11 * * * *","time_zone":"UTC"}}`,
			canonical: "cron 1-3,10-11 * * * * UTC",
		},
		{
			name:      "cron encodes a wide pair",
			args:      `{"title":"cron pair","prompt":"run","cron":{"expression":"0 0 1,15 * *","time_zone":"UTC"}}`,
			canonical: "cron 0 0 1-15/14 * * UTC",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := invoke(t, create, testCase.args)
			if result.IsError {
				t.Fatalf("create = %q", result.Content)
			}
			requireContains(t, result.Content, "schedule="+testCase.canonical)
		})
	}

	// The DSH title is stored as the Rig task name.
	if result := invoke(t, create, `{"title":"Titled","prompt":"run","after_seconds":60}`); result.IsError ||
		!strings.Contains(result.Content, "name=Titled") {
		t.Fatalf("title create = %q (IsError=%v)", result.Content, result.IsError)
	}

	// The list table shows the canonical schedule string.
	listed := invoke(t, toolByName(t, tools, schedule.ToolScheduleList), `{}`)
	requireContains(t, listed.Content, "after 90s", "every 300s",
		"daily 23:00:00.000 Asia/Shanghai", "weekly 09:00:00.000 UTC 1,3,5",
		"cron */15 9-17 * * 1-5 Asia/Shanghai")
}

// TestScheduleDSHLegacyScheduleVerbatim keeps the pre-DSH free-form schedule
// string stored exactly as supplied.
func TestScheduleDSHLegacyScheduleVerbatim(t *testing.T) {
	scheduler := schedule.NewScheduler()
	byName := toolMapOf(t, schedule.Tools(scheduler))

	created := invoke(t, byName[schedule.ToolScheduleCreate], `{"name":"legacy","prompt":"run","schedule":"*/15 * * * *"}`)
	requireContains(t, created.Content, "schedule=*/15 * * * *")
	if got := scheduleOf(t, scheduler, "task-1"); got != "*/15 * * * *" {
		t.Fatalf("stored schedule = %q", got)
	}

	updated := invoke(t, byName[schedule.ToolScheduleUpdate], `{"id":"task-1","schedule":"@daily"}`)
	requireContains(t, updated.Content, "schedule=@daily")
	if got := scheduleOf(t, scheduler, "task-1"); got != "@daily" {
		t.Fatalf("updated schedule = %q", got)
	}

	// Updating an existing task with a typed selector replaces the legacy form.
	updated = invoke(t, byName[schedule.ToolScheduleUpdate], `{"id":"task-1","daily":{"time":"08:15:00","time_zone":"UTC"}}`)
	requireContains(t, updated.Content, "schedule=daily 08:15:00.000 UTC")

	// An empty legacy schedule clears the stored value.
	updated = invoke(t, byName[schedule.ToolScheduleUpdate], `{"id":"task-1","schedule":""}`)
	requireContains(t, updated.Content, "schedule=-")
}

// TestScheduleDSHUpdateSelectors covers typed selector updates, the create-only
// after_seconds rule, and title updates.
func TestScheduleDSHUpdateSelectors(t *testing.T) {
	scheduler := schedule.NewScheduler()
	byName := toolMapOf(t, schedule.Tools(scheduler))
	create := byName[schedule.ToolScheduleCreate]
	update := byName[schedule.ToolScheduleUpdate]

	invoke(t, create, `{"name":"task","prompt":"run","schedule":"@daily"}`)

	cases := []struct {
		name      string
		args      string
		canonical string
	}{
		{
			name:      "every_seconds",
			args:      `{"id":"task-1","every_seconds":600}`,
			canonical: "every 600s",
		},
		{
			name:      "daily",
			args:      `{"id":"task-1","daily":{"time":"06:30:00","time_zone":"Asia/Shanghai"}}`,
			canonical: "daily 06:30:00.000 Asia/Shanghai",
		},
		{
			name:      "weekly",
			args:      `{"id":"task-1","weekly":{"time":"17:00:00","time_zone":"UTC","weekdays":[7,2]}}`,
			canonical: "weekly 17:00:00.000 UTC 2,7",
		},
		{
			name:      "cron",
			args:      `{"id":"task-1","cron":{"expression":"0 9 * * 1-5","time_zone":"UTC"}}`,
			canonical: "cron 0 9 * * 1-5 UTC",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := invoke(t, update, testCase.args)
			if result.IsError {
				t.Fatalf("update = %q", result.Content)
			}
			requireContains(t, result.Content, "schedule="+testCase.canonical)
			if got := scheduleOf(t, scheduler, "task-1"); got != testCase.canonical {
				t.Fatalf("stored schedule = %q, want %q", got, testCase.canonical)
			}
		})
	}

	// Title updates the stored name and applies the DSH length rule.
	if result := invoke(t, update, `{"id":"task-1","title":"Renamed"}`); result.IsError ||
		!strings.Contains(result.Content, "name=Renamed") {
		t.Fatalf("title update = %q (IsError=%v)", result.Content, result.IsError)
	}
	requireError(t, invoke(t, update, `{"id":"task-1","title":"   "}`), "title is required")

	// after_seconds is create-only.
	requireError(t, invoke(t, update, `{"id":"task-1","after_seconds":30}`), "create-only")
	// Two timing selectors are ambiguous.
	requireError(t, invoke(t, update, `{"id":"task-1","every_seconds":60,"daily":{"time":"01:00:00","time_zone":"UTC"}}`), "at most one timing selector")
	requireError(t, invoke(t, update, `{"id":"task-1","schedule":"@hourly","every_seconds":60}`), "at most one timing selector")
	// Invalid selector values are rejected without touching the task.
	requireError(t, invoke(t, update, `{"id":"task-1","at":"2000-01-01T00:00:00Z"}`), "strictly in the future")
	requireError(t, invoke(t, update, `{"id":"task-1","every_seconds":59}`), "at least 60")
	requireError(t, invoke(t, update, `{"name":"x"}`), "id is required")
	requireError(t, invoke(t, update, `{"id":"task-1"}`), "nothing to update")
	requireError(t, invoke(t, update, `{"id":"missing","every_seconds":60}`), "no scheduled task")
}

// TestScheduleDSHSelectorErrors covers the validation rules of the typed
// selectors.
func TestScheduleDSHSelectorErrors(t *testing.T) {
	tools := schedule.Tools(schedule.NewScheduler())
	create := toolByName(t, tools, schedule.ToolScheduleCreate)

	longTitle := strings.Repeat("x", 121)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name string
		args string
		want string
	}{
		{"after zero", `{"title":"t","prompt":"p","after_seconds":0}`, "after_seconds must be a positive integer"},
		{"after negative", `{"title":"t","prompt":"p","after_seconds":-5}`, "after_seconds must be a positive integer"},
		{"every too frequent", `{"title":"t","prompt":"p","every_seconds":59}`, "at least 60"},
		{"every zero", `{"title":"t","prompt":"p","every_seconds":0}`, "at least 60"},
		{"at not future", fmt.Sprintf(`{"title":"t","prompt":"p","at":%q}`, past), "strictly in the future"},
		{"at malformed", `{"title":"t","prompt":"p","at":"tomorrow"}`, "RFC 3339"},
		{"at wrong shape", `{"title":"t","prompt":"p","at":42}`, "at must be"},
		{"at object missing zone", `{"title":"t","prompt":"p","at":{"date":"2099-01-01","time":"00:00:00"}}`, "at.time_zone"},
		{"at object impossible date", `{"title":"t","prompt":"p","at":{"date":"2099-02-30","time":"00:00:00","time_zone":"UTC"}}`, "real ISO calendar date"},
		{"at object daylight-saving gap", `{"title":"t","prompt":"p","at":{"date":"2027-03-14","time":"02:30:00","time_zone":"America/New_York"}}`, "does not exist"},
		{"daily bad time", `{"title":"t","prompt":"p","daily":{"time":"24:00:00","time_zone":"UTC"}}`, "daily.time"},
		{"daily missing zone", `{"title":"t","prompt":"p","daily":{"time":"10:00:00"}}`, "daily.time_zone"},
		{"daily unknown zone", `{"title":"t","prompt":"p","daily":{"time":"10:00:00","time_zone":"Mars/Olympus"}}`, "daily.time_zone"},
		{"weekly empty", `{"title":"t","prompt":"p","weekly":{"time":"10:00:00","time_zone":"UTC","weekdays":[]}}`, "weekdays must not be empty"},
		{"weekly out of range", `{"title":"t","prompt":"p","weekly":{"time":"10:00:00","time_zone":"UTC","weekdays":[0]}}`, "ISO weekdays"},
		{"weekly duplicate", `{"title":"t","prompt":"p","weekly":{"time":"10:00:00","time_zone":"UTC","weekdays":[1,1]}}`, "must not repeat"},
		{"cron four fields", `{"title":"t","prompt":"p","cron":{"expression":"* * * *","time_zone":"UTC"}}`, "exactly five"},
		{"cron minute out of range", `{"title":"t","prompt":"p","cron":{"expression":"60 * * * *","time_zone":"UTC"}}`, "outside 0-59"},
		{"cron inverted range", `{"title":"t","prompt":"p","cron":{"expression":"10-5 * * * *","time_zone":"UTC"}}`, "inverted"},
		{"cron bare value with step", `{"title":"t","prompt":"p","cron":{"expression":"5/2 * * * *","time_zone":"UTC"}}`, "field element"},
		{"two selectors", `{"title":"t","prompt":"p","after_seconds":60,"every_seconds":60}`, "at most one timing selector"},
		{"legacy and typed", `{"title":"t","prompt":"p","schedule":"@daily","daily":{"time":"10:00:00","time_zone":"UTC"}}`, "at most one timing selector"},
		{"name and title differ", `{"name":"a","title":"b","prompt":"p"}`, "provide name or title, not both"},
		{"title too long", fmt.Sprintf(`{"title":%q,"prompt":"p"}`, longTitle), "at most 120 characters"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			requireError(t, invoke(t, create, testCase.args), testCase.want)
		})
	}

	// A blank title with no legacy name is rejected as a missing name.
	requireError(t, invoke(t, create, `{"title":"   ","prompt":"p"}`), "name or title is required")
}

// TestScheduleDSHMetadata checks the advertised schema of the extended tools.
func TestScheduleDSHMetadata(t *testing.T) {
	tools := schedule.Tools(schedule.NewScheduler())
	create := toolByName(t, tools, schedule.ToolScheduleCreate)
	update := toolByName(t, tools, schedule.ToolScheduleUpdate)

	for _, name := range []string{schedule.ToolScheduleCreate, schedule.ToolScheduleUpdate} {
		tool := toolByName(t, tools, name)
		if !strings.Contains(tool.Description(), "timing selector") {
			t.Fatalf("%s description does not mention timing selectors: %q", name, tool.Description())
		}
	}

	createProperties := create.Schema()["properties"].(map[string]any)
	afterSchema, ok := createProperties["after_seconds"].(map[string]any)
	if !ok || afterSchema["type"] != "integer" {
		t.Fatalf("create after_seconds schema = %v", createProperties["after_seconds"])
	}
	for _, name := range []string{"at", "every_seconds", "daily", "weekly", "cron"} {
		if _, ok := createProperties[name]; !ok {
			t.Fatalf("create schema is missing %q", name)
		}
	}
	atSchema, ok := createProperties["at"].(map[string]any)
	if !ok || atSchema["oneOf"] == nil {
		t.Fatalf("create at schema = %v", createProperties["at"])
	}
	dailySchema, ok := createProperties["daily"].(map[string]any)
	if !ok || dailySchema["properties"] == nil {
		t.Fatalf("create daily schema = %v", createProperties["daily"])
	}
	weeklyProperties, ok := createProperties["weekly"].(map[string]any)["properties"].(map[string]any)
	if !ok {
		t.Fatalf("create weekly schema = %v", createProperties["weekly"])
	}
	if _, ok := weeklyProperties["weekdays"]; !ok {
		t.Fatalf("weekly schema is missing weekdays: %v", weeklyProperties)
	}

	updateProperties := update.Schema()["properties"].(map[string]any)
	for _, name := range []string{"id", "name", "title", "prompt", "enabled", "schedule", "at", "every_seconds", "daily", "weekly", "cron"} {
		if _, ok := updateProperties[name]; !ok {
			t.Fatalf("update schema is missing %q", name)
		}
	}
	if _, ok := updateProperties["after_seconds"]; ok {
		t.Fatalf("update schema must not advertise after_seconds: %v", updateProperties["after_seconds"])
	}
}
