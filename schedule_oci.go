package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/workrequests"
)

type scheduleBackend interface {
	Get(context.Context, string) (core.ComputeCapacityReservation, *string, error)
	Update(context.Context, string, *string, []core.InstanceReservationConfigDetails) (string, error)
	Wait(context.Context, string, time.Duration) error
}
type ociScheduleBackend struct {
	compute core.ComputeClient
	work    workrequests.WorkRequestClient
}

func newOCIScheduleBackend(o scheduleOptions) (scheduleBackend, string, error) {
	var provider common.ConfigurationProvider
	if o.Auth == "instance_principal" {
		var err error
		provider, err = auth.InstancePrincipalConfigurationProvider()
		if err != nil {
			return nil, "", err
		}
	} else if o.ConfigPath != "" || o.Profile != "DEFAULT" {
		path := o.ConfigPath
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, "", err
			}
			path = filepath.Join(home, ".oci", "config")
		}
		provider = common.CustomProfileConfigProvider(path, o.Profile)
	} else {
		provider = common.DefaultConfigProvider()
	}
	region := o.Region
	if region == "" {
		var err error
		region, err = provider.Region()
		if err != nil {
			return nil, "", err
		}
	}
	if strings.TrimSpace(region) == "" {
		return nil, "", errors.New("OCI region is required in profile or --region")
	}
	compute, err := core.NewComputeClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, "", err
	}
	compute.SetRegion(region)
	work, err := workrequests.NewWorkRequestClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, "", err
	}
	work.SetRegion(region)
	return ociScheduleBackend{compute, work}, region, nil
}
func (b ociScheduleBackend) Get(ctx context.Context, id string) (core.ComputeCapacityReservation, *string, error) {
	r, err := b.compute.GetComputeCapacityReservation(ctx, core.GetComputeCapacityReservationRequest{CapacityReservationId: common.String(id)})
	return r.ComputeCapacityReservation, r.Etag, err
}
func (b ociScheduleBackend) Update(ctx context.Context, id string, etag *string, configs []core.InstanceReservationConfigDetails) (string, error) {
	// An ambiguous timeout must not cause the SDK to blindly repeat a write.
	policy := common.NoRetryPolicy()
	r, err := b.compute.UpdateComputeCapacityReservation(ctx, core.UpdateComputeCapacityReservationRequest{
		CapacityReservationId: common.String(id), IfMatch: etag,
		UpdateComputeCapacityReservationDetails: core.UpdateComputeCapacityReservationDetails{InstanceReservationConfigs: configs},
		RequestMetadata:                         common.RequestMetadata{RetryPolicy: &policy},
	})
	return value(r.OpcWorkRequestId), err
}
func (b ociScheduleBackend) Wait(ctx context.Context, id string, interval time.Duration) error {
	work, err := waitForWorkRequest(ctx, b.work, id, interval)
	if err != nil {
		return err
	}
	if work.Status != workrequests.WorkRequestStatusSucceeded {
		return workRequestFailure(ctx, b.work, id, work.Status)
	}
	return nil
}

func sizeOfConfig(c core.InstanceReservationConfig) scheduleSize {
	return scheduleSize{value(c.InstanceShape), float32Value(shapeConfigOCPUs(c.InstanceShapeConfig)), float32Value(shapeConfigMemory(c.InstanceShapeConfig))}
}
func writableScheduleConfig(c core.InstanceReservationConfig) core.InstanceReservationConfigDetails {
	return core.InstanceReservationConfigDetails{InstanceShape: c.InstanceShape, InstanceShapeConfig: c.InstanceShapeConfig, FaultDomain: c.FaultDomain, ClusterConfig: c.ClusterConfig, ClusterPlacementGroupId: c.ClusterPlacementGroupId, ReservedCount: c.ReservedCount}
}
func matchesSchedulePlacement(c core.InstanceReservationConfig, o scheduleOptions) bool {
	if o.FaultDomain != "" {
		fd := o.FaultDomain
		if strings.EqualFold(fd, "ANY") {
			fd = ""
		}
		if value(c.FaultDomain) != fd {
			return false
		}
	}
	return o.PlacementGroup == "" || value(c.ClusterPlacementGroupId) == o.PlacementGroup
}

