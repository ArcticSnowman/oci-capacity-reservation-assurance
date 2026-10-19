package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/oracle/oci-go-sdk/v65/core"
)

// CPU and memory identify a VM size; count is an absolute target, including zero.
type scheduleSize struct {
	Shape         string
	OCPUs, Memory float32
}
type scheduleGroup struct{ Reservation, Shape string }
type scheduleSlot struct {
	Group scheduleGroup
	Day   string
	Hour  int
}
type scheduleRow struct {
	Size   scheduleSize
	Count  int64
	Source string
}
type capacitySchedule struct {
	Weekly    map[scheduleSlot][]scheduleRow
	Overrides map[scheduleSlot][]scheduleRow
	Managed   map[scheduleGroup]map[scheduleSize]bool
	Files     []string
}
type stringFlags []string

func (s *stringFlags) String() string { return strings.Join(*s, ",") }
func (s *stringFlags) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("empty selection")
	}
	*s = append(*s, v)
	return nil
}

type scheduleOptions struct {
	Action, Directory, Timezone, At, From, Auth, Profile, ConfigPath, Region, LockDir, FaultDomain, PlacementGroup string
	Files, Shapes                                                                                                  stringFlags
	Hours                                                                                                          int
	DryRun                                                                                                         bool
	Timeout, PollInterval                                                                                          time.Duration
}

func parseScheduleOptions(args []string) (scheduleOptions, error) {
	o := scheduleOptions{Action: "help"}
	if len(args) == 0 {
		args = []string{"help"}
	}
	o.Action = args[0]
	fs := flag.NewFlagSet("schedule "+o.Action, flag.ContinueOnError)
	fs.StringVar(&o.Directory, "schedule-dir", ".", "directory containing weekly-*.csv and override-*.csv")
	fs.Var(&o.Files, "file", "explicit CSV path; repeat to select files instead of scanning a directory")
	fs.Var(&o.Shapes, "shape", "exact OCI shape to select; repeat for multiple shapes")
	fs.StringVar(&o.Timezone, "timezone", "", "required IANA schedule timezone, e.g. America/Detroit")
	fs.StringVar(&o.At, "at", "", "RFC3339 instant for preview or apply --dry-run; never a live apply")
	fs.StringVar(&o.From, "from", "", "preview start: YYYY-MM-DD or RFC3339; defaults to current hour")
	fs.IntVar(&o.Hours, "hours", 168, "number of elapsed hours to preview (1-8784)")
	fs.BoolVar(&o.DryRun, "dry-run", false, "GET and print full proposed OCI update without writing")
	fs.StringVar(&o.Auth, "auth", "config", "config or instance_principal")
	fs.StringVar(&o.Profile, "profile", "DEFAULT", "OCI config profile")
	fs.StringVar(&o.ConfigPath, "oci-config-file", "", "OCI credentials file; defaults to ~/.oci/config")
	fs.StringVar(&o.Region, "region", "", "OCI region override")
	fs.StringVar(&o.LockDir, "lock-dir", "", "local reservation lock directory; default ~/.oci-capacity-grow")
	fs.StringVar(&o.FaultDomain, "fault-domain", "", "optional placement selector; ANY selects no fault domain")
	fs.StringVar(&o.PlacementGroup, "cluster-placement-group-id", "", "optional placement selector")
	fs.DurationVar(&o.Timeout, "timeout", 10*time.Minute, "total apply invocation timeout")
	fs.DurationVar(&o.PollInterval, "poll-interval", 10*time.Second, "work request / verification polling interval")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: reserve-capacity schedule validate|preview|apply --timezone America/Detroit [flags]")
		fs.PrintDefaults()
	}
	if o.Action == "help" || o.Action == "--help" || o.Action == "-h" {
		fs.Usage()
		return o, flag.ErrHelp
	}
	if o.Action != "validate" && o.Action != "preview" && o.Action != "apply" {
		return o, fmt.Errorf("unknown schedule action %q", o.Action)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if o.Timezone == "" {
		return o, errors.New("--timezone is required")
	}
	if _, err := time.LoadLocation(o.Timezone); err != nil {
		return o, fmt.Errorf("timezone: %w", err)
	}
	if o.Hours < 1 || o.Hours > 8784 {
		return o, errors.New("--hours must be between 1 and 8784")
	}
	if o.Timeout <= 0 || o.PollInterval <= 0 {
		return o, errors.New("timeout and poll interval must be positive")
	}
	if o.Auth != "config" && o.Auth != "instance_principal" {
		return o, errors.New("--auth must be config or instance_principal")
	}
	if o.At != "" {
		if o.Action == "validate" {
			return o, errors.New("--at is only supported for preview or apply --dry-run")
		}
		if _, err := time.Parse(time.RFC3339, o.At); err != nil {
			return o, fmt.Errorf("--at: %w", err)
		}
		if o.Action == "apply" && !o.DryRun {
			return o, errors.New("--at is only allowed with apply --dry-run; live apply always uses current time")
		}
	}
	if o.From != "" && o.Action != "preview" {
		return o, errors.New("--from is only supported for preview")
	}
	if o.From != "" && o.At != "" {
		return o, errors.New("use either --from or --at")
	}
	if o.DryRun && o.Action != "apply" {
		return o, errors.New("--dry-run is only supported for apply; preview is already offline")
	}
	return o, nil
}

