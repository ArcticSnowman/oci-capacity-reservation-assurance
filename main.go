package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/workrequests"
)

const createdByTag = "oci-capacity-reservation-assurance"

type options struct {
	Shape              string
	OCPUs              float64
	MemoryGBs          float64
	CompartmentID      string
	Quantity           int64
	ConfigFile         string
	AvailabilityDomain string
	FaultDomain        string
	DisplayName        string
	PollInterval       time.Duration
	ReservationCheck   time.Duration
	Timeout            time.Duration
	DryRun             bool
	PreflightOnly      bool
	SkipPreflight      bool
	ResourceManagement string
	Configs            []core.InstanceReservationConfigDetails
	ConfigADs          []string
	Profile            string
	Update             bool
	ReservationID      string
}

type configComparison struct {
	RequestedTotal int64
	ActualTotal    int64
	Mismatches     []string
}

type availabilityDomainPlan struct {
	AvailabilityDomain string
	Options            options
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "schedule" {
		if err := scheduleMain(context.Background(), os.Args[2:], os.Stdout); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	if err := run(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (options, error) {
	var opts options
	fs := flag.NewFlagSet("reserve-capacity", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fs.StringVar(&opts.Shape, "instance-type", "", "OCI instance shape, for example VM.Standard.E4.Flex")
	fs.StringVar(&opts.Shape, "shape", "", "alias for --instance-type")
	fs.Float64Var(&opts.OCPUs, "ocpus", 0, "number of OCPUs per reserved instance")
	fs.Float64Var(&opts.OCPUs, "ocpu", 0, "alias for --ocpus")
	fs.Float64Var(&opts.MemoryGBs, "memory-gbs", 0, "memory in GB per reserved instance")
	fs.Float64Var(&opts.MemoryGBs, "memory", 0, "alias for --memory-gbs")
	fs.StringVar(&opts.CompartmentID, "compartment-id", "", "target compartment OCID")
	fs.StringVar(&opts.CompartmentID, "compartment", "", "alias for --compartment-id")
	fs.Int64Var(&opts.Quantity, "quantity", 0, "number of instances to reserve")
	fs.StringVar(&opts.ConfigFile, "config-file", "", "JSON file containing one or more instanceReservationConfigs")
	fs.StringVar(&opts.AvailabilityDomain, "availability-domain", "", "availability domain name, for example Uocm:US-ASHBURN-AD-1; use ALL to spread counts across all ADs in the DEFAULT profile region; defaults to the first AD in the tenancy")
	fs.StringVar(&opts.FaultDomain, "fault-domain", "", "optional fault domain, for example FAULT-DOMAIN-1")
	fs.StringVar(&opts.DisplayName, "display-name", "", "optional display name for the capacity reservation")
	fs.DurationVar(&opts.PollInterval, "poll-interval", 15*time.Second, "work request polling interval")
	fs.DurationVar(&opts.ReservationCheck, "reservation-check-interval", 30*time.Second, "how often to check reserved quantity after a work request succeeds")
	fs.DurationVar(&opts.Timeout, "timeout", 30*time.Minute, "maximum time to wait for the work request")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "print the create request body and exit without calling OCI")
	fs.BoolVar(&opts.PreflightOnly, "preflight-only", false, "check requested shapes against OCI and print the create request body without creating a reservation")
	fs.BoolVar(&opts.SkipPreflight, "skip-preflight", false, "skip the OCI shape availability preflight before creating the reservation")
	fs.StringVar(&opts.ResourceManagement, "resource-management", "", "optional internal shape resource management value: STATIC or DYNAMIC")
	fs.StringVar(&opts.Profile, "profile", "DEFAULT", "OCI config profile to use, defaults to DEFAULT")
	fs.BoolVar(&opts.Update, "update", false, "update an existing capacity reservation")
	fs.StringVar(&opts.ReservationID, "reservation-id", "", "optional OCID of the capacity reservation to manage")

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintln(fs.Output(), "Creates an OCI compute capacity reservation using ~/.oci/config DEFAULT profile, waits for the returned work request, and verifies the final reserved quantity.")
		fmt.Fprintln(fs.Output(), "\nRequired flags:")
		fmt.Fprintln(fs.Output(), "  --compartment-id plus either --config-file or all of --instance-type, --ocpus, --memory-gbs, --quantity")
		fmt.Fprintln(fs.Output(), "\nExample:")
		fmt.Fprintf(fs.Output(), "  %s --instance-type VM.Standard.E4.Flex --ocpus 4 --memory-gbs 64 --compartment-id ocid1.compartment.oc1..example --quantity 3 --availability-domain Uocm:US-ASHBURN-AD-1 --profile DEFAULT\n\n", os.Args[0])
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return opts, err
	}

	if strings.TrimSpace(opts.CompartmentID) == "" {
		return opts, errors.New("--compartment-id is required")
	}
	if opts.PollInterval <= 0 {
		return opts, errors.New("--poll-interval must be greater than 0")
	}
	if opts.ReservationCheck <= 0 {
		return opts, errors.New("--reservation-check-interval must be greater than 0")
	}
	if opts.Timeout <= 0 {
		return opts, errors.New("--timeout must be greater than 0")
	}
	opts.ResourceManagement = strings.ToUpper(strings.TrimSpace(opts.ResourceManagement))
	switch opts.ResourceManagement {
	case "", "STATIC", "DYNAMIC":
	default:
		return opts, errors.New("--resource-management must be STATIC or DYNAMIC when provided")
	}

	if strings.TrimSpace(opts.ConfigFile) != "" {
		configs, configADs, err := loadInstanceReservationConfigs(opts.ConfigFile)
		if err != nil {
			return opts, err
		}
		if singleConfigFlagsProvided(opts) {
			return opts, errors.New("--config-file cannot be combined with --instance-type, --ocpus, --memory-gbs, --quantity, --fault-domain, or --resource-management")
		}
		opts.Configs = configs
		opts.ConfigADs = configADs
	} else {
		config, err := buildSingleInstanceReservationConfig(opts)
		if err != nil {
			return opts, err
		}
		opts.Configs = []core.InstanceReservationConfigDetails{config}
	}

	if err := validateInstanceReservationConfigs(opts.Configs); err != nil {
		return opts, err
	}
	if err := validateConfigAvailabilityDomains(opts.Configs, opts.ConfigADs); err != nil {
		return opts, err
	}
	opts.Quantity = requestedConfigTotal(opts.Configs)

	if opts.DisplayName == "" {
		opts.DisplayName = defaultDisplayName(opts.Configs)
	}

	return opts, nil
}

func singleConfigFlagsProvided(opts options) bool {
	return strings.TrimSpace(opts.Shape) != "" ||
		opts.OCPUs != 0 ||
		opts.MemoryGBs != 0 ||
		opts.Quantity != 0 ||
		strings.TrimSpace(opts.FaultDomain) != "" ||
		strings.TrimSpace(opts.ResourceManagement) != ""
}

func run(parent context.Context, opts options) error {
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()

	provider := common.CustomProfileConfigProvider("", opts.Profile)

	region, err := provider.Region()
	if err != nil {
		return fmt.Errorf("read region from %s OCI profile: %w", opts.Profile, err)
	}

	var availabilityDomains []string
	if usesConfigAvailabilityDomains(opts) {
		if strings.TrimSpace(opts.AvailabilityDomain) != "" {
			fmt.Println("Config file availabilityDomain values found; using them instead of --availability-domain")
		}
		availabilityDomains = availabilityDomainsFromConfigDistribution(opts.ConfigADs)
	} else {
		var err error
		availabilityDomains, err = resolveAvailabilityDomains(ctx, provider, opts.AvailabilityDomain)
		if err != nil {
			return err
		}
	}
	for _, availabilityDomain := range availabilityDomains {
		if err := validateAvailabilityDomainRegion(region, availabilityDomain); err != nil {
			return err
		}
	}

	plans, err := buildAvailabilityDomainPlans(opts, availabilityDomains)
	if err != nil {
		return err
	}

	if opts.Update && opts.ReservationID == "" {
		return errors.New("--update requires --reservation-id to be specified")
	}
	if !opts.Update && opts.ReservationID != "" {
		return errors.New("--reservation-id can only be used with --update")
	}
	if opts.Update && len(plans) != 1 {
		return errors.New("--update requires exactly one plan to be specified")
	}

	if opts.DryRun {
		return printDryRunPayloads(plans)
	}

	computeClient, err := core.NewComputeClientWithConfigurationProvider(provider)
	if err != nil {
		return fmt.Errorf("create compute client: %w", err)
	}

	preflightRan := false
	if !opts.SkipPreflight {
		for _, plan := range plans {
			ran, err := preflightRequestedShapes(ctx, computeClient, plan.Options, plan.AvailabilityDomain)
			preflightRan = preflightRan || ran
			if err != nil {
				return err
			}
		}
	} else {
		fmt.Println("Shape preflight skipped")
	}
	if opts.PreflightOnly {
		if preflightRan {
			fmt.Println("Preflight passed; OCI create call was not sent.")
		} else if opts.SkipPreflight {
			fmt.Println("Preflight skipped; OCI create call was not sent.")
		} else {
			fmt.Println("Preflight could not be completed; OCI create call was not sent.")
		}
		return printCreatePayloads(plans)
	}

	workRequestClient, err := workrequests.NewWorkRequestClientWithConfigurationProvider(provider)
	if err != nil {
		return fmt.Errorf("create work request client: %w", err)
	}

	var verb string
	if opts.Update {
		verb = "Updating"
	} else {
		verb = "Creating"
	}

	for _, plan := range plans {
		availabilityDomain := plan.AvailabilityDomain
		planOpts := plan.Options

		fmt.Printf("%s capacity reservation %q in region %s, availability domain %s\n", verb, planOpts.DisplayName, region, availabilityDomain)
		var reservationID string

		if !opts.Update {
			createResp, err := createCapacityReservation(ctx, computeClient, planOpts, availabilityDomain)
			if err != nil {
				return createCapacityReservationError(err, planOpts, availabilityDomain)
			}

			reservationID = value(createResp.ComputeCapacityReservation.Id)
			workRequestID := value(createResp.OpcWorkRequestId)
			fmt.Printf("Create request accepted\n")
			if reservationID != "" {
				fmt.Printf("Reservation ID: %s\n", reservationID)
			}
			if workRequestID == "" {
				return errors.New("create response did not include an opc-work-request-id")
			}
			fmt.Printf("Work request ID: %s\n", workRequestID)

			workRequest, err := waitForWorkRequest(ctx, workRequestClient, workRequestID, planOpts.PollInterval)
			if err != nil {
				return err
			}

			if workRequest.Status != workrequests.WorkRequestStatusSucceeded {
				return workRequestFailure(ctx, workRequestClient, workRequestID, workRequest.Status)
			}

			if reservationID == "" {
				reservationID = reservationIDFromWorkRequest(workRequest)
			}
		} else {
			reservationID = opts.ReservationID
			if reservationID != "" {
				fmt.Printf("Reservation ID: %s\n", reservationID)
			}

		}

		if reservationID == "" {
			return errors.New("work request succeeded, but no compute capacity reservation OCID was found")
		}

		if err := ensureReservedQuantity(ctx, computeClient, workRequestClient, planOpts, reservationID); err != nil {
			return err
		}
	}

	return nil
}

func createCapacityReservation(ctx context.Context, client core.ComputeClient, opts options, availabilityDomain string) (core.CreateComputeCapacityReservationResponse, error) {
	return client.CreateComputeCapacityReservation(ctx, core.CreateComputeCapacityReservationRequest{
		OpcRetryToken:                           common.String(fmt.Sprintf("%s-%d", createdByTag, time.Now().UnixNano())),
		CreateComputeCapacityReservationDetails: buildCreateDetails(opts, availabilityDomain),
	})
}

func updateCapacityReservation(ctx context.Context, client core.ComputeClient, opts options, reservationID string, etag *string) (core.UpdateComputeCapacityReservationResponse, error) {
	return client.UpdateComputeCapacityReservation(ctx, core.UpdateComputeCapacityReservationRequest{
		CapacityReservationId: common.String(reservationID),
		IfMatch:               etag,
		UpdateComputeCapacityReservationDetails: core.UpdateComputeCapacityReservationDetails{
			InstanceReservationConfigs: opts.Configs,
		},
	})
}

func buildAvailabilityDomainPlans(opts options, availabilityDomains []string) ([]availabilityDomainPlan, error) {
	if len(availabilityDomains) == 0 {
		return nil, errors.New("no availability domains were resolved")
	}

	if usesConfigAvailabilityDomains(opts) {
		return buildAvailabilityDomainPlansFromConfigDistribution(opts, availabilityDomains)
	}

	if len(availabilityDomains) == 1 {
		return []availabilityDomainPlan{{
			AvailabilityDomain: availabilityDomains[0],
			Options:            opts,
		}}, nil
	}

	splitConfigs := splitConfigsAcrossAvailabilityDomains(opts.Configs, len(availabilityDomains))
	plans := make([]availabilityDomainPlan, 0, len(availabilityDomains))
	fmt.Printf("Spreading requested capacity across %d availability domains\n", len(availabilityDomains))
	for i, availabilityDomain := range availabilityDomains {
		if len(splitConfigs[i]) == 0 {
			fmt.Printf("Availability domain %s receives no reserved capacity; skipping create\n", availabilityDomain)
			continue
		}

		planOpts := opts
		planOpts.Configs = splitConfigs[i]
		planOpts.Quantity = requestedConfigTotal(splitConfigs[i])
		planOpts.DisplayName = displayNameForAvailabilityDomain(opts.DisplayName, availabilityDomain)
		fmt.Printf("Availability domain %s requested reservedCount total: %d\n", availabilityDomain, planOpts.Quantity)
		plans = append(plans, availabilityDomainPlan{
			AvailabilityDomain: availabilityDomain,
			Options:            planOpts,
		})
	}
	if len(plans) == 0 {
		return nil, errors.New("split produced no capacity reservation configs")
	}

	return plans, nil
}

func buildAvailabilityDomainPlansFromConfigDistribution(opts options, availabilityDomains []string) ([]availabilityDomainPlan, error) {
	configsByAD := make(map[string][]core.InstanceReservationConfigDetails, len(availabilityDomains))
	for i, config := range opts.Configs {
		availabilityDomain := opts.ConfigADs[i]
		configsByAD[availabilityDomain] = append(configsByAD[availabilityDomain], config)
	}

	plans := make([]availabilityDomainPlan, 0, len(availabilityDomains))
	fmt.Printf("Using availabilityDomain values from config file across %d availability domains\n", len(availabilityDomains))
	for _, availabilityDomain := range availabilityDomains {
		configs := configsByAD[availabilityDomain]
		if len(configs) == 0 {
			continue
		}

		planOpts := opts
		planOpts.Configs = configs
		planOpts.ConfigADs = nil
		planOpts.Quantity = requestedConfigTotal(configs)
		if len(availabilityDomains) > 1 {
			planOpts.DisplayName = displayNameForAvailabilityDomain(opts.DisplayName, availabilityDomain)
		}
		fmt.Printf("Availability domain %s requested reservedCount total: %d\n", availabilityDomain, planOpts.Quantity)
		plans = append(plans, availabilityDomainPlan{
			AvailabilityDomain: availabilityDomain,
			Options:            planOpts,
		})
	}
	if len(plans) == 0 {
		return nil, errors.New("config file availabilityDomain distribution produced no capacity reservation configs")
	}

	return plans, nil
}

func splitConfigsAcrossAvailabilityDomains(configs []core.InstanceReservationConfigDetails, availabilityDomainCount int) [][]core.InstanceReservationConfigDetails {
	splitConfigs := make([][]core.InstanceReservationConfigDetails, availabilityDomainCount)
	for _, config := range configs {
		total := int64Value(config.ReservedCount)
		base := total / int64(availabilityDomainCount)
		remainder := total % int64(availabilityDomainCount)

		for i := 0; i < availabilityDomainCount; i++ {
			count := base
			if int64(i) < remainder {
				count++
			}
			if count == 0 {
				continue
			}

			configCopy := config
			configCopy.ReservedCount = common.Int64(count)
			splitConfigs[i] = append(splitConfigs[i], configCopy)
		}
	}
	return splitConfigs
}

func displayNameForAvailabilityDomain(baseDisplayName string, availabilityDomain string) string {
	cleanAD := availabilityDomain
	if idx := strings.LastIndex(cleanAD, ":"); idx >= 0 && idx+1 < len(cleanAD) {
		cleanAD = cleanAD[idx+1:]
	}
	cleanAD = strings.NewReplacer(":", "-", ".", "-", "_", "-").Replace(cleanAD)
	return fmt.Sprintf("%s-%s", baseDisplayName, cleanAD)
}

func preflightRequestedShapes(ctx context.Context, client core.ComputeClient, opts options, availabilityDomain string) (bool, error) {
	available, err := listAvailableReservationShapes(ctx, client, opts.CompartmentID, availabilityDomain)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: unable to preflight capacity reservation shapes; continuing with create: %v\n", err)
		return false, nil
	}

	requested := requestedShapes(opts.Configs)
	var missing []string
	for shape := range requested {
		if !available[shape] {
			missing = append(missing, shape)
		}
	}
	if len(missing) == 0 {
		fmt.Printf("Shape preflight passed for %d requested shape(s)\n", len(requested))
		return true, nil
	}

	sort.Strings(missing)
	availableList := sortedStringKeys(available)
	return true, fmt.Errorf(
		"requested shape(s) are not available for compute capacity reservations in compartment %s, availability domain %s: %s. Available shapes: %s%s",
		opts.CompartmentID,
		availabilityDomain,
		strings.Join(missing, ", "),
		strings.Join(availableList, ", "),
		shapePreflightHint(opts),
	)
}

func createCapacityReservationError(err error, opts options, availabilityDomain string) error {
	serviceErr, ok := common.IsServiceError(err)
	if !ok || serviceErr.GetHTTPStatusCode() != 404 || !strings.EqualFold(serviceErr.GetCode(), "NotAuthorizedOrNotFound") {
		return fmt.Errorf("create capacity reservation: %w", err)
	}

	return fmt.Errorf(
		"create capacity reservation: %w\nrequested configs:\n%s\nThis 404 can mean the compartment OCID is wrong, the DEFAULT profile lacks permission in that compartment, the availability domain does not belong to the DEFAULT profile region, or one of the requested shapes is not reservable in that compartment/AD.%s Run with --preflight-only to check shapes without creating a reservation",
		err,
		describeRequestedConfigsForError(opts.Configs, availabilityDomain),
		compartmentOCIDHint(opts.CompartmentID),
	)
}

func shapePreflightHint(opts options) string {
	return compartmentOCIDHint(opts.CompartmentID) + " Use a shape from the available list, choose a compartment/AD where the shape appears, or pass --skip-preflight to send the create request anyway."
}

func compartmentOCIDHint(compartmentID string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(compartmentID)), "ocid1.tenancy.") {
		return " You passed a tenancy OCID, which targets the root compartment; if you intended a child compartment, use its ocid1.compartment... OCID."
	}
	return ""
}

