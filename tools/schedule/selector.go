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
	_ "time/tzdata" // 选择器接受 IANA 时区；不要依赖宿主操作系统的时区数据库
	"unicode/utf8"
)

const (
	// MinEveryIntervalSeconds 是 DSH every_seconds 选择器接受的最小固定间隔。
	MinEveryIntervalSeconds = 60
	// MaxTitleLength 是 DSH title 参数接受的最大任务标题长度（Unicode 码点数）。
	MaxTitleLength = 120
)

// dailyInput 是解码后的 daily 选择器。
type dailyInput struct {
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
}

// weeklyInput 是解码后的 weekly 选择器。
type weeklyInput struct {
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
	Weekdays []int  `json:"weekdays"`
}

// cronInput 是解码后的 cron 选择器。
type cronInput struct {
	Expression string `json:"expression"`
	TimeZone   string `json:"time_zone"`
}

// atInput 是 at 选择器解码后的结构化形式。
type atInput struct {
	Date     string `json:"date"`
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
}

// createTimingArgs 是 schedule_create 的时间参数集合。旧版 schedule 字符串与 DSH 选择器
// 互斥。
type createTimingArgs struct {
	Schedule     string          `json:"schedule"`
	AfterSeconds *int            `json:"after_seconds"`
	At           json.RawMessage `json:"at"`
	EverySeconds *int            `json:"every_seconds"`
	Daily        *dailyInput     `json:"daily"`
	Weekly       *weeklyInput    `json:"weekly"`
	Cron         *cronInput      `json:"cron"`
}

// resolveCreateTiming 校验 schedule_create 的选择器参数并返回要保存的 schedule 字符串。
// 为兼容旧版可选 schedule 字符串，不提供任何选择器也是允许的；此时原样返回（可能为空的）
// 旧版字符串。
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

// resolveUpdateTiming 校验 schedule_update 的选择器参数并返回新的 schedule 字符串。布尔
// 返回值表示是否提供了选择器；提供了旧版 schedule 字符串（包括表示清除的空字符串）时原样
// 返回。
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

// canonicalAfterSeconds 校验并规范化一次性相对延迟。
func canonicalAfterSeconds(seconds int) (string, error) {
	if seconds <= 0 {
		return "", errors.New("after_seconds must be a positive integer")
	}
	return fmt.Sprintf("after %ds", seconds), nil
}

// canonicalEverySeconds 校验并规范化固定间隔。
func canonicalEverySeconds(seconds int) (string, error) {
	if seconds < MinEveryIntervalSeconds {
		return "", fmt.Errorf("every_seconds must be at least %d", MinEveryIntervalSeconds)
	}
	return fmt.Sprintf("every %ds", seconds), nil
}

// canonicalAt 校验并规范化绝对时间目标。它接受带显式偏移的 RFC 3339 日期时间字符串，或
// 携带本地日期、时间与 IANA 时区的对象；两种形式都规范化为同一个 UTC 时刻。
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

// canonicalDaily 校验并规范化每日墙钟时间选择器。
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

// canonicalWeekly 校验并规范化每周墙钟时间选择器。
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

// canonicalCron 校验并规范化五字段 cron 选择器。
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

