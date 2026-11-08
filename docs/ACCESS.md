# Access policy

This document uses ASD-STE100 Simplified Technical English.

Stowr follows the protocol ProcurAuth for the access of people. A reference such as P 9.4 is a paragraph of this protocol.
The text of this document is the behavior of the code. The generated list of the keys is in
[configuration-reference.md](configuration-reference.md). The result of the conformance test is in
[CONFORMANCE.md](CONFORMANCE.md).

## Principle

The access policy has three parts:

1. **Authentication.** The edge authenticates the person. The edge signs an identity token and puts it in a request header.
2. **Check of the token.** Stowr checks the token on each request. The token gives the subject: the id of the person.
3. **Decision.** Stowr asks a PDP (Policy Decision Point) for each operation. The PDP uses the AuthZEN Authorization API 1.0.

Stowr applies each decision on the server. The page of the browser shows or hides controls, but it never decides.

The edge and the PDP are outside Stowr. This document names no product for them. The edge and the PDP can be
any programs that follow ProcurAuth.

## The block and the two modes

The `access` block is mandatory. A file without it stops the start. Thus nobody gets the open mode by mistake.
The key `mode` has two values.

| Mode | Behavior |
|---|---|
| `authzen` | Stowr checks the identity token and asks the PDP. The blocks `identity` and `pdp` are mandatory. |
| `open` | Stowr checks no token and asks no PDP. The two blocks stop the start. The route of the action document and the route of the options give 404. |

The mode `open` is for a deployment where a reverse proxy that authenticates the users decides who can reach Stowr. In this mode, each
request that reaches Stowr can do each operation that the process ceilings let. Stowr writes a warning at start.

[configuration-reference.md](configuration-reference.md) gives each key with its default, its range and its forms.
These items stop the start:

- No `access` block, no `mode`, or a `mode` that is not `authzen` or `open` (lower case).
- `mode = "open"` with an `identity` block or a `pdp` block.
- `mode = "authzen"` without the `identity` block or the `pdp` block, or without a mandatory key.
- A `pdp.url` with `http://` and `plain_http = false`. A `pdp.url` with `https://` and `plain_http = true`.
- A `pdp.token` in clear text in the file. Use `token_env` or `token_file`.
- `usage_timeout` that is less than `timeout`.
- More than 256 `bucket` and `volume` blocks together, or a label of more than 256 bytes.
- An action document of more than 64 KiB.

A full example for the mode `authzen`:

```hcl
access {
  mode = "authzen"

  identity {
    header           = "X-Identity-Token"
    issuer           = "https://edge.example"
    audience         = "infra-s3"
    public_keys_file = "/run/keys/edge-jwks.json"
    max_age          = "5m"
    clock_skew       = "60s"
  }

  pdp {
    url             = "https://pdp.example:4182"
    token_file      = "/run/secrets/pdp-token"
    timeout         = "500ms"
    usage_timeout   = "2s"
    cache_ttl       = "30s"
    max_evaluations = 64
  }
}
```

An example for the mode `open`:

```hcl
access {
  mode = "open"
}
```

A duration is one whole number and one unit: `ms`, `s`, `m` or `h`. For example: `500ms`, `30s`, `5m`. A sign, a fraction or two
units (`1m30s`) stops the start. `clock_skew` must be a whole number of seconds.

At start, Stowr writes a `config.warning` line in these cases:

- `mode = "open"`.
- `pdp.plain_http = true`: Stowr sends the PDP token and the questions without TLS.
- `mode = "authzen"` and `server.listen` has no host or a wildcard host (`0.0.0.0`, `::`). Limit the network (refer to
  "The network rule").

## What the operator gives Stowr and what Stowr gives the operator

Before the integration, the operator and Stowr exchange the facts of P 10.0.

| Fact | Direction | Key or value |
|---|---|---|
| The id of the service | Stowr to the operator | The value of `access.identity.audience`. It is also the `server` of the action document and the root of each chain of resources. |
| The origin of the action document | Stowr to the operator | The origin of Stowr and the path `/.well-known/procurauth/actions`. The reader needs no token. |
| The name of the token header | Operator to Stowr | `access.identity.header` (default `X-Identity-Token`) |
| The issuer | Operator to Stowr | `access.identity.issuer`: the `iss` of the tokens |
| The public keys of the edge | Operator to Stowr | `access.identity.public_keys`, `_env` or `_file`: PEM or a JWK Set |
| The maximum age of a token | Operator to Stowr | `access.identity.max_age` (default `5m`) |
| The clock skew | Operator to Stowr | `access.identity.clock_skew` (default `60s`) |
| The URL of the PDP | Operator to Stowr | `access.pdp.url`, `_env` or `_file` |
| The token of Stowr for the PDP | Operator to Stowr | `access.pdp.token_env` or `token_file` (a secret) |

