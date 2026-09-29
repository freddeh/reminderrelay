# ReminderRelay

Bidirectional sync daemon that keeps **Apple Reminders** and **Home Assistant** todo lists in sync — automatically, in the background, on macOS.

```
Apple Reminders  ←──────────────────→  Home Assistant
   (EventKit)          ReminderRelay      (REST + WebSocket)
```

## Features

- **Bidirectional sync** — changes made in either app appear in the other within seconds.
- **Field-level 3-way merge** — title, notes, due date, priority, and completed status are each attributed independently to whichever side changed them; an unrelated change on the other side is never silently discarded. See [Conflict Resolution](#conflict-resolution).
- **Real-time HA updates** — WebSocket subscription for instant propagation from HA → Reminders.
- **Polling for Reminders changes** — configurable 10 s – 5 m interval (default 30 s).
- **Priority mapping** — Apple Reminders priorities are encoded as `[High]`, `[Medium]`, `[Low]` prefixes in HA descriptions.
- **Due-date fidelity** — a due time-of-day is sent as `due_datetime` when the target HA entity supports it, instead of always truncating to a date. See [Feature Storage Reference](#feature-storage-reference).
- **Optional calendar mirroring** — due-dated items can also be mirrored onto a Home Assistant calendar entity for a real due-date UI and calendar-triggered automations. See [Calendar Mirroring](#calendar-mirroring).
- **First-run bootstrap** — interactive wizard that matches existing items between both sides by title and prompts before writing anything.
- **Persistent state database** — SQLite tracks sync metadata so resuming after a restart is safe.

## Prerequisites

| Requirement | Version |
|---|---|
| macOS | 13 Ventura or later |
| Apple ID / iCloud | Signed in with Reminders enabled |
| Home Assistant | ≥ 2023.11 (Todo integration required) |
| HA long-lived access token | Profile → Security → Long-Lived Access Tokens |

## Quick Start

### 1. Install devbox (once)

```bash
curl -fsSL https://get.jetify.com/devbox | bash
```

### 2. Clone and enter the dev shell

```bash
git clone https://github.com/njoerd114/reminderrelay.git
cd reminderrelay
devbox shell
```

### 3. Run the setup wizard

```bash
just build
reminderrelay setup
```

The wizard will walk you through:
1. Optionally resetting the sync state database, if a previous install left one behind (declined by default — see below)
2. Connecting to your Home Assistant instance
3. Discovering Reminders lists and HA todo entities
4. Mapping lists to entities interactively
5. Writing the config file
6. Optionally installing as a background daemon

On first sync you will be prompted to review and confirm bootstrap matches — nothing is written until you type **y**.

<details>
<summary>Manual config (alternative to wizard)</summary>

```bash
mkdir -p ~/.config/reminderrelay
cp config.example.yaml ~/.config/reminderrelay/config.yaml
$EDITOR ~/.config/reminderrelay/config.yaml
```

Key fields:

```yaml
ha_url: "http://homeassistant.local:8123"
ha_token: "your-long-lived-access-token-here"
poll_interval: 30s
list_mappings:
  "Shopping": "todo.shopping"
  "Work":     "todo.work_tasks"
  # "Personal":
  #   ha_entity: "todo.personal"
  #   ha_calendar_entity: "calendar.personal_due_dates"  # optional — see Calendar Mirroring
```

Then test with `just sync-once` and install with `just install`.

</details>

## CLI Reference

```bash
reminderrelay setup                     # interactive first-run wizard
reminderrelay daemon [--config <path>]  # start polling + WebSocket listener
reminderrelay sync-once [--config ...]  # single reconcile pass then exit
reminderrelay status                    # show daemon & config state
reminderrelay uninstall [--purge]       # stop daemon and remove files
reminderrelay version                   # print version
```

Legacy flag-based invocation (`--daemon`, `--sync-once`) is still supported for backward compatibility.

## Configuration Reference

| Key | Type | Default | Description |
|---|---|---|---|
| `ha_url` | string | — | Home Assistant base URL (`http://…` or `https://…`) |
| `ha_token` | string | — | Long-lived access token |
| `poll_interval` | duration | `30s` | How often Reminders are polled (10 s – 5 m) |
| `list_mappings` | map | — | `"Reminders list name": "todo.entity_id"`, or `{ha_entity, ha_calendar_entity}` for optional calendar mirroring — see [Calendar Mirroring](#calendar-mirroring) |
| `telemetry` | object | *(disabled)* | Optional OpenTelemetry export (see below) |

### Telemetry (optional)

Export traces, metrics, and logs to any OTLP-compatible collector (e.g. Grafana Alloy, Jaeger, Dash0).

```yaml
telemetry:
  otlp_endpoint: "localhost:4317"
  insecure: true
  service_name: "reminderrelay"   # optional, defaults to "reminderrelay"
  headers:                          # optional gRPC metadata
    Authorization: "Bearer <token>"
```

## Discovering Your HA Entity IDs

1. Open Home Assistant → **Settings → Devices & services → Entities**.
2. Filter by domain **todo** (or **calendar**, if setting up [Calendar Mirroring](#calendar-mirroring)).
3. Copy the entity IDs (e.g. `todo.shopping`, `calendar.work_due_dates`) into `list_mappings`.

Or run:

```bash
just sync-once -- --verbose 2>&1 | grep "entity"
```

## Feature Storage Reference

Apple Reminders (via EventKit) and Home Assistant's `todo` domain don't expose the same fields. This table shows what ReminderRelay actually syncs today, and where each field physically lives on each side.

| Feature | Apple Reminders (EventKit) | HA `todo` entity | HA calendar entity (optional mirror) | Synced? |
|---|---|---|---|---|
| Title | `Title` | `summary` | `summary` (`[Done] ` prefix once completed) — one-way into the calendar only | ✅ |
| Notes | `Notes` (plain text) | `description` (rendered as **markdown** in the HA frontend — see note below) | fixed marker text, not the real notes (see [Calendar Mirroring](#calendar-mirroring)) — one-way into the calendar only | ✅ |
| Due date | `DueDate` (date or datetime) | `due_date` / `due_datetime`, whichever the entity supports — see [Due-Date Fidelity](#due-date-fidelity) | `dtstart`/`dtend`, all-day or timed — **bidirectional** once calendar mirroring is on, see below | ✅ |
| Priority | `Priority` (0–9, normalised to 4 levels) | `[High]`/`[Medium]`/`[Low]` prefix on `description` — see [Priority Encoding](#priority-encoding) | *(no representation — priority isn't due-date-related)* | ✅ |
| Completed | `Completed` (bool) | `status` (`needs_action`/`completed`) | `[Done] ` summary prefix, event kept not deleted — one-way into the calendar only | ✅ |
| Completion timestamp | `CompletionDate` | *(not exposed by HA's `todo.get_items` at all)* | — | ❌ |
| Created/modified timestamps | `CreatedAt`, `ModifiedAt` | *(HA never reports a per-item modified timestamp — see [Conflict Resolution](#conflict-resolution))* | — | ❌ (used internally, not synced as a field) |
| Flagged | `Flagged` | — | — | ❌ — EventKit itself never reports the real value (always `false`), regardless of HA support |
| URL | `URL` | — | — | ❌ — no native HA field; a future prefix-encoding similar to priority is the planned approach, not yet implemented |
| Recurrence | `RecurrenceRules` (RRULE) | — (backend-dependent; most `todo` integrations don't expose it) | native `rrule` support exists on the calendar domain | ❌ — not yet implemented; the calendar entity is the intended future home for this, not the todo entity |
| Alarms / reminder time | `Alarms`, `RemindMeDate` | — (no concept in the `todo` domain) | *(a mirrored event's start time can drive an HA `calendar` automation trigger, but this isn't a synced "alarm" field)* | ❌ |

**Notes syncing is a plain-text pass-through in both directions** — the same text renders as markdown in HA and as plain text in Reminders (EventKit has no rich-text support). This is expected, not a bug: nothing is lost, but the same notes can look different in each app if they contain markdown syntax.

## Due-Date Fidelity

HA's `todo` domain supports two different due-date service fields, gated by the target entity's declared `supported_features`:

- `due_date` — date only.
- `due_datetime` — full date and time, only accepted by entities that declare `SET_DUE_DATETIME_ON_ITEM` (e.g. HA's **Local To-do** integration; not every `todo` backend supports it).

ReminderRelay checks the target entity's supported features (cached per entity for the process's lifetime) and sends `due_datetime` when the Reminders due date carries a time-of-day and the entity supports it, falling back to `due_date` otherwise. A due date at **exactly midnight** is treated as date-only, since neither EventKit nor HA can otherwise distinguish "no specific time" from "due at midnight."

## Conflict Resolution

Each reconcile pass compares five fields independently — title, notes, due date, priority, completed — against the last-synced snapshot, rather than hashing the whole item and picking one side as the winner:

- If only one side changed a field, that change wins — cleanly, with no risk of clobbering an unrelated change on another side. For example, marking an item complete in HA while adding a due date in Reminders now applies **both** changes, instead of one silently overwriting the other.
- If every side that changed a field changed it to the *same* value, that's not treated as a conflict.
- Only when sides changed the *same* field to *different* values is it a genuine conflict, requiring a tie-break.

Four fields (title, notes, priority, completed) are always 2-way: Reminders vs HA-todo. **The due date becomes a genuine 3-way field once calendar mirroring has created a mirrored event for an item** (see [Calendar Mirroring](#calendar-mirroring)) — Reminders, HA-todo, and the calendar event's start time are all live candidates, so dragging or deleting the event directly on the HA calendar changes the due date in Reminders and HA-todo too, the same way an edit on either of those changes it everywhere else.

**The tie-break is an approximation, not true last-write-wins**, because Home Assistant never reports a per-item modified timestamp — not on `todo.get_items`, and not on calendar events either — so there's nothing to compare Reminders' real EventKit timestamp against on either HA-side source. ReminderRelay approximates each HA-side "changed at" time as the wall-clock moment the reconciler first notices that side's content differ from what it last observed — which lags the real edit by up to one poll interval (default 30 s) or arrives instantly via the WebSocket listener. In practice this is usually accurate enough (human edits are rarely seconds apart), but it is an approximation: if you need guaranteed-correct conflict resolution, treat simultaneous edits to the same field on multiple sides as something to avoid rather than something this tool resolves perfectly. On an exact tie, precedence is **Reminders, then HA-todo, then calendar** — Reminders because it's treated as the primary source elsewhere too (e.g. bootstrap matching), and HA-todo over calendar because it's the older, more-established source of the two.

Item identity when talking to HA (`todo.update_item` / `todo.remove_item`) is targeted by the item's HA UID first, falling back to matching by title if that's rejected — recent Home Assistant core versions resolve either in the `item` field, but this fallback keeps older versions working too.

## Calendar Mirroring

Home Assistant's `todo` domain has no due-date UI beyond a plain date/time, and no way to trigger an automation off a due date. HA's `calendar` domain has both — a real timeline UI, and a `platform: calendar` automation trigger that can fire notifications ahead of an event's start time. ReminderRelay can optionally mirror due-dated items onto a calendar entity to get this, in addition to (not instead of) the normal todo sync.

This is **opt-in per list**. **The due date is bidirectional**: an edit made directly on the HA calendar — dragging an event to a new time, deleting it — flows back into both Reminders and HA-todo, via the 3-way merge described in [Conflict Resolution](#conflict-resolution). Title, notes, and completion status stay **one-way**, flowing into the mirrored event but never back out — there's no natural inbound meaning for renaming a mirrored event's summary, for instance, and folding every field into a 3-way merge would multiply the approximate-clock problem across three fields instead of one.

This bidirectionality assumes **the calendar entity is dedicated to ReminderRelay** — used for nothing else. ReminderRelay only ever acts on events it created itself, tracked by UID in its own state database; an event you add to that same calendar directly, for an unrelated appointment, is simply invisible to it — ignored, not adopted as a new reminder, since its UID doesn't match anything tracked. The dedication matters for a softer reason than correctness, though: if the calendar is genuinely used for nothing but mirrored reminders, there's nothing else on it to *accidentally* drag or delete. Reuse a general-purpose calendar you also put real appointments on, and that protection goes away — you could drag the wrong event without meaning to. Creating new items by adding events directly to the calendar isn't supported either way — see the last point in [What gets mirrored, and how](#what-gets-mirrored-and-how).

### Enabling it

Add `ha_calendar_entity` to a list mapping (the setup wizard offers this as a step after mapping the todo entity):

```yaml
list_mappings:
  "Work":
    ha_entity: "todo.work_tasks"
    ha_calendar_entity: "calendar.work_due_dates"
```

The calendar entity must already exist in HA — ReminderRelay can't create it. **Home Assistant's built-in Local Calendar integration is the tested, recommended choice** (`Settings → Devices & Services → Add Integration → Local Calendar`). At sync time, ReminderRelay checks the entity's declared `supported_features` and requires all three of create, update, and delete support; if the entity doesn't declare all three, mirroring for that list is skipped with a logged error rather than failing the whole sync — other calendar integrations (Google Calendar, CalDAV, etc.) may not support full programmatic mutation and are not a tested path.

### What gets mirrored, and how

Only items with a due date get a mirrored event. Title, due date/time, and completion status all round-trip through the mirror; notes and priority don't (there's no natural place for them on a calendar event, and duplicating notes into a second visible surface wasn't worth the redundancy).

| Reminder ↔ event state | What happens |
|---|---|
| Due date set (Reminders or HA-todo) | Event created — all-day if the due date has no time-of-day (same midnight heuristic as [Due-Date Fidelity](#due-date-fidelity)), else a timed event starting at the due time with a 30-minute default duration |
| Due date changed on Reminders or HA-todo | Mirrored event updated in place |
| Event dragged to a new time on the HA calendar | Reminders' and HA-todo's due dates updated to match — see [Conflict Resolution](#conflict-resolution) for what happens if another side also changed the due date in the same pass |
| Marked completed (Reminders or HA-todo) | Event **kept**, summary prefixed with `[Done] ` — not deleted, so completed items don't just vanish from the calendar |
| Due date cleared (either side), or event deleted directly on the HA calendar | Due date cleared everywhere: the mirrored event is deleted (if it wasn't already) and Reminders'/HA-todo's due dates are cleared to match |
| Item deleted (Reminders or HA-todo) | Mirrored event deleted |
| A brand-new event added directly to the calendar | **Not supported** — ReminderRelay only acts on events tied to an item it already tracks (see the dedicated-calendar note above); a genuinely new event is invisible to it, not adopted as a new reminder |
| Event description | A fixed marker ("Synced from Reminders by ReminderRelay."), not the reminder's real notes — this field isn't synced in either direction |

If updating a mirrored event fails and the event still shows up in that pass's calendar listing (a step ReminderRelay takes before deciding whether to create, update, or delete, precisely to avoid mistaking "deleted" for "broken"), ReminderRelay recreates it rather than leaving the mirror permanently broken.

### Why this needs the WebSocket connection

Home Assistant only exposes `calendar.create_event` and `calendar.get_events` as REST-callable services as of HA core 2026.x — `calendar.event/update` and `calendar.event/delete` exist only as WebSocket commands (`calendar/event/update`, `calendar/event/delete`), not REST services, even though the underlying entity feature (`CalendarEntityFeature.UPDATE_EVENT`/`DELETE_EVENT`) has been fully implemented for a while, including by Local Calendar. ReminderRelay's calendar adapter rides the same authenticated WebSocket connection already used for real-time HA → Reminders updates. This means calendar mirroring needs that WebSocket connection even in `sync-once` mode (normally only opened in `daemon` mode) — this happens automatically whenever a list mapping has `ha_calendar_entity` set.

There is also no way to supply your own UID when creating an event, and the create command doesn't return the assigned UID either — ReminderRelay works around this by looking the new event back up by summary within a narrow time window immediately after creating it.

### How calendar-side edits are detected

Reading events back (the input side of the 3-way due-date merge) uses the plain REST `GET /api/calendars/<entity_id>` endpoint instead of the WebSocket, fetched once per list per reconcile pass — a due date more than 1 year in the past or 2 years in the future falls outside that window and won't be detected as changed until it moves back inside it; an accepted limitation for a personal task list. If that listing fails for any reason, mirroring for that pass falls back to the 2-way Reminders/HA-todo merge rather than blocking the whole sync — a calendar-side edit made during an outage is simply picked up on the next successful pass.

### HA-side examples

The [`examples/`](examples/) directory has two Home Assistant automation/script YAML files that pair with calendar mirroring: a notification automation using the `calendar` domain's due-date-adjacent trigger (something the `todo` domain has no equivalent of), and a script for creating new to-do items with the same priority/due-date encoding ReminderRelay itself uses, so they sync cleanly.

## Priority Encoding

Apple Reminders supports four priority levels.  
Home Assistant todo has no native priority field, so ReminderRelay encodes priority as a prefix in the task description:

| Reminders priority | Description prefix |
|---|---|
| High | `[High] ` |
| Medium | `[Medium] ` |
| Low | `[Low] ` |
| None | *(no prefix)* |

## Justfile Recipes

```bash
just build        # compile binary
just test         # run all tests
just lint         # run golangci-lint
just run          # run daemon in foreground (Ctrl-C to stop)
just sync-once    # run one sync cycle and exit
just install      # build + install + load launchd agent
just uninstall    # unload + remove binary and plist
```

## Logs

| Location | Contents |
|---|---|
| `~/Library/Logs/reminderrelay/output.log` | Info and debug output |
| `~/Library/Logs/reminderrelay/errors.log` | Errors and warnings |

Tail logs live:

```bash
tail -f ~/Library/Logs/reminderrelay/errors.log
```

## Uninstall

```bash
reminderrelay uninstall          # stop daemon + remove binary and plist
reminderrelay uninstall --purge  # also remove config, state DB, and logs
```

## Troubleshooting

### Reminders access denied (TCC)

macOS requires explicit permission for apps to access Reminders.  
On first run a system dialog appears — click **OK**.  
If you previously denied access:

1. Open **System Settings → Privacy & Security → Reminders**.
2. Enable access for Terminal (or your shell app).

### HA connection refused

- Confirm `ha_url` is reachable: `curl -s <ha_url>/api/ -H "Authorization: Bearer <token>"`
- Ensure the token has not expired or been revoked.

### Items duplicated after restart

This usually means the state database was deleted (or reset — see below) while items still existed in both systems, so bootstrap's title-matching ran again on top of items it already knows about. Re-running bootstrap after a reset is expected to produce clean matches as long as titles haven't diverged between the two sides; if it doesn't, remove the DB and re-run manually:

```bash
rm ~/.local/share/reminderrelay/state.db
just sync-once
```

### Resetting sync history

`reminderrelay setup` asks up front whether to reset the state database (declined by default, since it forgets every tracked link between Reminders and HA items — the next sync re-runs bootstrap's title-matching from scratch). Useful after significantly reconfiguring list mappings, or to recover from state that's gotten confused. You can also do this without running the full wizard:

```bash
rm -f ~/.local/share/reminderrelay/state.db ~/.local/share/reminderrelay/state.db-wal ~/.local/share/reminderrelay/state.db-shm
```

### Sync is slow

Decrease `poll_interval` (minimum `10s`). Real-time HA → Reminders flow is already push-based via WebSocket; the interval only affects Reminders → HA propagation.

## Architecture

```
cmd/reminderrelay/        Entry point, subcommand dispatch, wiring
internal/config/          YAML config loader + validation
internal/state/           SQLite repository (WAL mode)
internal/model/           Shared Item type, priority encoding, content hash
internal/reminders/       Apple Reminders adapter (EventKit via cgo)
internal/homeassistant/   HA REST + WebSocket adapter, retry logic, calendar mirroring
internal/sync/            Reconciler (field-level merge, calendar mirror), bootstrap wizard, daemon engine
internal/setup/           Interactive setup wizard, daemon install/uninstall
internal/telemetry/       Optional OpenTelemetry OTLP gRPC export
deployment/               launchd plist, install/uninstall scripts
```

## License

MIT — see [LICENSE](LICENSE).