func describeRequestedConfigsForError(configs []core.InstanceReservationConfigDetails, availabilityDomain string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  availabilityDomain=%s\n", availabilityDomain)
	for i, config := range configs {
		fmt.Fprintf(&b, "  [%d] %s reservedCount=%d\n", i, describeRequestedConfig(config), int64Value(config.ReservedCount))
	}
	return b.String()
}

func listAvailableReservationShapes(ctx context.Context, client core.ComputeClient, compartmentID string, availabilityDomain string) (map[string]bool, error) {
	available := make(map[string]bool)
	var page *string

	for {
		resp, err := client.ListComputeCapacityReservationInstanceShapes(ctx, core.ListComputeCapacityReservationInstanceShapesRequest{
			AvailabilityDomain: common.String(availabilityDomain),
			CompartmentId:      common.String(compartmentID),
			Limit:              common.Int(1000),
			Page:               page,
		})
		if err != nil {
			return nil, err
		}

		for _, item := range resp.Items {
			if item.InstanceShape != nil {
				available[*item.InstanceShape] = true
			}
		}

		if resp.OpcNextPage == nil {
			break
		}
		page = resp.OpcNextPage
	}

	if len(available) == 0 {
		return nil, errors.New("OCI returned no capacity reservation shapes for this compartment and availability domain")
	}
	return available, nil
}

