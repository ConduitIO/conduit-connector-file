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
A position records the file's identity (device, inode, a hash of its first
4 KiB) and the byte offset. On restart the source resumes at the offset if
the file at the path is that file; if it was rotated while the connector
was stopped it finishes the rotated-away file when it is still next to the
path and then reads the new file from its start. Positions written by
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
(both the source and the destination use it).<!-- /readmegen:description -->

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
* **Rotation is followed.** Before, rotating the file ended the follow
  silently (empty records were emitted). Now the old file is finished and the
  new one is read from its first byte.
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
* Rotation is followed when the source sees the new file while it is at the
  path (it checks about every 200 ms while reading and every 50 ms while idle,
  and keeps files it has seen open until it gets to them). A file that is
  created and rotated away again within one check, or two rotations that
  happen while the connector is stopped, skip the file in between. After one
  rotation while stopped, the rotated-away file is found only if it is still
  in the same directory as the path (rename-style rotation); a compressed or
  moved file is not read.
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
