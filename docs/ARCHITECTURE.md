# Architecture

## Runtime shape

The application is one static Go binary with embedded frontend assets. It does not execute helper processes, require CGO, or write application state to disk. The only files that it writes on the local disk are the files of a `volume` block, in the folder of that block (refer to "Volumes").

The runtime can be placed in a scratch container with a read-only root filesystem. Only the HCL file and any credential files need to be mounted read-only.

## Configuration and shared provider clients

HCL defines reusable authentication objects and bucket views. Buckets referencing the same authentication share provider credentials, token refresh state, HTTP transport, and connection pool. Each bucket independently defines:

- provider bucket name;
- display name;
- virtual root prefix;
- permission ceiling;
- navigation scan limit;
- learned capability state.

## Storage interfaces

The backend separates listing, object reads, ranged reads, writes, deletion, copy/rewrite, multipart upload, and version operations. Read-only preview code receives only the operations it needs.

All provider traffic passes through:

- a global concurrency gate;
- a per-bucket concurrency gate;
- a request counter;
- a transferred-byte counter;
- request cancellation;
- strict response validation.

Provider redirects and environment-derived proxies are disabled.

## Stateless state model

The server has no local database, cache directory, checkpoint directory, or temporary-file contract. A volume is the only exception: an upload to a volume writes a temporary file in the folder of the target, in the volume (refer to "Volumes").

Process memory contains only bounded live state:

- at most four active or queued background jobs;
- a small terminal-job history;
- current upload sessions;
- current preview sessions;
- short-lived range caches;
- process-local permission observations;
- short-lived Insights results.

A process restart discards all of this state. No hidden object is written to a connected bucket to replace local state.

## Upload resume protocol

A resumable upload creates a provider-side S3 multipart upload or GCS resumable session. The server returns an opaque `stowr1` token containing the encrypted and authenticated provider coordinates, target object, size, media type, chunk size, and expiry.

The token key is derived from the configured provider credential material when available. This allows another process with the same HCL credentials to open the token without a server-side record. The server then reconciles the actual provider state:

- S3: lists uploaded parts and validates contiguous part numbers and sizes;
- GCS: queries the resumable-session offset;
- volume: reads the size of the temporary file of the upload (refer to "Volumes").

The process tracks at most four active upload sessions. Completed and canceled sessions are removed from process memory. Cancel also aborts the provider session when supported.

The browser currently keeps active tokens in page memory. An API client may retain the opaque token outside the server and submit it to `/api/uploads/resume` after a server restart.

## Upload checksums

An S3 upload can carry a SHA-256 checksum. The GCS provider does not use it.

The browser computes the SHA-256 digest of each chunk with `crypto.subtle`. A page that is not secure has no `crypto.subtle`. A page is secure with HTTPS or on `localhost`. Such a page sends no checksum.

The request `POST /api/uploads` has the field `checksum`. The value is `""` or `"sha256"`. The answer shows the value that the server accepts. With `sha256`, the server starts the multipart upload with `x-amz-checksum-algorithm: SHA256`.

Each chunk of such an upload needs the header `X-Checksum-Sha256`. The value is the base64 digest of the chunk. A chunk without this header gets `400 checksum_required`. A value that is not a 32-byte digest gets `400 invalid_checksum`.

The server sends the digest to the provider in `x-amz-checksum-sha256`. The provider refuses a part with other bytes. At the end, the server sends the digest of each part in `CompleteMultipartUpload`.

The answer has the field `checksumVerified`. It is `true` when the provider returned the digest of each part. The server also reads the checksum of the object. This checksum is the SHA-256 of the part digests. S3 writes it as `<digest>-<number of parts>`. Garage writes the digest only. A different value makes the upload fail.

A direct upload (`PUT /s3`) accepts the same header. The browser uses it for a file under 32 MiB. The answer has `X-Checksum-Sha256-Verified: true` when the provider returned the same digest. A provider that does not store checksums gives no confirmation. The upload still succeeds.

The resume token keeps the algorithm. After a restart, the server reads the digest of each part from the list of parts of the provider.

## Object tags and lifecycle

Only the S3 provider has object tags. A provider that does not know them answers `501`. The server turns this answer into the error `tagging_unsupported`. The interface then hides the tag controls. Garage does not support tags. SeaweedFS does.

`GET /api/tags?key=` gives the tags of an object. `PUT /api/tags` replaces the whole set. An empty set removes all tags. The server checks the limits of S3 before it sends the request. An object has at most 10 tags. A key has 1 to 128 characters and does not start with `aws:`. A value has at most 256 characters. A tag has letters, digits, spaces and `+ - = . _ : / @` only.

`POST /api/tags/bulk` starts the job `tag_prefix`. The body has `prefix`, `add` (tags to set) and `remove` (keys to remove). The job reads the tags of each object under the prefix. It writes them again only when the result is different. An object that would have more than 10 tags stops the job with its key in the message.

At start, the server probes each instance with a tag read on a key that does not exist. A provider with tags answers "no such key". A provider without tags answers `501` or `405`. The instance then reports `tagsSupported: false` in `/api/instances`. A denied or slow probe does not count as a missing feature. The page hides the tag controls from the first load and sends no tag with an upload. Garage accepts `x-amz-tagging` and drops the tags without a word, so the server refuses an upload with tags for an instance without tags: it answers `501` with `tagging_unsupported`.

An upload can carry tags. A direct upload takes the header `X-Amz-Tagging` (a query string such as `a=b&c=d`). `POST /api/uploads` takes the field `tags`. The server sends them to the provider in `x-amz-tagging`.

In a bucket with versioning, a tag request names the version of the object. The server reads it with a HEAD request. It is `null` for an object made before the versioning. SeaweedFS takes more than 10 seconds to read the tags of such an object when no version is named. A request that has `version` uses it as it is.

