package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
)

const testReservation = "ocid1.capacityreservation.oc1.us-ashburn-1.test"
const testE5 = "VM.Standard.E5.Flex"
const testE6 = "VM.Standard.E6.Flex"

func writeWeeklyFixture(t *testing.T, dir, name, shape string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"capacity reservation OCID", "weekday", "hour", "shape", "ocpus", "memory_gbs", "count"})
	for d := time.Sunday; d <= time.Saturday; d++ {
		for h := 0; h < 24; h++ {
			_ = w.Write([]string{testReservation, d.String(), fmt.Sprint(h), shape, "1", "4", "10"})
		}
	}
	w.Flush()
	if err = w.Error(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
func writeOverrideFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("capacity reservation OCID,date,hour,shape,ocpus,memory_gbs,count\n"+body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func loadScheduleFixture(t *testing.T, dir string) *capacitySchedule {
	t.Helper()
	s, err := loadCapacitySchedule(scheduleOptions{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func localInstant(t *testing.T, stamp string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/Detroit")
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return at.In(loc)
}
func targetCount(s *capacitySchedule, g scheduleGroup, at time.Time) int64 {
	var n int64
	for _, r := range s.resolve(g, at) {
		n += r.Count
	}
	return n
}

func TestScheduleOverridePrecedenceAndShapeIsolation(t *testing.T) {
	dir := t.TempDir()
	writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	writeWeeklyFixture(t, dir, "weekly-e6.csv", testE6)
	writeOverrideFixture(t, dir, "override-e5.csv", fmt.Sprintf("%s,2026-12-25,,%s,1,4,0\n%s,2026-12-25,12,%s,1,4,25\n", testReservation, testE5, testReservation, testE5))
	s := loadScheduleFixture(t, dir)
	e5 := scheduleGroup{testReservation, testE5}
	e6 := scheduleGroup{testReservation, testE6}
	for _, tc := range []struct {
		stamp string
		want  int64
	}{{"2026-12-24T12:00:00-05:00", 10}, {"2026-12-25T11:00:00-05:00", 0}, {"2026-12-25T12:00:00-05:00", 25}, {"2026-12-25T13:00:00-05:00", 0}, {"2026-12-26T12:00:00-05:00", 10}} {
		at := localInstant(t, tc.stamp)
		if got := targetCount(s, e5, at); got != tc.want {
			t.Fatalf("%s got %d want %d", tc.stamp, got, tc.want)
		}
		if got := targetCount(s, e6, at); got != 10 {
			t.Fatal("E5 override changed E6")
		}
	}
}
func TestScheduleValidation(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"mixed-shape", fmt.Sprintf("%s,2026-12-25,0,%s,1,4,1\n%s,2026-12-25,1,%s,1,4,1\n", testReservation, testE5, testReservation, testE6), "exactly one shape"},
		{"negative-count", fmt.Sprintf("%s,2026-12-25,0,%s,1,4,-1\n", testReservation, testE5), "nonnegative"},
		{"bad-date", fmt.Sprintf("%s,2026-02-30,0,%s,1,4,1\n", testReservation, testE5), "YYYY-MM-DD"},
		{"bad-hour", fmt.Sprintf("%s,2026-12-25,24,%s,1,4,1\n", testReservation, testE5), "0-23"},
		{"nan", fmt.Sprintf("%s,2026-12-25,0,%s,NaN,4,1\n", testReservation, testE5), "finite positive"},
		{"infinity", fmt.Sprintf("%s,2026-12-25,0,%s,1,Inf,1\n", testReservation, testE5), "finite positive"},
		{"bad-ocid", fmt.Sprintf("ocid1.instance.oc1.us-ashburn-1.test,2026-12-25,0,%s,1,4,1\n", testE5), "invalid capacity reservation OCID"},
		{"fraction-count", fmt.Sprintf("%s,2026-12-25,0,%s,1,4,1.5\n", testReservation, testE5), "nonnegative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := writeOverrideFixture(t, dir, "override-test.csv", tc.body)
			_, _, _, err := readScheduleCSV(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want %s", err, tc.want)
			}
		})
	}
}
func TestScheduleCoverageDuplicatesAndSelection(t *testing.T) {
	dir := t.TempDir()
	e5 := writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	writeWeeklyFixture(t, dir, "weekly-e6.csv", testE6)
	s, err := loadCapacitySchedule(scheduleOptions{Directory: dir, Shapes: stringFlags{testE5}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Managed) != 1 || len(s.Files) != 1 {
		t.Fatal("shape selection failed")
	}
	s, err = loadCapacitySchedule(scheduleOptions{Files: stringFlags{e5}})
	if err != nil || len(s.Managed) != 1 {
		t.Fatalf("file selection: %v", err)
	}
	if _, err = loadCapacitySchedule(scheduleOptions{Files: stringFlags{e5, e5}}); err == nil {
		t.Fatal("duplicate file accepted")
	}
	body, err := os.ReadFile(e5)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "weekly-copy.csv")
	if err = os.WriteFile(copyPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadCapacitySchedule(scheduleOptions{Directory: dir}); err == nil || !strings.Contains(err.Error(), "duplicate configuration") {
		t.Fatalf("duplicate slot: %v", err)
	}
	if err = os.Remove(copyPath); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if err = os.WriteFile(e5, []byte(strings.Join(lines[:len(lines)-1], "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadCapacitySchedule(scheduleOptions{Directory: dir}); err == nil || !strings.Contains(err.Error(), "missing weekly slot") {
		t.Fatalf("missing hour: %v", err)
	}
}
func TestScheduleBOMAndBlankRows(t *testing.T) {
	dir := t.TempDir()
	p := writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, append([]byte("\ufeff"), append(body, []byte("\n,,,,,,\n")...)...), 0600); err != nil {
		t.Fatal(err)
	}
	loadScheduleFixture(t, dir)
	quoted := strings.Replace(string(body), "capacity reservation OCID", "\"capacity reservation OCID\"", 1)
	if err = os.WriteFile(p, []byte("\ufeff"+quoted), 0600); err != nil {
		t.Fatal(err)
	}
	loadScheduleFixture(t, dir)
}
func TestScheduleSizeTransitionBelowUsedAndPreservation(t *testing.T) {
	dir := t.TempDir()
	writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	writeWeeklyFixture(t, dir, "weekly-e6.csv", testE6)
	writeOverrideFixture(t, dir, "override-e5.csv", fmt.Sprintf("%s,2026-12-25,12,%s,2,8,3\n", testReservation, testE5))
	s := loadScheduleFixture(t, dir)
	old := actualConfig(testE5, 1, 4, 10)
	old.UsedCount = common.Int64(8)
	old.FaultDomain = common.String("FAULT-DOMAIN-1")
	unrelated := actualConfig(testE5, 8, 32, 7)
	unrelated.ClusterPlacementGroupId = common.String("placement")
	unrelated.ClusterConfig = &core.ClusterConfigDetails{HpcIslandId: common.String("island")}
	current := core.ComputeCapacityReservation{InstanceReservationConfigs: []core.InstanceReservationConfig{old, unrelated, actualConfig(testE6, 1, 4, 20)}}
	plan, err := buildReservationSchedulePlan(s, testReservation, current, scheduleOptions{}, localInstant(t, "2026-12-25T12:00:00-05:00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Configs) != 4 || len(plan.Changes) != 3 {
		t.Fatalf("expected merged update with new size: %+v", plan)
	}
	if *plan.Configs[0].ReservedCount != 0 || *plan.Configs[2].ReservedCount != 10 || *plan.Configs[3].ReservedCount != 3 {
		t.Fatal("incorrect absolute targets")
	}
	if value(plan.Configs[0].FaultDomain) != "FAULT-DOMAIN-1" {
		t.Fatal("lost placement")
	}
	if requestedConfigKey(plan.Configs[1]) != actualConfigKey(unrelated) || *plan.Configs[1].ReservedCount != 7 {
		t.Fatal("lost unrelated configuration")
	}
	if *old.ReservedCount != 10 {
		t.Fatal("mutated GET response")
	}
	// The weekly size returns when the override expires; the override size becomes zero.
	current.InstanceReservationConfigs = append(current.InstanceReservationConfigs, actualConfig(testE5, 2, 8, 3))
	plan, err = buildReservationSchedulePlan(s, testReservation, current, scheduleOptions{}, localInstant(t, "2026-12-26T12:00:00-05:00"))
	if err != nil {
		t.Fatal(err)
	}
	if *plan.Configs[3].ReservedCount != 0 {
		t.Fatal("expired override left capacity reserved")
	}
}
func TestSchedulePreservesUnselectedShapesAndRejectsAmbiguousPlacement(t *testing.T) {
	dir := t.TempDir()
	writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	s := loadScheduleFixture(t, dir)
	first := actualConfig(testE5, 1, 4, 20)
	first.FaultDomain = common.String("FAULT-DOMAIN-1")
	second := actualConfig(testE5, 1, 4, 30)
	second.FaultDomain = common.String("FAULT-DOMAIN-2")
	current := core.ComputeCapacityReservation{InstanceReservationConfigs: []core.InstanceReservationConfig{first, second, actualConfig(testE6, 1, 4, 99)}}
	at := localInstant(t, "2026-10-09T12:00:00-04:00")
	if _, err := buildReservationSchedulePlan(s, testReservation, current, scheduleOptions{}, at); err == nil {
		t.Fatal("ambiguous placement accepted")
	}
	plan, err := buildReservationSchedulePlan(s, testReservation, current, scheduleOptions{FaultDomain: "FAULT-DOMAIN-1"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if *plan.Configs[0].ReservedCount != 10 || *plan.Configs[1].ReservedCount != 30 || *plan.Configs[2].ReservedCount != 99 {
		t.Fatal("selector changed unrelated config")
	}
}
func TestScheduleDSTPreview(t *testing.T) {
	dir := t.TempDir()
	writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	for _, tc := range []struct {
		from string
		want []string
	}{
		{"2026-03-08T00:00:00-05:00", []string{"2026-03-08T00:00:00-05:00", "2026-03-08T01:00:00-05:00", "2026-03-08T03:00:00-04:00", "2026-03-08T04:00:00-04:00"}},
		{"2026-11-01T00:00:00-04:00", []string{"2026-11-01T00:00:00-04:00", "2026-11-01T01:00:00-04:00", "2026-11-01T01:00:00-05:00", "2026-11-01T02:00:00-05:00"}},
	} {
		var out bytes.Buffer
		if err := scheduleMain(context.Background(), []string{"preview", "--schedule-dir", dir, "--timezone", "America/Detroit", "--from", tc.from, "--hours", "4"}, &out); err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(&out).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range tc.want {
			if rows[i+1][0] != want {
				t.Fatalf("DST row %d: %s != %s", i, rows[i+1][0], want)
			}
		}
	}
}
func TestScheduleCLIRejectsHistoricalLiveApply(t *testing.T) {
	_, err := parseScheduleOptions([]string{"apply", "--timezone", "UTC", "--at", "2026-10-09T12:00:00Z"})
	if err == nil || !strings.Contains(err.Error(), "live apply always uses current time") {
		t.Fatalf("got %v", err)
	}
	if _, err = parseScheduleOptions([]string{"validate"}); err == nil {
		t.Fatal("timezone not required")
	}
}

type fakeScheduleBackend struct {
	Current     core.ComputeCapacityReservation
	ETag        *string
	Updates     int
	Submitted   []core.InstanceReservationConfigDetails
	UpdateError error
	Fulfill     bool
	WorkID      string
	WaitError   error
	OnGet       func()
}

func (b *fakeScheduleBackend) Get(context.Context, string) (core.ComputeCapacityReservation, *string, error) {
	if b.OnGet != nil {
		b.OnGet()
	}
	return b.Current, b.ETag, nil
}
func (b *fakeScheduleBackend) Update(_ context.Context, _ string, etag *string, c []core.InstanceReservationConfigDetails) (string, error) {
	b.Updates++
	if value(etag) != value(b.ETag) {
		return "", errors.New("wrong ETag")
	}
	b.Submitted = c
	if b.UpdateError != nil {
		return "", b.UpdateError
	}
	if b.Fulfill {
		b.Current.InstanceReservationConfigs = nil
		for _, r := range c {
			b.Current.InstanceReservationConfigs = append(b.Current.InstanceReservationConfigs, core.InstanceReservationConfig{InstanceShape: r.InstanceShape, InstanceShapeConfig: r.InstanceShapeConfig, ReservedCount: r.ReservedCount, UsedCount: common.Int64(20), FaultDomain: r.FaultDomain, ClusterConfig: r.ClusterConfig, ClusterPlacementGroupId: r.ClusterPlacementGroupId})
		}
	}
	return b.WorkID, nil
}
func (b *fakeScheduleBackend) Wait(context.Context, string, time.Duration) error { return b.WaitError }
func backendFixture() *fakeScheduleBackend {
	return &fakeScheduleBackend{Current: core.ComputeCapacityReservation{LifecycleState: core.ComputeCapacityReservationLifecycleStateActive, InstanceReservationConfigs: []core.InstanceReservationConfig{actualConfig(testE5, 1, 4, 30)}}, ETag: common.String("etag"), Fulfill: true}
}
func TestScheduleApplyVerificationAndNoRepeatedWrites(t *testing.T) {
	dir := t.TempDir()
	writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	s := loadScheduleFixture(t, dir)
	at := localInstant(t, "2026-10-09T12:00:00-04:00")
	now := func() time.Time { return at }
	for _, tc := range []struct {
		name    string
		modify  func(*fakeScheduleBackend)
		dry     bool
		wantErr bool
		writes  int
	}{
		{"success", func(b *fakeScheduleBackend) { b.Current.InstanceReservationConfigs[0].UsedCount = common.Int64(20) }, false, false, 1},
		{"unchanged", func(b *fakeScheduleBackend) { b.Current.InstanceReservationConfigs[0].ReservedCount = common.Int64(10) }, false, false, 0},
		{"dry-run", func(*fakeScheduleBackend) {}, true, false, 0},
		{"shortfall", func(b *fakeScheduleBackend) { b.Fulfill = false }, false, true, 1},
		{"conflict", func(b *fakeScheduleBackend) { b.UpdateError = errors.New("412 ETag conflict") }, false, true, 1},
		{"ambiguous-timeout", func(b *fakeScheduleBackend) { b.UpdateError = context.DeadlineExceeded }, false, true, 1},
		{"failed-work", func(b *fakeScheduleBackend) { b.WorkID = "work"; b.WaitError = errors.New("FAILED") }, false, true, 1},
		{"no-etag", func(b *fakeScheduleBackend) { b.ETag = nil }, false, true, 0},
		{"updating", func(b *fakeScheduleBackend) {
			b.Current.LifecycleState = core.ComputeCapacityReservationLifecycleStateUpdating
		}, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := backendFixture()
			tc.modify(b)
			err := applyScheduledReservation(context.Background(), b, s, testReservation, scheduleOptions{DryRun: tc.dry}, now, io.Discard)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error %v", err)
			}
			if b.Updates != tc.writes {
				t.Fatalf("writes %d want %d", b.Updates, tc.writes)
			}
		})
	}
	// Running twice against fulfilled state submits only one update.
	b := backendFixture()
	for i := 0; i < 2; i++ {
		if err := applyScheduledReservation(context.Background(), b, s, testReservation, scheduleOptions{}, now, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if b.Updates != 1 {
		t.Fatal("repeated tick wrote again")
	}
}
func TestScheduleCrossingHourNeverSubmitsStaleTarget(t *testing.T) {
	dir := t.TempDir()
	writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	s := loadScheduleFixture(t, dir)
	at := localInstant(t, "2026-10-09T12:59:59-04:00")
	calls := 0
	now := func() time.Time {
		calls++
		if calls > 1 {
			return at.Add(time.Second)
		}
		return at
	}
	b := backendFixture()
	err := applyScheduledReservation(context.Background(), b, s, testReservation, scheduleOptions{}, now, io.Discard)
	if err == nil || b.Updates != 0 {
		t.Fatalf("stale update: %v writes=%d", err, b.Updates)
	}
}

func TestScheduleApplyCombinesShapesAndZeroTarget(t *testing.T) {
	dir := t.TempDir()
	writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	writeWeeklyFixture(t, dir, "weekly-e6.csv", testE6)
	writeOverrideFixture(t, dir, "override-e5.csv", fmt.Sprintf("%s,2026-12-25,,%s,1,4,0\n", testReservation, testE5))
	s := loadScheduleFixture(t, dir)
	b := backendFixture()
	b.Current.InstanceReservationConfigs[0].UsedCount = common.Int64(25)
	b.Current.InstanceReservationConfigs = append(b.Current.InstanceReservationConfigs, actualConfig(testE6, 1, 4, 20))
	now := func() time.Time { return localInstant(t, "2026-12-25T12:00:00-05:00") }
	if err := applyCapacitySchedule(context.Background(), b, s, scheduleOptions{LockDir: t.TempDir()}, "us-ashburn-1", now, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b.Updates != 1 || *b.Submitted[0].ReservedCount != 0 || *b.Submitted[1].ReservedCount != 10 {
		t.Fatalf("expected one merged update with zero below usage: %+v", b)
	}
}
func TestScheduleResolvesAfterSlowGET(t *testing.T) {
	dir := t.TempDir()
	p := writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	old := fmt.Sprintf("%s,Friday,13,%s,1,4,10", testReservation, testE5)
	newRow := fmt.Sprintf("%s,Friday,13,%s,1,4,17", testReservation, testE5)
	if err = os.WriteFile(p, []byte(strings.Replace(string(body), old, newRow, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	s := loadScheduleFixture(t, dir)
	b := backendFixture()
	at := localInstant(t, "2026-10-09T12:59:59-04:00")
	b.OnGet = func() { at = localInstant(t, "2026-10-09T13:00:00-04:00") }
	if err := applyScheduledReservation(context.Background(), b, s, testReservation, scheduleOptions{}, func() time.Time { return at }, io.Discard); err != nil {
		t.Fatal(err)
	}
	if *b.Submitted[0].ReservedCount != 17 {
		t.Fatal("applied target from before slow GET")
	}
}
func TestScheduleRejectsConfigurationLimit(t *testing.T) {
	dir := t.TempDir()
	writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	s := loadScheduleFixture(t, dir)
	current := core.ComputeCapacityReservation{}
	for i := 0; i < 50; i++ {
		current.InstanceReservationConfigs = append(current.InstanceReservationConfigs, actualConfig(testE6, float32(i+1), float32(4*(i+1)), 1))
	}
	if _, err := buildReservationSchedulePlan(s, testReservation, current, scheduleOptions{}, time.Now()); err == nil || !strings.Contains(err.Error(), "at most 50") {
		t.Fatalf("limit: %v", err)
	}
}

func TestScheduleLocalLockPreventsOverlappingWrites(t *testing.T) {
	dir := t.TempDir()
	writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	s := loadScheduleFixture(t, dir)
	lockDir := t.TempDir()
	key := fmt.Sprintf("%x", sha256.Sum256([]byte("us-ashburn-1:"+testReservation)))
	lock := flock.New(filepath.Join(lockDir, key+".lock"))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		t.Fatalf("test lock: %v", err)
	}
	defer lock.Close()
	b := backendFixture()
	now := func() time.Time { return localInstant(t, "2026-10-09T12:00:00-04:00") }
	err = applyCapacitySchedule(context.Background(), b, s, scheduleOptions{LockDir: lockDir}, "us-ashburn-1", now, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "local invocation") || b.Updates != 0 {
		t.Fatalf("overlap: %v writes=%d", err, b.Updates)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err = applyCapacitySchedule(context.Background(), b, s, scheduleOptions{LockDir: lockDir}, "us-ashburn-1", now, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b.Updates != 1 {
		t.Fatal("lock release did not allow next tick")
	}
}

func TestScheduleAcceptsRegionalCapacityReservationOCID(t *testing.T) {
	const frankfurtID = "ocid1.capacityreservation.oc1.eu-frankfurt-1.antheljtlwu57wqcppfslqccat4jgqjourkb6sztwaui7du5viaznzlcpk3a"
	dir := t.TempDir()
	weekly := writeWeeklyFixture(t, dir, "weekly-e5.csv", testE5)
	content, err := os.ReadFile(weekly)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(weekly, []byte(strings.ReplaceAll(string(content), testReservation, frankfurtID)), 0600); err != nil {
		t.Fatal(err)
	}
	writeOverrideFixture(t, dir, "override-e5.csv", fmt.Sprintf("%s,2026-12-25,12,%s,1,4,25\n", frankfurtID, testE5))
	s := loadScheduleFixture(t, dir)
	group := scheduleGroup{frankfurtID, testE5}
	if got := targetCount(s, group, localInstant(t, "2026-12-25T12:00:00-05:00")); got != 25 {
		t.Fatalf("Frankfurt override count: got %d want 25", got)
	}
	// The previous, incorrect resource type must not be accepted.
	invalidID := strings.Replace(frankfurtID, "ocid1.capacityreservation.", "ocid1.computecapacityreservation.", 1)
	path := writeOverrideFixture(t, dir, "override-invalid.csv", fmt.Sprintf("%s,2026-12-25,12,%s,1,4,25\n", invalidID, testE5))
	if _, _, _, err := readScheduleCSV(path); err == nil || !strings.Contains(err.Error(), "expected ocid1.capacityreservation") {
		t.Fatalf("incorrect resource type accepted: %v", err)
	}
}