var reservationOCID = regexp.MustCompile(`^ocid1\.capacityreservation\.[a-z0-9]+\.[a-z0-9-]*\.[A-Za-z0-9._-]+$`)

func loadCapacitySchedule(o scheduleOptions) (*capacitySchedule, error) {
	files := append([]string(nil), o.Files...)
	if len(files) == 0 {
		entries, err := os.ReadDir(o.Directory)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() && strings.HasSuffix(name, ".csv") && (strings.HasPrefix(name, "weekly-") || strings.HasPrefix(name, "override-")) {
				files = append(files, filepath.Join(o.Directory, name))
			}
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, errors.New("no weekly-*.csv or override-*.csv files selected")
	}
	s := &capacitySchedule{Weekly: map[scheduleSlot][]scheduleRow{}, Overrides: map[scheduleSlot][]scheduleRow{}, Managed: map[scheduleGroup]map[scheduleSize]bool{}}
	seenPaths := map[string]bool{}
	for _, path := range files {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if seenPaths[abs] {
			return nil, fmt.Errorf("file selected twice: %s", path)
		}
		seenPaths[abs] = true
		rows, override, shape, err := readScheduleCSV(path)
		if err != nil {
			return nil, err
		}
		if len(o.Shapes) > 0 && !containsString(o.Shapes, shape) {
			continue
		}
		s.Files = append(s.Files, path)
		target := s.Weekly
		if override {
			target = s.Overrides
		}
		for _, r := range rows {
			for _, old := range target[r.Slot] {
				if old.Size == r.Row.Size {
					return nil, fmt.Errorf("duplicate configuration at %s and %s", old.Source, r.Row.Source)
				}
			}
			target[r.Slot] = append(target[r.Slot], r.Row)
			if s.Managed[r.Slot.Group] == nil {
				s.Managed[r.Slot.Group] = map[scheduleSize]bool{}
			}
			s.Managed[r.Slot.Group][r.Row.Size] = true
		}
	}
	if len(s.Files) == 0 {
		return nil, errors.New("no files match the selected shapes")
	}
	for _, shape := range o.Shapes {
		found := false
		for g := range s.Managed {
			found = found || g.Shape == shape
		}
		if !found {
			return nil, fmt.Errorf("no schedules for selected shape %s", shape)
		}
	}
	// Every managed reservation/shape needs a complete weekly fallback. Each slot
	// is a set of active sizes; other managed sizes resolve to count zero.
	for _, g := range sortedScheduleGroups(s.Managed) {
		for d := time.Sunday; d <= time.Saturday; d++ {
			for h := 0; h < 24; h++ {
				if len(s.Weekly[scheduleSlot{g, d.String(), h}]) == 0 {
					return nil, fmt.Errorf("missing weekly slot: %s %s %s hour %d (all 168 hours required)", g.Reservation, g.Shape, d, h)
				}
			}
		}
	}
	return s, nil
}
func containsString(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}

type parsedScheduleRow struct {
	Slot scheduleSlot
	Row  scheduleRow
}

