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
of a line. A last line without a newline is read after it has stayed
unchanged for `quietPeriod` (default 1s), when the file is rotated or
truncated, or when the connector is stopped gracefully (that record is
checkpointed); data appended to such a line later becomes a record of its
own.
When the file is rotated (renamed, removed or replaced) the source reads the
old file to its end and then the new file from its first byte. When the file
is truncated (copytruncate) it is read again from its start.
The source never skips the unread data of a rotated file silently. If it
cannot account for it, it stops with error code FILE_ROTATION_TAIL_UNREADABLE
(naming the path and the fix). That happens when the file the saved position
belongs to cannot be found after a restart (deleted, compressed, moved
away, or truncated in place), and when a file was written between the rotated file and the current
one that the source never saw at the path (two rotations in a row). Set
`rotatedPattern`, a glob such as `app.log.*`, and the source finds the
rotation chain itself, orders it by modification time and reads each file
in turn: the old file's tail, the files in between from their first byte,
then the current file. A compressed rotated file cannot be read and stops
the source too. Operators who prefer to continue can set
`rotation.acceptLoss: true`: the source then logs the same code as a
warning, with the size of what it skips, and carries on.
A position records the file's identity (device, inode, a hash of its first
4 KiB) and the byte offset. On restart the source resumes at the offset if
the file at the path is that file; if it was rotated while the connector
was stopped it finishes the rotated-away file (found by inode next to the
path, or among the files `rotatedPattern` matches) and then reads the new
file from its start. Positions written by
v0.10.x (a bare offset) are accepted if the offset is within the file and
follows a newline; otherwise the source refuses to start with error code
FILE_POSITION_UNVERIFIABLE. Other codes: FILE_POSITION_INVALID,
FILE_IDENTITY_UNKNOWN. After the first record, positions are in the new
format, which v0.10.x cannot read.

### Destination

The Destination connector will create the file if it doesn't exist, and
records with changes will be appended to the destination file when received.
A file created by the connector is readable and writable by its owner only
(mode 0600, further restricted by the process umask). The mode of an
existing file is never changed.
An embedding engine can supply the open function through `file.WithOpener`
(both the source and the destination use it) and a check for the directories
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
          # QuietPeriod is how long the last line of the file, when it has no
          # trailing newline, must stay unchanged before it is read as a record.
          # Lines that end in a newline are read as soon as they are written. 0
          # reads such a line at once, as versions before v0.11.0 did.
          # Type: duration
          # Required: no
          quietPeriod: "1s"
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
          # Maximum delay before an incomplete batch is read from the source.
          # Type: duration
          # Required: no
          sdk.batch.delay: "0"
          # Maximum size of batch before it gets read from the source.
          # Type: int
          # Required: no
          sdk.batch.size: "0"
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

Behavior changes of the source:

* **Complete lines only.** A line is read once its newline is written. Before,
  a half-written last line was read at once and the rest of it later became a
  second record. A last line without a newline now waits `quietPeriod`
  (default `1s`; `0` restores the old behavior), and is read earlier on
  rotation, truncation or graceful stop.
* **Rotation is followed, and never skipped silently.** Before, rotating the
  file ended the follow silently (empty records were emitted). Now the old file
  is finished and the new one is read from its first byte. When data of a
  rotated file cannot be accounted for the source stops with
  `FILE_ROTATION_TAIL_UNREADABLE`; the new optional `rotatedPattern` (for
  example `app.log.*`) lets it find and read the whole chain, and
  `rotation.acceptLoss: true` makes it continue with a warning instead.
* **New position format.** The first record after upgrading carries a position
  that includes the file's identity. v0.10.x cannot read it, so a downgrade
  needs the pipeline's position reset. Offset-only positions from v0.10.x are
  accepted when they are within the file and follow a newline, otherwise the
  source refuses to start (`FILE_POSITION_UNVERIFIABLE`) instead of guessing.
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
  Timestamps of equal value are ordered by inode; on file systems with coarse
  timestamps, two files written within one tick are not told apart.
* With `sdk.batch.size` or `sdk.batch.delay` set, a graceful stop does not
  deliver the unterminated last line (the SDK stops collecting when it is
  cancelled); it is read again, whole, after the restart.
* A truncation (copytruncate) is detected by the file shrinking or its first
  bytes changing, checked each time the read buffer is refilled; lines that
  were copied away before the truncation but not read yet are not recovered.
* After a graceful stop the unterminated last line is checkpointed; if the
  writer completes that line later, the rest of it arrives as a separate
  record.
* The connectors can only access local files on the machine where Conduit
  is running. So, running Conduit on a server means it can't access a file
  on your local machine.
* Currently, only works reliably with text files (may work with non-text
  files, but not guaranteed)

![scarf pixel](https://static.scarf.sh/a.png?x-pxid=42ff59b7-f26d-468d-8c8d-eafc530290cc)
