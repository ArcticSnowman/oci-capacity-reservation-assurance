# Scheduled OCI capacity reservations

The scheduler sets absolute reserved instance counts on existing reservations.
It uses separate CSVs per shape, repeats a weekly schedule, and applies dated
holiday or demand-spike overrides. It never launches or changes instances.
The original reservation creation commands still work.

## Prepare your files

Put files in `schedules/`, named `weekly-<name>.csv` or `override-<name>.csv`.
The name after the prefix is your choice; the `shape` column determines the shape.
Only files with these prefixes and the `.csv` extension are discovered.

Copy a matching example from `schedule-examples/`. Those files contain synthetic
counts and an example OCID for offline previews. Replace the OCID, per-VM sizes,
counts, and override dates with your values before using OCI.

Weekly header and illustrative rows:

```csv
capacity reservation OCID,weekday,hour,shape,ocpus,memory_gbs,count
ocid1.capacityreservation.oc1.us-ashburn-1.example,Monday,0,VM.Standard.E5.Flex,1,4,10
ocid1.capacityreservation.oc1.us-ashburn-1.example,Monday,1,VM.Standard.E5.Flex,1,4,5
```

Overrides:

```csv
capacity reservation OCID,date,hour,shape,ocpus,memory_gbs,count
ocid1.capacityreservation.oc1.us-ashburn-1.example,2026-12-25,,VM.Standard.E5.Flex,1,4,0
ocid1.capacityreservation.oc1.us-ashburn-1.example,2026-12-25,12,VM.Standard.E5.Flex,1,4,25
```

- Each file contains exactly one shape, but can contain multiple reservation OCIDs
  and VM sizes. Multiple files for the same shape are allowed.
- Weekly schedules require all 168 weekday/hour combinations for each reservation
  and shape, across the selected files. The complete example files provide them.
  Use full or abbreviated English weekday names, e.g. `Monday` or `Mon`.
- `hour` is an integer from 0 through 23. Only overrides permit a blank hour,
  which applies to the whole day. Dates must be `YYYY-MM-DD`.
- `ocpus` and `memory_gbs` are positive values **per VM**; `count` is the absolute
  nonnegative integer target, including zero. No historical importer or total
  demand conversion is performed. OCI validates supported sizes for the shape.
- A slot can have multiple rows for different VM sizes. Duplicate sizes in the
  same slot are rejected, even across separate files.
- Dated hourly overrides replace the full set of scheduled sizes for the matching
  reservation and shape at that hour. They take precedence over whole-day
  overrides, which take precedence over the weekly schedule. Other shapes and
  reservations retain their own schedules. The weekly schedule resumes after
  the override expires.
- Every size appearing anywhere in the selected schedule files is managed for its
  reservation/shape. Sizes absent from the active slot target zero. Existing
  configurations of other sizes or unselected shapes are preserved.
- Removing a size/file from the selected schedules relinquishes management; it
  does not automatically release that capacity. First schedule its count to zero
  and apply that target before removing it if you want to release capacity.
- Excel-exported UTF-8 BOMs, CRLF, and fully empty trailing rows are supported.
  Save each sheet as its own CSV; do not upload `.xlsx` files to this scheduler.

## Validate and preview without OCI

Run commands from the repository directory with Go 1.24 or later:

```bash
go run . schedule validate --schedule-dir schedules --timezone America/Detroit

go run . schedule preview --schedule-dir schedules --timezone America/Detroit \
  --from 2026-12-25 --hours 24
```

To try the included examples immediately, use `--schedule-dir schedule-examples`.
Validation and preview do not read OCI credentials or call OCI. Preview outputs
CSV with timestamps, resolved counts, total CPU/memory, and the source row. It
also shows inactive managed sizes with count zero.

Select shapes granularly:

```bash
go run . schedule preview --schedule-dir schedules --timezone America/Detroit \
  --shape VM.Standard.E5.Flex --from 2026-12-25 --hours 24
```

Repeat `--shape` to select multiple shapes. Directory candidates are parsed to
identify and validate their shape, then selected shapes are combined. Use explicit
files to exclude all other files from parsing:

```bash
go run . schedule validate --timezone America/Detroit \
  --file schedules/weekly-e5.csv --file schedules/override-e5.csv
```

When `--file` is used, directory discovery is disabled; include both the weekly
and applicable override files. Overrides need a complete weekly fallback.

Set the timezone to match the schedule's business hours. Preview advances by
elapsed hours: during spring daylight-saving changes the skipped local hour is
not replayed, and during fall changes the repeated hour uses the same target in
both occurrences. Output includes UTC offsets to distinguish repeated hours.
Use `--from YYYY-MM-DD` for local midnight, or an RFC3339 timestamp with an
explicit offset. `--at` selects a single instant for a one-hour preview when
combined with `--hours 1`.

