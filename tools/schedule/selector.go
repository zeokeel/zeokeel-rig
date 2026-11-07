// DSH-compatible typed timing selectors for the schedule_* tools.
//
// Rig's frozen rig.Scheduler contract stores one free-form Schedule string, so
// a typed selector cannot be preserved structurally: the tools validate it and
// canonicalize it into a stable, documented string form that the host scheduler
// can parse:
//
//	after <seconds>s                                  (one-shot relative delay)
//	at <RFC 3339 UTC instant, millisecond precision>   (one-shot absolute target)
//	every <seconds>s                                  (fixed-rate recurring)
//	daily <HH:MM:SS.SSS> <time zone>                  (daily wall clock)
//	weekly <HH:MM:SS.SSS> <time zone> <weekday CSV>   (weekly wall clock)
//	cron <canonical five-field expression> <time zone>
//
// A legacy free-form `schedule` string is stored verbatim and never rewritten.
//
// The validation rules mirror the DeepSeek Harness schedule selectors: positive
// whole-second delay, a strictly future absolute instant, at least one minute
// between fixed-rate runs, six-digit-free HH:MM:SS times with optional 1-3
// fractional digits, explicit UTC/IANA zones, unique ascending ISO weekdays, and
// a five-field Vixie cron expression canonicalized to its shortest equivalent.
package schedule

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the selectors accept IANA zones; do not depend on the host OS zone database
	"unicode/utf8"
)

const (
	// MinEveryIntervalSeconds is the smallest fixed-rate interval the DSH
	// every_seconds selector accepts.
	MinEveryIntervalSeconds = 60
	// MaxTitleLength is the largest stored task title the DSH title argument
	// accepts, in characters (Unicode code points).
	MaxTitleLength = 120
)

// dailyInput is the decoded daily selector.
type dailyInput struct {
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
}

// weeklyInput is the decoded weekly selector.
type weeklyInput struct {
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
	Weekdays []int  `json:"weekdays"`
}

// cronInput is the decoded cron selector.
type cronInput struct {
	Expression string `json:"expression"`
	TimeZone   string `json:"time_zone"`
}

// atInput is the decoded structured form of the at selector.
type atInput struct {
	Date     string `json:"date"`
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
}

// createTimingArgs is the timing argument set of schedule_create. The legacy
// schedule string and the DSH selectors are mutually exclusive.
type createTimingArgs struct {
	Schedule     string          `json:"schedule"`
	AfterSeconds *int            `json:"after_seconds"`
	At           json.RawMessage `json:"at"`
	EverySeconds *int            `json:"every_seconds"`
	Daily        *dailyInput     `json:"daily"`
	Weekly       *weeklyInput    `json:"weekly"`
	Cron         *cronInput      `json:"cron"`
}

// resolveCreateTiming validates the selector arguments of schedule_create and
// returns the schedule string to store. No selector is accepted for backward
// compatibility with the legacy optional schedule string, in which case the
// (possibly empty) legacy string is returned verbatim.
func resolveCreateTiming(args createTimingArgs) (string, error) {
	selectors := make([]string, 0, 7)
	if args.AfterSeconds != nil {
		selectors = append(selectors, "after_seconds")
	}
	if len(args.At) > 0 {
		selectors = append(selectors, "at")
	}
	if args.EverySeconds != nil {
		selectors = append(selectors, "every_seconds")
	}
	if args.Daily != nil {
		selectors = append(selectors, "daily")
	}
	if args.Weekly != nil {
		selectors = append(selectors, "weekly")
	}
	if args.Cron != nil {
		selectors = append(selectors, "cron")
	}
	legacy := strings.TrimSpace(args.Schedule)
	if legacy != "" {
		selectors = append(selectors, "schedule")
	}
	if len(selectors) > 1 {
		return "", fmt.Errorf("accepts at most one timing selector (after_seconds, at, every_seconds, daily, weekly, cron, or the legacy schedule string), got %s", strings.Join(selectors, ", "))
	}
	switch {
	case args.AfterSeconds != nil:
		return canonicalAfterSeconds(*args.AfterSeconds)
	case len(args.At) > 0:
		return canonicalAt(args.At)
	case args.EverySeconds != nil:
		return canonicalEverySeconds(*args.EverySeconds)
	case args.Daily != nil:
		return canonicalDaily(args.Daily)
	case args.Weekly != nil:
		return canonicalWeekly(args.Weekly)
	case args.Cron != nil:
		return canonicalCron(args.Cron)
	default:
		return legacy, nil
	}
}

