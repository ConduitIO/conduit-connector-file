# Conduit Connector File

The File plugin is one of [Conduit](https://github.com/ConduitIO/conduit) builtin plugins.
It provides both source and destination File connectors, allowing for a file to be either
a source, or a destination in a Conduit pipeline.

<!-- readmegen:description -->
## How it works

The Source connector listens for changes appended to the source file and
sends records with the changes.
The Destination connector receives records and writes them to a file.

### Source

The Source connector only cares to have a valid path, even if the file
doesn't exist, it will still run and wait until a file with the configured
name is there, then it will start listening to changes and sending records.
The file is followed by the connector itself.
A line is read once its newline has been written; a record is never a piece
of a line. A last line without a newline is held until its newline is
written or the file is rotated or truncated. A graceful stop does not emit
it either: the position stays at the start of that line and the whole line
is read after the restart. For writers that never terminate their last line, set
`quietPeriod` (off by default) and the line is also read once the file has
not changed for that long.
A line longer than `maxLineSize` (default 16 MiB) stops the source with
FILE_LINE_TOO_LONG; lines are never truncated. A path that is not a regular
file (a FIFO, a device) is refused.
When the file is rotated (renamed, removed or replaced) the source reads the
old file until its size has been stable for `rotation.settle` (default 1s):
a writer that still holds the old file open keeps appending until it is told
to reopen. Then it reads the new file from its first byte. The old file is
kept open for a few minutes and read again if it grows once more. When the file
is truncated (copytruncate) it is read again from its start; lines that
were written but not yet read when it was truncated now exist only in the
copy: with `rotatedPattern` the source finds the copy by its first bytes and
reads them from there, without it the source stops with
FILE_ROTATION_TAIL_UNREADABLE.
The source never skips the unread data of a rotated file silently. If it
cannot account for it, it stops with error code FILE_ROTATION_TAIL_UNREADABLE
(naming the path and the fix). That happens when the file the saved position
belongs to cannot be found after a restart (deleted, compressed, moved
away, or truncated in place), when a file was written between the rotated
file and the current one that the source never saw at the path (two
rotations in a row), and when a file is truncated with unread data in it. Set
`rotatedPattern`, a glob such as `app.log.*`, and the source finds the
rotation chain itself, orders it by modification time and reads each file
in turn: the old file's tail, the files in between from their first byte,
then the current file. A compressed rotated file cannot be read and stops
the source too. Operators who prefer to continue can set
`rotation.acceptLoss: true`: the source then logs the same code as a
warning, with the size of what it skips, and carries on.
A position records the file's identity (device, inode, a hash of its first
4 KiB) and the byte offset. On restart the source resumes at the offset if
the file at the path is that file (same inode, size at least the offset and
same first bytes; the device number may differ, it changes across remounts); if it was rotated while the connector
was stopped it finishes the rotated-away file (found by inode next to the
path, or among the files `rotatedPattern` matches) and then reads the new
file from its start. Positions written by
v0.10.x (a bare offset) are accepted if the offset is within the file and
follows a newline, with a warning (code FILE_POSITION_UNVERIFIABLE) that
gives the offset, size, inode and modification time, because such a position
carries no file identity: if the file was rotated while upgrading, the first
offset bytes of the current file are skipped. Otherwise the source refuses to
start with error code FILE_POSITION_UNVERIFIABLE. Other codes: FILE_POSITION_INVALID,
FILE_IDENTITY_UNKNOWN. After the first record, positions are in the new
format, which v0.10.x cannot read.

### Destination

> **Output files created by earlier versions stay world-readable (0644).**
> This version creates them 0600, but it never changes the mode of an
> existing file: run `chmod 600` on the output files of existing pipelines.

The Destination connector will create the file if it doesn't exist, and
records with changes will be appended to the destination file when received.
A file created by the connector is readable and writable by its owner only
(mode 0600, further restricted by the process umask). The mode of an
existing file is never changed: **output files created by earlier versions
are world-readable (0644); run `chmod 600` on them after upgrading.**

#### Delivery semantics