## Preview the actual OCI update

With real reservation OCIDs and working OCI credentials:

```bash
go run . schedule apply --schedule-dir schedules --timezone America/Detroit \
  --region us-ashburn-1 --dry-run
```

This reads existing reservations and prints full proposed update payloads,
including preserved configurations, and changes with before/target/used counts.
It does not write to OCI. Add `--at 2026-12-25T12:00:00-05:00` to dry-run a future
override. A live apply rejects `--at` and always resolves current local time.

Authentication defaults to `~/.oci/config`, profile `DEFAULT`. Use `--profile`
and `--oci-config-file` for alternatives, or `--auth instance_principal` on an
OCI VM with the required IAM permissions. The region comes from the provider
unless overridden. Select reservations in one region per invocation.

The CSV selects configurations by shape, OCPU, and memory. Matching placement
settings are preserved. If multiple existing configurations match that size,
the scheduler stops rather than choosing arbitrarily. Use `--fault-domain
FAULT-DOMAIN-1` (or `ANY` for no fault domain) and/or
`--cluster-placement-group-id <OCID>` to disambiguate. These selectors also set
placement for newly added sizes. Different placements can use separate
invocations with explicit files and matching selectors.

OCI does not allow changing CPU/memory in an existing flexible-shape configuration.
When an active scheduled size does not exist, the scheduler adds a configuration
inside the same existing reservation. It does not create a new reservation.
Inactive absent sizes are not added. Updates containing more than 50
configurations are rejected locally.

## Apply and schedule

After reviewing your preview and dry-run:

```bash
go run . schedule apply --schedule-dir schedules --timezone America/Detroit \
  --region us-ashburn-1
```

Each run merges all selected shapes into one update per reservation, preserving
other configurations. It intentionally submits targets below `used-count`,
including zero; no client-side usage floor is imposed. An OCI rejection is
reported, not silently changed into a larger target. Oracle's Console guide
currently describes a floor at the number of instances in the configuration;
API acceptance below usage must be confirmed with a live reservation test.
See [Oracle's editing guide](https://docs.oracle.com/en-us/iaas/Content/Compute/Tasks/capacity-reservations-configuration-edit_tabs.htm).

No live API writes were made during development. Below-usage updates and
preservation of instances are represented by simulated API responses in tests.
The scheduler has no instance lifecycle API calls.

On a Linux VM, build/install the executable and CSVs before scheduling. For a
local Linux build:

```bash
go build -o /tmp/reserve-capacity .
install -m 0755 /tmp/reserve-capacity /home/opc/bin/reserve-capacity
```

When building on macOS for an x86 Linux VM, use `CGO_ENABLED=0 GOOS=linux
GOARCH=amd64 go build -o /tmp/reserve-capacity-linux .` and copy the result to the
VM. Use `GOARCH=arm64` for an Arm VM.

Example `opc` crontab, checking every minute:

```cron
* * * * * /home/opc/bin/reserve-capacity schedule apply --schedule-dir /home/opc/schedules --timezone America/Detroit --auth instance_principal --region us-ashburn-1 --timeout 50s >> /home/opc/capacity-scheduler.log 2>&1
```

This reconciles the current hourly target; unchanged counts produce no update.
Choose a timeout suited to work-request duration. A timeout can leave an update
in progress; later ticks read the current state and reconcile when it is ACTIVE.
No cron entries or services are installed automatically.

Local locking uses `~/.oci-capacity-grow` (override with `--lock-dir`) and the same
reservation key as `grow_capacity_reservation.py`. Use the same account and lock
directory for local runs. ETags protect writes against stale reads, including
other machines. A busy reservation, conflict, capacity shortage, or failed work
request produces a nonzero exit; the next timer tick retries against the current
schedule. An ambiguous write is never repeated in the same invocation.

Work-request success is followed by a fresh GET to verify actual capacity.
Verification checks all requested and preserved configurations. Each reservation
is processed independently; one failure is reported while remaining reservations
are attempted within the total `--timeout` budget. Success exits 0; errors exit 1.

Make the scheduler the writer for the configurations it manages. Disable the
incrementing grow cron for those configurations, and coordinate Terraform so
an apply does not restore counts over the schedule.

## Development checks

```bash
go test ./...
go vet ./...
```

Tests cover parsing, complete weekly coverage, duplicates, selection, overrides,
size transitions, below-usage/zero targets, preserving unrelated configurations,
ETags, shortages, work-request failures, ambiguous timeouts, and daylight saving.