// resolveUpdateTiming validates the selector arguments of schedule_update and
// returns the new schedule string. The boolean reports whether a selector was
// provided at all; a provided legacy schedule string (including the empty
// string, which clears it) is returned verbatim.
func resolveUpdateTiming(args updateArgs) (string, bool, error) {
	if args.AfterSeconds != nil {
		return "", false, errors.New("after_seconds is create-only; create a new scheduled task to change a relative delay")
	}
	selectors := make([]string, 0, 6)
	if args.Schedule != nil {
		selectors = append(selectors, "schedule")
	}
	if len(args.At) > 0 {
		selectors = append(selectors, "at")
	}
	if args.EverySeconds != nil {
		selectors = append(selectors, "every_seconds")
	}
	if args.Daily != nil {
		selectors = append(selectors, "daily")
	}
	if args.Weekly != nil {
		selectors = append(selectors, "weekly")
	}
	if args.Cron != nil {
		selectors = append(selectors, "cron")
	}
	if len(selectors) > 1 {
		return "", false, fmt.Errorf("accepts at most one timing selector (at, every_seconds, daily, weekly, cron, or the legacy schedule string), got %s", strings.Join(selectors, ", "))
	}
	switch {
	case len(selectors) == 0:
		return "", false, nil
	case args.Schedule != nil:
		return strings.TrimSpace(*args.Schedule), true, nil
	case len(args.At) > 0:
		canonical, err := canonicalAt(args.At)
		return canonical, err == nil, err
	case args.EverySeconds != nil:
		canonical, err := canonicalEverySeconds(*args.EverySeconds)
		return canonical, err == nil, err
	case args.Daily != nil:
		canonical, err := canonicalDaily(args.Daily)
		return canonical, err == nil, err
	case args.Weekly != nil:
		canonical, err := canonicalWeekly(args.Weekly)
		return canonical, err == nil, err
	case args.Cron != nil:
		canonical, err := canonicalCron(args.Cron)
		return canonical, err == nil, err
	default:
		return "", false, nil
	}
}

// canonicalAfterSeconds validates and canonicalizes a relative one-shot delay.
func canonicalAfterSeconds(seconds int) (string, error) {
	if seconds <= 0 {
		return "", errors.New("after_seconds must be a positive integer")
	}
	return fmt.Sprintf("after %ds", seconds), nil
}

// canonicalEverySeconds validates and canonicalizes a fixed-rate interval.
func canonicalEverySeconds(seconds int) (string, error) {
	if seconds < MinEveryIntervalSeconds {
		return "", fmt.Errorf("every_seconds must be at least %d", MinEveryIntervalSeconds)
	}
	return fmt.Sprintf("every %ds", seconds), nil
}

// canonicalAt validates and canonicalizes an absolute target. It accepts an RFC
// 3339 date-time string with an explicit offset or an object carrying a local
// date, time, and IANA zone; both forms canonicalize to the same UTC instant.
func canonicalAt(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", errors.New(`at must be an RFC 3339 date-time string or an object with date, time, and time_zone`)
	}
	var instant time.Time
	switch trimmed[0] {
	case '"':
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "", fmt.Errorf("at: %w", err)
		}
		parsed, err := parseAbsoluteInstant(value)
		if err != nil {
			return "", err
		}
		instant = parsed
	case '{':
		var value atInput
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "", fmt.Errorf("at: %w", err)
		}
		parsed, err := parseLocalAbsolute(value)
		if err != nil {
			return "", err
		}
		instant = parsed
	default:
		return "", errors.New(`at must be an RFC 3339 date-time string or an object with date, time, and time_zone`)
	}
	if !instant.After(time.Now()) {
		return "", errors.New("at must be strictly in the future")
	}
	return "at " + formatUTCInstant(instant), nil
}

// canonicalDaily validates and canonicalizes a daily wall-clock selector.
func canonicalDaily(input *dailyInput) (string, error) {
	hour, minute, second, millis, err := parseClockTime(input.Time)
	if err != nil {
		return "", fmt.Errorf("daily.time: %w", err)
	}
	zone, _, err := canonicalTimeZone(input.TimeZone, "daily.time_zone")
	if err != nil {
		return "", err
	}
	return "daily " + formatClockTime(hour, minute, second, millis) + " " + zone, nil
}

