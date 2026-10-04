---
title: "GitHub"
description: "GitHub"
versionIntroduced: "v1.76.0"
---

# GitHub

This is a backend for [GitHub](https://github.com/): it exposes a repository
at a given ref (branch, tag or commit SHA) as a filesystem. All traffic goes
directly to GitHub (REST API and raw.githubusercontent.com), no proxying
involved.

## Features

- Listing directories and files, recursive listing (`--fast-list`) via the git trees API.
- Downloads either through raw.githubusercontent.com direct links (default) or
  the contents API raw media type.
- Uploads through the Git Blob API, streamed and base64 encoded on the fly,
  with a hard limit of 100 MiB per file.
- Moving, renaming, copying, creating and deleting entries are pure git tree
  operations: they never transfer file content and are server-side operations.
- Every write operation creates a commit. Commit messages are configurable
  templates.
- The `gitsha1` hash (the git blob SHA-1) is supported, so `rclone check
  --checksum` and `rclone hashsum gitsha1` work without downloading files.

## Limitations

- Git does not track empty directories: `rclone mkdir` creates a `.gitkeep`
  placeholder file, and directories that only contain `.gitkeep` appear empty.
  `.gitkeep` files are hidden from listings.
- Git does not store modification times. By default all files have zero
  modtime, so use `--checksum` or `--size-only` for sync decisions. The
  `accurate_modified_time` option fetches the last commit time of each file
  via the GraphQL API during listings (best effort, adds one request per
  directory, up to 200 entries).
- Every write is a commit: syncing many small files creates many commits and
  is limited by the GitHub API rate limit (5000 requests/hour for
  authenticated users).
- Files larger than 100 MiB are rejected before upload (Git Blob API limit).
- Git submodules are skipped in listings and cannot be downloaded, moved,
  copied or removed.
- Write operations require a branch ref. Tags and commit SHAs are read-only.
- On an empty repository, leave `ref` blank so the default branch is used and
  the first commit can be created.

## Configuration

Here is an example of making a github configuration. First run:

```console
rclone config
```

This will guide you through an interactive setup process:

```text
No remotes found, make a new one\?
n) New remote
s) Set configuration password
q) Quit config
n/s/q> n
name> github
Type of storage to configure.
Enter a string value. Press Enter for the default ("").
Choose a number from below, or type in your own value
[snip]
XX / GitHub (repositories as filesystems, via Git Data API)
   \ "github"
[snip]
Storage> github
** See help for github backend at: https://rclone.org/github/ **

GitHub personal access token with repo scope.
Enter a string value. Press Enter for the default ("").
token> ghp_xxxxxxxxxxxxxxxxxxxxxxxx

Owner of the repository (user or organization).
Enter a string value. Press Enter for the default ("").
owner> example

Name of the repository.
Enter a string value. Press Enter for the default ("").
repo> files

A branch, a tag or a commit SHA. Leave blank for the default branch.
Enter a string value. Press Enter for the default ("").
ref>
```

The token needs a personal access token (classic) with the `repo` scope, or a
fine-grained token with read/write access to "Contents" and "Metadata" of the
repository.

Paths are specified as `remote:path/to/dir`, relative to the repository root.
You can also point the remote at a subdirectory: `remote:sub/dir`.

### Modified time

Git does not store modification times. The backend fetches the last commit
time of each file via the GraphQL API during listings (one request per
directory, up to 200 entries; one request per `NewObject`). This is on by
default because it is the only source of modification times. Turn it off for
maximum listing speed:

```
rclone config update github accurate_modified_time false
```

Note that the reported time is the time of the last commit that touched the
file, not a real mtime, and `rclone sync` will not use it for sync decisions
(the backend reports "modtime not supported" precision).

### Download channel

By default files are downloaded through `raw.githubusercontent.com` direct
links pinned to the current branch head commit. Pinning matters: the CDN
caches branch-path URLs aggressively, so a plain branch URL can return stale
content right after an upload. Commit-pinned URLs are immutable and always
fresh; the head commit is cached for 5 minutes and refreshed immediately
after each rclone commit.

Set `download_via` to `api` to always download through the contents API
instead (always fresh, but downloads then count against the 5000/h REST
quota).

For private repositories the token is sent to `raw.githubusercontent.com`
automatically. When `gh_proxy` is set, the token is NOT sent to the proxy, so
proxied downloads only work for public repositories.

### Commit messages

Each write operation produces one commit. The commit message is rendered from
a text/template template with these variables:

- `{{.ObjName}}`, `{{.ObjPath}}`: name and path of the object being operated on
- `{{.ParentName}}`, `{{.ParentPath}}`: name and path of the parent directory
- `{{.TargetName}}`, `{{.TargetPath}}`: name and path of the destination (rename/copy/move)

The defaults are `rclone upload {{.ObjPath}}`, `rclone delete {{.ObjPath}}`,
etc. Configure them with the `put_commit_message`, `delete_commit_message`,
`mkdir_commit_message`, `rename_commit_message`, `copy_commit_message` and
`move_commit_message` options.

### Standard options

#### --github-token

GitHub personal access token with repo scope.

Properties: Required, Sensitive.

#### --github-owner

Owner of the repository (user or organization).

Properties: Required.

#### --github-repo

Name of the repository.

Properties: Required.

#### --github-ref

A branch, a tag or a commit SHA. Leave blank for the default branch.

Write operations require a branch. On an empty repository leave this blank
so the default branch is used and the first commit can be created.

#### --github-download_via

How files are downloaded.

Properties: Default "raw".

- "raw" - Direct link from raw.githubusercontent.com (fast, CDN cached).
- "api" - Contents API raw media type (always fresh, needed right after writes).

### Modified options

#### --github-gh_proxy

Replacement for https://raw.githubusercontent.com in download links, e.g.
https://gh-proxy.com/raw.githubusercontent.com.

When set, no Authorization header is sent to the proxy, so it only works for
public repositories.

#### --github-accurate_modified_time

Fetch the last commit time of each file via the GraphQL API during listings.

This is the only source of modification times. Adds one request per listed
directory (up to 200 entries) and one per NewObject, falling back to zero
time on failure. Turn off for maximum listing speed if you don't need mtimes.

#### --github-committer-name / --github-committer-email

Committer name and email. Must be set together.

#### --github-author-name / --github-author-email

Author name and email. Must be set together.

#### --github-put-commit-message, --github-delete-commit-message, --github-mkdir-commit-message, --github-rename-commit-message, --github-copy-commit-message, --github-move-commit-message

Commit message templates for the respective operations.

## Rate limits

GitHub allows 5000 requests per hour for authenticated users. Each write
operation costs several API calls (blob + trees + commit + ref), and every
`rclone sync` of many small files will create many commits. The backend
honors `X-RateLimit-Reset` and `Retry-After` headers and retries
automatically, but expect long waits if you exhaust the quota. For bulk
uploads, consider committing files with git directly and using rclone for
reads.

## Hash support

The backend registers the `gitsha1` hash type, which is the SHA-1 of the git
blob object (`sha1("blob <size>\x00" + content)`). This is exactly the `sha`
value GitHub reports, so:

```console
rclone hashsum gitsha1 github:
rclone check --checksum localdir github:dir
```

both work without downloading file content.