func readScheduleCSV(path string) ([]parsedScheduleRow, bool, string, error) {
	name := filepath.Base(path)
	override := strings.HasPrefix(name, "override-")
	if !strings.HasSuffix(name, ".csv") || (!override && !strings.HasPrefix(name, "weekly-")) {
		return nil, false, "", fmt.Errorf("%s: filename must be weekly-*.csv or override-*.csv", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, override, "", err
	}
	defer f.Close()
	input := bufio.NewReader(f)
	// Remove the BOM before CSV parsing, including when the first header is quoted.
	if prefix, _ := input.Peek(3); string(prefix) == "\ufeff" {
		_, _ = input.Discard(3)
	}
	reader := csv.NewReader(input)
	reader.TrimLeadingSpace = true
	header, err := reader.Read()
	if err != nil {
		return nil, override, "", fmt.Errorf("%s: header: %w", path, err)
	}
	expected := []string{"capacity reservation OCID", "weekday", "hour", "shape", "ocpus", "memory_gbs", "count"}
	if override {
		expected[1] = "date"
	}
	if len(header) != len(expected) {
		return nil, override, "", fmt.Errorf("%s: expected header %s", path, strings.Join(expected, ","))
	}
	for i, h := range header {
		h = strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))
		if h != expected[i] {
			return nil, override, "", fmt.Errorf("%s: column %d must be %q", path, i+1, expected[i])
		}
	}
	var rows []parsedScheduleRow
	shape := ""
	for {
		cells, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, override, "", fmt.Errorf("%s: %w", path, err)
		}
		line, _ := reader.FieldPos(0)
		source := fmt.Sprintf("%s:%d", path, line)
		empty := true
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
			empty = empty && cells[i] == ""
		}
		if empty {
			continue
		}
		fail := func(message string) ([]parsedScheduleRow, bool, string, error) {
			return nil, override, "", fmt.Errorf("%s: %s", source, message)
		}
		if !reservationOCID.MatchString(cells[0]) {
			return fail("invalid capacity reservation OCID; expected ocid1.capacityreservation...")
		}
		day := cells[1]
		if override {
			date, err := time.Parse("2006-01-02", day)
			if err != nil || date.Format("2006-01-02") != day {
				return fail("date must be YYYY-MM-DD")
			}
		} else {
			matched := false
			for d := time.Sunday; d <= time.Saturday; d++ {
				if strings.EqualFold(day, d.String()) || strings.EqualFold(day, d.String()[:3]) {
					day = d.String()
					matched = true
					break
				}
			}
			if !matched {
				return fail("weekday must be a weekday name, e.g. Monday or Mon")
			}
		}
		hour := -1
		if cells[2] != "" {
			hour, err = strconv.Atoi(cells[2])
			if err != nil || hour < 0 || hour > 23 {
				return fail("hour must be an integer 0-23")
			}
		} else if !override {
			return fail("weekly hour is required (0-23)")
		}
		if !strings.HasPrefix(cells[3], "VM.") && !strings.HasPrefix(cells[3], "BM.") {
			return fail("shape must be an OCI VM.* or BM.* shape")
		}
		if shape == "" {
			shape = cells[3]
		} else if shape != cells[3] {
			return fail("each CSV must contain exactly one shape")
		}
		ocpus, e1 := strconv.ParseFloat(cells[4], 32)
		memory, e2 := strconv.ParseFloat(cells[5], 32)
		if e1 != nil || e2 != nil || math.IsNaN(ocpus) || math.IsNaN(memory) || math.IsInf(ocpus, 0) || math.IsInf(memory, 0) || ocpus <= 0 || memory <= 0 || float32(ocpus) <= 0 || float32(memory) <= 0 {
			return fail("ocpus and memory_gbs must be finite positive numbers per VM")
		}
		count, err := strconv.ParseInt(cells[6], 10, 64)
		if err != nil || count < 0 {
			return fail("count must be a nonnegative integer, including zero")
		}
		rows = append(rows, parsedScheduleRow{scheduleSlot{scheduleGroup{cells[0], shape}, day, hour}, scheduleRow{scheduleSize{shape, float32(ocpus), float32(memory)}, count, source}})
	}
	if len(rows) == 0 {
		return nil, override, "", fmt.Errorf("%s: no schedule rows", path)
	}
	return rows, override, shape, nil
}

