# Home Assistant examples

These are Home Assistant automation/script YAML, not Go code — they complement ReminderRelay's [calendar mirroring](../README.md#calendar-mirroring) and [due-date fidelity](../README.md#due-date-fidelity) features from the HA side.

| File | What it does |
|---|---|
| [`notify_due_todo_automation.yaml`](notify_due_todo_automation.yaml) | Sends a notification ahead of a mirrored item's due time, using HA's `calendar` trigger (the `todo` domain has no due-date trigger of its own — this is the reason calendar mirroring exists). |
| [`add_todo_item_script.yaml`](add_todo_item_script.yaml) | Creates a to-do item with the same priority-prefix and due-date encoding ReminderRelay itself uses, so it syncs cleanly into Reminders. It only touches the `todo` entity — the mirrored calendar event is created automatically by ReminderRelay's own next sync pass. |
| [`recreate_recurring_todo_automation.yaml`](recreate_recurring_todo_automation.yaml) | Recreates a fresh to-do item on a schedule (calls the script above). ReminderRelay doesn't sync recurrence in any form — see the note in README.md's "Calendar Mirroring" section — so this is the practical stand-in: an independent new item each time, not a true repeating series. |

## Installing

All three are plain automation/script definitions. Either:

- **Via the UI**: Settings → Automations & Scenes → Automations (or Scripts) → **+ Add** → ⋮ → **Edit in YAML**, then paste the file's contents in.
- **Via YAML config**: add the block as its own top-level entry in `automations.yaml` / `scripts.yaml` (or wherever your `automation:`/`script:` config is split out).

Each file has inline comments marking what you need to replace (calendar entity ID, notification target, etc.) for your own setup.