Each record is one line: its bytes in the configured record format and a
newline. A batch is acknowledged only after it is on disk: the destination
writes the batch, fsyncs the output, records the new length of the output
in `<path>.committed` (next to the output) and fsyncs that file, and only
then returns. On `Open` it also fsyncs the output's directory, so a newly
created output survives a crash. A kill of the process, an operating-system
crash or a power loss therefore never loses an acknowledged record, provided
the storage honors fsync (a disk write cache without power-loss protection,
or a filesystem mounted without write barriers, can still lose data the
operating system reported as synced). Delivery is at-least-once: after a
crash the batch that was being written, and not yet acknowledged, is
delivered again, so the output can hold a record twice. After a failed
write (an I/O error, a full disk, a failed fsync) the destination refuses
further writes until it is opened again, so the next batch never lands
after part of the failed one.

Some filesystems (some network and FUSE filesystems) reject an fsync of a
directory as unsupported (EINVAL, ENOTSUP, EOPNOTSUPP). There `Open` logs a
warning with code FILE_DIR_SYNC_UNSUPPORTED and the directory, and
continues: every batch is still synced before it is acknowledged, but a
newly created output file may not survive an operating-system crash or a
power loss, and the records acknowledged into it are then lost with it. On
such a filesystem, create the output file before the pipeline first starts.
Any other error syncing the directory fails `Open`.

Every Write ends with the two fsyncs, whatever its size. A larger batch
takes longer to write and sync than a smaller one, but it shares the fixed
cost of the fsyncs among all its records, so the cost per record falls by
orders of magnitude from batches of one record to batches of a thousand
(`BenchmarkDestinationWrite1` to `BenchmarkDestinationWrite1000` in
`destination_bench_test.go` measure it). The
destination's `sdk.batch.size` defaults to 0 (each Write gets what the
engine passes; under the default engine that is a whole source batch,
`sdk.batch.size` 100 on the source). For more throughput, write larger
batches: for example set `sdk.batch.size` to 1000 and `sdk.batch.delay`
to `100ms` on the destination (the delay bounds how long a partial batch
waits).

A process that is killed while writing (SIGKILL, the OOM killer) can leave
the file ending in part of a line: Linux stops a write at a page boundary
when the process is killed. On `Open` the destination cuts the file back
to the length recorded in `<path>.committed`, and logs a warning with the
path, the old and new size and the number of bytes removed. Those bytes are
the start of a batch that was never acknowledged and is delivered again, so
the next record never lands on a partial line. After an operating-system
crash the recorded length can be behind the output by at most that one
batch (the output was synced, the length was not yet), and the cut removes
only that unacknowledged batch.

`<path>.committed` is part of the output: keep it with the file (it has
the output file's mode), back it up and restore it together with the file,
and do not edit it. It holds 8 bytes while the destination runs and is
emptied when the destination stops cleanly.

When `<path>.committed` is missing or empty (an output written by v0.12.0
or earlier, or after a clean stop) and the file does not end with a newline
(a crash of v0.12.0 or earlier), `Open` can only cut after the last
newline, and does so only if a record cannot contain one. That is decided
by the record format's name: every format whose name ends in `/json` (the
default `opencdc/json`, and `debezium/json`) is encoded by the SDK's JSON
encoder, which escapes newlines inside strings and emits no whitespace, so
`Open` removes what follows the last newline, with the same warning. With
any other record format (for example `template`) a record can contain a
newline, so `Open` fails with FILE_OUTPUT_TAIL_UNTERMINATED instead of
guessing: inspect the end of the file, remove the partial record (or
truncate the file), then restart.

When the output is empty but `<path>.committed` holds a length (the output
was deleted or emptied), the destination starts again from empty with a
warning. When the output is shorter than the recorded length, or the
recorded length does not end a line, `Open` fails with
FILE_COMMITTED_LENGTH_MISMATCH without changing anything. Neither a kill
nor an operating-system crash can cause this: the output was truncated,
replaced or restored from a backup without its `<path>.committed`, or the
storage lost writes it had reported as synced. Check the file. If it holds
what you expect, delete `<path>.committed` and restart: the output is then
kept up to its last newline, under the rule above for a missing length.
Otherwise restore the output and `<path>.committed` together from the same
backup. A `<path>.committed` that is not 0 or 8 bytes long, is a symlink or
is not a regular file fails with FILE_COMMITTED_LENGTH_INVALID. A path that
is not a regular file (a device such as `/dev/stdout`, a FIFO) is written
as before, without `<path>.committed` and without fsync.

