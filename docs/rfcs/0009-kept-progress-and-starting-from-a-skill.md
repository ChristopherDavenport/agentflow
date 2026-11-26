# RFC 0009: kept progress, and a workflow a model starts by its skill

Status: spiked. Item 7: a script's progress kept in the session, where
a front reads it back after a restart, and no model reads it.

## 7. Kept progress

RFC 0005 made a script's progress a live update of its call, which the
loop raises as a `ToolUpdate` and agentturn/session writes nothing for.
A front that restarts, or opens a session another process ran, saw no
progress at all.

Each update is now also written to the session, as a long-running tool
writes a handle it must not lose:

- `run_script` calls `agenttool.WriteRecord` with a
  `script.ProgressRecord` (`seq`, `progress`, `total`, `message`) for each
  line it reports. The recorder a host installs as
  `agentturn.Config.ToolRecorder`, agentturn/session's
  `Recorder.RecordFunc`, writes it at once and durably as a **custom
  entry** of namespace `agentflow:progress` (`script.ProgressNS`), with
  `call_id` naming the script step's call.
- **What a front reads.** The custom entries of that namespace on the
  session's path, in order, each naming its call: `seq` counts the
  call's kept updates from 1, and the numbers and message are the line's.
  The entry's own time is when it was written.
- **What it is not.** Custom entries are out of context: agentturn/session
  rebuilds no item from them, so no request carries progress and the
  transcript holds none. Position, `with` values and the result never
  see one. The session format already has custom entries; nothing in it
  changed.
- **Bounds.** The first 256 updates of a call are kept
  (`script.MaxKeptProgress`); later ones are reported live and not kept.
  The bounds of RFC 0005 still hold before that: a line over 4 KiB is not
  reported, nor anything past the file's first 1 MiB. So a call keeps at
  most 1 MiB of progress.
- **A record that cannot be written** costs a line of progress, never the
  script its run. Without a recorder on the call's context, nothing is
  written, as before.