func requestedShapes(configs []core.InstanceReservationConfigDetails) map[string]bool {
	shapes := make(map[string]bool)
	for _, config := range configs {
		if config.InstanceShape != nil {
			shapes[*config.InstanceShape] = true
		}
	}
	return shapes
}

func sortedStringKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func usesConfigAvailabilityDomains(opts options) bool {
	if len(opts.ConfigADs) == 0 {
		return false
	}
	for _, availabilityDomain := range opts.ConfigADs {
		if strings.TrimSpace(availabilityDomain) != "" {
			return true
		}
	}
	return false
}

func availabilityDomainsFromConfigDistribution(configADs []string) []string {
	seen := make(map[string]bool, len(configADs))
	availabilityDomains := make([]string, 0, len(configADs))
	for _, availabilityDomain := range configADs {
		if seen[availabilityDomain] {
			continue
		}
		seen[availabilityDomain] = true
		availabilityDomains = append(availabilityDomains, availabilityDomain)
	}
	return availabilityDomains
}

func loadInstanceReservationConfigs(path string) ([]core.InstanceReservationConfigDetails, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read --config-file %s: %w", path, err)
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, nil, fmt.Errorf("--config-file %s is empty", path)
	}

	var rawConfigs []json.RawMessage
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(data, &rawConfigs); err != nil {
			return nil, nil, fmt.Errorf("parse --config-file %s as instanceReservationConfigs array: %w", path, err)
		}
	} else {
		var envelope struct {
			InstanceReservationConfigs []json.RawMessage `json:"instanceReservationConfigs"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, nil, fmt.Errorf("parse --config-file %s: %w", path, err)
		}
		rawConfigs = envelope.InstanceReservationConfigs
	}

	configs := make([]core.InstanceReservationConfigDetails, 0, len(rawConfigs))
	configADs := make([]string, 0, len(rawConfigs))
	for i, rawConfig := range rawConfigs {
		var config core.InstanceReservationConfigDetails
		if err := json.Unmarshal(rawConfig, &config); err != nil {
			return nil, nil, fmt.Errorf("parse --config-file %s instanceReservationConfigs[%d]: %w", path, i, err)
		}
		var metadata struct {
			AvailabilityDomainDash  string `json:"availability-domain"`
			AvailabilityDomainSnake string `json:"availability_domain"`
			AvailabilityDomainCamel string `json:"availabilityDomain"`
		}
		if err := json.Unmarshal(rawConfig, &metadata); err != nil {
			return nil, nil, fmt.Errorf("parse --config-file %s instanceReservationConfigs[%d] metadata: %w", path, i, err)
		}
		configs = append(configs, config)
		configADs = append(configADs, firstNonEmpty(metadata.AvailabilityDomainCamel, metadata.AvailabilityDomainSnake, metadata.AvailabilityDomainDash))
	}

	return configs, configADs, nil
}

func buildCreateDetails(opts options, availabilityDomain string) core.CreateComputeCapacityReservationDetails {
	return core.CreateComputeCapacityReservationDetails{
		AvailabilityDomain: common.String(availabilityDomain),
		CompartmentId:      common.String(opts.CompartmentID),
		DisplayName:        common.String(opts.DisplayName),
		FreeformTags: map[string]string{
			"created-by": createdByTag,
		},
		InstanceReservationConfigs: opts.Configs,
		IsDefaultReservation:       common.Bool(false),
	}
}

func buildSingleInstanceReservationConfig(opts options) (core.InstanceReservationConfigDetails, error) {
	if strings.TrimSpace(opts.Shape) == "" {
		return core.InstanceReservationConfigDetails{}, errors.New("--instance-type is required when --config-file is not provided")
	}
	if opts.OCPUs <= 0 {
		return core.InstanceReservationConfigDetails{}, errors.New("--ocpus must be greater than 0 when --config-file is not provided")
	}
	if opts.MemoryGBs <= 0 {
		return core.InstanceReservationConfigDetails{}, errors.New("--memory-gbs must be greater than 0 when --config-file is not provided")
	}
	if opts.Quantity <= 0 {
		return core.InstanceReservationConfigDetails{}, errors.New("--quantity must be greater than 0 when --config-file is not provided")
	}

	config := core.InstanceReservationConfigDetails{
		InstanceShape: common.String(opts.Shape),
		InstanceShapeConfig: &core.InstanceReservationShapeConfigDetails{
			Ocpus:       common.Float32(float32(opts.OCPUs)),
			MemoryInGBs: common.Float32(float32(opts.MemoryGBs)),
		},
		ReservedCount: common.Int64(opts.Quantity),
	}
	if opts.ResourceManagement != "" {
		config.InstanceShapeConfig.ResourceManagement = core.InstanceReservationShapeConfigDetailsResourceManagementEnum(opts.ResourceManagement)
	}
	if opts.FaultDomain != "" {
		config.FaultDomain = common.String(opts.FaultDomain)
	}

	return config, nil
}

func validateInstanceReservationConfigs(configs []core.InstanceReservationConfigDetails) error {
	if len(configs) == 0 {
		return errors.New("at least one instance reservation config is required")
	}

	for i, config := range configs {
		prefix := fmt.Sprintf("instanceReservationConfigs[%d]", i)
		if strings.TrimSpace(value(config.InstanceShape)) == "" {
			return fmt.Errorf("%s.instanceShape is required", prefix)
		}
		if config.ReservedCount == nil || *config.ReservedCount <= 0 {
			return fmt.Errorf("%s.reservedCount must be greater than 0", prefix)
		}
		if config.InstanceShapeConfig != nil {
			if config.InstanceShapeConfig.Ocpus != nil && *config.InstanceShapeConfig.Ocpus <= 0 {
				return fmt.Errorf("%s.instanceShapeConfig.ocpus must be greater than 0 when provided", prefix)
			}
			if config.InstanceShapeConfig.MemoryInGBs != nil && *config.InstanceShapeConfig.MemoryInGBs <= 0 {
				return fmt.Errorf("%s.instanceShapeConfig.memoryInGBs must be greater than 0 when provided", prefix)
			}
			switch config.InstanceShapeConfig.ResourceManagement {
			case "", core.InstanceReservationShapeConfigDetailsResourceManagementStatic, core.InstanceReservationShapeConfigDetailsResourceManagementDynamic:
			default:
				return fmt.Errorf("%s.instanceShapeConfig.resourceManagement must be STATIC or DYNAMIC when provided", prefix)
			}
		}
	}

	return nil
}

func validateConfigAvailabilityDomains(configs []core.InstanceReservationConfigDetails, configADs []string) error {
	if len(configADs) == 0 {
		return nil
	}
	if len(configADs) != len(configs) {
		return fmt.Errorf("internal error: config availability domain count %d does not match config count %d", len(configADs), len(configs))
	}

	anyProvided := false
	allProvided := true
	for i, availabilityDomain := range configADs {
		availabilityDomain = strings.TrimSpace(availabilityDomain)
		configADs[i] = availabilityDomain
		if availabilityDomain == "" {
			allProvided = false
			continue
		}
		anyProvided = true
		if strings.EqualFold(availabilityDomain, "ALL") {
			return fmt.Errorf("instanceReservationConfigs[%d].availabilityDomain must be an actual availability domain name, not ALL", i)
		}
	}
	if anyProvided && !allProvided {
		return errors.New("when any config-file entry includes availabilityDomain, every instanceReservationConfigs entry must include availabilityDomain")
	}
	return nil
}

func printDryRunPayloads(plans []availabilityDomainPlan) error {
	fmt.Println("Dry run enabled; OCI create call was not sent.")
	return printCreatePayloads(plans)
}

func printCreatePayloads(plans []availabilityDomainPlan) error {
	for i, plan := range plans {
		if len(plans) > 1 {
			fmt.Printf("Create payload %d/%d for availability domain %s:\n", i+1, len(plans), plan.AvailabilityDomain)
		}
		if err := printCreatePayload(plan.Options, plan.AvailabilityDomain); err != nil {
			return err
		}
	}
	return nil
}

func printCreatePayload(opts options, availabilityDomain string) error {
	req := core.CreateComputeCapacityReservationRequest{
		CreateComputeCapacityReservationDetails: buildCreateDetails(opts, availabilityDomain),
	}
	httpReq, err := req.HTTPRequest("POST", "/20160918/computeCapacityReservations", nil, nil)
	if err != nil {
		return fmt.Errorf("build dry-run request: %w", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		return fmt.Errorf("read dry-run request body: %w", err)
	}
	fmt.Println(string(body))
	return nil
}

func resolveAvailabilityDomains(ctx context.Context, provider common.ConfigurationProvider, explicitAD string) ([]string, error) {
	explicitAD = strings.TrimSpace(explicitAD)
	switch {
	case strings.EqualFold(explicitAD, "ALL"):
		availabilityDomains, err := listAvailabilityDomainNames(ctx, provider)
		if err != nil {
			return nil, err
		}
		fmt.Printf("--availability-domain ALL requested; using %d availability domains: %s\n", len(availabilityDomains), strings.Join(availabilityDomains, ", "))
		return availabilityDomains, nil
	case explicitAD != "":
		return []string{explicitAD}, nil
	}

	availabilityDomains, err := listAvailabilityDomainNames(ctx, provider)
	if err != nil {
		return nil, err
	}
	ad := availabilityDomains[0]
	fmt.Printf("No --availability-domain provided; using first tenancy AD: %s\n", ad)
	return []string{ad}, nil
}

func listAvailabilityDomainNames(ctx context.Context, provider common.ConfigurationProvider) ([]string, error) {
	tenancyID, err := provider.TenancyOCID()
	if err != nil {
		return nil, fmt.Errorf("read tenancy OCID from DEFAULT OCI profile while resolving availability domain: %w", err)
	}

	client, err := identity.NewIdentityClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("create identity client while resolving availability domain: %w", err)
	}

	resp, err := client.ListAvailabilityDomains(ctx, identity.ListAvailabilityDomainsRequest{
		CompartmentId: common.String(tenancyID),
	})
	if err != nil {
		return nil, fmt.Errorf("list availability domains for tenancy %s: %w", tenancyID, err)
	}
	var availabilityDomains []string
	for _, item := range resp.Items {
		if item.Name != nil && strings.TrimSpace(*item.Name) != "" {
			availabilityDomains = append(availabilityDomains, *item.Name)
		}
	}
	if len(availabilityDomains) == 0 {
		return nil, errors.New("no availability domains were returned; provide --availability-domain explicitly")
	}
	return availabilityDomains, nil
}

func validateAvailabilityDomainRegion(region string, availabilityDomain string) error {
	regionMarkers := availabilityDomainRegionMarkers(region)
	if len(regionMarkers) == 0 || availabilityDomain == "" {
		return nil
	}

	adMarker := strings.ToUpper(availabilityDomain)
	for _, regionMarker := range regionMarkers {
		if strings.Contains(adMarker, regionMarker+"-AD-") {
			return nil
		}
	}

	return fmt.Errorf("availability domain %q does not appear to belong to DEFAULT profile region %q; update ~/.oci/config or pass an AD for %s", availabilityDomain, region, strings.Join(regionMarkers, " or "))
}

func availabilityDomainRegionMarkers(region string) []string {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		return nil
	}

	longMarker := strings.ToUpper(region)
	if lastDash := strings.LastIndex(longMarker, "-"); lastDash > 0 {
		longMarker = longMarker[:lastDash]
	}

	markers := []string{longMarker}
	shortMarkers := map[string][]string{
		"us-phoenix-1": []string{"PHX"},
		"us-ashburn-1": []string{"IAD", "US-ASHBURN"},
	}
	for _, marker := range shortMarkers[region] {
		if marker != longMarker {
			markers = append(markers, marker)
		}
	}
	return markers
}

func waitForWorkRequest(ctx context.Context, client workrequests.WorkRequestClient, workRequestID string, pollInterval time.Duration) (workrequests.WorkRequest, error) {
	for {
		resp, err := client.GetWorkRequest(ctx, workrequests.GetWorkRequestRequest{
			WorkRequestId: common.String(workRequestID),
		})
		if err != nil {
			return workrequests.WorkRequest{}, fmt.Errorf("get work request %s: %w", workRequestID, err)
		}

		fmt.Printf("Work request status: %s", resp.Status)
		if resp.PercentComplete != nil {
			fmt.Printf(" (%.1f%%)", *resp.PercentComplete)
		}
		fmt.Println()

		switch resp.Status {
		case workrequests.WorkRequestStatusSucceeded, workrequests.WorkRequestStatusFailed, workrequests.WorkRequestStatusCanceled:
			return resp.WorkRequest, nil
		}

		select {
		case <-ctx.Done():
			return workrequests.WorkRequest{}, fmt.Errorf("timed out waiting for work request %s: %w", workRequestID, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

func workRequestFailure(ctx context.Context, client workrequests.WorkRequestClient, workRequestID string, status workrequests.WorkRequestStatusEnum) error {
	resp, err := client.ListWorkRequestErrors(ctx, workrequests.ListWorkRequestErrorsRequest{
		WorkRequestId: common.String(workRequestID),
	})
	if err != nil {
		return fmt.Errorf("work request %s ended with status %s; also failed to list work request errors: %w", workRequestID, status, err)
	}
	if len(resp.Items) == 0 {
		return fmt.Errorf("work request %s ended with status %s", workRequestID, status)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "work request %s ended with status %s:", workRequestID, status)
	for _, item := range resp.Items {
		fmt.Fprintf(&b, "\n- %s: %s", value(item.Code), value(item.Message))
	}
	return errors.New(b.String())
}

func reservationIDFromWorkRequest(workRequest workrequests.WorkRequest) string {
	for _, resource := range workRequest.Resources {
		identifier := value(resource.Identifier)
		entityType := strings.ToLower(value(resource.EntityType))
		if strings.Contains(strings.ToLower(identifier), "capacityreservation") || strings.Contains(entityType, "capacityreservation") {
			return identifier
		}
	}
	return ""
}

func ensureReservedQuantity(ctx context.Context, computeClient core.ComputeClient, workRequestClient workrequests.WorkRequestClient, opts options, reservationID string) error {
	for {
		resp, err := computeClient.GetComputeCapacityReservation(ctx, core.GetComputeCapacityReservationRequest{
			CapacityReservationId: common.String(reservationID),
		})
		if err != nil {
			return fmt.Errorf("get capacity reservation %s: %w", reservationID, err)
		}

		reservation := resp.ComputeCapacityReservation
		comparison := compareReservationConfigs(opts.Configs, reservation.InstanceReservationConfigs)
		fmt.Printf("Reservation check: lifecycle=%s requested=%d reservedCount=%d\n", reservation.LifecycleState, comparison.RequestedTotal, comparison.ActualTotal)

		if comparison.matches() {
			return validateReservedQuantity(opts, reservation)
		}
		printConfigMismatches(comparison)

		switch reservation.LifecycleState {
		case core.ComputeCapacityReservationLifecycleStateDeleted, core.ComputeCapacityReservationLifecycleStateDeleting:
			return fmt.Errorf("reservation %s is %s before requested quantity was reserved", reservationID, reservation.LifecycleState)
		case core.ComputeCapacityReservationLifecycleStateCreating, core.ComputeCapacityReservationLifecycleStateUpdating, core.ComputeCapacityReservationLifecycleStateMoving:
			fmt.Printf("Reservation is still %s; checking again in %s\n", reservation.LifecycleState, opts.ReservationCheck)
			if err := waitForNextReservationCheck(ctx, opts.ReservationCheck); err != nil {
				return err
			}
			continue
		}

		fmt.Printf("Reserved configuration does not match request; updating reservation to requested configs\n")
		updateResp, err := updateCapacityReservation(ctx, computeClient, opts, reservationID, resp.Etag)
		if err != nil {
			if reason, retry := retryableReservationUpdateError(err); retry {
				fmt.Printf("Update is not ready yet: %s; checking again in %s\n", reason, opts.ReservationCheck)
				if err := waitForNextReservationCheck(ctx, opts.ReservationCheck); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("update capacity reservation %s to requested configs: %w", reservationID, err)
		}

		updateWorkRequestID := value(updateResp.OpcWorkRequestId)
		if updateWorkRequestID == "" {
			fmt.Printf("Update accepted without a work request ID; checking again in %s\n", opts.ReservationCheck)
			if err := waitForNextReservationCheck(ctx, opts.ReservationCheck); err != nil {
				return err
			}
			continue
		}

		fmt.Printf("Update work request ID: %s\n", updateWorkRequestID)
		workRequest, err := waitForWorkRequest(ctx, workRequestClient, updateWorkRequestID, opts.PollInterval)
		if err != nil {
			return err
		}
		if workRequest.Status != workrequests.WorkRequestStatusSucceeded {
			return workRequestFailure(ctx, workRequestClient, updateWorkRequestID, workRequest.Status)
		}

		fmt.Printf("Update work request succeeded; checking reserved quantity again in %s\n", opts.ReservationCheck)
		if err := waitForNextReservationCheck(ctx, opts.ReservationCheck); err != nil {
			return err
		}
	}
}

func (comparison configComparison) matches() bool {
	return len(comparison.Mismatches) == 0
}

func printConfigMismatches(comparison configComparison) {
	for _, mismatch := range comparison.Mismatches {
		fmt.Printf("Config mismatch: %s\n", mismatch)
	}
}

func retryableReservationUpdateError(err error) (string, bool) {
	serviceErr, ok := common.IsServiceError(err)
	if !ok {
		return "", false
	}

	code := serviceErr.GetCode()
	status := serviceErr.GetHTTPStatusCode()
	if status != 409 && !strings.EqualFold(code, "IncorrectState") && !strings.EqualFold(code, "Conflict") {
		return "", false
	}

	return fmt.Sprintf("%s (%d): %s", code, status, serviceErr.GetMessage()), true
}

func waitForNextReservationCheck(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for reservation quantity to match request: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func validateReservedQuantity(opts options, reservation core.ComputeCapacityReservation) error {
	comparison := compareReservationConfigs(opts.Configs, reservation.InstanceReservationConfigs)
	reservationID := value(reservation.Id)

	fmt.Printf("Reservation completed: %s\n", reservationID)
	fmt.Printf("Lifecycle state: %s\n", reservation.LifecycleState)
	fmt.Printf("Requested quantity total: %d\n", comparison.RequestedTotal)
	fmt.Printf("Reserved count total: %d\n", comparison.ActualTotal)

	for i, config := range reservation.InstanceReservationConfigs {
		fmt.Printf("Config %d: shape=%s reservedCount=%d", i+1, value(config.InstanceShape), int64Value(config.ReservedCount))
		if config.InstanceShapeConfig != nil {
			fmt.Printf(" ocpus=%.2f memoryGBs=%.2f", float32Value(config.InstanceShapeConfig.Ocpus), float32Value(config.InstanceShapeConfig.MemoryInGBs))
		}
		if config.FaultDomain != nil {
			fmt.Printf(" faultDomain=%s", *config.FaultDomain)
		}
		fmt.Println()
	}

	if !comparison.matches() {
		printConfigMismatches(comparison)
		return fmt.Errorf("reservation quantity mismatch for %s: requested %d, OCI reserved %d; the reservation remains in OCI, so review or delete it if this partial reservation is not desired", reservationID, comparison.RequestedTotal, comparison.ActualTotal)
	}

	fmt.Println("Validation passed: reservedCount values match requested configs")
	return nil
}

func requestedConfigTotal(configs []core.InstanceReservationConfigDetails) int64 {
	var total int64
	for _, config := range configs {
		if config.ReservedCount != nil {
			total += *config.ReservedCount
		}
	}
	return total
}

func actualConfigTotal(configs []core.InstanceReservationConfig) int64 {
	var total int64
	for _, config := range configs {
		if config.ReservedCount != nil {
			total += *config.ReservedCount
		}
	}
	return total
}

func compareReservationConfigs(requested []core.InstanceReservationConfigDetails, actual []core.InstanceReservationConfig) configComparison {
	requestedCounts, requestedLabels := requestedConfigCounts(requested)
	actualCounts, actualLabels := actualConfigCounts(actual)

	comparison := configComparison{
		RequestedTotal: requestedConfigTotal(requested),
		ActualTotal:    actualConfigTotal(actual),
	}

	for key, requestedCount := range requestedCounts {
		actualCount := actualCounts[key]
		if actualCount != requestedCount {
			comparison.Mismatches = append(comparison.Mismatches, fmt.Sprintf("%s requested reservedCount=%d actual reservedCount=%d", requestedLabels[key], requestedCount, actualCount))
		}
	}

	for key, actualCount := range actualCounts {
		if _, ok := requestedCounts[key]; !ok && actualCount != 0 {
			comparison.Mismatches = append(comparison.Mismatches, fmt.Sprintf("unexpected %s actual reservedCount=%d", actualLabels[key], actualCount))
		}
	}

	return comparison
}

func requestedConfigCounts(configs []core.InstanceReservationConfigDetails) (map[string]int64, map[string]string) {
	counts := make(map[string]int64, len(configs))
	labels := make(map[string]string, len(configs))
	for _, config := range configs {
		key := requestedConfigKey(config)
		counts[key] += int64Value(config.ReservedCount)
		labels[key] = describeRequestedConfig(config)
	}
	return counts, labels
}

func actualConfigCounts(configs []core.InstanceReservationConfig) (map[string]int64, map[string]string) {
	counts := make(map[string]int64, len(configs))
	labels := make(map[string]string, len(configs))
	for _, config := range configs {
		key := actualConfigKey(config)
		counts[key] += int64Value(config.ReservedCount)
		labels[key] = describeActualConfig(config)
	}
	return counts, labels
}

func requestedConfigKey(config core.InstanceReservationConfigDetails) string {
	shapeConfig := config.InstanceShapeConfig
	return strings.Join([]string{
		value(config.InstanceShape),
		float32PointerKey(shapeConfigOCPUs(shapeConfig)),
		float32PointerKey(shapeConfigMemory(shapeConfig)),
		value(config.FaultDomain),
		value(config.ClusterPlacementGroupId),
		clusterConfigKey(config.ClusterConfig),
	}, "|")
}

func actualConfigKey(config core.InstanceReservationConfig) string {
	shapeConfig := config.InstanceShapeConfig
	return strings.Join([]string{
		value(config.InstanceShape),
		float32PointerKey(shapeConfigOCPUs(shapeConfig)),
		float32PointerKey(shapeConfigMemory(shapeConfig)),
		value(config.FaultDomain),
		value(config.ClusterPlacementGroupId),
		clusterConfigKey(config.ClusterConfig),
	}, "|")
}

func describeRequestedConfig(config core.InstanceReservationConfigDetails) string {
	return describeConfig(value(config.InstanceShape), config.InstanceShapeConfig, value(config.FaultDomain), value(config.ClusterPlacementGroupId))
}

func describeActualConfig(config core.InstanceReservationConfig) string {
	return describeConfig(value(config.InstanceShape), config.InstanceShapeConfig, value(config.FaultDomain), value(config.ClusterPlacementGroupId))
}

func describeConfig(shape string, shapeConfig *core.InstanceReservationShapeConfigDetails, faultDomain string, clusterPlacementGroupID string) string {
	parts := []string{fmt.Sprintf("shape=%s", shape)}
	if shapeConfig != nil {
		if shapeConfig.Ocpus != nil {
			parts = append(parts, fmt.Sprintf("ocpus=%s", float32PointerKey(shapeConfig.Ocpus)))
		}
		if shapeConfig.MemoryInGBs != nil {
			parts = append(parts, fmt.Sprintf("memoryInGBs=%s", float32PointerKey(shapeConfig.MemoryInGBs)))
		}
	}
	if faultDomain != "" {
		parts = append(parts, fmt.Sprintf("faultDomain=%s", faultDomain))
	}
	if clusterPlacementGroupID != "" {
		parts = append(parts, fmt.Sprintf("clusterPlacementGroupId=%s", clusterPlacementGroupID))
	}
	return strings.Join(parts, " ")
}

func shapeConfigOCPUs(config *core.InstanceReservationShapeConfigDetails) *float32 {
	if config == nil {
		return nil
	}
	return config.Ocpus
}

func shapeConfigMemory(config *core.InstanceReservationShapeConfigDetails) *float32 {
	if config == nil {
		return nil
	}
	return config.MemoryInGBs
}

func float32PointerKey(ptr *float32) string {
	if ptr == nil {
		return ""
	}
	return fmt.Sprintf("%g", *ptr)
}

func clusterConfigKey(config *core.ClusterConfigDetails) string {
	if config == nil {
		return ""
	}
	body, err := json.Marshal(config)
	if err != nil {
		return fmt.Sprintf("%v", config)
	}
	return string(body)
}

func defaultDisplayName(configs []core.InstanceReservationConfigDetails) string {
	name := "multi"
	if len(configs) == 1 {
		name = value(configs[0].InstanceShape)
	}
	clean := strings.NewReplacer(".", "-", "_", "-").Replace(name)
	return fmt.Sprintf("cap-res-%s-%s", clean, time.Now().UTC().Format("20060102-150405"))
}

func value(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return *ptr
}

func int64Value(ptr *int64) int64 {
	if ptr == nil {
		return 0
	}
	return *ptr
}

func float32Value(ptr *float32) float32 {
	if ptr == nil {
		return 0
	}
	return *ptr
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