func buildReservationSchedulePlan(s *capacitySchedule, id string, current core.ComputeCapacityReservation, o scheduleOptions, at time.Time) (reservationSchedulePlan, error) {
	plan := reservationSchedulePlan{Reservation: id, At: at.Format(time.RFC3339), Changes: []scheduleChange{}, Configs: make([]core.InstanceReservationConfigDetails, 0, len(current.InstanceReservationConfigs))}
	for _, c := range current.InstanceReservationConfigs {
		if value(c.InstanceShape) == "" || c.ReservedCount == nil || *c.ReservedCount < 0 {
			return plan, errors.New("OCI returned invalid existing configuration; refusing lossy update")
		}
		plan.Configs = append(plan.Configs, writableScheduleConfig(c))
	}
	for _, g := range sortedScheduleGroups(s.Managed) {
		if g.Reservation != id {
			continue
		}
		for _, row := range s.resolve(g, at) {
			matches := []int{}
			for i, c := range current.InstanceReservationConfigs {
				if sizeOfConfig(c) == row.Size && matchesSchedulePlacement(c, o) {
					matches = append(matches, i)
				}
			}
			if len(matches) > 1 {
				return plan, fmt.Errorf("ambiguous existing configuration: %s %g OCPU / %g GB; use --fault-domain or --cluster-placement-group-id", g.Shape, row.Size.OCPUs, row.Size.Memory)
			}
			if len(matches) == 0 {
				// Do not materialize zero-capacity sizes for future dated overrides.
				if row.Count == 0 {
					continue
				}
				c := core.InstanceReservationConfigDetails{InstanceShape: common.String(g.Shape), InstanceShapeConfig: &core.InstanceReservationShapeConfigDetails{Ocpus: common.Float32(row.Size.OCPUs), MemoryInGBs: common.Float32(row.Size.Memory)}, ReservedCount: common.Int64(row.Count)}
				if o.FaultDomain != "" && !strings.EqualFold(o.FaultDomain, "ANY") {
					c.FaultDomain = common.String(o.FaultDomain)
				}
				if o.PlacementGroup != "" {
					c.ClusterPlacementGroupId = common.String(o.PlacementGroup)
				}
				plan.Configs = append(plan.Configs, c)
				plan.Changes = append(plan.Changes, scheduleChange{Shape: g.Shape, OCPUs: row.Size.OCPUs, Memory: row.Size.Memory, Target: row.Count, Source: row.Source})
				continue
			}
			index := matches[0]
			c := current.InstanceReservationConfigs[index]
			before := *c.ReservedCount
			if before == row.Count {
				continue
			}
			// Intentionally no used-count floor. OCI decides whether the update is valid;
			// a rejection is surfaced, never converted into an increased count.
			plan.Configs[index].ReservedCount = common.Int64(row.Count)
			plan.Changes = append(plan.Changes, scheduleChange{g.Shape, row.Size.OCPUs, row.Size.Memory, before, row.Count, int64Value(c.UsedCount), row.Source})
		}
	}
	if len(plan.Configs) > 50 {
		return plan, fmt.Errorf("update would contain %d configurations; OCI allows at most 50", len(plan.Configs))
	}
	return plan, nil
}

func applyCapacitySchedule(ctx context.Context, b scheduleBackend, s *capacitySchedule, o scheduleOptions, region string, now func() time.Time, out io.Writer) error {
	dir := o.LockDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".oci-capacity-grow")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, g := range sortedScheduleGroups(s.Managed) {
		if !seen[g.Reservation] {
			ids = append(ids, g.Reservation)
			seen[g.Reservation] = true
		}
	}
	var failures []error
	for _, id := range ids {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		// Matches the grow_capacity_reservation.py lock key on the same machine.
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(region+":"+id)))
		lock := flock.New(filepath.Join(dir, key+".lock"))
		acquired, err := lock.TryLock()
		if err == nil && !acquired {
			err = errors.New("another local invocation holds the reservation lock; retry next timer tick")
		}
		if err == nil {
			err = applyScheduledReservation(ctx, b, s, id, o, now, out)
		}
		closeErr := lock.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			wrapped := fmt.Errorf("%s: %w", id, err)
			fmt.Fprintf(out, "ERROR: %v\n", wrapped)
			failures = append(failures, wrapped)
		}
	}
	return errors.Join(failures...)
}

func applyScheduledReservation(ctx context.Context, b scheduleBackend, s *capacitySchedule, id string, o scheduleOptions, now func() time.Time, out io.Writer) error {
	current, etag, err := b.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("read reservation: %w", err)
	}
	if current.LifecycleState != core.ComputeCapacityReservationLifecycleStateActive {
		return fmt.Errorf("reservation state is %s; retry next timer tick", current.LifecycleState)
	}
	// Resolve after GET, since a slow read could cross an hour boundary.
	at := now()
	plan, err := buildReservationSchedulePlan(s, id, current, o, at)
	if err != nil {
		return err
	}
	if o.DryRun {
		return printSchedulePlan(out, plan)
	}
	if len(plan.Changes) == 0 {
		fmt.Fprintf(out, "UNCHANGED: %s at %s\n", id, plan.At)
		return nil
	}
	if value(etag) == "" {
		return errors.New("GET returned no ETag; refusing unprotected update")
	}
	if err = printSchedulePlan(out, plan); err != nil {
		return err
	}
	if !sameScheduleHour(at, now()) {
		return errors.New("schedule hour changed before update; retry next timer tick")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	workID, err := b.Update(ctx, id, etag, plan.Configs)
	if err != nil {
		return fmt.Errorf("OCI update rejected or outcome unknown (no second write attempted): %w", err)
	}
	if workID != "" {
		if err = b.Wait(ctx, workID, o.PollInterval); err != nil {
			return fmt.Errorf("update work request %s: %w", workID, err)
		}
	}
	// Work request success alone does not prove all requested capacity was allocated.
	// Verify the complete payload, including preserved configurations. No second
	// write occurs in this invocation, even if capacity remains short.
	for {
		actual, _, err := b.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("verification GET: %w", err)
		}
		comparison := compareReservationConfigs(plan.Configs, actual.InstanceReservationConfigs)
		if actual.LifecycleState == core.ComputeCapacityReservationLifecycleStateActive && comparison.matches() {
			fmt.Fprintf(out, "VERIFIED: %s scheduled target at %s\n", id, plan.At)
			return nil
		}
		if actual.LifecycleState != core.ComputeCapacityReservationLifecycleStateUpdating && actual.LifecycleState != core.ComputeCapacityReservationLifecycleStateActive {
			return fmt.Errorf("verification state=%s", actual.LifecycleState)
		}
		if actual.LifecycleState == core.ComputeCapacityReservationLifecycleStateActive && !comparison.matches() {
			return fmt.Errorf("requested capacity not fulfilled: %s; retry against current schedule on next tick", strings.Join(comparison.Mismatches, "; "))
		}
		if err = waitForNextReservationCheck(ctx, o.PollInterval); err != nil {
			return err
		}
	}
}
func sameScheduleHour(a, b time.Time) bool {
	return a.Format("2006-01-02T15Z07:00") == b.Format("2006-01-02T15Z07:00")
}