// validateTitle 校验一个 DSH 任务标题：trim 后非空，且不超过 MaxTitleLength 个字符。
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
	// instantPattern 是 DSH 对绝对时间目标接受的严格 RFC 3339 子集：四位年份、真实时间与
	// 显式偏移。
	instantPattern = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?(Z|[+-]\d{2}:\d{2})$`)
	// localDatePattern 是严格的 ISO 日历日期。
	localDatePattern = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	// localTimePattern 是严格的 HH:MM:SS 时间，可带 1-3 位小数。
	localTimePattern = regexp.MustCompile(`^(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?$`)
	// ianaZonePattern 是 DSH 接受的 Area/Location 形状（UTC 除外）。
	ianaZonePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)
)

// parseAbsoluteInstant 解析 at 选择器的 RFC 3339 字符串形式。
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

// parseLocalAbsolute 把 at 选择器的结构化形式解析为 UTC 时刻。在时区中不存在的墙钟时间
// （夏令时缺口）会被拒绝，与 DSH 行为一致。
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

// parseClockTime 解析 at、daily 与 weekly 选择器共享的严格 HH:MM:SS[.fff] 本地时钟时间。
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

// canonicalTimeZone 校验一个 UTC/IANA 时区，并返回其规范拼写与已加载的位置。
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

// normalizeWeekdays 校验并排序一组 weekly 星期值。
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

// formatClockTime 渲染规范本地时钟时间（毫秒精度），匹配 DSH 归一化的
// HH:mm:ss.SSS 形式。
func formatClockTime(hour, minute, second, millis int) string {
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hour, minute, second, millis)
}

// formatUTCInstant 渲染规范的四位年份 UTC 时刻（毫秒精度），匹配 DSH 的 scheduledAt
// 形式。
func formatUTCInstant(instant time.Time) string {
	return instant.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// decimal 把正则证明过的数字串转换为 int。
func decimal(text string) int {
	value, _ := strconv.Atoi(text)
	return value
}

// fractionMillis 把时钟时间可选的 1-3 位小数转换为毫秒。
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

// cronFieldSpec 描述五字段 cron 某一列接受的范围。
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

// cronField 是一个已解析并规范化的 cron 列。
type cronField struct {
	values    []int
	star      bool
	canonical string
}

var (
	// cronCommaElementPattern 匹配一个非 * 的列表元素。
	cronCommaElementPattern = regexp.MustCompile(`^(\d+)(?:-(\d+))?(?:/(\d+))?$`)
	// cronStarStepPattern 匹配 */n 形式。
	cronStarStepPattern = regexp.MustCompile(`^\*/(\d+)$`)
)

// canonicalizeCronExpression 校验一个五字段 Vixie cron 表达式，并返回其最短等价拼写，
// 与 DSH 行为一致。
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

// parseCronField 展开一个 cron 列并做规范化编码。
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

// cronFieldValues 把一个 cron 列展开为匹配、折叠、升序的取值集合。
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

// foldCronValue 把星期字段的另一个最大值（7，周日）映射到 0。
func foldCronValue(value int, spec cronFieldSpec) int {
	if spec.canonicalMax != spec.max && value == spec.max {
		return spec.min
	}
	return value
}

// cronFieldValue 解析一个范围内 cron 值。
func cronFieldValue(text string, spec cronFieldSpec) (int, error) {
	value, err := strconv.Atoi(text)
	if err != nil || value < spec.min || value > spec.max {
		return 0, fmt.Errorf("cron.expression %s field value %s is outside %d-%d", spec.name, text, spec.min, spec.max)
	}
	return value, nil
}

// cronFieldStep 解析一个正数 cron 步长。
func cronFieldStep(text string, spec cronFieldSpec) (int, error) {
	step, err := strconv.Atoi(text)
	if err != nil || step < 1 {
		return 0, fmt.Errorf("cron.expression %s field step must be a positive integer", spec.name)
	}
	return step, nil
}

// invalidCronFieldError 构造稳定的畸形元素诊断。
func invalidCronFieldError(spec cronFieldSpec, element string) error {
	return fmt.Errorf("cron.expression %s field element %q must be *, a value, a-b, */n, a-b/n, or a comma-separated list of those", spec.name, element)
}

// encodeCronField 把匹配值集合编码为最短等价的逗号分隔字段，且从不使用前导 *。
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

// encodeStarCronField 编码源文本以 '*' 开头的列，并保留该星号标志（它决定 Vixie 的
// day-of-month/day-of-week 采用 AND 还是 OR 规则）。
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
		// 不可达：步长为 1 的遍历从最小值开始，而每个星号开头的列都匹配该最小值。
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

// starWalk 返回 */step 遍历从列最小值开始匹配的升序折叠集合。
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