// canonicalWeekly validates and canonicalizes a weekly wall-clock selector.
func canonicalWeekly(input *weeklyInput) (string, error) {
	hour, minute, second, millis, err := parseClockTime(input.Time)
	if err != nil {
		return "", fmt.Errorf("weekly.time: %w", err)
	}
	zone, _, err := canonicalTimeZone(input.TimeZone, "weekly.time_zone")
	if err != nil {
		return "", err
	}
	weekdays, err := normalizeWeekdays(input.Weekdays)
	if err != nil {
		return "", err
	}
	parts := make([]string, len(weekdays))
	for index, weekday := range weekdays {
		parts[index] = strconv.Itoa(weekday)
	}
	return "weekly " + formatClockTime(hour, minute, second, millis) + " " + zone + " " + strings.Join(parts, ","), nil
}

// canonicalCron validates and canonicalizes a five-field cron selector.
func canonicalCron(input *cronInput) (string, error) {
	expression, err := canonicalizeCronExpression(input.Expression)
	if err != nil {
		return "", err
	}
	zone, _, err := canonicalTimeZone(input.TimeZone, "cron.time_zone")
	if err != nil {
		return "", err
	}
	return "cron " + expression + " " + zone, nil
}

// validateTitle checks one DSH task title: non-empty after trimming and at most
// MaxTitleLength characters.
func validateTitle(title string) (string, error) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", errors.New("title is required and must be non-empty after trimming")
	}
	if utf8.RuneCountInString(trimmed) > MaxTitleLength {
		return "", fmt.Errorf("title must be at most %d characters", MaxTitleLength)
	}
	return trimmed, nil
}