The destination owns its output file: do not point two destinations, or
another writer, at the same path, and do not let a source's
`rotatedPattern` match `<path>.committed`.

Downgrading: v0.12.0 and earlier ignore `<path>.committed` and append to
the output without updating it. After a clean stop the file is empty and
nothing is lost when you upgrade again. After a crash it still holds a
length, and if an older version then appends acknowledged records, the
next `Open` of this version cuts them off as unacknowledged: those records
are removed from the output and not delivered again. So when you go back to
an older version after a crash, delete `<path>.committed` before starting
this version again.

Windows: the destination is compile-checked on Windows but not tested
there. The output and `<path>.committed` are flushed with FlushFileBuffers;
the directory is not flushed (NTFS journals file creation).
`<path>.committed` is opened without following a reparse point (a
symlink, a junction), and one is refused by the opened handle's
attributes; for that it is opened directly, not through `file.WithOpener`.
On platforms that are neither unix nor Windows, a destination writing a
regular file fails to open: the connector cannot open `<path>.committed`
there without following a symlink.

An embedding engine can supply the open function through `file.WithOpener`
(both the source and the destination use it, the destination also for
`<path>.committed`, except on Windows) and a check for the directories
the source lists to find rotated files through `file.WithDirChecker`.<!-- /readmegen:description -->

## Source Configuration Parameters

<!-- readmegen:source.parameters.yaml -->
```yaml
version: 2.2
pipelines:
  - id: example
    status: running
    connectors:
      - id: example
        plugin: "file"
        type: source
        settings:
          # Path is the file path used by the connector to read/write records.
          # Type: string
          # Required: yes
          path: ""
          # MaxLineSize is the longest line, in bytes, that is read. A longer
          # line stops the source with the error FILE_LINE_TOO_LONG; a line is
          # never truncated.
          # Type: int
          # Required: no
          maxLineSize: "16777216"
          # QuietPeriod is opt-in and off (0) by default. By default the last
          # line of the file, when it has no trailing newline yet, is held until
          # its newline is written, the file is rotated or truncated, or the
          # connector is stopped gracefully, so a record is never a piece of a
          # line. Set a duration to also read such a line once the file has not
          # changed for that long, for writers that never terminate their last
          # line.
          # Type: duration
          # Required: no
          quietPeriod: "0"
          # RotatedPattern is a glob that matches the rotated files of the path,
          # for example "app.log.*" (relative to the directory of path unless it
          # is absolute). Wildcards are only allowed in the last component, so
          # the directory is literal; an embedding engine may refuse it
          # (file.WithDirChecker). With it the source finds the rotation chain
          # itself, orders it by modification time and reads every file in turn:
          # the tail of the file it was reading, the files rotated in between
          # from their first byte, then the current file. Without it the source
          # stops with the error FILE_ROTATION_TAIL_UNREADABLE when it cannot
          # account for a rotated file's unread tail. Compressed files are not
          # read.
          # Type: string
          # Required: no
          rotatedPattern: ""
          # RotationAcceptLoss makes the source continue, with a warning that
          # names the code and the size of what is skipped, when the unread tail
          # of a rotated file cannot be read, instead of stopping.
          # Type: bool
          # Required: no
          rotation.acceptLoss: "false"
          # RotationSettle is how long a rotated file's size must stay unchanged
          # before the source leaves it for the next file. A writer that still
          # holds the old file open (logrotate renames, then signals the program
          # later) keeps appending to it, and those lines are read first. A file
          # that grows again after the source left it is read again for a while.
          # 0 switches at once.
          # Type: duration
          # Required: no
          rotation.settle: "1s"
          # Maximum delay before an incomplete batch is read from the source. 0
          # returns what is already queued without waiting.
          # Type: duration
          # Required: no
          sdk.batch.delay: "0"
          # Maximum size of batch before it gets read from the source.
          # Type: int
          # Required: no
          sdk.batch.size: "100"
          # Specifies whether to use a schema context name. If set to false, no
          # schema context name will be used, and schemas will be saved with the
          # subject name specified in the connector (not safe because of name
          # conflicts).
          # Type: bool
          # Required: no
          sdk.schema.context.enabled: "true"
          # Schema context name to be used. Used as a prefix for all schema
          # subject names. If empty, defaults to the connector ID.
          # Type: string
          # Required: no
          sdk.schema.context.name: ""
          # Whether to extract and encode the record key with a schema.
          # Type: bool
          # Required: no
          sdk.schema.extract.key.enabled: "false"
          # The subject of the key schema. If the record metadata contains the
          # field "opencdc.collection" it is prepended to the subject name and
          # separated with a dot.
          # Type: string
          # Required: no
          sdk.schema.extract.key.subject: "key"
          # Whether to extract and encode the record payload with a schema.
          # Type: bool
          # Required: no
          sdk.schema.extract.payload.enabled: "false"
          # The subject of the payload schema. If the record metadata contains
          # the field "opencdc.collection" it is prepended to the subject name
          # and separated with a dot.
          # Type: string
          # Required: no
          sdk.schema.extract.payload.subject: "payload"
          # The type of the payload schema.
          # Type: string
          # Required: no
          sdk.schema.extract.type: "avro"
```
<!-- /readmegen:source.parameters.yaml -->