The caller of the options route reads it at the origin of the action document. It sends the identity token in the token
header. The operator must give this caller a network path to Stowr.

## The identity token

The token is a JWT that the edge signs. Stowr checks each request that is not exempt (refer to "Routes with no PDP
question"). The rules:

- The header `alg` is exactly `Ed25519` or `ES256`. Stowr refuses `EdDSA`, `none`, `HS256` and each other name.
- The header `typ` is exactly `JWT`. Stowr refuses another value (for example `at+jwt`) and a token without `typ`.
- The header `kid` selects the key. Stowr refuses an unknown `kid`, and a `kid` that belongs to a key of
  another algorithm. A token without `kid` is checked with each key of its algorithm.
- Stowr refuses a header `crit`, a duplicate member in the header or in the claims, and a token of more than 16384 bytes.
- The claims `iss`, `sub`, `aud`, `iat` and `exp` are mandatory. `iss` must be the configured issuer. `aud` must be the id of
  the service, or an array that holds it.
- `sub` has 1 to 256 bytes of UTF-8, without a control character. Stowr uses it as opaque text. It sends it to the PDP as
  the subject with the type `user`, and sends no other claim.
- `iat` and `exp` are whole numbers of seconds. Stowr applies the clock skew to both: it refuses a token when its clock is
  later than `exp` plus the skew, and when `iat` is later than its clock plus the skew.
- Stowr refuses a token when `exp` is less than `iat`, or when `exp` minus `iat` is more than `max_age`.
- Stowr ignores `nbf` and each unknown claim. It reads `owner` for the log only. It never uses `owner` for a decision.
- Stowr reads the token only from the configured header. It never reads `Authorization`, a cookie or a query parameter.
  The header name must not be `Cookie`, `Authorization`, `Host`, `Content-Type` or `Content-Length`.
- Two copies of the header are a refusal. The edge must replace the header (P 9.1).

Stowr keeps the result of a valid token until `exp` plus the skew. The cache has 4096 entries. When it is full, Stowr
drops the expired entries. When it is still full, Stowr keeps no new result.

A request with no valid token gets `401 unauthenticated`. The body has no reason. For a request with an unsafe method,
Stowr also sets `Connection: close`, so that it does not read the body. The log line `access.unauthenticated` gives the
reason: `missing`, `malformed`, `algorithm`, `type`, `kid`, `signature`, `issuer`, `audience`, `expired`, `not_yet_valid`,
`max_age`, `subject` or `claims`.

### Public keys

`access.identity.public_keys` holds PEM text or a JWK Set (RFC 7517), with the rules of ClickHouse Dash. Stowr reads the
text as a JWK Set when it starts with `{` after spaces, tabs and line ends. Else it reads PEM.

- PEM: 1 to 16 `-----BEGIN PUBLIC KEY-----` blocks. Each key is Ed25519 (`Ed25519`) or P-256 with an uncompressed point
  (`ES256`). Outside a block, a line must be empty. Other text, a PEM header, a private key and another type of key stop
  the start.
- JWK Set: a JSON object with a `keys` array of 1 to 16 keys. A key of type `OKP` has `crv` `Ed25519` and `x` (RFC 8037). A
  key of type `EC` has `crv` `P-256`, `x` and `y` (RFC 7518). The point of a P-256 key must be on the curve.
- The `alg` of a JWK is optional. When it is present, it must be `Ed25519` (or `EdDSA`) for `OKP`, and `ES256` for `EC`. A
  token header with the `alg` `EdDSA` stays refused. The members `use` and `key_ops` have no effect.
- A JWK with a private member (`d`, `p`, `q`, `dp`, `dq`, `qi`, `oth` or `k`), or with another `kty` or `crv`, stops the
  start. The JWK parser also refuses a duplicate member and data after the JSON object.

The `kid` of a key is the `kid` that the JWK declares: 1 to 256 bytes without a control character. A PEM key, or a JWK
without `kid`, gets the RFC 7638 thumbprint of the key as `kid`. Two keys of a JWK Set cannot have the same `kid`.
`print-config` shows each key as its type and its `kid`, for example `["Ed25519 kid=kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k"]`.

## The network rule

Stowr must accept requests only through the edge (P 9.4). A client that reaches Stowr without the edge can use an identity
token that it got, until the `exp` of the token. It then skips the other checks of the edge, for example the decision for the
action `access`. The network rule stops this. Use a firewall, a network policy or a private network. Do not publish the port of
Stowr.

The PDP token is a secret of Stowr. Keep the network of the PDP closed.

## Routes with no PDP question

Two routes need no identity token:

| Route | Reason |
|---|---|
| `GET`, `HEAD /healthz` | The health check of the container calls it. It gives no data. |
| `GET`, `HEAD /.well-known/procurauth/actions` | P 5.1: the reader of the document has no token. It asks the PDP nothing. |

These routes need the identity token but ask no PDP question:

| Route | Reason |
|---|---|
| The static files (`/`) and the pages (`/-/...`) | They hold no data of a bucket. |
| `GET /api/build` | It gives the version of the program. |
| `OPTIONS /s3` | It gives a fixed `Allow` header. |
| `/api/jobs` (without an id) | It always gives 404. |

These routes are a deviation from P 10.0 step 2 (the route `/healthz`), and from P 10.1 and P 15 item 6 (the other
routes). The deviation has one reason for each route, in the tables above. Each other route asks the PDP.

A route that has no access rule answers `403 forbidden` with the action `unknown`, and Stowr writes the line `access.no_rule`.

## The resource tree

Stowr gives each request a node of this tree:

```text
service   (access.identity.audience)
└── bucket       the label of a bucket block or of a volume block
    └── folder       the first part of the object key, below root_prefix
        └── subfolder    the second part of the object key
```

An object is not a node. The node of a key is the deepest folder that holds the key. A grant on a node covers each node
below it. Stowr sends the node and the chain of its ancestors in `resource.properties.ancestors` (P 4.1). The chain
comes from the configuration and from the key, never from a value that the client names.

The id of a bucket is the label of its block, not the name of the bucket at the provider. Two blocks have two nodes, also
when they show one provider bucket. An id has 1 to 256 bytes of UTF-8, without a control character.

### From a key to a node

The node comes from the exact relative key or prefix that the route sends to the storage. Stowr removes the `/` at the start
and the NUL bytes. It does not clean the other parts: a part `..` is an id like each other part.

The rule: cut the key at each `/`. Remove the last part. This part is the name of an object, the empty part after the `/`
at the end of a folder path, or the incomplete last part of a list prefix. The other parts are the folders, from the
top. The walk stops at the depth of the tree, and at the first part that cannot be an id. A walk that stops early gives
a node nearer to the bucket. Thus a grant must be broader, never narrower.

Examples for the service `svc` and the bucket `b`:

| Key or prefix | Node |
|---|---|
| `` (empty) | `bucket:b` |
| `a.txt` | `bucket:b` |
| `rep` (a list prefix) | `bucket:b`, because the prefix matches keys of several folders |
| `reports/` | `folder:reports` |
| `reports/q1.csv` | `folder:reports` |
| `reports/20` (a list prefix) | `folder:reports` |
| `reports/2026/` | `subfolder:2026` below `folder:reports` |
| `reports/2026/q1.csv` | `subfolder:2026` below `folder:reports` |
| `reports/2026/q1/x.csv` | `subfolder:2026` below `folder:reports` (the tree has two folder levels) |
| `reports//x` | `folder:reports` (the empty part stops the walk) |
| `/x` | `bucket:b` (the `/` at the start is removed, then the first part is empty) |
| `team a/x` | `folder:team a` (the text form of the id is `team%20a`) |
| a first part of more than 256 bytes | `bucket:b` |
| a first part with a control character, or that is not UTF-8 | `bucket:b` |
| `public/../secret/x` | `subfolder:..` below `folder:public` |

The text form of a chain is `service:svc/bucket:b/folder:reports`. Each id is percent-encoded (P 5.2). The log and the cache
use this form.

## The actions

The service publishes five actions. No action is named `access`: the edge decides it.

| Action | Types | What it lets a person do |
|---|---|---|
| `object.read` | service, bucket, folder, subfolder | List, preview, inspect and download objects. Show versions, tags, metadata, retention, archive entries, the usage of a bucket, the lifecycle rules, the encryption and the incomplete uploads. Run Insights and the check of integrity. Download a ZIP of a folder. |
| `object.write` | service, bucket, folder, subfolder | Upload objects. Make folders. Restore versions. Change tags and metadata. Abort incomplete uploads. Be the destination of a copy, a move or an extraction. |
| `object.delete` | service, bucket, folder, subfolder | Delete objects, versions and folders. Be the source of a move or a rename. |
| `object.lock` | service, bucket, folder, subfolder | Set or remove a legal hold. Set, extend or shorten the retention of an object version. |
| `bucket.admin` | service, bucket | Save the lifecycle rules of a bucket. |

The action document publishes an action only when the configuration serves it:

- `object.read` needs one block with `read` in its `permissions`, or with no `permissions` key.
- `object.write` needs one block with `write`, and `object.delete` needs one block with `delete`. A block with no `permissions` key
  gives both. Neither is published with `server.access_mode = "force_read_only"`.
- `object.lock` and `bucket.admin` need `object.write` published, and one `bucket` block with `read` and `write`.
  A volume has no Object Lock and no lifecycle rules.

The permissions of the blocks are the process ceiling (refer to "Order of the checks"). When the action of an operation is
not published, Stowr answers `403 forbidden` and asks the PDP nothing.

### Limits

The document declares two limits:

| Limit | Kind | Unit | Range | Listed on |
|---|---|---|---|---|
| `writes` | budget | `operation` | no range | `object.write`, `object.delete`, `object.lock`, `bucket.admin` |
| `max_object_size` | ceiling | `byte` | 1 to 5497558138880 | `object.read`, `object.write` |

The PDP counts a budget. Stowr applies a ceiling. A grant gives a budget value only on the grant of the action `access`,
which the edge decides. A grant gives a ceiling value on the grant of the action.

### The action document

`GET /.well-known/procurauth/actions` gives the document. Stowr builds it one time at start, and gives the same bytes at each
read. It needs no token and asks the PDP nothing. A bad token does not change the answer. The old path
`/.well-known/access-actions` has no alias and gives 404.

The document has the `server` (the id of the service), the types of the tree, the actions with their limits, the limits, and
one resource for each `bucket` block and each `volume` block. A resource has the path `bucket:<id>` and the label (the name of
the block, when it has 1 to 60 characters). The document lists every bucket label. Thus a hidden bucket is not a secret.

## Order of the checks

The handler of a route does these checks in this order:

1. The middleware checks the identity token (401).
2. The handler finds the instance. An unknown instance id gives `404 unknown_instance` and no PDP question. The document
   already publishes each instance id.
3. The handler checks the shape of the request: the body, the key, the prefix.
4. The handler checks the process ceiling: the `permissions` of the block and `server.access_mode`. A refusal gives
   `403 permission_denied` and no PDP question.
5. The handler asks the PDP (`authorize`).
6. The handler does the effect.

The decision of the PDP comes before the first byte of a body that Stowr reads, and before the first call to the storage.

## The routes

Notation: `N(key)` and `N(prefix)` are the nodes of "From a key to a node". `B` is the bucket node. "Others" are the
other permissions of the operation. "Use" is the permission that the operation uses (P 5.5). A use with `(usage)` is an
action that lists the budget `writes`: Stowr asks it with usage (refer to "Budget writes"). A permission that is not a use
or an other is not asked.

### Read routes

The use of each read route is `object.read`. It lists no budget, so no question has usage. The Others column is empty.

| Route | Use |
|---|---|
| `GET`, `HEAD /api/list` | `N(prefix)` |
| `GET`, `HEAD /s3` with `key`; `GET`, `HEAD /open/` | `N(key)` |
| `GET /s3` without `key` (a raw list) | `N(prefix)` |
| `GET /api/stats`, `GET /api/archive` (a ZIP of a folder), `GET /api/multiparts` | `N(prefix)` |
| `/api/spreadsheet`, `/api/delimited`, `/api/document-count`, `/api/document/word`, `/api/parquet` | `N(key)` |
| `/api/json/raw`, `/api/json/beautify`, `/api/json/summary`, `/api/json/tree`, `/api/search`, `/api/media-info` | `N(key)` |
| `/api/structured-preview`, `/api/archive-preview`, `GET /api/archive-entry`, `/api/image-preview`, `/api/inspect` | `N(key)` |
| `GET /api/versions`, `GET /api/tags`, `GET /api/metadata`, `GET /api/object-lock` | `N(key)` |
| `POST /api/integrity`, `POST /api/archive-entry/integrity` (both start a job) | `N(key)` |
| `POST /api/version-counts` | One permission for each key. The first key is the use. All go in one batch. |
| `POST /api/sqlite/sessions` | `N(key)` |
| `GET /api/usage`, `GET /api/encryption`, `GET /api/lifecycle`, `POST /api/permissions/refresh` | `B` |

A grant on a folder does not cover the bucket node. A person with a grant on one folder only gets no usage, no lifecycle
rules and no encryption of the bucket.

### Change routes

| Route | Others | Use |
|---|---|---|
| `PUT /s3` with `key` (a key that ends with `/` makes a folder) | none | `object.write` `N(key)` (usage) |
| `DELETE /s3` with `key`, with or without `version` | none | `object.delete` `N(key)` (usage) |
| `DELETE /api/versions` | none | `object.delete` `N(key)` (usage) |
| `POST /api/uploads` (create) | none | `object.write` `N(key)` (usage) |
| `PUT /api/tags`, `PUT /api/metadata` | none | `object.write` `N(key)` (usage) |
| `PUT /api/object-lock` | none | `object.lock` `N(key)` (usage) |
| `POST /api/copy` (an object, or a prefix with `isPrefix`) | `object.read` `N(src)` | `object.write` `N(dst)` (usage) |
| `POST /api/rename` (an object, or a prefix) | `object.read` `N(src)`, `object.write` `N(dst)` | `object.delete` `N(src)` (usage) |
| `POST /api/delete-prefix` | `object.read` `N(prefix)` | `object.delete` `N(prefix)` (usage) |
| `POST /api/versions/restore` | `object.read` `N(key)` | `object.write` `N(key)` (usage) |
| `POST /api/tags/bulk` | `object.read` `N(prefix)` | `object.write` `N(prefix)` (usage) |
| `PUT /api/lifecycle` | `object.read` `B`, `object.delete` `B` | `bucket.admin` `B` (usage) |
| `POST /api/multiparts/abort` with `uploads` | `object.write` `N(key)` of each upload except the first | `object.write` `N(key)` of the first upload (usage) |
| `POST /api/multiparts/abort` with `olderThanDays` | `object.read` `N(prefix)` | `object.write` `N(prefix)` (usage) |
| `POST /api/archive-extract` | `object.read` `N(archive key)` in the source, `object.read` `N(target prefix)` in the target | `object.write` `N(target prefix)` in the target (usage) |

A move or a rename uses `object.delete` on the source. It is the most destructive action of the operation. The read of the
source and the write of the destination are Others, asked without usage. The target of an extraction can be another instance.

The save of the lifecycle rules always asks the two Others. It does so also when the rules delete nothing.

A route that starts a job asks its permissions when it starts the job. The job asks again at each read and each control. A
job is a copy or a move of a prefix, a delete of a prefix, bulk tags, an abort by age, an extraction, an integrity check or Insights.

### Continuations

A continuation continues a use that already took its unit. It asks its permissions again, in one batch, without usage. This
rule applies also when the action lists a budget.

| Route | Permissions |
|---|---|
| `POST /api/uploads/resume`, `GET`, `PUT`, `DELETE /api/uploads` | `object.write` `N(key)`. The instance and the key come from the sealed resume token, never from the client. |
| `GET /api/jobs/<id>`, `POST /api/jobs/<id>/pause`, `resume`, `cancel` | The Others and the use of the request that made the job. The job type gives them. |
| `GET`, `DELETE /api/sqlite/sessions/<id>`, `GET /api/sqlite/sessions/<id>/table` | `object.read` `N(key)` of the session. The instance and the key come from the session. |

A job keeps no subject. The permissions of the person who asks decide, at each request. A job whose permissions the PDP
refuses answers `404 unknown_job`, as a job that does not exist. A SQLite session whose permissions the PDP refuses answers
`404 sqlite_session_not_found`. A refusal of an upload continuation gives 403 or 429, and a PDP failure gives 503.

A request for `/api/stats` asks its permission each time, also when it reuses a job.

## Budget writes

The budget `writes` counts operations that change objects or bucket settings. The PDP holds the counter. Stowr asks one question
for each use.

- The use of an action that lists `writes` is one Access Evaluation with `"context":{"usage":{"units":1}}`. The context holds
  nothing else. The question is never part of a batch and never has the match `subtree`.
- Stowr asks the Others first, without usage, in one batch when there are two or more. A refused Other gives 403 and no usage
  question. Then Stowr asks the use.
- Stowr removes each repeated Other, and each Other that equals the use. Thus the same question without usage never comes
  before the question with usage (P 10.1).
- The PDP takes the unit when it answers the question. Stowr takes the unit before the effect. A request that fails later
  (an unknown key, an error of the provider) has used its unit. Stowr never returns a unit.
- An upload takes one unit when it starts. A job takes one unit when its route starts it. A chunk, a read of a job and
  a control of a job take none.
- The process ceiling comes before the PDP. A request that the `permissions` of the block refuse takes no unit.

The answers:

| Answer of the PDP | Result |
|---|---|
| `true` with `context.usage.units` 0 or 1 | The operation runs. |
| `true` without `context.usage.units` | Stowr reads it as `false` (P 6.12): `403 forbidden`. |
| `false` with `budget_exceeded` and a `retry_after` of 1 or more | `429 too_many_requests` with `Retry-After` |
| `false` with another reason, or no reason | `403 forbidden`. Stowr never sends the reason to the client. |
| No answer, an HTTP error, or an answer that is not valid | `503 pdp_unavailable` |
| `false` with `budget_exceeded` and a `retry_after` that is not a whole number of 1 or more | `503 pdp_unavailable` |

Stowr never sends a usage question again, also after a timeout or a lost answer (P 10.6). The PDP can have counted the unit.
Stowr answers 503.

## The ceiling max_object_size

The PDP gives the ceiling in `context.limits` of a `true` answer. The value is a number of bytes. Stowr applies it to the part
of the operation that the permission lets run. When two permissions of one operation give a value for one action, the lower
value applies.

- The value of `object.read` bounds each object that the operation reads: a `GET` of bytes, a range read for a preview, the
  source of a copy or a move, and the archive that a route opens.
- The value of `object.write` bounds each object that the operation writes: a `PUT`, the destination of a copy or a move, a
  restored version, a copy for a change of metadata, an extracted member and an upload.
- A `HEAD`, a list, the tags and the reads of metadata have no check.

A size that is over the ceiling gives `403` with this body, before the first byte:

```json
{"error":{"code":"ceiling_exceeded","action":"object.read","ceiling":"max_object_size","limit":1048576,"message":"..."}}
```

Stowr writes the line `access.ceiling_refused`. The body has no `context.reason`. Stowr never cuts an answer.

- A ZIP of a folder, a copy or a move of a prefix, and an extraction check all the known sizes before the first effect.
- A job keeps the ceilings of the request that made it. Its reads and controls do not change them.
- When the size of an object is not known and a ceiling applies, Stowr refuses the operation.

A `true` answer with a ceiling name that the action does not list gives `403 forbidden` (P 7.5). Stowr writes the line
`access.ceiling_unenforceable`. A `true` answer with the name of a budget in `context.limits` is not valid: `503`. Stowr ignores
`context.limits` of a `false` answer. A value that is not a whole number of 0 or more is not valid: `503`.

## The options route

`GET /.well-known/procurauth/resources` lists the children of a node, page by page (P 5.6 to 5.8). It shows only the children
that the subject can see. It needs the identity token. It has no action of its own.

Query parameters:

| Parameter | Rule |
|---|---|
| `type` | Mandatory. `bucket`, `folder` or `subfolder`. It must be the type of the children of `parent`. |
| `parent` | Absent for the buckets. Else a path such as `bucket:b/folder:f`. Each id is percent-encoded. |
| `q` | A search text of 256 bytes at most. The rule depends on the type (refer below). |
| `limit` | A whole number of digits from 1. A value above 50 gives 50. |
| `after` | The cursor of the previous answer. |

The rules of the query:

- Stowr reads the raw query. It splits it at `&` and at the first `=`. It decodes each name and each value one time. It does
  not read `+` as a space. A bad percent-encoding gives `400 invalid_options_request`.
- A parameter that occurs two times gives `400`. A request without `type` gives `400`. A `type` that is not the type of the children
  of `parent` gives `400`. A parameter that Stowr does not know is ignored.
- The query has 16 KiB at most. A cursor that this route did not make for this type gives `400`.
- `HEAD` gives no body. Another method gives 405.

The answer has `items` and `next`. An item has `id`, `label` and `has_children`. An answer has 50 items at most. A label has 60
characters at most. `next` is `null` on the last page.

- For the buckets, `q` is a substring of the id or of the label, without case. The items are sorted by the lower-case id.
  `has_children` is true. The cursor holds the last id that Stowr examined.
- For the folders and the subfolders, `q` is a prefix of the id, with case. The storage can search only by prefix. A `q` with
  a `/` finds nothing. The children come from a list of the storage with the delimiter `/`, below `root_prefix` and the
  parent. The list uses the credentials of Stowr, and it reads at most `max_scan_pages` pages of 1000 keys. `has_children`
  is true for a folder and false for a subfolder.
- A parent that does not exist, and a bucket whose `permissions` have no `read`, give no items. This is not an error.

For each child, Stowr asks the PDP one question with the match `subtree` and no usage. It asks `object.read`, `object.write`,
`object.delete`, `object.lock` and `bucket.admin`, in this order. It asks only the actions that the document publishes for the type
of the child. It stops at the first `true` answer. A child is visible when one answer is `true`.

One request asks at most 200 questions. When this bound stops the walk, the answer holds the items that Stowr found and a
cursor. The answer can have no item and a cursor. A PDP that gives no valid answer gives `503 pdp_unavailable`, never an
empty list. The log line `access.options` gives the number of items and of questions.

## Display rules

The interface shows what the person can do. These rules are for the display only. The server checks each operation (refer
to "The routes").

- `GET /api/instances` lists only the buckets where the person holds one action somewhere. For each bucket, Stowr asks the
  published actions on the bucket node with the match `subtree`. When the person holds `object.read`, `object.write` or
  `object.delete` nowhere in a bucket, the capability `read`, `write` or `delete` shows the source `access`. A question takes
  no unit. A PDP that gives no valid answer gives `503`, never an empty list.
- `GET /api/list` gives `"access": {"read","write","delete"}` for the node of the prefix. The interface uses it to hide
  the controls of upload and delete.
- When the person cannot read the node but can read a deeper folder, `GET /api/list` gives a navigation list. It holds only the
  folders on the path to a folder that the person can read. Stowr removes the objects.
- At the deepest level (a subfolder), such a request gets 403.

A match `subtree` never lets an operation run (P 4.4). The document publishes every bucket label. Thus a hidden bucket is not a
secret.

## Errors

The body of an error is `{"error":{"code":"...","message":"..."}}`. An access error also has `action`. The routes `/s3` and
`/open/` answer the message as plain text with the same status.

| Status | Code | Cause |
|---|---|---|
| 401 | `unauthenticated` | No token, a token that is not valid, or two copies of the header. |
| 403 | `forbidden` | The PDP answered `false` without `budget_exceeded`. Or an unpublished action, a ceiling that the action does not list, a route with no rule (action `unknown`). The body has `action`. |
| 403 | `ceiling_exceeded` | A size over `max_object_size`. The body has `action`, `ceiling` and `limit`. |
| 403 | `permission_denied` | The `permissions` of the bucket or the volume, or `server.access_mode`, refuse the operation. No PDP question. |
| 404 | `unknown_instance` | The instance id does not exist. No PDP question. |
| 404 | `unknown_job` | The job does not exist, or its permissions are refused. |
| 404 | `sqlite_session_not_found` | The session does not exist, or its permissions are refused. |
| 400 | `invalid_options_request` | A query of the options route is not valid. |
| 429 | `too_many_requests` | The budget is used. The header `Retry-After` has the wait in seconds. The body has `retryable: true`. |
| 429 | `resource_budget_exceeded` | The budget of the process for one request (`server.max_storage_*`). It has no `Retry-After`. |
| 503 | `pdp_unavailable` | The PDP gave no valid answer. The body has `retryable: true`. |

Do not confuse the two codes 429. The budget `writes` is a ProcurAuth budget that the PDP counts: the code is
`too_many_requests`. The budget of the process limits the provider bytes and calls of one request: the code is
`resource_budget_exceeded`.

A PDP failure gives 503, never 403. The route fails closed. For the code `pdp_unavailable`, the page of the browser shows
"The access decision service does not answer. Try again later." It does not show the notice of a server that does not answer.

## The PDP client

Stowr sends each question to the PDP with the AuthZEN Authorization API 1.0.

- The default paths are `/access/v1/evaluation` and `/access/v1/evaluations`. Stowr reads no metadata of the PDP.
- Stowr sends the PDP token in `Authorization: Bearer`. It sends `Content-Type`, `Accept` and `X-Request-ID`. The request id
  is the `X-Request-ID` of the client when it has 1 to 128 safe characters. Else Stowr makes one.
- One question is one Access Evaluation. Two or more questions without usage are one Access Evaluations request, with the
  subject at the top. A request has `max_evaluations` items at most. Stowr splits a longer batch. Stowr asks a repeated
  question one time.
- `timeout` is the deadline of a call without usage. One deadline covers all the requests of the call. `usage_timeout` is the
  deadline of a question with usage. The default of `timeout` is `500ms`. The default of `usage_timeout` is `2s`.
- Stowr never sends a question again. When a request fails, Stowr sends no other request of the call. Each question without
  an answer is a failure.
- The client follows no redirect, uses no proxy, and uses TLS 1.2 or newer. It reads at most 256 KiB of an answer. The
  dial timeout is 5 s.
- The client keeps at most 4 idle connections. It closes an idle connection after 5 s, before the PDP does.
- `plain_http = true` accepts an `http` URL. Use it only on a trusted network.
- A failure is: no answer before the deadline, an HTTP status other than 200, a body that is not valid, or a wrong number
  of items. A status 401 or 403 of the PDP writes the line `access.pdp_credential_refused`.

### The answer cache

`cache_ttl` is the longest life of a kept answer. The default is `30s`. `0s` keeps no answer. A revoked grant can stay active
for `cache_ttl` at most.

- The key is the subject, the action, the resource with its chain, and the match.
- Stowr keeps the answers of the usage questions apart from the others.
- Stowr keeps a `true` answer of a usage question only when `units` is 0. It keeps it for `cache_ttl`. A kept answer of this
  kind lets the use run without a question. Stowr never keeps a `true` answer with `units` of 1.
- Stowr keeps a `budget_exceeded` answer for the lower of `retry_after` and `cache_ttl`. A hit gives the seconds that remain,
  at least 1.
- Stowr never keeps a failure. It keeps no other answer of a usage question.
- The two tables hold 16384 entries and 8 MiB at most. When they are full, Stowr drops the expired entries. When they are
  still full, Stowr keeps no new answer.
- A cache hit writes no `access.decision` line, unless `server.log_mode` is `detailed`.

## Log lines

Each line is `key=value`. A request with a valid token also gives `subject` and `request_id` in the request log.

| Event | When |
|---|---|
| `access.unauthenticated` | A token is refused. Fields: `request_id`, `method`, `path`, `reason`. |
| `access.no_rule` | A route has no access rule. |
| `access.decision` | One question. Fields: `request_id`, `subject`, `action`, `resource`, `match`, `usage`, `decision`, `units`, `retry_after`, `reason`, `source` (`pdp`, `cache` or `error`), `failed`. |
| `access.pdp_failed` | A request to the PDP failed. Fields: `request_id`, `evaluations`, `error`. |
| `access.pdp_credential_refused` | The PDP answered 401 or 403. |
| `access.pdp_invalid_answer` | A `true` answer lists a budget in `context.limits`. |
| `access.ceiling_unenforceable` | A `true` answer has a ceiling that the action does not list. |
| `access.ceiling_refused` | A size is over `max_object_size`. Fields: `request_id`, `action`, `ceiling`, `limit`, `size`. |
| `access.options` | A request to the options route. Fields: `request_id`, `subject`, `type`, `parent`, `items`, `questions`. |
| `config.warning` | A warning at start. |

The line `access.decision` shows the subject and the node. With `server.log_mode = "anonymous"`, it shows `-` in place of
the folder ids and the subfolder ids, because they are parts of object keys. The mode `detailed` shows them.

## Volumes and symbolic links

A grant gives access to a folder by the key of the object, not by its real path. A symbolic link from one folder to another
folder would let a person read the other folder.

With `mode = "authzen"`, a volume follows no symbolic link in the path of a key. This rule applies to each link, also a link to
a place inside the volume.

- Each link in the path of a key stops the operation with `403 volume_link_refused`.
- The list of a folder shows such a link as an unreadable entry. A walk of a tree leaves it out.
- Stowr opens the last part of a file for a read or a copy with `O_NOFOLLOW`.
- Stowr resolves `path` and `root_prefix` one time at start. A link in them is not a link of a key.

With `mode = "open"`, the old rule stays: Stowr follows a link to a place inside the root, and refuses a link that leaves it.
Stowr never makes a symbolic link. A process outside Stowr that changes a link between the check and the use of a path can
race the check.

## Rotate a key

1. Add the new public key to the text of `public_keys`: a new PEM block, or a new key of the JWK Set with its `kid`.
   Keep the old key. Restart Stowr.
2. Make the edge sign with the new key.
3. Wait for `max_age` plus `clock_skew`: then each token that the old key signed is expired.
4. Remove the old key from the text of `public_keys`. Restart Stowr.

Stowr reads the keys one time at start. A restart clears the cache of tokens.

## Bounds that are not keys

| Bound | Value |
|---|---|
| Cache of identity tokens | 4096 entries |
| Size of an identity token | 16384 bytes |
| Keys in `public_keys` (PEM blocks or keys of the JWK Set) | 16 |
| Base64 text of one PEM block | 4096 bytes |
| Answer cache of the PDP | 16384 entries and 8 MiB |
| Body of a PDP answer | 256 KiB |
| Idle connections to the PDP | 4, closed after 5 s |
| Dial timeout to the PDP | 5 s |
| Questions of one options request | 200 |
| Items of one options page | 50 |
| Query of the options route | 16 KiB; `q` 256 bytes; cursor 2048 bytes |
| Action document | 64 KiB, 256 resources |
| Id of a node, `sub`, `kid`, issuer | 256 bytes (issuer 1024) |
| Depth of the JSON of a token | 32 |

## Deviations from ProcurAuth

| Deviation | Reason |
|---|---|
| `/healthz` needs no token (P 10.0 step 2). | The health check of the container calls it. It gives no data. |
| The static files, the pages, `/api/build` and `OPTIONS /s3` need the token but ask no question (P 10.1, P 15 item 6). | They hold no data of a bucket. The protocol has no rule for them yet. |
| `plain_http = true` accepts an `http` URL for the PDP (P 10.0 step 4). | A trusted network can be enough. Stowr writes a warning at start. |
| Stowr reads no `/.well-known/authzen-configuration` of the PDP. | P 10.0 step 4 accepts the default paths. |
| A continuation asks without usage, also when the action lists a budget. | The use already took its unit. The protocol has no rule for a continuation. |
| A move uses `object.delete`. | It is the most destructive action of the move. One operation takes one unit. |
| The options route searches a substring for the buckets and a prefix for the folders. | P 5.6 lets the service choose its search rules. The storage can search only by prefix. |

## Conformance

The test of the ProcurAuth conformance runner against Stowr is in [CONFORMANCE.md](CONFORMANCE.md).
