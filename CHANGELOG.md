# Changes

This document uses ASD-STE100 Simplified Technical English.

## 0.1.2 (2026-11-08)

### Changes

- The list page has no "Back to site" link. Stowr behind a reverse proxy path prefix, for example `/s3/`, showed it on
  its home page. A reverse proxy or the address bar gives the way back to the site.

## 0.1.1 (2026-10-23)

Stowr has one browser interface and no authentication of its own. The API, the routes, the other keys and the
behaviour of the storage operations do not change.

### Changes

- The binary embeds one browser interface: the newer of the two interfaces of 0.1.0, with the dense tables, the
  bucket selector and the information page. The interface `default` and the key `server.frontend` are removed.
- The `access` block is removed: the modes `authzen` and `open`, the verification of the identity token, the client
  of the AuthZEN PDP and the route `/.well-known/access-actions`. The permissions of the buckets and the volumes, and
  `server.access_mode`, do not change.
- The vendored styles that only the removed interface used (Buefy, github-markdown-css and the style of highlight.js)
  are removed. PDF.js, highlight.js and marked stay.
- The browser keeps its settings under new keys that start with `stowr.`. The settings of 0.1.0 (the theme, the
  columns of the listing, the open sections of the details and the tags of the uploads) go back to their defaults
  one time.
- The browser tests are in `test/frontend/`.

### Upgrade steps from 0.1.0

1. Delete the line `frontend = "..."` from the `server` block. A file with this key stops the server at start:
   `unknown attribute "frontend" in server block`.
2. Delete the `access { ... }` block. A file with this block stops the server at start:
   `unknown block type "access"`.
3. Put a reverse proxy that authenticates the users in front of Stowr, and do not publish the port of Stowr. Each
   request that reaches Stowr can do each operation that the permissions of the buckets and the volumes let.

## 0.1.0 (2026-10-23)

The first release of Stowr: one Go binary that browses S3-compatible buckets, Google Cloud Storage buckets and the
folders of the server (volumes), with an embedded browser interface.

- Storage: S3-compatible providers (AWS S3, MinIO, SeaweedFS, Garage), Google Cloud Storage, and volumes (a folder of
  the server, such as a Docker volume or a FUSE mount). Shared `auth` blocks serve many `bucket` blocks.
- Configuration: one HCL file with a closed schema. An error stops the start and names the key. Secrets come only
  from `_env` or `_file`. `print-config` shows the value and the source of each key. The reference comes from the
  table of keys in the code.
- Listing: dense tables, folders, search, sort, tags, versions, and the settings of the columns.
- Previews of many file types with bounded byte ranges: text, code, Markdown, JSON, CSV, Parquet, SQLite, Excel,
  Word, PDF, images, audio, video, archives and email.
- Operations: upload with resume and SHA-256 checksums, download, copy, move, rename and delete of files and folders,
  archive download and extraction, edit of the metadata and of the tags, restore and delete of versions, comparison
  of two versions.
- Bucket functions: lifecycle rules, Object Lock (retention and legal hold), encryption (SSE-S3, SSE-KMS and SSE-C),
  cleanup of incomplete multipart uploads, and the usage and the quota of a bucket.
- Information page of a bucket or a volume (`?info`): the permissions, the functions and the storage.
- Permissions: a ceiling of read, write and delete for each bucket and volume, and `server.access_mode =
  "force_read_only"`.
- Runtime: no local state, bounded memory, provider requests and transfers, a read-only root file system, and a
  static binary in a scratch image.