`GET /api/lifecycle` gives the lifecycle rules of the bucket in `rules`. A bucket without rules gives an empty list. The details of a file use the rules to tell what happens to the object, also for tags that the person did not save yet. The answer also has the state of the editor of the rules. Refer to [Edit of the lifecycle rules](#edit-of-the-lifecycle-rules).

The listing of the provider does not give tags, and S3 has no request that lists objects by tag. The tag column and the tag filter read the tags of the files on the page. They are in the settings of the listing, behind the gear. The tag column is on by default, and the tag filter is off. They read 4 files at the same time and at most 100. The tag filter hides rows of this page only.

## Versions in the interface

The details of a file have a picker of versions. It shows each version with its number (v1 is the oldest), its date, its size and the change of size since the version before. A choice opens the details of that version. The tags of an older version are read only.

The picker sorts the versions by date, the newest first. In S3, the current version is the newest version that holds data, so the picker shows no text "Current" or "Older". A small dot of the accent colour marks the version that the provider gives with `IsLatest`, with the tooltip "Current". The dot is the first column of the menu, and the first element of the button for the current version. Two versions with the same date (SeaweedFS gives the date to the second) put the current version first. When the current version is a delete marker, no version gets the dot.

Each row of the picker also has frameless icon buttons: Download, Restore and Delete. They show on hover and on keyboard focus. Restore is not on the current version and needs the write permission. Delete needs the delete permission. Restore and Delete open a confirmation first, and call `POST /api/versions/restore` or `DELETE /api/versions`. A delete that a lock refuses shows the message of `409 object_locked`. After a restore or a delete, the details open again, the preview page reads the versions and the preview again, and the listing reads its folder again.

A row of the picker is a `div` with the role `option`. It has a button for the choice and the action buttons. Up and Down go from row to row. Left and Right go from the choice to the action buttons of the row. Tab goes through every button of the menu, and Escape closes it. The pickers From and To of a comparison have no action buttons.

The picker does not list the delete markers. When the current version of an object is a delete marker, the listing does not show the object, so the page cannot open its versions. This is an old limit.

The comparison of two versions runs in the browser, in the preview page and not in a dialog. The address has the parameter `compare` (the older version) and the parameter `version` (the other version). The comparison takes the place of the preview. The browser back button and the button "Back to the preview" show the preview again. The picker of the versions of the preview stays in its place and becomes the version To. The version From appears at its left, and the statistics of the change are between the two. The header gets no new row. A button closes the comparison. The picker of versions opens this address. The page reads each version with `GET /s3?key=&version=`. A version larger than 2 MiB is not read. A version with a NUL byte or with a text that is not UTF-8 is not text: the page says so, and offers the comparison of the bytes.

The page picks the view from the extension of the key. The two versions show side by side by default, as on diffchecker.com. Other views are the unified list and the patch of git.

- Text: a diff of lines (Myers), with 3 lines of context. The changed words show in a changed line. A side that has no line shows hatched. A fold hides the unchanged lines, and a click shows them.
- JSON: the modes of the preview. Raw shows the text as stored. Beautify shows the indented text, with an option to sort the keys. Tree shows the paths with their state (added, removed, changed). A branch with a change is open, and a click opens the other branches. The format and the order of the keys are not changes. A version that is not JSON gets the text views.
- CSV, TSV and PSV: two tables side by side, or one table, with the rows that were added, removed or modified and the cells that changed. A removed row and an added row that have the same first cell, or most cells equal, are one modified row.
- Patch: the patch in the unified format of git, with `diff --git`, `index`, `---`, `+++`, the hunks `@@` and the line `\ No newline at end of file`. It applies with `git apply` and `patch -p1`. The buttons copy it or download it as `<name>.patch`.

The last line of a text with no line break is not the same line as the same text with a break, as in git.

The picker of versions is also in the bar of the preview page, with the same actions. It replaces the list of versions there, and the action "Versions" of the menus. The badge with the count in the listing opens the details of the file, with the picker. The page has no table of the versions.

A diff that needs more than 2500 edits shows as a replacement of the lines. The algorithms are in `assets/js/diff.js`. They have no DOM, and `test/frontend/diff-tests.js` tests them in Node.

## Settings of the listing

The gear of the toolbar opens the settings of the listing. The group "Columns" has Name, Tags, Versions, Size, Modified and "Encryption and lock". Each column is on by default. A column that the instance cannot have stays hidden: Tags without `tagsSupported`, Versions without `versioningSupported`. The group "Filters" has the tag filter, and a field gives the tags of the new uploads. The button "Reset to defaults" gives the defaults again. Its tooltip names what it resets.

The menu is as wide as its longest line needs, with a limit of 300 px and of the width of the window. A longer text wraps in the menu. The browser keeps the choices of the person in `stowr.listing`:

| Field | Value |
| --- | --- |
| `version` | `1` |
| `columns` | Only the columns that the person changed, for example `{"size": false}` |
| `tagFilterOn` | The state of the tag filter |
| `encryption` | The choice of "Encryption and lock" for each instance id. An instance without a choice has the setting on |

A saved choice wins over the default. The page ignores a stored value with another `version`.

The listing has no totals at its foot. The foot has the range of the page, the rows of each page and the pager.

## Information page of a bucket

Each bucket and each volume has one information page at `/-/<host>/<bucket>/?info`. The page is read only. The server sends `info.html` for `?info` on the root of an instance. An instance that does not exist, or that the person cannot see, gives the usual 404. The page uses `assets/js/info.js` and `assets/css/info.css`.

Only the bucket selector opens the page:

- The seven state icons of the trigger are one link to the page of the current bucket. The link is above the trigger and in its grid cell, after the name, before the usage. The trigger keeps a place for it. The rest of the trigger opens the list of the buckets.
- A host with one bucket or one volume has no row to open in the list: its only entry is always shown. In the list of the buckets, the seven icons of each entry are one link to the page of that bucket. The link is next to the button of the entry, not in it: a button cannot hold a link.
- Enter and Space open the link. Tab goes from the trigger to the link. In the list, the arrow Right goes from an entry to its link, and the arrow Left goes back.
- The accessible name of the link is "Information and permissions of this bucket", with the name of the bucket in the list, then the state of each icon. The link has no tooltip: the accessible name and the information page give each state.

The page has the header of the listing in one row, with the same parts: the brand at the left edge, the bucket selector, and the theme at the right edge. The link "Browse" is in the header, after the brand, with no chevron: it opens the files of the bucket. The parts are 8 px apart. The page has this visual language:

- The panel colour goes from edge to edge, with the gutter of the header (12 px, 10 px on a narrow window).
- The header has the bucket selector of the listing: one implementation for the two pages (`assets/js/switcher.js`), with the same markup, the same facts, the same reads and the same widths. One visible instance gives the static trigger on the two pages. On the listing, a bucket of the list opens in the listing; on the information page, it opens the information page of that bucket. The link of the state icons and of the usage opens the information page on the two pages. The page is one page with no tabs: three columns of cards, then the part "Lifecycle rules" under them.
- The page has no head: the selector names the bucket. A hidden heading (h1) names the page for a screen reader. A bucket that does not exist, or a failed read of the buckets, gives a small message with the reason, and no part.
- The first column has the card Identity. The second column has the cards Permissions, Data protection and Operations, one under the other. The third column has the storage: the cards of what the bucket holds, of the quota and of the storage. The columns have the widths 3 / 6 / 4: the second column is the widest. At 1000 px and less, the page has one column. The part "Lifecycle rules" has a heading (14 px) and spans the width under the columns.
- The cards Data protection and Operations (and the card of a volume) show pairs of a muted term and its value side by side; the pairs wrap in the width of the card. A card has a title (14 px) and a key-value list: a border, the keys in muted text, and a darker shade on each second row (12 px). On a phone, the key is above its value.
- The quota and the storage are gauges: the name and its chips, the percent (16 px) over a track of 8 px, a summary ("12 MB used of 20 GB") and facts.
- The states are chips of 18 px, in the colours of the state icons: green (on, allowed), orange (on, with a problem) and grey (off, not available, denied). The page has no red chip: a denied permission is grey, as in the selector.

Each row shows only when the server knows its value:

| Column | Content |
| --- | --- |
| Identity | The card Identity: the name, the kind of storage (AWS S3, MinIO, SeaweedFS, Garage, or "S3" for a provider that the probe did not recognise; "Mounted folder of the server" for a volume), the host, the bucket (not for a volume) and the region |
| Permissions and functions | The card Permissions: read, write and delete on one line, with the reason of a refusal in the title of its chip (the ceiling of the configuration, `server.access_mode = "force_read_only"` or a refusal of the provider). The card Data protection: versioning, the encryption of the writes, the default encryption of the bucket, if the provider applies SSE-S3, and Object Lock with its default retention. The card Operations: the state of the lifecycle rules (a chip "on" with the number of the rules that are on, else a chip "off"; the chip is a link to the part "Lifecycle rules"), tags, the edit of the metadata, the incomplete uploads and the SHA-256 checksum of the uploads. A volume has one card with the rows that apply to a folder |
| Storage | A card for what the bucket or the volume holds (with the source and the time of the count), a gauge for the quota (with the bytes left and the objects), and a gauge for the storage behind it (with its free bytes). An approximate number has a `~`, a chip "Approximate" and the reason. A full bucket gets the banner of the listing above the parts |
| Lifecycle rules | The editor of the rules (`assets/js/lifecycle.js`), in the page, at the anchor `#info-lifecycle`. Refer to [The editor in the page](#the-editor-in-the-page) |

The page never shows an endpoint, an admin URL, a token or the path of a volume on the server. A volume has the rows that apply to a folder, in one card.

The page reads `/api/instances`, `GET /api/usage` and `GET /api/lifecycle`, and no other endpoint. `/api/instances` gives `flavour`: the S3 provider that the probe at start recognised (`aws`, `minio`, `seaweedfs` or `garage`), or no field. Each read obeys the permissions of the person. A part that the person cannot read says "Not allowed", in a box with a dashed border. An instance without a `usage` block says "Not available", with the reason. While the count of a volume runs, the page reads the usage again every 3 seconds, 20 times at most.

The lifecycle rules are edited in the part "Lifecycle rules" of this page, and in no other place. The part is not shown for a volume, for a provider without lifecycle rules or for a person who cannot list the bucket. The editor has little text: one line "Not offered: <fields>" with the reasons in its title, and no note of the provider. The listing has no window of the rules and no `?lifecycle` address. The page builds the editor one time: a new read of the usage does not touch the form. A save sends the event `bb:lifecycle-changed`: the page reads the rules again, and the state icons of the selector (the hourglass and its count) change.

## Tags in the listing

A tag badge has a swatch. The colour comes from the key: one of 18 colours, chosen with the hash FNV-1a. A click on a badge filters the page by this tag. A second click clears the filter. Ctrl, Cmd or Shift adds the tag to the other words of the filter. A row shows three badges and a count for the other tags.

## Background jobs

Jobs are memory-only and use two workers. The manager admits no more than four active or queued jobs. Terminal results are retained only up to `job_history_limit`.

Supported job types are limited to operations that can stream with bounded memory:

- prefix copy, move, and delete;
- copy or move of one object in parts (`copy_object`, `move_object`);
- Insights statistics;
- per-object integrity verification;
- selected archive-entry extraction;
- selected archive-entry integrity verification;
- abort of the incomplete multipart uploads older than a number of days (`abort_multiparts`).

Insights uses a 100 ms synchronous fast path. A fresh completed result can be reused for 30 seconds. Longer work continues in memory and can be paused, resumed, or canceled while the process remains alive.

A job that deletes a prefix deletes the objects in batches of 1000 keys. It uses `DeleteObjects` of S3. The same applies to the deletion of the sources in a move of a prefix. The job counts a batch only after the provider deleted it. After a pause or a stop, the job continues after the last deleted key.

A provider that refuses `DeleteObjects` with `501` or `405` gets one request for each key. The server remembers this refusal. A key with a character that XML cannot carry always gets its own request. The GCS provider has no batch delete.

A key that an active Object Lock keeps does not stop a delete job or a move job. The job counts it in `locked`, keeps the first 20 keys in `lockedKeys`, and continues. Such a key is not in `processed` for a delete. The source of a move stays, and its copy exists. Refer to [Object Lock](#object-lock).

## Copy in parts

`CopyObject` of S3 refuses a source above 5 GiB. Thus a copy, a move, a rename and the restore of a version use a copy in parts from a threshold up. The threshold is 5 GiB minus 256 MiB. The engine is in the S3 backend (`src/s3_copy.go`), behind the optional interface `ObjectMultipartCopier`. The GCS backend does not have it: its copy has no such limit.

The engine does these steps:

1. It reads the source with `HEAD`, with `versionId` when the copy names a version. It gets the size, the ETag and the headers.
2. It reads the tags of the source. A source with `x-amz-tagging-count: 0` needs no request. A provider without tags (`501` or `405`) gives no tag.
3. It sends `CreateMultipartUpload` with `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`, the `x-amz-meta-*` headers and `x-amz-tagging`.
4. It sends one `UploadPartCopy` for each part of 256 MiB, with `x-amz-copy-source-range`. Four parts go at the same time. Above 10000 parts, the parts become larger.
5. It sends `CompleteMultipartUpload`.

Each part has `x-amz-copy-source-if-match` with the ETag of the source. A source that changes during the copy makes the copy fail. On an error or a cancel, the engine sends `AbortMultipartUpload`. The abort has its own time limit of 30 seconds, because the context of the copy can be canceled. No byte of the object goes through Stowr.

Each request of the engine takes a slot of the concurrency gates of the bucket. Thus the limits `max_concurrent_requests_per_storage` and `max_concurrent_storage_requests` apply. A request of the engine waits for its slot with no time limit.

The engine has an options structure: the source version, new metadata, new tags, other headers of `CreateMultipartUpload`, the ETag that the caller read, and a progress function. When the options give an ETag, a source with another ETag stops the copy with `412` before the upload starts. The metadata edit uses the same engine with new metadata.

`POST /api/copy` and `POST /api/rename` of one object first read its size with `HEAD`. Under the threshold, the copy stays one synchronous `CopyObject` request, and the answer is `200`. From the threshold up, the answer is `202` with a job `copy_object` or `move_object`. The job has `totalBytes`, the size of the source, and `copiedBytes`, which grows after each copied part. A move deletes the source after the copy. A pause or a cancel aborts the multipart upload. A resume starts the copy again from the first part. The jobs of one object use the background request budget (`max_background_storage_requests`).

A prefix copy and a prefix move use the size of each object in the listing. An object from the threshold up goes in parts. The job counts `copiedBytes`, with the parts of the object in progress.

The restore of a version from the threshold up uses the engine with the version as the source. It is a synchronous request, as before.

The threshold and the part size are unexported variables. Go tests set them. The test image sets them at build with `-ldflags -X main.multipartCopyThresholdBuild=<bytes> -X main.multipartCopyPartSizeBuild=<bytes>` (Docker build arguments `MULTIPART_COPY_THRESHOLD` and `MULTIPART_COPY_PART_SIZE` of `test/browser/Dockerfile`). These are not configuration keys. A release build leaves them empty. A value that is not an integer in its range stops the program at start. The log shows each value that the build changed.

## Edit of the metadata

`GET /api/metadata?key=&version=` gives the stored metadata of an object, or of one version: the standard headers `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`, and the user metadata (`x-amz-meta-*`). The server reads them with a HEAD request to the provider. The gateway `/s3` puts `private, no-store` in front of the `Cache-Control` of the provider, so the page does not read the values from the gateway. The answer also has `etag`, `lastModified`, `version`, `size`, `multipartETag` (the ETag has the form `<digest>-<parts>`), `copyInParts` (the save copies the object in parts) and `maxMetadataBytes`.

`PUT /api/metadata` replaces the metadata. The body has `key`, `version` (optional), `etag` and `lastModified` (the values that the form read), `headers` and `metadata` (a list of `key` and `value`). The body gives the complete state: a header or a user metadata that the body does not give is removed. The answer is the new state of the current object. The server checks each value before it sends a request:

- The six headers are a closed set. An unknown field in `headers` or in the body gives `400 invalid_json`.
- Each value has 1 to 1024 printable ASCII characters, with no space at the start or the end. `Content-Type` is a media type, `Content-Disposition` is `inline` or `attachment` with parameters, `Content-Encoding` and `Content-Language` are lists of tokens, `Cache-Control` is a list of directives, `Expires` is an HTTP date in GMT.
- A user metadata key has lowercase letters, digits, `.`, `_` and `-`, and starts with a letter or a digit. Each key is unique. A value has printable ASCII characters and is not empty. The keys and the values have 2048 bytes at most in total, as in S3.

A wrong value gives `400 invalid_metadata` with the name of the field. The server then does these steps:

1. It reads the object (or the version) with HEAD. An ETag or a modification date that is not the value of the form gives `412 object_changed`. SeaweedFS, MinIO and Garage keep the ETag when only the metadata changes, so the date is necessary.
2. Under the threshold of the copy in parts, it sends `CopyObject` onto the same key with `x-amz-metadata-directive: REPLACE`, `x-amz-tagging-directive: COPY` and `x-amz-copy-source-if-match` with the ETag of the form. A provider answer `412` also gives `412 object_changed`.
3. From the threshold up, it uses the engine of the copy in parts, with the new metadata at `CreateMultipartUpload`, the tags of the source and the ETag of the form.

The copy also sends the storage class, the website redirect and the server-side encryption of the source again, when the HEAD answer has them. Else AWS would change them. An object with an encryption key of the customer (SSE-C) gives `400`: Stowr does not have the key.

A request with `version` copies that version onto the key. The version becomes the current version, as a restore does. The tags of that version stay.

The copy needs the write permission of the instance. No new permission exists.

The form is a grid of labels and fields. Each field has the width of its value, and 100% at most on a narrow window. `BB.suggest` (`assets/js/common/suggest.js`) adds the suggestions to a text field with the ARIA combobox pattern. The popover is in the large window, with the look of the menu of the version picker. The typed text filters the values, and a free value stays possible. Down and Up move the mark, Enter chooses, and Escape closes the popover and not the window. The first value of Content-Type is the type that `detect.js` guesses from the name.

At start, the server probes each instance that has the write permission: a copy with REPLACE of an object that does not exist, with an ETag that no object has. A provider with `CopyObject` answers "no such key" and writes nothing. A provider without it answers `501` or `405`, and the instance then reports `metadataEditSupported: false` in `/api/instances`. A later `501` of an edit has the same effect. The GCS backend has no edit: it answers `501 metadata_unsupported`, and the page hides the button.

The details of a file have the section "Metadata" with the stored values and a frameless "Edit" button. The form is in the same window, with Save and Revert, as the tags section. Before the save, the form tells what changes. The browser tests check each sentence on each provider of the test stack:

| Sentence of the form | When | SeaweedFS 4.41 | MinIO 2025-10-15 | Garage v2.4.1 |
| --- | --- | --- | --- | --- |
| The modification date changes. | Always | Yes | Yes | Yes |
| A new version is created. | Versioning is on | Yes | Yes | No versioning: the sentence is not shown |
| This version becomes the current version. | The form shows an older version | Yes | Yes | No versioning |
| The ETag changes: Stowr copies an object of this size in parts. | `copyInParts` | Yes (`<digest>-<parts>`) | Yes | Yes |
| The ETag can change: the object was uploaded in parts. | `multipartETag`, under the threshold | No: the ETag stays | Yes: the ETag becomes the MD5 of the object | No: the ETag stays |
| (No sentence) | One part, under the threshold | The ETag stays | The ETag stays | The ETag stays |

In the test stack, each multipart object is above the threshold of 8 KiB, so the page copies it in parts. The row of a multipart object under the threshold comes from S3 requests to the three providers (the table of [test/README.md](../test/README.md)).

On all three providers, the tags stay. A copy with REPLACE and no `Content-Type` gives `binary/octet-stream` on SeaweedFS and MinIO, and no `Content-Type` on Garage. `x-amz-copy-source-if-match` with another ETag gives `412` on all three. For a copy of an older version, the three compare it with the ETag of that version.

## Cleanup of incomplete multipart uploads

An incomplete multipart upload keeps its parts on the provider until a person completes it or aborts it. The parts use storage, but no listing shows them. The code is in `src/multiparts.go`, behind the optional interface `MultipartUploadLister` of the S3 backend.

`GET /api/multiparts?prefix=&max=&token=` gives the incomplete uploads under `prefix`. The answer has `uploads`, `nextToken`, `now` (the clock of the server), `minimumAbortAgeHours` and `partsPageSize`. Each upload has `key`, `uploadId`, `initiated`, `parts`, `bytes`, `partsAtLeast`, `partsUnknown` and `active`. `max` is from 1 to 500, and the default is 100. Another value gives `400 invalid_multipart_request`.

The server reads the list with these rules:

- It sends `ListMultipartUploads` with no prefix. MinIO gives an empty list for a prefix that is not a full key. The server keeps the uploads under the root prefix of the instance and under `prefix`.
- It sends the next request with the markers that the provider sent, as the provider sent them. SeaweedFS sends only `NextUploadIdMarker`. Garage and AWS send `NextKeyMarker` and `NextUploadIdMarker`. MinIO ignores `max-uploads` and sends all the uploads in one page.
- `nextToken` has the markers and a position in the page. Thus the next answer can start in the middle of a page that has more uploads than the request asked for.
- One answer reads 10 pages of the provider at most. A provider that sends the same markers again gives `502 provider_paging`, not a loop.
- For each upload, the server sends one `ListParts` with `max-parts=1000`, four at the same time. An upload with more parts gets `partsAtLeast: true`: the count and the size are those of the first 1000 parts. An upload that the provider no longer knows (`404 NoSuchUpload`) is not in the answer.

`initiated` is the `Initiated` date of the provider. SeaweedFS gives no date. The page then shows "unknown" for the start and the age.

`POST /api/multiparts/abort` has two forms. The body gives one of them. Both forms, or none, give `400 invalid_multipart_request`.

- `uploads`, a list of 1 to 1000 `key` and `uploadId`, aborts each upload at any age. The page asks for a confirmation first. The answer is `200` with `aborted`, `skipped` and `failed`. An upload that the provider no longer knows counts as aborted.
- `olderThanDays` (1 to 36500) and `prefix` start the job `abort_multiparts`. The answer is `202` with the job. The job reads the whole list and aborts each upload under `prefix` that started before the cutoff. The cutoff is the time of the request minus `olderThanDays` days. A resumed job uses the same cutoff.

The job counts `aborted`, `skippedUndated`, `skippedRecent`, `skippedActive` and `failed` in `multipartAbort`. These safety rules apply:

- The job never aborts an upload without a start date. The provider does not let Stowr know its age.
- The job never aborts an upload younger than 24 hours, also when the cutoff is later.
- No request aborts an upload that the resumable-upload store of this process sends now (status `uploading`, `src/uploads.go`). The list shows such an upload with `active: true`.

`GET /api/multiparts` needs the read permission of the instance. An abort needs the read and the write permissions.

At start, the server sends `ListMultipartUploads` with `max-uploads=1` to each instance that has the read permission. A provider that answers `501` or `405` gets `multipartUploadsSupported: false` in `/api/instances`, and the page hides the item. The GCS backend has no list: it answers `501 multipart_unsupported`.

The item "Incomplete uploads" of the Actions menu opens a large window for the current folder, or for the bucket at the root. The window has a table with the key, the start, the parts, the size and the age of each upload. It has a checkbox and a frameless abort button in each row, "Abort selected", and "Abort everything older than N days" (N is 1 by default). The window tells how many uploads have no start date. A person without the write permission sees the list with no abort control. A note tells that a lifecycle rule can abort the incomplete uploads. Its link opens the part "Lifecycle rules" of the information page. Refer to [Edit of the lifecycle rules](#edit-of-the-lifecycle-rules).

## Edit of the lifecycle rules

`PutBucketLifecycleConfiguration` replaces the whole configuration of a bucket. The providers do not keep the same settings, and some drop a setting without an error. The code is in `src/lifecycle.go`, behind the optional interface `BucketLifecycleWriter` of the S3 backend. The GCS backend has no lifecycle rules: it answers `501 lifecycle_unsupported`.

### The state of the editor

`GET /api/lifecycle` gives `editor` next to `rules`. `editor` has these fields:

- `hash`: the SHA-256 of the configuration as the provider sent it, and of the indexes of the editable rules. A rule that becomes "kept as is" thus also changes the hash.
- `entries`: one entry for each rule, in the order of `rules`. An editable entry has `rule`. Each other entry has `xml` (the bytes of the rule) and `reason`.
- `fields` and `hidden`: the fields that the editor offers, and the reason for each other field.
- `provider`, `providerNote`, `versioning`, `rootPrefix`, `writable`, `readOnlyReason`, `maxDays` (36500) and `maxRules` (1000).

A rule is editable when it has an ID and only these elements:

- `Status`;
- a `Filter` with a `Prefix`, one `Tag`, or an `And` with a `Prefix` and one `Tag`;
- an `Expiration` with `Days`, or with `ExpiredObjectDeleteMarker`;
- a `NoncurrentVersionExpiration` with `NoncurrentDays`, and an `AbortIncompleteMultipartUpload`.

Each field of the rule must also be offered for the instance. For an instance with a root prefix, the prefix of the rule must start with the root prefix. The editor shows the prefix after the root prefix, and the server adds the root prefix again. Thus a new rule of such an instance never applies to the whole bucket.

The editor keeps each other rule as is: a transition, an expiration on a date, a size filter, a filter with two tags or more, the old `Prefix` element outside of `Filter`, `NewerNoncurrentVersions`, or an XML attribute. It also keeps a text with spaces at the start or at the end, a tag key with `=` or `,`, and a tag value with `,`: the form cannot show them exactly. The window shows such a rule as a read-only row with the note "Kept as is", the reason and its XML. For an instance with a root prefix, a rule outside of the root prefix shows no XML and no filter: it belongs to another part of the bucket.

### The save

`PUT /api/lifecycle` has `instance`, `hash` and `rules`, the complete list of the editable rules. Each rule has `id`, `enabled`, `prefix`, `tag`, `expirationDays`, `noncurrentDays`, `abortMultipartDays` and `expiredDeleteMarker`. A zero number of days means "no such action". The server does these steps under a lock of the instance:

1. It reads the configuration again and compares its hash with `hash`. A different hash gives `409 lifecycle_changed`. The page then offers "Reload the rules".
2. It checks the rules. A refusal gives `400 invalid_lifecycle`, or `400 lifecycle_field_unsupported` for a field that the editor does not offer.
3. It writes the document: the start tag of the root as the provider sent it, the editable rules, then each other element of the root with its bytes. An editable rule that the save does not change also keeps its bytes.
4. It reads the configuration again and compares each rule with the rule that it sent.

The checks of step 2 are the limits of S3 and of AWS:

- A bucket has 1000 rules at most. An ID has 1 to 255 characters, and no other rule has the same ID. MinIO refuses two rules with one ID.
- A number of days is from 1 to 36500. A rule has one action or more.
- A rule cannot have `Days` and `ExpiredObjectDeleteMarker` in one `Expiration`.
- A rule with a tag filter cannot abort incomplete uploads or remove delete markers. AWS refuses these rules.
- A tag follows the rules of the object tags.

MinIO and Garage refuse a configuration without a rule. When no element stays, the server sends `DeleteBucketLifecycle` in place of the write.

The comparison of step 4 ignores the spaces, the namespaces and the order of the children: MinIO changes the order in `And`. A rule without an ID matches a rule with the same content, because MinIO gives an ID to such a rule. For each difference, the answer has a sentence in `warnings`, for example "The storage provider dropped the tag filter retention=30d of the rule "logs". The rule now applies to every object under logs/". The server restores nothing. The page shows the sentences and the rules as the provider keeps them. Then this process does not offer the dropped field again for the instance.

A refusal of the write by the provider gives these answers:

- `403` gives `403 lifecycle_write_denied`. `501` or `405` gives `501 lifecycle_write_unsupported`. The editor of the instance is then read only, with the reason, until a later write works.
- Another `4xx` gives `400 lifecycle_refused`. The rules of the bucket do not change.

When the read of step 4 fails, the answer is `200` with a sentence in `warnings` and no `editor`: the rules are saved, and the page reads them again.

S3 has no condition on `PutBucketLifecycleConfiguration`. The lock and the hash stop a save over a change of another window of this process, or over a change before the save. A change by another S3 client between the check and the write is lost.

### The fields of each provider

Stowr never writes to a bucket to find what the provider keeps. It uses a table for each provider that it knows. The facts come from requests to the providers of the test stack (refer to `test/README.md`) and from the documentation of AWS S3.

At start, the server reads the configuration of each instance with the read permission. A provider that answers `501` or `405` gets `lifecycleSupported: false` in `/api/instances`, and the page hides the item. The server also sends one `GetObject` for the key `.stowr-provider-probe` under the root prefix. The object does not exist. The answer tells the provider:

- An endpoint under `amazonaws.com`, or the header `Server: AmazonS3`, is AWS S3.
- `Server: MinIO` is MinIO. `Server: SeaweedFS <size> <version>` is SeaweedFS.
- Garage v2.4.1 sends no `Server` header and no request id. Its error document has a `Region` element and no `RequestId` element.
- Any other answer is a provider that Stowr does not know.

| Field | AWS S3 | SeaweedFS | MinIO | Garage | Provider that Stowr does not know |
| --- | --- | --- | --- | --- | --- |
| Expiration after N days, with a prefix | Yes | Yes | Yes | Yes | Yes |
| Tag filter | Yes | Yes | Yes | No: dropped, and the rule applies to every object under the prefix | Only when a rule of the bucket has one |
| Expiration of noncurrent versions | Yes | Yes | Yes | No: dropped | Only when a rule of the bucket has one |
| Abort of incomplete multipart uploads | Yes | Yes | No: `400` in a rule with this action only, dropped in a rule with another action | Yes | Only when a rule of the bucket has one |
| Removal of expired delete markers | Yes | Yes | Yes | No: `400` | Only when a rule of the bucket has one |

A field also needs its condition. The tag filter needs `tagsSupported`. The noncurrent versions and the delete markers need the status `Enabled` or `Suspended` from `GetBucketVersioning`. The reason of the provider table comes first in `hidden`.

The reason for this decision: a provider can drop a field without an error, as Garage does. A dropped tag filter makes a rule apply to every object under its prefix. Thus the editor offers only the expiration after N days for a provider that it does not know. A rule of the bucket that already uses a field shows that the provider keeps it, so the editor then offers the field. After each save, the comparison of step 4 finds a dropped field, and the editor does not offer it again.

The effect of the rules also depends on the provider. SeaweedFS 4.41 keeps all the fields above and applies the rules with its lifecycle worker, in a daily pass. Its TTL fast path at write is off by default and never applies to a bucket with versioning. In the test stack, an object written after a rule has no TTL. Garage applies the expiration and the abort of incomplete uploads in a daily pass. The editor shows these notes.

### The editor in the page

The part "Lifecycle rules" of the information page (`#info-lifecycle`) has the editor. It is a part of the page, not a window, and it opens no dialog. It has two columns under the three columns of the page. At 1000 px and less, the list goes under the form.

- At the left, one form (`[data-lifecycle-form]`): "New rule", or "Edit <id>" for a rule of the list. It has the name, the prefix and the tag filter side by side, then one line for each action with a short label and a number of days: "Delete objects after 30 d", "Delete noncurrent versions after 30 d", "Abort incomplete uploads after 7 d" and "Remove expired delete markers". The title of each line gives the whole sentence. The form shows only the fields that the provider keeps. One line "Not offered" names the other fields, with their reasons in its title. "Add" puts a new rule in the list. For a rule of the list, "Update" changes it and "Cancel" goes back to a new rule. The form checks the rule before it goes in the list.
- At the right, the rules of the bucket, one compact row each: "Enabled", the ID, a short summary (the prefix, each action with its days, the tag), "Edit" and "Delete". "Delete" asks "Delete?" in the row, with "Delete" and "Cancel". A rule that the editor does not know has a read-only row with "On" or "Off" and the badge "Kept as is", with its reason in the title. Over the list, "Save" sends the whole list, "Revert" puts back the rules of the server, and "Reload" reads them again after a conflict. A change that is not saved gives the line "Not saved yet".

Before the save, a confirmation in the page names each action that deletes data and that is new or changed, with its prefix and its days. An example is "Objects under logs/ older than 30 days will be deleted." The confirmation is a box under the rules, in place of the buttons, with "Save the rules" and "Cancel". A save that adds or changes no such action asks nothing, for example a rule that becomes disabled.

The editor is read only, with the reason, when `writable` is false or when the person cannot write in the bucket. A volume, a provider without rules and a person who cannot read the bucket get no part "Lifecycle rules". After a save, the details of a file read the rules again.

The note of the window "Incomplete uploads" has a link to the part "Lifecycle rules" of the information page. Where the provider keeps the abort of incomplete uploads, the link "Add a lifecycle rule" is `?info&lifecycle-abort=<folder>#info-lifecycle`. The page then puts the rule `abort-incomplete-uploads` in the form, ready to add: it aborts the uploads under the folder 7 days after their start. The page removes the parameter from the address. The person adds the rule, then saves. On MinIO, the note gives the reason, and the link "Lifecycle rules" opens the part without a new rule.

`GET /api/lifecycle` needs the read permission of the instance. `PUT /api/lifecycle` needs the read and write permissions of the instance. A rule deletes objects. Thus a new or changed enabled rule that deletes objects, noncurrent versions or delete markers also needs the delete permission of the instance. The abort of incomplete uploads needs the write permission only, as in the window "Incomplete uploads".

## Object Lock

Object Lock keeps one version of an object: a legal hold, or a retention until a date. The code is in `src/objectlock.go`, behind the optional interface `ObjectLockManager` of the S3 backend. A bucket gets Object Lock only at its creation, and such a bucket always keeps versions. Each lock applies to one version.

### Detection

At start, the server reads `GetObjectLockConfiguration` of each instance with the read permission. The request is a read. Only the answer `ObjectLockEnabled: Enabled` gives `objectLockSupported: true` in `/api/instances`. These answers give `false`, and the page shows nothing of Object Lock:

- a bucket without Object Lock: `404 ObjectLockConfigurationNotFoundError` on SeaweedFS and MinIO;
- a provider without Object Lock: `501 NotImplemented` on Garage, or `405`;
- any other failure, for example a refusal or a timeout. The instance then hides the feature until the next start.

The GCS backend has no Object Lock: the routes answer `501 object_lock_unsupported`.

### Read

`GET /api/object-lock?key=&version=` gives the lock of one version. An empty `version` reads the current version: a HEAD request gives its id. The server reads `GetObjectRetention`, `GetObjectLegalHold` and `GetObjectLockConfiguration`. The answer has these fields:

| Field | Value |
| --- | --- |
| `version` | The version that the lock applies to. A change names it. |
| `retention` | `null`, or `mode` (`GOVERNANCE` or `COMPLIANCE`), `retainUntil` (RFC 3339, UTC) and `active` (`false` for a date in the past). |
| `legalHold` | `true` when the legal hold is on. |
| `bucketDefault` | `null`, or the default retention of the bucket: `mode` and `days` or `years`. |
| `now` | The date of the server. |
| `maxRetentionDays` | 36500, the latest date that Stowr sets, in days from now. |

The providers give "no lock" with different answers. SeaweedFS gives `404 ObjectLockConfigurationNotFoundError` for a version without a retention, and `404 NoSuchObjectLegalHold` without a legal hold. MinIO gives `400 NoSuchObjectLockConfiguration` for both, and AWS S3 gives `404 NoSuchObjectLockConfiguration`. The server reads each of these codes as "no lock". `NoSuchKey` stays an error.

### Change

`PUT /api/object-lock` changes the lock of one version. The body has `key`, `version` (required), and `legalHold`, `retention` (`mode` and `retainUntil`) or both. The server checks the body before a request to the provider:

- `mode` is `GOVERNANCE` or `COMPLIANCE`, in uppercase. No other value is accepted.
- `retainUntil` is an RFC 3339 date in the future and at most 36500 days from now. Stowr sends it with a precision of one second.
- `version` has 1 to 1024 printable ASCII characters.

A wrong value gives `400 invalid_object_lock`. The server then reads the lock of the version and applies these rules:

| Current retention | Change | Result |
| --- | --- | --- |
| None, or a date in the past | Any mode, a date in the future | Sent |
| Governance | A later date, or the same date, in Governance | Sent |
| Governance | An earlier date, or the mode Compliance | Needs `bypassGovernance: true` in the body. The server then sends `x-amz-bypass-governance-retention: true`. Without it: `409 governance_bypass_required` |
| Compliance | A later date, or the same date, in Compliance | Sent |
| Compliance | An earlier date, or the mode Governance | `409 compliance_retention`. Nobody can shorten a Compliance retention, not even the owner of the bucket |

The comparison of the dates uses the seconds: SeaweedFS keeps the seconds, and MinIO keeps the milliseconds. The server sends the retention first and then the legal hold. It does not send a part that does not change. A provider that refuses a change gives `409 object_lock_refused` with the status and the code of the provider. The answer of a save is the new state. The server writes one log line for each change. The line has the key, the version, the change and the use of the bypass.

The read needs the read permission of the instance, and a change needs the write permission. No new permission exists. The bypass also needs the bypass permission of the provider for the credentials of the instance (on AWS S3, `s3:BypassGovernanceRetention`). Stowr does not set a retention at upload, and does not change the default retention of a bucket.

### Deletes that a lock refuses

The providers refuse the delete of a locked version with different answers. SeaweedFS gives `403 AccessDenied`. MinIO gives `400 InvalidRequest` with the message "Object is WORM protected and cannot be overwritten". `DeleteObjects` gives the same codes for each refused key, in the XML body of a `200` answer. The status alone does not tell a lock from a missing permission. Thus, after a refusal with one of these codes, the server reads the lock of the version. Only a legal hold or an active retention makes the error `409 object_locked`. Its message names the lock and tells what the person can do. Any other refusal stays the error of the provider.

A refusal for a lock does not change the delete permission that the instance reports. A delete without a version id is not refused on SeaweedFS and MinIO: the provider writes a delete marker, and the locked version stays. This is the correct behaviour of S3. Thus, on a bucket with `objectLockSupported`, the confirmation of the delete of a file or of a folder has the sentence "This bucket has Object Lock. The delete adds a delete marker. A version that a lock protects stays in the bucket." A warning after the delete tells the same. The code also covers a provider that refuses such a delete:

| Operation | Effect of a lock |
| --- | --- |
| Delete of a version (`DELETE /api/versions`, `DELETE /s3` with `version`) | `409 object_locked` |
| Delete of an object (`DELETE /s3`) | `409 object_locked` |
| Delete of a folder (job `delete_prefix`) | The job counts the key in `locked` and continues |
| Move of a folder (job `move_prefix`) | The copy exists, the source stays. The job counts it in `locked` |
| Rename or move of one object (`POST /api/rename`, job `move_object`) | `409 object_locked`: "The copy is made, but the source stays." |

For a batch delete, the server reads the lock of up to 100 refused keys of each batch. The other refused keys stay a failure of the job.

### The lock in the listing, the bucket selector and the preview

The HEAD answer of a version gives its lock: `x-amz-object-lock-mode`, `x-amz-object-lock-retain-until-date` and `x-amz-object-lock-legal-hold`. The gateway `/s3` gives these headers. The listing reads them from the HEAD request that it sends for the encryption (refer to "Marks in the bucket selector, the listing and the preview"). Thus the lock adds no request. A file with a legal hold, or with a retention whose date is in the future, gets a small closed padlock at the right of its name. The tooltip gives the lock:

| Lock | Tooltip |
| --- | --- |
| Governance retention | "Object Lock: Governance until 2027-10-20 10:00 UTC" |
| Compliance retention | "Object Lock: Compliance until 2027-10-20 10:00 UTC" |
| Legal hold | "Object Lock: legal hold" |
| Both | "Object Lock: Governance until 2027-10-20 10:00 UTC, legal hold" |

The page makes the tooltip at each drawing. A retention whose date is past does not count. A change of the lock in the section "Retention" makes the listing read that file again: the change keeps the ETag and the date of the file.

The bucket selector shows a closed padlock in the facts of a bucket with `objectLockSupported`. The tooltip says "Object Lock is on". The probe at start also reads the default retention of the bucket. `/api/instances` gives it in `objectLockDefault` (`mode`, and `days` or `years`), and the tooltip then adds "Default retention: Governance, 30 days". A change of the default retention after the start shows after the next start. The bar of the preview page shows the padlock of the version on the page, from the HEAD request of the preview.

On AWS S3, the HEAD answer has the lock headers only for a caller with `s3:GetObjectRetention` and `s3:GetObjectLegalHold`. Without them, the listing shows no padlock.

### The section "Retention"

When `objectLockSupported` is true, the details of a file have the section "Retention". It shows the mode, the retain-until date, the legal hold and the default retention of the bucket, for the version of the details. A frameless "Edit" button opens the form in the same window, with Save and Revert, as the sections "Metadata" and "Tags". A person without the write permission sees no button.

The form refuses an earlier date for an active Compliance retention, and says why, with no request. A change that needs the bypass of a Governance retention opens a confirmation first. It tells what the bypass does. A delete that a lock refuses shows the message of the server. A folder job with locked keys tells how many keys stay, and why.

## Encryption

The code is in `src/encryption.go`. Stowr shows the server-side encryption that the provider reports. It can also ask for SSE-S3 for each object that it writes. It does not support SSE-C or SSE-KMS for the writes. It never shows, asks for or keeps a key of the client.

### Encryption of the writes

The key `encryption` of a `bucket` block is `""` (the default) or `"sse-s3"`. Any other value stops the start, and the error says that SSE-C and SSE-KMS are not supported. A `gcs` bucket with `"sse-s3"` stops the start: the GCS API has no such header. Refer to [configuration-reference.md](configuration-reference.md).

With `"sse-s3"`, the S3 backend adds `x-amz-server-side-encryption: AES256` to each request that makes an object:

| Operation | Request with the header |
| --- | --- |
| Upload in one request, empty file, extraction of an archive | `PutObject` |
| Upload in parts | `CreateMultipartUpload` |
| Copy, move and rename | `CopyObject` |
| Copy in parts (copy, move, rename, metadata edit, restore) | `CreateMultipartUpload` |
| Metadata edit | `CopyObject` with `REPLACE` |
| Restore of a version | `CopyObject` |

`UploadPart`, `UploadPartCopy` and `CompleteMultipartUpload` do not send it: S3 takes the encryption of a multipart upload from its start. A request that already names an encryption keeps it. With `""`, the server sends no header, and the default encryption of the bucket applies.

A copy without the header gets the default encryption of the destination bucket, not the encryption of the source. On SeaweedFS and MinIO, a copy of an SSE-S3 object without the header is not encrypted. Thus, with `"sse-s3"`, each copy sends the header. The metadata edit and the restore of a version replace an object with itself. They also send the SSE-S3 or SSE-KMS headers of their source (`x-amz-server-side-encryption`, the KMS key id and the S3 Bucket Key), with any value of the key. Thus an SSE-KMS object stays SSE-KMS after a metadata edit or a restore.

The providers of the test stack give these results:

| Provider | `x-amz-server-side-encryption: AES256` |
| --- | --- |
| SeaweedFS 4.41 | Encrypts when the S3 server has a key encryption key (`WEED_S3_SSE_KEK`, or `s3.sse.kek` in `security.toml`). Without it: `500 InternalError` |
| MinIO RELEASE.2025-10-15T17-29-55Z | Encrypts when MinIO has a KMS (`MINIO_KMS_SECRET_KEY` or a KES server). Without it: `501 NotImplemented` |
| Garage v2.4.1 | Accepts the header with `200` and does not encrypt |

A provider that refuses the header refuses the write: no object is written without its encryption. Garage does not refuse. The probe at start finds the provider, and the server writes a warning for a Garage bucket with `"sse-s3"`.

### Display

`GET /api/media-info` gives these headers of the HEAD answer: `x-amz-server-side-encryption`, `x-amz-server-side-encryption-aws-kms-key-id`, `x-amz-server-side-encryption-bucket-key-enabled` and `x-amz-server-side-encryption-customer-algorithm`. The download gives them too. The MD5 of a key of the client does not go to the page.

A provider refuses the HEAD request of an SSE-C object without its key, with `400` and no body. The server then reads one byte with `GET`, and the text of the refusal gives the reason. SeaweedFS says "Customer provided keys", Garage says "Object is encrypted", AWS S3 and MinIO say "Server Side Encryption". For such a refusal, `GET /api/media-info` answers `200` with `customerKey: true`, and the size, the ETag and the date of the listing. MinIO accepts SSE-C only over TLS.

`GET /api/encryption?instance=` gives `uploads` (the value of the key) and `bucketDefault`. `bucketDefault` is `null`, or `algorithm` (`AES256`, `aws:kms` or `aws:kms:dsse`), `kmsKeyId` and `bucketKeyEnabled`, from `GetBucketEncryption`. At start, the server reads it for each instance with the read permission. An answer, or `404 ServerSideEncryptionConfigurationNotFoundError`, gives `bucketEncryptionSupported: true` in `/api/instances`. `501` (Garage), `405` and any other failure give `false`, and the route then answers `501 bucket_encryption_unsupported`. The route needs the read permission of the instance.

The details of a file have the section "Encryption" after "Storage object". It shows the encryption of the object: SSE-S3, SSE-KMS with its key, or "Key of the client, SSE-C". It also shows the default encryption of the bucket. For an SSE-C object, a note tells that the preview, the download and the copy need the key, and the details have no metadata section. The section stays hidden when the provider reports no encryption.

The preview page of an SSE-C object shows the name and the path of the object, and the message "This object is encrypted with a key of the client (SSE-C). The browser cannot read it without that key." The link to the original and the downloads give the same message. The page uses this message only when the HEAD request fails with `400` and `GET /api/media-info` gives `customerKey: true`. Any other `400` stays an error.

### Marks in the bucket selector, the listing and the preview

`/api/instances` gives three facts for each instance:

| Field | Value |
| --- | --- |
| `encryption` | The key `encryption` of the bucket: `""` or `"sse-s3"` |
| `defaultEncryption` | The algorithm of the default encryption of the bucket at start (`AES256`, `aws:kms`), or `""` |
| `encryptionApplied` | For `"sse-s3"` only: `"yes"` on AWS S3, MinIO and SeaweedFS, `"no"` on Garage, `"unknown"` for a provider that Stowr does not know |

The mark of the encryption is a key (Tabler `key`). The mark of Object Lock is a closed padlock (Tabler `lock`). The bucket selector shows a key in the facts of a bucket that encrypts. The tooltip says "Encrypted: SSE-S3 for each new object" or "Default encryption of the bucket: AES256". The key tells the truth of the provider. Garage accepts the header and does not encrypt, so a Garage bucket with `"sse-s3"` gets a key in the colour of a warning. For a provider that Stowr does not know, the tooltip says "SSE-S3 requested; the provider may not encrypt". A bucket with Object Lock also gets its padlock: the two marks are separate, and each shows only when it is true.

A listing does not give the encryption of an object. Thus the listing reads it with a HEAD request for each file on the page, as it reads the tags: 4 requests at the same time, 100 files at most. A file waits with a mark, and an answer stays for the ETag and the date of the file. An encrypted file gets a small key at the right of its name, in the cell of the name. The padlock of a lock follows the key. The tooltip of the key names the encryption: SSE-S3, SSE-KMS with its key, or "key of the client" (SSE-C). A HEAD request of an SSE-C object fails with `400`, and `GET /api/media-info` then confirms the key of the client. Such a file gets no padlock, because its HEAD answer gives no header. A file without an encryption gets no key.

The column "Encryption and lock" of the settings starts or stops these requests for each bucket. The browser keeps the choice in `stowr.listing`, in the field `encryption`. Without a choice, the setting is on for each bucket. The HEAD request of a file gives its encryption and its lock, so the lock adds no request.

The bar of the preview page shows the key and the padlock of the version on the page, next to its size, with the tooltips of the listing. The marks come from the HEAD request that the preview sends for the version: a version in `?version=` shows its own encryption and its own lock. An SSE-C object gets the key with the tooltip "Encrypted with a key of the client (SSE-C)". An entry of an archive and the comparison of two versions show no mark.

## Usage and quota of a bucket

The code is in `src/usage.go`, with one reader for each source: `src/usage_garage.go`, `src/usage_seaweedfs.go` and `src/usage_minio.go`. The S3 API gives no usage and no quota of a bucket. Each provider gives them in its own admin API. The `usage` block of an `auth` block names the source and its URLs. The server never guesses the source from the provider. Refer to [CONFIGURATION.md](CONFIGURATION.md).

### Sources

| Source | Requests | Used bytes | Objects | Limit |
| --- | --- | --- | --- | --- |
| `garage` (v2.4.1) | `GET <endpoint>/v2/GetBucketInfo?globalAlias=<bucket>` with `Authorization: Bearer <token>` | `bytes` | `objects` | `quotas.maxSize` and `quotas.maxObjects`. `null` is no limit |
| `seaweedfs` (4.41) | `GET <endpoint>/vol/status` (the master) and `GET <filer_endpoint>/buckets/<bucket>?metadata=true` (the filer), with no credential | The sum of `Size - DeletedByteCount` of the volumes whose `Collection` is the bucket, one time for each volume id | Unknown | `Quota` of the entry of the bucket. 0 or a negative value is no limit |
| `minio` (RELEASE.2025-10-15T17-29-55Z) | `GET <endpoint>/minio/admin/v3/get-bucket-quota?bucket=<bucket>` and `GET <endpoint>/minio/admin/v3/datausageinfo`, signed with SigV4 (service `s3`) and the S3 credentials of the `auth` block | `bucketsUsageInfo.<bucket>.size` | `bucketsUsageInfo.<bucket>.objectsCount` | `size`, else `quota`, of the quota of the bucket. 0 is no limit |

AWS S3 has no quota of a bucket: give no `usage` block. GCS has no reader: a `usage` block in a `gcs` auth block stops the start.

These facts come from the test stack:

- Garage counts a write at once on one node. In a cluster, the counters follow after a short delay. A wrong token gives `403`, and an unknown bucket gives `404`. `garage bucket set-quotas <bucket> --max-size 20GiB --max-objects 100000` sets the quota.
- SeaweedFS gives the size of its volumes, not the size of the objects. The bytes of a deleted file stay in its volume until a compaction, and the reader subtracts them. Each volume adds 8 bytes. The volume servers send their sizes to the master every few seconds. `weed shell` with `s3.bucket.quota -name=<bucket> -op=set -sizeMB=<n>` sets the quota, and `s3.bucket.quota.enforce -apply` makes a bucket over its quota read only. The reader expects the buckets in the folder `/buckets` of the filer (the default). The master and the filer have no credential in this reader: keep them on a closed network.
- MinIO counts the usage in a background scan, about one time each minute on a small server. A zero `lastUpdate` is a server that has not counted yet: the read fails. The quota works in this release: `PUT /minio/admin/v3/set-bucket-quota?bucket=<bucket>` with `{"quota": <bytes>, "quotatype": "hard"}` sets it, and MinIO refuses a write over it. A bucket without a quota gives `200` with `"quota": 0`. Older releases answer `404 XMinioAdminBucketQuotaNotConfigured`: the reader takes it as no limit. The credentials need the admin actions `admin:GetBucketQuota` and `admin:DataUsageInfo` (for example the policy `consoleAdmin`, or the root user).

The numbers of SeaweedFS and MinIO are approximate. The numbers are for the whole bucket, also for an instance with a root prefix.

### Capacity of the storage

The usage of a bucket also gives the capacity of the storage behind it: the disks or the nodes of the provider, for all
its buckets. The reader of the `usage` block reads it with the same endpoint, the same credentials, the same time limit
and the same size limits. No key of the configuration changes. The two parts of a read run at the same time, and a
failure of one part does not hide the other part. The numbers are raw: the replication or the erasure code of the
provider uses more than one byte of the disks for one byte of an object.

| Source | Requests | Scope | Total, used, free |
| --- | --- | --- | --- |
| `garage` | `GET <endpoint>/v2/GetClusterStatus` with the admin token | `cluster` | The sum of `dataPartition.total` and `dataPartition.available` of each node with a role and a capacity. A gateway node stores no data. Used = total - available |
| `seaweedfs` | `GET <endpoint>/dir/status` (the master), then `GET http://<volume server>/status` for each volume server, 4 at a time, 64 at most | `disks` | The sum of `all`, `used` and `free` of the `DiskStatuses` of each volume server, one time for each folder |
| `minio` | `GET <endpoint>/minio/admin/v3/storageinfo`, signed | `disks` | The sum of `totalspace`, `usedspace` and `availspace` of each drive with the state `ok` |
| volume (`statfs`) | `statfs` of the root of the volume | `filesystem` | Used = size - available, total = size, free = available |

These facts come from the test stack:

- Garage v2.4.1 does not give the replication factor in its admin API: `GetClusterLayout` gives `zoneRedundancy` only.
  The note of the answer says it. `/v1/status` answers `400`: the v1 API is no longer supported.
- The master of SeaweedFS 4.41 gives no bytes of the disks. `/dir/status` lists the volume servers with their
  addresses, and each volume server gives its disks in `/status`. A volume server with a loopback address (for example
  `weed server -ip=127.0.0.1`) is on the host of the master: the reader asks the host of the master at the port of the
  volume server. The volume servers must be on the closed network of the master.
- MinIO RELEASE.2025-10-15T17-29-55Z gives `StandardSCParity` in `Backend`. The note gives the parity when it is more
  than 0. `usedspace` + `availspace` can be less than `totalspace`: the file system keeps blocks for itself. The
  credentials need the admin action `admin:StorageInfo`.

A storage node or a volume server that does not answer, and a drive that is offline, are not counted: the capacity is
then approximate, and `approximateReason` says why. AWS S3, an `s3` auth block without a `usage` block and GCS have no
capacity: the page shows nothing about it.

### Cost and safety

- The server makes an admin request only for an instance whose `auth` block has a `usage` block. The probe at start makes no admin request.
- Each request is a `GET`. One read has a time limit of 5 seconds for all its requests. Each answer has a size limit: 1 MiB for Garage and for the filer, 16 MiB for `/vol/status` and `datausageinfo`, 64 KiB for the quota of MinIO.
- The server keeps the result of each instance for 60 seconds, and a failure for 10 seconds. One read at a time runs for an instance: the other requests wait for its result. The read does not stop when the request that started it stops.
- `fresh=1` reads again a result that is older than 10 seconds. The page sends it after a change and after a click on the reload icon.
- The token and the URLs of the admin API never go into an answer or into a log line. An error names the step and the HTTP status only. `print-config` and the effective configuration in the log show the URLs and hide the token.
- A failure does not break the page. The usage of the bucket selector shows no number, and the information page gives the reason.
- At start, the server compares the source with the provider that the lifecycle probe found. A different known provider gives a warning in the log. The source of the file applies.

### Route

`GET /api/usage?instance=<id>[&fresh=1]` needs the read permission of the instance. `/api/instances` gives `usageSupported` for each instance. The answer has these forms:

- An instance without a `usage` block: `{"supported": false}`.
- A read that works: `supported` and `available` are `true`, with `instance`, `bucket`, `rootPrefix`, `source`, `usedBytes`, `usedObjects` (`-1` when unknown), `limitBytes` and `limitObjects` (`0` is no limit), `limitSource`, `approximate`, `approximateReason`, `asOf`, `percent` and `level`.
- A read that fails: `200` with `available: false` and `error` (`code` `usage_unavailable` and `message`). An answer `502` would tell the page that the server does not answer.
- The capacity of the storage, when the reader gives it: `storage` with `scope` (`cluster`, `disks` or `filesystem`), `usedBytes`, `totalBytes`, `freeBytes`, `percent`, `level`, `approximate`, `approximateReason`, `note` and `asOf`. A failed read of it gives `storageError` (`code` `storage_unavailable`), and the part of the bucket stays.
- A volume also gives `selfBytes` (the size of its files) and `selfState`: `computing` while the count runs (at least `selfBytes`), `partial` when the count stopped at its budget (at least `selfBytes`), or `ready`. Refer to "Volumes".

`limitSource` is `provider`, `config` or `""`. The key `quota_bytes` of a bucket block gives the limit when the source gives no byte limit. The server does not enforce this limit. `quotaPercent` is the used part of the quota in percent, with one decimal. The server cuts the next decimals: 99.99 % gives 99.9. With a byte limit and an object limit, the larger part applies. `quotaLevel` is `""` without a limit, `ok`, `warning` from 80 %, and `error` from 95 %. `storage.percent` and `storage.level` are the same values for the storage.

`percent` and `level` are the more constraining part: the quota or the storage, the part with the larger percent. `constraint` names it: `quota`, `storage`, or `""` when the answer has no limit. An equal percent keeps the quota. The colour of the bar, the short text of the chip and the warning of a full bucket follow `percent`.

### The state icons and the facts of the buckets in the bucket selector

The trigger of the bucket selector has the name of the current bucket, its seven state icons and its usage. Each entry of the list of the buckets has the same parts, in the same order, with the values of that bucket. A volume has the same parts, with three icons (refer to the next paragraph).

The seven state icons are always there, in this order. Each icon is grey (off, not available or denied), green (on, allowed or active) or orange (on, with a problem):

| Icon | Green | Orange | Grey | Source |
| --- | --- | --- | --- | --- |
| Read (Tabler `eye`), write (`pencil`), delete (`trash`) | The permission is allowed, or allowed on use | - | The permission is denied | `capabilities` of `/api/instances` |
| Encryption (`key`) | The provider applies `encryption = "sse-s3"`, or the bucket has a default encryption | `encryption = "sse-s3"`, and the provider does not apply it (Garage) | No encryption | `encryption`, `defaultEncryption` and `encryptionApplied` of `/api/instances` |
| Object Lock (`lock`) | Object Lock is on. The title gives the default retention | - | Object Lock is off | `objectLockSupported` and `objectLockDefault` of `/api/instances` |
| Versioning (`git-commit`) | Versioning is enabled | Versioning is suspended | Never enabled, or not available | The current bucket: `GET /api/lifecycle`. The other buckets: `versioningState` of `/api/instances` |
| Lifecycle rules (`hourglass`) | One rule or more is on. The count of these rules follows the icon | - | No rule, or not available | The current bucket: the rules in `GET /api/lifecycle`. The other buckets: `lifecycleRules` of `/api/instances` |

A volume has only its read, write and delete icons. A mounted folder cannot have encryption, Object Lock, versioning or lifecycle rules, so the selector does not draw these four icons. Colour is not the only sign: each icon has a title and a hidden text with the word of its state, for example "Encryption: on (SSE-S3)", "Encryption: requested but not applied by this provider" or "Versioning: off".

The usage follows the icons: the amount that the bucket holds (`3 MB`), then a thin bar and a short text. The short text is the more constraining part (`62 %`, of the quota or of the storage), or the used bytes (`12.3 GB`) for a bucket without a limit. A bucket without a limit has no amount: the short text is the amount. For a volume, the amount is what its folder holds: `computing…` before the first bytes, and `≥ 3 MB` while the count runs or after it stopped at its limit. The whole text (`12.3 GB of 20 GB` or `Storage: 910 GB of 1000 GB`) is for a screen reader and on the information page. A click on the usage (the amount, the short text or the bar) opens the information page of the bucket, as a click on the state icons does. The state icons and the usage are one link, with no tooltip. In a menu entry, this click does not select the bucket. Its source is `GET /api/usage`.

An approximate number starts with `~`. The bar has the colour of a warning from 80 % and the colour of an error from 95 %. The information page names the bucket, the used bytes, the limit and its source, and the objects. It also gives the time of the count and the reason of an approximate number. The numbers are for the whole bucket, also for an instance with a root prefix.

The trigger is a button, and a button cannot hold a link. Thus the seven icons and the usage are elements of their own, over the trigger and in its grid. The seven icons are one link to the information page of the bucket. The lifecycle rules are edited in the part "Lifecycle rules" of the information page. The keyboard goes from the trigger to the link.

A click on the rest of the selector, also on the usage, opens the list of the buckets. A failed read of the usage of the current bucket gives a muted icon, with the reason in its tooltip. The trigger names the link and the usage in `aria-describedby`.

The selector has one layout at every width. The trigger is never on two lines:

- The trigger is one line of 28 px (44 px on a coarse pointer). The selector is a grid with five columns: the icon, the name, the seven icons, the usage and the chevron. The trigger is a subgrid that spans the five columns, so the elements over it share its columns.
- The icons are 14 px with a gap of 2 px. The link is 116 px, and 124 px with the count of the lifecycle rules. Without the name, the trigger needs 342 px with every part.
- The selector is 460 px wide, and less when the header has less space. The selector is a CSS container. A narrower selector shows fewer parts, in this order: first the amount of the usage goes (440 px and less), then the short text of the usage (400 px), then the usage (360 px), then the icon of the trigger (300 px). At 240 px and less, the icons have a gap of 1 px, the trigger has a smaller padding, and the count of the lifecycle rules goes (the hourglass stays; the tooltip and the name of the link give the count).
- The seven icons of a bucket stay at every width. On a window of 320 px, the selector is 220 px: the name (38 px, with an ellipsis), the seven icons with the count, and the chevron. The name keeps 32 px at least.
- The tooltip of the trigger gives each part at every width. The parts never wrap and are never cut. The name gets an ellipsis.
- The list of the buckets has the same left and right edges as the selector. The open trigger covers the top of the list, and the two make one shape.
- Each entry of the list is one line: the name, a root prefix after it in small muted text, the seven icons, then the usage. The icons are a link next to the button of the entry, over it. The menu, its hosts and their lists share three columns (CSS subgrid): the name, the icons and the usage. Thus the icons of all the buckets make one column. The current bucket has `aria-current="true"`. The list is a non-modal dialog with buttons and links, not a listbox. The prefix gets an ellipsis before the name. A narrow list drops the usage of its entries in the order of the trigger. Each host of the list is one line: its name, then the provider and the number of buckets.

`/api/instances` gives two facts of each bucket for the list:

| Field | Value |
| --- | --- |
| `versioningState` | `"enabled"`, `"suspended"` or `"off"` (also a provider without versions). Absent when the server could not read it |
| `lifecycleRules` | The number of lifecycle rules of the bucket. Absent when the server could not read the rules, or for an instance without lifecycle rules |

The probe at start reads them (`GetBucketVersioning` after the probe of the versions, and the configuration in the probe of the lifecycle). A failure hides the fact only and never stops the start. Each read of `GET /api/lifecycle` keeps the new values. A request of `/api/instances` reads them again in the background when they are older than 60 seconds: one read at a time for each instance, 5 seconds at most, inside the limits of the storage requests. The request does not wait for this read.

The page reads the facts of the current bucket when it opens. It reads the usage again (`fresh=1`) 1.5 seconds and 12 seconds after a change: an upload, a delete, a copy, a move, a rename, a metadata edit, a restore or the end of a job. A group of changes gives one read. A click on the reload icon reads the facts again. At load, and again when the menu opens, the page reads the usage of each bucket with `usageSupported`. It sends 4 reads at the same time at most. An entry shows "…" while its read waits. A failed read of another bucket shows nothing. The page and the server keep each answer for 60 seconds. The tooltip of each bucket also has its usage. The preview page has no facts.

### The usage on the information page

The selector shows the amount, the bar and the percent only. The information page of the bucket gives each line of the usage, in a table (`dl`): "This bucket holds" (or "This volume holds") with the used bytes and the objects, and "Quota" with the used part of the quota and its bar. It also gives "Objects" for a limit of objects. "Storage" has the used bytes, the size, the percent and the free bytes, with its bar and its note (for example "Raw capacity of 1 storage node, before the replication"). For a volume, the first line is "computing…" or "at least 10 GB" while the count runs.

### The warning of a full bucket

A bucket is full when the more constraining part is at 90 % or more: `percent >= 90` of `GET /api/usage`, with a
`constraint`. A bucket without a quota and without a capacity never gets the warning. The warning has these parts:

- A banner at the top of the card of the listing, as wide as the listing, in the colours of an error, with the alert icon (Tabler `alert-triangle`). Its text names the part. The quota: "This bucket is 93 % full: 18.6 GB used of 20 GB. New uploads can fail." From 100 %, the text is "This bucket is full: 20 GB used of 20 GB (100 %). New uploads fail." The storage: "The storage of this bucket is 91 % full: 910 GB of 1000 GB, 90 GB left. New uploads can fail." A volume: "This volume is on a file system that is 91 % full: 910 GB of 1000 GB, 90 GB left. New uploads can fail." An approximate number has a `~` and the reason in a second sentence. The banner has `role="alert"`, so a screen reader reads it. It has no close button. It goes when the number goes down.
- The trigger of the selector gets a border in the colour of an error and the alert icon after the name. The bar of the usage is red from 90 %.
- In the list, a full bucket gets the alert icon after its name and a red bar.
- When a bucket other than the current one is full, the trigger gets a small alert icon in the colour of a warning. The tooltip says "Another bucket is almost full: <name>".
- The tooltip of the upload button gives the text of the banner. The upload stays possible.

### The summary of the details

The details of a file start with a strip of pills: the version (`v5 of 5`, only for a file with more than one version), the encryption (a key and `SSE-S3`, `SSE-KMS` or `Key of the client`), the lock (a padlock and the retention or `legal hold`) and the number of tags. A pill shows only when its fact is true. A click on the pill of the version opens the version picker. A click on another pill opens its section and moves to it.

The sections keep all their content. The sections "Storage object" and "Tags" start open. The other sections start closed. A chevron button at the left of each title opens or closes the section. A click on the title or on its "Edit" button also opens it. The browser keeps the choice of each section in `stowr.details.sections`.

The body of the details of a file is a container (`container-type: inline-size`). When the body is 820 px wide or more, the sections are in two columns (`.bb-details-column`). The first column has the object: "Storage object", "Encryption" and "Metadata". The second column has the facts that describe it: "Tags", "Retention", "File metadata" and "Document dimensions". A narrower window or a phone shows one column. The page order of the sections is the reading order and the tab order in the two layouts. Each column has its own height, so a section never moves to the other column when a section opens or closes. An open editor stays in its column. The summary strip, the header and the tool cards "Verify integrity" and "Inspect file" use the full width.

## Volumes

A `volume` block shows a folder of the server as the browser shows a bucket. The folder can be a Docker volume, an NFS
or CephFS mount, or a FUSE mount such as `weed mount` of SeaweedFS. The server reads and writes the files with the
system calls of its process, under the identity of the process. The code is in the files `src/volume_*.go`. These files
are the only files of the program that write to the local file system: the CI checks it (refer to
[DEVELOPMENT.md](DEVELOPMENT.md)). The configuration is in [CONFIGURATION.md](CONFIGURATION.md), section "Volume
block". The test with a real `weed mount` is in [test/README.md](../test/README.md).

### Instance and address

- A volume is a storage instance with the provider `fs`. `/api/instances` gives it with `"provider": "fs"` and
  `"host": "volume"`. The address of a volume in the interface is `/-/volume/<id>/`.
- The labels of the `bucket` blocks and of the `volume` blocks are one set of ids: two blocks with the same label stop
  the start. An `auth` block with the label `volume` stops the start when the file has a `volume` block, because the
  two addresses would be the same.
- The list of the interface gives the buckets first, then the volumes, each in the order of the file.

### Checks at start

- `path` must be absolute, without `/` at the end and without an empty, `.` or `..` part. The folder must exist, and
  the process must read it. `print-config` does these checks too.
- `root_prefix` must be a relative folder path that ends with `/`. The folder must exist in `path`. The root of the
  volume is then `path/root_prefix`, after the symbolic links.
- When the server can change the files (the permissions include `write` or `delete`, or the block has no
  `permissions`), the start makes and removes a temporary file in the root. A folder that the process cannot write
  stops the start. Give `permissions = ["read"]` for a folder that is read only. `server.access_mode =
  "force_read_only"` makes no such check.

### Keys and paths

- A key is a path relative to the root of the volume. The server refuses a key with an empty, `.` or `..` part, a NUL
  byte, or a part of more than 255 bytes (`400 invalid_key`).
- The server resolves each part of a path, one after the other. A symbolic link to a place inside the root is
  followed. A symbolic link that leaves the root, or that does not resolve, stops the operation with
  `403 volume_link_refused`. The listing with a delimiter shows such a link as an entry with `"unreadable": true`.
  The interface shows it with a ban icon and no action. A delete of such a link is also refused.
- A file that is not a regular file (a device, a socket, a pipe) is also unreadable. The server never opens it.
- Hidden files (a name that starts with `.`) are visible. The names keep the case that the file system gives.
- A name that is not valid UTF-8 shows with the replacement character. The server cannot read, write or delete it.
  A key of a write must be valid UTF-8.
- The names that start with `.stowr-tmp-` are for the temporary files of Stowr. A write with such a name is
  refused, and the listings do not show these files.
- Limit: a process outside Stowr that changes a symbolic link between the check and the use of a path can race
  the check. The API of Stowr cannot make a symbolic link.

### Listing

- With the delimiter `/`, the server reads one folder. The subfolders are the prefixes, also an empty folder and a
  symbolic link to a folder of the root. The other entries are the objects.
- Without a delimiter, the server reads the tree: the files, and each empty folder as the key `<folder>/` with size 0
  (as the folder objects of S3). It does not follow a symbolic link to a folder, because a link can make a loop.
- The order is the order of the keys of S3 (bytes of UTF-8). The page token is the last key of the page. A token
  stays valid when the folder changes: the next page starts after that key.
- A page has 1000 keys at most. `max_scan_pages` of the block combines pages as for a bucket.
- A folder with more than 250000 entries gives `422 volume_folder_too_large`: the server reads and sorts each folder
  of a listing in memory.
- The size and the date of a file come from `stat`. The content type comes from the extension of the name.

### Reads

- The ETag of a file is the weak validator `W/"<size>-<modification time in nanoseconds>"`. A file has no MD5.
- `HEAD` and `GET` give `Content-Length`, `Content-Type`, `ETag`, `Last-Modified` and `Accept-Ranges: bytes`.
- One byte range gives `206` with `Content-Range`. A range after the end gives `416`. A Range header that is not one
  valid byte range gives the whole file, as RFC 9110 lets a server do. An empty file has no range.
- `If-Match` and `If-Unmodified-Since` give `412`. `If-None-Match` and `If-Modified-Since` give `304`. The comparison
  of an ETag is weak. An `If-Range` with an ETag never matches, because the ETag is weak: the answer is the whole
  file. An `If-Range` with a date compares the date to the second.
- The range readers of the previews send `If-Match` with the ETag of the file. A file that changes during a preview
  thus gives `412`, as an object of S3.
- The previews, the Insights, the search, the integrity check and the ZIP download read a volume through the same
  interface as a bucket.

### Writes

- A write of a file goes to a new temporary file `.stowr-tmp-<random>` in the folder of the target. The server writes
  the bytes, synchronizes the file to the disk (`fsync`), and gives it the name of the target with one rename. A
  reader sees the old file or the new file, never a part. An existing file is replaced. A folder with the name of the
  target stops the write (`409 volume_folder_exists`).
- A write with the header `X-Checksum-Sha256` compares the SHA-256 of the received bytes with the digest of the
  client. A different digest stops the write (`400 bad_digest`), and the target does not change.
- A write makes the missing folders of the path. A part of the path that is a file stops the write (`409`).
- A write through a symbolic link to a file of the root writes the target file. The link stays.
- A key that ends with `/` and has no content makes a folder (`mkdir`). The Actions menu of the interface has the
  item "New folder" for a volume.
- A delete of a file removes the file. A delete of a folder key (`<folder>/`) removes the folder when it is empty. A
  key that does not exist, and a folder that is not empty, give no error: a delete of S3 does the same.
- A copy reads the source and writes the destination as a write of a file. From the threshold of the copy in parts
  up (refer to "Copy in parts"), the copy is a job that shows the copied bytes and can stop.
- A move and a rename of a file are one rename. A rename to an other file system (`EXDEV`) copies the file and
  removes the source. A move of a folder moves each file with one rename, then removes the folders that it emptied.
- A delete of a folder (the job `delete_prefix`) deletes the files of the tree, then removes the empty folders from
  the bottom. A folder that keeps a file (for example a symbolic link that leaves the root) stays.
- A full file system gives `507 volume_full`. A read-only file system gives `403 volume_read_only`.

### Uploads and incomplete uploads

- A resumable upload (`/api/uploads`) writes its chunks to a temporary file in the folder of the target:
  `.stowr-tmp-<random>-<name of the target>`. The next chunk starts at the size of the temporary file, so a client can
  resume after a lost connection. The last chunk synchronizes the file and gives it the name of the target.
- The resume token holds the path of the temporary file and the key of the target, encrypted and authenticated. A
  volume has no credential: the key of the token is a random key of the process. Thus a token does not stay valid
  after a restart of the server. The temporary file stays.
- The window "Incomplete uploads" lists the temporary files of the volume: the key of the target, the last write to
  the file (as the date) and its size. The abort of an upload removes its temporary file. The abort by age keeps a file
  younger than 24 hours, and no abort touches a file of an upload that this process sends now.
- One page of this list reads at most 20000 entries of the tree. The list does not follow a symbolic link.

### Functions of S3 that a volume does not have

A volume has no versions, no tags, no metadata of S3, no lifecycle rules, no Object Lock and no encryption of S3. The
backend does not have these interfaces, so the server answers `501` to their routes. `/api/instances` gives `false`
for `versioningSupported`, `tagsSupported`, `metadataEditSupported`, `lifecycleSupported`, `objectLockSupported` and
`bucketEncryptionSupported`. The interface hides their controls, and reads no HEAD for the marks of the listing.

### Usage

`GET /api/usage` of a volume (source `statfs`) has two parts:

- What the volume holds: `selfBytes` (also `usedBytes`), the sum of the sizes of the regular files of the folder, and
  `usedObjects`, the number of these files. A sizer counts them in the background: a request never walks the tree. One
  walk at a time runs. A walk reads at most 1,000,000 entries in 20 seconds; a larger tree gives `selfState:
  "partial"`. While a walk runs, the answer gives its progress with `selfState: "computing"`. A walk does not follow a
  symbolic link, and does not count the temporary files of Stowr. The count stays for 5 minutes. A write of
  Stowr in the volume (an upload, a copy, a move, a delete) makes it old: the next read starts a new walk. A change
  that a process outside Stowr makes shows after the 5 minutes. The sizes are the sizes of the files, not the
  blocks of the disk.
- The storage: the file system of the folder (`statfs`, scope `filesystem`). The used bytes are the size minus the
  available bytes, the total is the size, and the free bytes are the available bytes. `storage.approximate` is `true` on a
  FUSE, NFS, CephFS, SMB 2 or CIFS file system (the type of the answer of `statfs`): such a file system can count the
  used bytes with a delay. The numbers are for the whole file system, also for the other folders on it. A size of 0
  (`weed mount` gave it once, after a delete) gives `storageError`.

A volume has no quota: the storage is its limit, and the warning applies from 90 % of the file system. The server reads
a volume at each request, without the cache: a read is a `statfs` call and the count in memory. Only a Linux build reads
`statfs`: on an other system, a volume has no usage.

### File systems

- The server needs a file system with rename in a folder, `fsync`, and `O_EXCL`. A hard link (for the archive
  extraction that must not replace a file) is optional: without it, the server checks the target and renames.
- The test with a real `weed mount` of SeaweedFS 4.41 (test/volume-fuse) passes: the Go tests of the volumes and a
  browser smoke test run on the mount. The facts of that test are in [test/README.md](../test/README.md).
- A network file system can show a change of an other client with a delay. A listing can then be old by a few
  seconds. The writes of Stowr through the same mount are visible at once.

## Insights treemap normalization

The statistics worker builds the semantic treemap on the backend after the complete Insights scan finishes. The backend:

- calculates the exact global one-percent byte threshold using integer arithmetic;
- groups sub-threshold direct children into exact `Others` aggregates;
- preserves exact bytes and object counts for every emitted node;
- contracts folder chains that contain only one child and therefore introduce no real branch;
- removes a sole `Others` child because the parent already represents the same exact scope;
- returns an immutable `treemap` tree plus `treemapThresholdBytes`.

The browser performs only geometric squarified layout, label fitting, focus feedback, and navigation. It does not reconstruct folders, regroup data, or decide which single-child nodes to remove.

The browser chooses the label colour of a file tile from the WCAG contrast: black or white text, the colour with the higher ratio on the colour of the tile. That ratio is always 4.5 or more. The tile "Others" has a light tint of the warning colour, and its label has the colour of the text of the theme.

The subtitle of the window gives the path, the count of objects, the total size and the time of the scan: `<path> · N objects · SIZE · computed in N ms`. The overview fits in the window with no scrollbar at 1280x720 and larger. The lists "Most recent files" and "Largest files" have 10 rows at most. They share the height that is left, and each list scrolls when it is too short. A very small window scrolls.

## Version capability and version access

At startup, each bucket performs one bounded version-list probe. A successful provider response enables version controls. HTTP 501, access denial, timeout, malformed response, or any other error disables them for that bucket without failing application startup.

When available:

- `/api/versions` pages exact versions or generations of one key;
- `/api/version-counts` counts exact non-delete-marker versions for visible keys;
- object, metadata, preview, inspection, and integrity endpoints accept an exact version identifier;
- restore and permanent exact-version deletion call the provider implementation.

The frontend never offers version controls for a bucket that failed the probe.

An object whose current version is a delete marker is not a live object. MinIO still lists its folder in a listing with a delimiter, and lists no key under it. For a bucket with versioning on MinIO, or on a provider that the server does not know, the listing therefore asks for one key under each folder of the page (4 requests at a time, 200 folders at most). It removes the folders that have no key. A failed request keeps its folder. SeaweedFS, AWS and Garage do not list such a folder, and the server sends no extra request to them.

## Browser-only version comparison

Comparison is deliberately limited to two immutable versions returned for the same object key. The browser obtains exact version sizes, reads both versions through strict 2 MiB Range requests, compares blocks, and stops at the first differing byte.

The backend has no generic comparison route and does not retain comparison state or content.

## Large-object readers

Range-aware readers use exact start and end offsets, validate HTTP 206 and `Content-Range`, bind sessions to ETag/generation when available, and reject expanded or full-object responses.

Caches are byte-bounded LRU structures. Parsers read format indexes, headers, trailers, central directories, and directly referenced members rather than accumulating complete objects.

Complete-object work is allowed only after an explicit action such as per-object integrity verification. It streams through small fixed buffers under background byte/request budgets.

## Archive extraction

ZIP-compatible archives use their central directory. A selected member is read from its exact compressed ranges, decompressed as a stream, and written directly to the explicit destination object.

The server never writes a temporary archive or member to disk. Unsafe paths, symbolic links, encrypted members, collisions, oversized expansion, and existing destinations are rejected. An interrupted provider upload is aborted where the provider API allows it.

## Browser interface

The binary embeds one browser interface: the tree `src/public/`. The top of `assets/css/stowr.css` gives the design rules as tokens.

The tree contains these pages and scripts:

- `index.html` is the browser page, `preview.html` the preview page, `info.html` the information page of an instance and `404.html` the page of an unknown path.
- `assets/js/common/` has the shared scripts (API, previews, dialogs). `preview.js` is the preview page.
- `assets/js/app.js` is the browser page. `uploads.js` is the bulk upload. `tags.js` and `metadata.js` are the sections of the details that edit the tags and the metadata. `theme.js` applies the theme before the first paint. `session.js` gives one message for an ended sign-in (401) or a server that does not answer.
- `assets/css/dialogs.css` and `assets/css/viewers.css` have the rules for the dialogs and the previews. Each colour is a token of `stowr.css`, so these rules follow the theme.
- `assets/icons/tabler/` has one Tabler icon (MIT) for each name. `assets/css/icons.css` gives the icon for each `mdi-*` name of the shared scripts.
- `assets/vendor/` has the vendored libraries: PDF.js, highlight.js and marked. A Go test makes sure that a page or a script loads each of them.

The header has these heights at every width, from 320 px up:

| Pointer | Header |
| --- | --- |
| Fine | 45 px: 44 px (`--header-h`) and the border line. The bucket selector and the theme button are 28 px high |
| Coarse | 57 px: the controls are 44 px high, with 6 px over them and under them |

The header is one row at every width. It has the full width of the window: the brand is at the left gutter, the bucket selector and the theme button are at the right gutter, and the free space is between them. The gutter of the header is 12 px, and 10 px at 820 px and less. The content of the page keeps its own gutter of 24 px (14 px at 760 px and less). The content of the page under the header keeps its centred column of 1400 px, so on a wide window the edges of the header are not the edges of the content. A narrow window gives less space to the brand, in this order: at 600 px and less, the name of the brand goes. At 420 px and less, the version goes. The mark of the brand always stays.

The bar of the preview page is one row of the same height: 45 px, or 57 px on a coarse pointer. It also has the full width of the window and the gutter of the header: the way back and the name at the left gutter, the tools and the theme button at the right gutter. It has the way back, then the name of the file, its size, its marks and its path from the bucket root on one line, then the tools and the theme button. The path gets an ellipsis before the name, and the title of the name gives the whole key. At 760 px and less, the path, the build tag and the date of the version picker go, and the way back keeps its chevron. At 420 px and less, the size goes. In a comparison, From, the statistics and To take the place of the path below 1101 px. At 760 px and less, the comparison puts its versions and its statistics in a row of their own, under the way back and the name.

On each page, the document never scrolls. `html` and `body` have the height of the window and no scroll. The body is a column: the header (or the bar of the preview page) stays at the top, and only the element under it scrolls (`.pageScroll`, with `data-page-scroll`). Thus the header never moves, and the scrollbar starts under the header and ends at the right edge of the window. This applies to the listing, the preview page (also a comparison of versions), the information page and the 404 page. Sticky elements, such as the head of a table, stick to the top of the element that scrolls, under the header. The upload dock, the drop overlay, the toasts and the windows are fixed to the window, outside the element that scrolls. The scripts that move the page with the wheel over a horizontal scroller use `BB.runtime.pageScroller()`.

The theme selector is one button of 34 x 28 px with a menu. The button has a border of 1 px and corners of 6 px. Its icon is an inline SVG icon: 18 px, 8 px from the left edge and 5 px from the top edge of the button, a stroke of 1.5 and no fill. The icon is muted text at 82 %, and the text colour under the pointer, with the focus and in an open menu. The focus ring is 2 px, on the edge of the button. The menu is 34 px wide, its edge has the colour `--theme-menu-border`, and each choice is 34 x 32 px with its icon at 8 px and 7 px. A coarse pointer keeps the controls of 44 px.

Each menu and popover of Stowr has the opaque colour `--panel`, with no backdrop filter: the menus of the header (theme, buckets), the menus Actions and settings, the menus of the rows and of the preview, the version picker and the suggestions of the metadata editor. A menu opens with a short move and no fade, so the page never shows through it, also in its first frame. An open menu of the header puts the header above the upload dock.

The fingerprint of the assets covers all the files of the tree. The server also adds the fingerprint to each `@import` of a stylesheet.

## Frontend path model

Assets and API URLs are relative to `document.baseURI`. A path-stripping reverse proxy can therefore expose the same build at `/`, `/stowr/`, or another prefix without an HCL public-path setting or a frontend rebuild.