## Destination Configuration Parameters

<!-- readmegen:destination.parameters.yaml -->
```yaml
version: 2.2
pipelines:
  - id: example
    status: running
    connectors:
      - id: example
        plugin: "file"
        type: destination
        settings:
          # Path is the file path used by the connector to read/write records.
          # Type: string
          # Required: yes
          path: ""
          # Maximum delay before an incomplete batch is written to the
          # destination.
          # Type: duration
          # Required: no
          sdk.batch.delay: "0"
          # Maximum size of batch before it gets written to the destination.
          # Type: int
          # Required: no
          sdk.batch.size: "0"
          # Allow bursts of at most X records (0 or less means that bursts are
          # not limited). Only takes effect if a rate limit per second is set.
          # Note that if `sdk.batch.size` is bigger than `sdk.rate.burst`, the
          # effective batch size will be equal to `sdk.rate.burst`.
          # Type: int
          # Required: no
          sdk.rate.burst: "0"
          # Maximum number of records written per second (0 means no rate
          # limit).
          # Type: float
          # Required: no
          sdk.rate.perSecond: "0"
          # The format of the output record. See the Conduit documentation for a
          # full list of supported formats
          # (https://conduitdata.io/docs/using/connectors/configuration-parameters/output-format).
          # Type: string
          # Required: no
          sdk.record.format: "opencdc/json"
          # Options to configure the chosen output record format. Options are
          # normally key=value pairs separated with comma (e.g.
          # opt1=val2,opt2=val2), except for the `template` record format, where
          # options are a Go template.
          # Type: string
          # Required: no
          sdk.record.format.options: ""
          # Whether to extract and decode the record key with a schema.
          # Type: bool
          # Required: no
          sdk.schema.extract.key.enabled: "true"
          # Whether to extract and decode the record payload with a schema.
          # Type: bool
          # Required: no
          sdk.schema.extract.payload.enabled: "true"
```
<!-- /readmegen:destination.parameters.yaml -->

## How to build it

Run `make`.

## Testing

Run `make test` to run all the tests.

## Upgrading to v0.11.0

> **Output files created by earlier versions stay world-readable (0644).** This
> version creates new ones 0600 but never changes an existing file's mode. Run
> `chmod 600` on the output files of existing pipelines.

Behavior changes of the source:

* **Complete lines only.** A line is read once its newline is written. Before,
  a half-written last line was read at once and the rest of it later became a
  second record. A last line without a newline is now held until its newline
  is written or the file is rotated or truncated. A graceful stop does not
  emit it; the whole line is read after the restart.
  `quietPeriod` (off by default) opts back in to reading such a line after it
  has stayed unchanged for that long.