var (
	// instantPattern is the strict RFC 3339 subset DSH accepts for an absolute
	// target: a four-digit year, a real time, and an explicit offset.
	instantPattern = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?(Z|[+-]\d{2}:\d{2})$`)
	// localDatePattern is a strict ISO calendar date.
	localDatePattern = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	// localTimePattern is a strict HH:MM:SS time with optional 1-3 fractional
	// digits.
	localTimePattern = regexp.MustCompile(`^(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?$`)
	// ianaZonePattern is the Area/Location shape DSH accepts (besides UTC).
	ianaZonePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)
)

// parseAbsoluteInstant parses the RFC 3339 string form of the at selector.
func parseAbsoluteInstant(value string) (time.Time, error) {
	match := instantPattern.FindStringSubmatch(value)
	if match == nil {
		return time.Time{}, errors.New(`at must be an RFC 3339 date-time with an explicit offset, for example "2026-12-01T09:30:00Z" or "2026-12-01T09:30:00+08:00"`)
	}
	year, month, day := decimal(match[1]), decimal(match[2]), decimal(match[3])
	hour, minute, second := decimal(match[4]), decimal(match[5]), decimal(match[6])
	millis := fractionMillis(match[7])
	if year == 0 || month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, errors.New("at must be a real ISO calendar date and time")
	}
	local := time.Date(year, time.Month(month), day, hour, minute, second, millis*int(time.Millisecond), time.UTC)
	if local.Year() != year || local.Month() != time.Month(month) || local.Day() != day {
		return time.Time{}, errors.New("at must be a real ISO calendar date and time")
	}
	offsetSeconds := 0
	zone := match[8]
	if zone != "Z" {
		offsetHour, offsetMinute := decimal(zone[1:3]), decimal(zone[4:6])
		if offsetHour > 23 || offsetMinute > 59 || (zone[0] == '-' && offsetHour == 0 && offsetMinute == 0) {
			return time.Time{}, errors.New("at has an invalid numeric UTC offset")
		}
		offsetSeconds = offsetHour*3600 + offsetMinute*60
		if zone[0] == '-' {
			offsetSeconds = -offsetSeconds
		}
	}
	return local.Add(-time.Duration(offsetSeconds) * time.Second), nil
}

// parseLocalAbsolute resolves the structured form of the at selector to its UTC
// instant. A wall-clock time that does not exist in the zone (a daylight-saving
// gap) is rejected, mirroring DSH.
func parseLocalAbsolute(input atInput) (time.Time, error) {
	dateMatch := localDatePattern.FindStringSubmatch(input.Date)
	if dateMatch == nil {
		return time.Time{}, errors.New("at.date must be YYYY-MM-DD")
	}
	year, month, day := decimal(dateMatch[1]), decimal(dateMatch[2]), decimal(dateMatch[3])
	if year == 0 || month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, errors.New("at.date must be a real ISO calendar date")
	}
	date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if date.Year() != year || date.Month() != time.Month(month) || date.Day() != day {
		return time.Time{}, errors.New("at.date must be a real ISO calendar date")
	}
	hour, minute, second, millis, err := parseClockTime(input.Time)
	if err != nil {
		return time.Time{}, fmt.Errorf("at.time: %w", err)
	}
	_, location, err := canonicalTimeZone(input.TimeZone, "at.time_zone")
	if err != nil {
		return time.Time{}, err
	}
	local := time.Date(year, time.Month(month), day, hour, minute, second, millis*int(time.Millisecond), location)
	if local.Year() != year || local.Month() != time.Month(month) || local.Day() != day ||
		local.Hour() != hour || local.Minute() != minute || local.Second() != second ||
		local.Nanosecond() != millis*int(time.Millisecond) {
		return time.Time{}, errors.New("at.time does not exist in at.time_zone")
	}
	return local.UTC(), nil
}

// parseClockTime parses the strict HH:MM:SS[.fff] local clock time shared by the
// at, daily and weekly selectors.
func parseClockTime(value string) (hour, minute, second, millis int, err error) {
	match := localTimePattern.FindStringSubmatch(value)
	if match == nil {
		return 0, 0, 0, 0, errors.New("time must be HH:MM:SS with optional one to three fractional digits")
	}
	hour, minute, second = decimal(match[1]), decimal(match[2]), decimal(match[3])
	if hour > 23 || minute > 59 || second > 59 {
		return 0, 0, 0, 0, errors.New("time must be a real 24-hour clock time")
	}
	return hour, minute, second, fractionMillis(match[4]), nil
}

// canonicalTimeZone validates one UTC/IANA zone and returns its canonical
// spelling together with the loaded location.
func canonicalTimeZone(value, field string) (string, *time.Location, error) {
	if value == "" || strings.TrimSpace(value) != value || (value != "UTC" && !ianaZonePattern.MatchString(value)) {
		return "", nil, fmt.Errorf("%s must be UTC or a valid IANA Area/Location name", field)
	}
	location, err := time.LoadLocation(value)
	if err != nil {
		return "", nil, fmt.Errorf("%s %q is not a known IANA time zone", field, value)
	}
	if location == time.UTC || location.String() == "UTC" {
		return "UTC", time.UTC, nil
	}
	return location.String(), location, nil
}

// normalizeWeekdays validates and sorts one weekly weekday set.
func normalizeWeekdays(values []int) ([]int, error) {
	if len(values) == 0 {
		return nil, errors.New("weekly.weekdays must not be empty")
	}
	seen := make(map[int]bool, len(values))
	normalized := make([]int, 0, len(values))
	for _, weekday := range values {
		if weekday < 1 || weekday > 7 {
			return nil, fmt.Errorf("weekly.weekdays must be ISO weekdays from 1 (Monday) through 7 (Sunday), got %d", weekday)
		}
		if seen[weekday] {
			return nil, fmt.Errorf("weekly.weekdays must not repeat weekday %d", weekday)
		}
		seen[weekday] = true
		normalized = append(normalized, weekday)
	}
	slices.Sort(normalized)
	return normalized, nil
}

// formatClockTime renders a canonical local clock time with millisecond
// precision, matching DSH's normalized HH:mm:ss.SSS form.
func formatClockTime(hour, minute, second, millis int) string {
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hour, minute, second, millis)
}

// formatUTCInstant renders a canonical four-digit-year UTC instant with
// millisecond precision, matching DSH's scheduledAt form.
func formatUTCInstant(instant time.Time) string {
	return instant.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// decimal converts a regexp-proven digit run to an int.
func decimal(text string) int {
	value, _ := strconv.Atoi(text)
	return value
}

// fractionMillis converts the optional 1-3 fractional digits of a clock time to
// milliseconds.
func fractionMillis(text string) int {
	if text == "" {
		return 0
	}
	for len(text) < 3 {
		text += "0"
	}
	value, _ := strconv.Atoi(text)
	return value
}

// --- Vixie cron -----------------------------------------------------------

// cronFieldSpec describes the accepted range of one five-field cron column.
type cronFieldSpec struct {
	name         string
	min          int
	max          int
	canonicalMax int
}

var cronFieldSpecs = [5]cronFieldSpec{
	{name: "minute", min: 0, max: 59, canonicalMax: 59},
	{name: "hour", min: 0, max: 23, canonicalMax: 23},
	{name: "day-of-month", min: 1, max: 31, canonicalMax: 31},
	{name: "month", min: 1, max: 12, canonicalMax: 12},
	{name: "day-of-week", min: 0, max: 7, canonicalMax: 6},
}

// cronField is one parsed and canonicalized cron column.
type cronField struct {
	values    []int
	star      bool
	canonical string
}

var (
	// cronCommaElementPattern matches one non-star list element.
	cronCommaElementPattern = regexp.MustCompile(`^(\d+)(?:-(\d+))?(?:/(\d+))?$`)
	// cronStarStepPattern matches the */n form.
	cronStarStepPattern = regexp.MustCompile(`^\*/(\d+)$`)
)

// canonicalizeCronExpression validates one five-field Vixie cron expression and
// returns its shortest equivalent spelling, mirroring DSH.
func canonicalizeCronExpression(expression string) (string, error) {
	if expression == "" || strings.TrimSpace(expression) != expression {
		return "", errors.New("cron.expression must be a non-empty trimmed string")
	}
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return "", errors.New("cron.expression must contain exactly five whitespace-separated fields: minute hour day-of-month month day-of-week")
	}
	canonical := make([]string, len(fields))
	for index, raw := range fields {
		field, err := parseCronField(raw, cronFieldSpecs[index])
		if err != nil {
			return "", err
		}
		canonical[index] = field.canonical
	}
	return strings.Join(canonical, " "), nil
}

// parseCronField expands one cron column and encodes it canonically.
func parseCronField(raw string, spec cronFieldSpec) (cronField, error) {
	values, err := cronFieldValues(raw, spec)
	if err != nil {
		return cronField{}, err
	}
	star := strings.HasPrefix(raw, "*")
	canonical := encodeCronField(values)
	if star {
		canonical = encodeStarCronField(values, spec)
	}
	return cronField{values: values, star: star, canonical: canonical}, nil
}

// cronFieldValues expands one cron column into its matched, folded, ascending
// value set.
func cronFieldValues(raw string, spec cronFieldSpec) ([]int, error) {
	matched := make(map[int]bool)
	for _, element := range strings.Split(raw, ",") {
		if element == "" {
			return nil, fmt.Errorf("cron.expression %s field must not contain an empty list element", spec.name)
		}
		if element == "*" {
			for value := spec.min; value <= spec.max; value++ {
				matched[value] = true
			}
			continue
		}
		if strings.HasPrefix(element, "*") {
			match := cronStarStepPattern.FindStringSubmatch(element)
			if match == nil {
				return nil, invalidCronFieldError(spec, element)
			}
			step, err := cronFieldStep(match[1], spec)
			if err != nil {
				return nil, err
			}
			for value := spec.min; value <= spec.max; value += step {
				matched[value] = true
			}
			continue
		}
		match := cronCommaElementPattern.FindStringSubmatch(element)
		if match == nil {
			return nil, invalidCronFieldError(spec, element)
		}
		start, err := cronFieldValue(match[1], spec)
		if err != nil {
			return nil, err
		}
		if match[2] == "" {
			if match[3] != "" {
				return nil, invalidCronFieldError(spec, element)
			}
			matched[start] = true
			continue
		}
		last, err := cronFieldValue(match[2], spec)
		if err != nil {
			return nil, err
		}
		if start > last {
			return nil, fmt.Errorf("cron.expression %s field range %d-%d is inverted", spec.name, start, last)
		}
		step := 1
		if match[3] != "" {
			step, err = cronFieldStep(match[3], spec)
			if err != nil {
				return nil, err
			}
		}
		for value := start; value <= last; value += step {
			matched[value] = true
		}
	}
	folded := make(map[int]bool, len(matched))
	for value := range matched {
		folded[foldCronValue(value, spec)] = true
	}
	values := make([]int, 0, len(folded))
	for value := range folded {
		values = append(values, value)
	}
	slices.Sort(values)
	return values, nil
}

// foldCronValue maps the alternate day-of-week maximum (7, Sunday) onto 0.
func foldCronValue(value int, spec cronFieldSpec) int {
	if spec.canonicalMax != spec.max && value == spec.max {
		return spec.min
	}
	return value
}

// cronFieldValue parses one in-range cron value.
func cronFieldValue(text string, spec cronFieldSpec) (int, error) {
	value, err := strconv.Atoi(text)
	if err != nil || value < spec.min || value > spec.max {
		return 0, fmt.Errorf("cron.expression %s field value %s is outside %d-%d", spec.name, text, spec.min, spec.max)
	}
	return value, nil
}

// cronFieldStep parses one positive cron step.
func cronFieldStep(text string, spec cronFieldSpec) (int, error) {
	step, err := strconv.Atoi(text)
	if err != nil || step < 1 {
		return 0, fmt.Errorf("cron.expression %s field step must be a positive integer", spec.name)
	}
	return step, nil
}

// invalidCronFieldError builds the stable malformed-element diagnostic.
func invalidCronFieldError(spec cronFieldSpec, element string) error {
	return fmt.Errorf("cron.expression %s field element %q must be *, a value, a-b, */n, a-b/n, or a comma-separated list of those", spec.name, element)
}

// encodeCronField encodes a matched value set as the shortest equivalent
// comma-separated field, never using a leading star.
func encodeCronField(values []int) string {
	if len(values) == 1 {
		return strconv.Itoa(values[0])
	}
	first, last := values[0], values[len(values)-1]
	step := values[1] - first
	uniform := true
	for index := 2; index < len(values); index++ {
		if values[index]-values[index-1] != step {
			uniform = false
			break
		}
	}
	if uniform {
		if step == 1 {
			return fmt.Sprintf("%d-%d", first, last)
		}
		return fmt.Sprintf("%d-%d/%d", first, last, step)
	}
	var builder strings.Builder
	runStart := first
	for index := 1; index < len(values); index++ {
		current, previous := values[index], values[index-1]
		if current == previous+1 {
			continue
		}
		if runStart == previous {
			builder.WriteString(strconv.Itoa(runStart))
		} else {
			fmt.Fprintf(&builder, "%d-%d", runStart, previous)
		}
		builder.WriteByte(',')
		runStart = current
	}
	if runStart == last {
		builder.WriteString(strconv.Itoa(runStart))
	} else {
		fmt.Fprintf(&builder, "%d-%d", runStart, last)
	}
	return builder.String()
}

// encodeStarCronField encodes a column whose source text started with '*' while
// preserving that star flag (which drives Vixie's day-of-month/day-of-week
// AND-vs-OR rule).
func encodeStarCronField(values []int, spec cronFieldSpec) string {
	if len(values) == spec.canonicalMax-spec.min+1 {
		return "*"
	}
	present := make(map[int]bool, len(values))
	for _, value := range values {
		present[value] = true
	}
	bestStep := 0
	var bestWalk []int
	for step := 1; step <= spec.max-spec.min+1; step++ {
		walk := starWalk(spec, step)
		if len(walk) <= len(bestWalk) {
			continue
		}
		contained := true
		for _, value := range walk {
			if !present[value] {
				contained = false
				break
			}
		}
		if contained {
			bestStep = step
			bestWalk = walk
		}
	}
	if bestStep == 0 {
		// Unreachable: the step-1 walk starts at the minimum, which every
		// star-led column matches.
		return encodeCronField(values)
	}
	walked := make(map[int]bool, len(bestWalk))
	for _, value := range bestWalk {
		walked[value] = true
	}
	remaining := make([]int, 0, len(values))
	for _, value := range values {
		if !walked[value] {
			remaining = append(remaining, value)
		}
	}
	walkText := fmt.Sprintf("*/%d", bestStep)
	if len(remaining) == 0 {
		return walkText
	}
	return walkText + "," + encodeCronField(remaining)
}

// starWalk returns the ascending folded value set a */step walk matches from the
// column minimum.
func starWalk(spec cronFieldSpec, step int) []int {
	walked := make(map[int]bool)
	for value := spec.min; value <= spec.max; value += step {
		walked[foldCronValue(value, spec)] = true
	}
	values := make([]int, 0, len(walked))
	for value := range walked {
		values = append(values, value)
	}
	slices.Sort(values)
	return values
}