func sortedScheduleGroups(groups map[scheduleGroup]map[scheduleSize]bool) []scheduleGroup {
	out := make([]scheduleGroup, 0, len(groups))
	for g := range groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reservation != out[j].Reservation {
			return out[i].Reservation < out[j].Reservation
		}
		return out[i].Shape < out[j].Shape
	})
	return out
}
func sizeLess(a, b scheduleSize) bool {
	if a.Shape != b.Shape {
		return a.Shape < b.Shape
	}
	if a.OCPUs != b.OCPUs {
		return a.OCPUs < b.OCPUs
	}
	return a.Memory < b.Memory
}
func (s *capacitySchedule) resolve(g scheduleGroup, at time.Time) []scheduleRow {
	rows := s.Weekly[scheduleSlot{g, at.Weekday().String(), at.Hour()}]
	if daily := s.Overrides[scheduleSlot{g, at.Format("2006-01-02"), -1}]; len(daily) > 0 {
		rows = daily
	}
	if hourly := s.Overrides[scheduleSlot{g, at.Format("2006-01-02"), at.Hour()}]; len(hourly) > 0 {
		rows = hourly
	}
	targets := map[scheduleSize]scheduleRow{}
	for size := range s.Managed[g] {
		targets[size] = scheduleRow{size, 0, "inactive managed size"}
	}
	for _, row := range rows {
		targets[row.Size] = row
	}
	out := make([]scheduleRow, 0, len(targets))
	for _, r := range targets {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return sizeLess(out[i].Size, out[j].Size) })
	return out
}
func floorScheduleHour(t time.Time) time.Time {
	return t.Add(-time.Duration(t.Minute())*time.Minute - time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond()))
}
func scheduleInstant(o scheduleOptions, loc *time.Location, now time.Time) (time.Time, error) {
	if o.At != "" {
		t, err := time.Parse(time.RFC3339, o.At)
		return t.In(loc), err
	}
	if o.From != "" {
		t, err := time.Parse(time.RFC3339, o.From)
		if err != nil {
			t, err = time.ParseInLocation("2006-01-02", o.From, loc)
		}
		return t.In(loc), err
	}
	return floorScheduleHour(now.In(loc)), nil
}

func scheduleMain(ctx context.Context, args []string, out io.Writer) error {
	o, err := parseScheduleOptions(args)
	if err != nil {
		return err
	}
	s, err := loadCapacitySchedule(o)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(o.Timezone)
	if err != nil {
		return err
	}
	at, err := scheduleInstant(o, loc, time.Now())
	if err != nil {
		return fmt.Errorf("preview start: %w", err)
	}
	switch o.Action {
	case "validate":
		_, err = fmt.Fprintf(out, "VALID: %d files, %d reservation/shape schedules; timezone=%s\n", len(s.Files), len(s.Managed), o.Timezone)
		return err
	case "preview":
		writer := csv.NewWriter(out)
		if err = writer.Write([]string{"timestamp", "capacity reservation OCID", "shape", "ocpus", "memory_gbs", "count", "total_ocpus", "total_memory_gbs", "source"}); err != nil {
			return err
		}
		for h := 0; h < o.Hours; h++ {
			instant := at.Add(time.Duration(h) * time.Hour)
			for _, g := range sortedScheduleGroups(s.Managed) {
				for _, row := range s.resolve(g, instant) {
					if err = writer.Write([]string{instant.Format(time.RFC3339), g.Reservation, g.Shape, fmt.Sprint(row.Size.OCPUs), fmt.Sprint(row.Size.Memory), strconv.FormatInt(row.Count, 10), fmt.Sprint(float64(row.Size.OCPUs) * float64(row.Count)), fmt.Sprint(float64(row.Size.Memory) * float64(row.Count)), row.Source}); err != nil {
						return err
					}
				}
			}
		}
		writer.Flush()
		return writer.Error()
	case "apply":
		backend, region, err := newOCIScheduleBackend(o)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, o.Timeout)
		defer cancel()
		clock := func() time.Time { return time.Now().In(loc) }
		if o.At != "" {
			clock = func() time.Time { return at }
		}
		return applyCapacitySchedule(ctx, backend, s, o, region, clock, out)
	}
	return errors.New("unknown schedule action")
}

// Printed dry-run payloads use the OCI update schema. They preserve unrelated
// configurations; the read-only used count is never sent in an update.
type reservationSchedulePlan struct {
	Reservation string                                  `json:"capacityReservationId"`
	At          string                                  `json:"at"`
	Changes     []scheduleChange                        `json:"changes"`
	Configs     []core.InstanceReservationConfigDetails `json:"instanceReservationConfigs"`
}
type scheduleChange struct {
	Shape  string  `json:"shape"`
	OCPUs  float32 `json:"ocpus"`
	Memory float32 `json:"memoryInGBs"`
	Before int64   `json:"before"`
	Target int64   `json:"target"`
	Used   int64   `json:"used"`
	Source string  `json:"source"`
}

func printSchedulePlan(out io.Writer, p reservationSchedulePlan) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}