* **Rotation is followed, and never skipped silently.** Before, rotating the
  file ended the follow silently (empty records were emitted). Now the old file
  is finished and the new one is read from its first byte. When data of a
  rotated file cannot be accounted for the source stops with
  `FILE_ROTATION_TAIL_UNREADABLE`; the new optional `rotatedPattern` (for
  example `app.log.*`) lets it find and read the whole chain, and
  `rotation.acceptLoss: true` makes it continue with a warning instead. A
  copytruncate that leaves unread data behind stops the source too (with
  `rotatedPattern` the data is read from the copy).
* **New position format.** The first record after upgrading carries a position
  that includes the file's identity. v0.10.x cannot read it, so a downgrade
  needs the pipeline's position reset. Offset-only positions from v0.10.x are
  accepted when they are within the file and follow a newline, otherwise the
  source refuses to start (`FILE_POSITION_UNVERIFIABLE`) instead of guessing.
* **Lines have a size limit.** `maxLineSize` (default 16 MiB): a longer line
  stops the source with `FILE_LINE_TOO_LONG`; it is never truncated.
* **Destination files are 0600 only when created by this version.** Output
  files that earlier versions created are mode 0644 and keep it. **Run
  `chmod 600` on them.**
* **Reads go through the engine's opener** (`file.WithOpener`) on every open
  and re-open of the path; a path that is not a regular file is refused.

## Limitations

* The  Source connector only detects appended changes to the file, so it
  doesn't detect deletes or edits.
* `rotatedPattern` may only use wildcards in its last component (the directory
  is literal) and is checked against the embedding engine's directory policy
  (`file.WithDirChecker`) at configuration, at open and before every listing;
  a directory the policy does not allow is refused with
  `FILE_ROTATED_PATTERN_DENIED`, a wildcard directory with
  `FILE_ROTATED_PATTERN_INVALID`.
* Without `rotatedPattern` a rotated file is only found if it is still in the
  same directory under any name (renamed, found by inode) and the source never
  skips one silently: a deleted, compressed or moved rotated file, or a file
  written between the rotated one and the current one that the source never
  saw at the path, stops it with `FILE_ROTATION_TAIL_UNREADABLE`. Unrelated
  files are not mistaken for rotated ones: only names that start with the
  path's name followed by something other than a letter count. Reading
  compressed files (`.gz`) is not supported.
* `rotatedPattern` orders the chain by modification time: a file that was
  written to again after it was rotated (a writer that kept its old file
  open) can be ordered after the file it was rotated before, and a file
  rotated away and deleted before the source saw it cannot be known.
  Files with equal timestamps are included and ordered by the numeric suffix of
  their name (a long suffix is a date and ascends, a short one is logrotate's
  index and descends), then by inode. A file with an older timestamp than the
  file before it is not found.
* A truncation (copytruncate) is detected by the file shrinking or its first
  bytes changing, checked each time the read buffer is refilled. If unread
  data was in the file at the previous check, it exists only in the copy: with
  `rotatedPattern` it is read from the copy (identified by its first bytes);
  without, the source stops with `FILE_ROTATION_TAIL_UNREADABLE`. The bytes
  written between the last check and the truncation cannot be known.
* A graceful stop never emits the unterminated last line: the position stays at
  the start of that line and the whole line is read after the restart (at-least-once).
* A rotated file is left only after its size was stable for `rotation.settle`;
  a writer that signals "reopen" later than that, and appends once more to the
  old file, is still read (the finished file is held open for 5 minutes, at most
  16 of them). A file that is never written again after the source left it, and
  was rotated away and deleted unread, cannot be known.
* The connectors can only access local files on the machine where Conduit
  is running. So, running Conduit on a server means it can't access a file
  on your local machine.
* Currently, only works reliably with text files (may work with non-text
  files, but not guaranteed)

![scarf pixel](https://static.scarf.sh/a.png?x-pxid=42ff59b7-f26d-468d-8c8d-eafc530290cc)
