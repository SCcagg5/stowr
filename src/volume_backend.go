package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The backend of a volume block: the files of a folder of the server as the objects of a bucket. Folders are real: a
// listing with the delimiter "/" gives the subfolders, and an empty folder exists. A listing without a delimiter gives
// the files and the empty folders (as "<folder>/" with size 0, as the folder objects of S3) in the order of the keys of
// S3, so that the jobs of Stowr (delete, copy, move, Insights, archive, search) work as on a bucket. The page
// token of a listing is the last key of the page: it stays valid when the folder changes.
//
// A file has no metadata of S3: the content type comes from the extension of the name, and the ETag is the weak
// validator W/"<size>-<modification time in nanoseconds>". Each write goes to a temporary file in the folder of the
// target, is synchronized to the disk, and replaces the target with one rename: a reader sees the old file or the new
// file, never a part. The files of this backend are the only files of the program that write to the local file system
// (the CI checks it).

const (
	// volumeMaxFolderEntries is the largest folder that a listing reads. A larger folder gives an error.
	volumeMaxFolderEntries = 250_000
	// volumeListPageSize is the largest page of a listing: the page size of S3.
	volumeListPageSize = 1000
	// volumeCopyProgressStep is the number of copied bytes between two progress reports of a large copy.
	volumeCopyProgressStep = int64(8 << 20)
	// volumeFolderContentType is the content type of a folder key ("<folder>/") in a HEAD answer.
	volumeFolderContentType = "application/x-directory"
)

type volumeBackend struct {
	cfg bucketConfig
	// root is the real path of the folder that the browser shows: path, then root_prefix, without symbolic links.
	root string
	// prefix is the root_prefix of the block. Each key of the storage instance starts with it.
	prefix string
	// refuseLinks is true when the volume follows no symbolic link (volumeConfig.RefuseLinks).
	refuseLinks bool
	// writes is true when the start checked that the process can write in the root.
	writes bool
	// sizer counts the size of the files of the root in the background. A write marks its count old.
	sizer *volumeSizer
}

// newVolumeBackend opens the folder of a volume block. When the server can write in the volume (the permissions
// include write or delete, and server.access_mode is not force_read_only), it writes and removes a temporary file in
// the root: a folder that the process cannot write stops the start.
func newVolumeBackend(volume volumeConfig, forceReadOnly bool) (*volumeBackend, error) {
	root, err := filepath.EvalSymlinks(volume.Path)
	if err != nil {
		return nil, fmt.Errorf("volume %q: path %q: cannot resolve the symbolic links: %v", volume.ID, volume.Path, err)
	}
	if volume.RootPrefix != "" {
		if root, err = resolveInside(root, strings.TrimSuffix(volume.RootPrefix, "/")); err != nil {
			return nil, fmt.Errorf("volume %q: root_prefix %q: %v", volume.ID, volume.RootPrefix, err)
		}
	}
	backend := &volumeBackend{cfg: volume.bucketConfig(), root: root, prefix: volume.RootPrefix, refuseLinks: volume.RefuseLinks}
	backend.sizer = newVolumeSizer(root)
	if volume.writes() && !forceReadOnly {
		if err := backend.probeWrite(); err != nil {
			return nil, fmt.Errorf("volume %q: the process cannot write in path %q, and the permissions include write or delete (a block without permissions has all three): %v; give permissions = [\"read\"] for a volume that is read only", volume.ID, volume.Path, err)
		}
		backend.writes = true
	}
	return backend, nil
}

// newVolumeInstance makes the storage instance of a volume block.
func newVolumeInstance(volume volumeConfig, policy runtimePolicy, concurrency *storageConcurrency) (*storageInstance, error) {
	backend, err := newVolumeBackend(volume, policy.forceReadOnly())
	if err != nil {
		return nil, err
	}
	instance := newInstanceOfBackend(backend.cfg, backend, policy, concurrency)
	// The checks at start read and wrote the folder: the permissions of the process are known.
	discovered, err := backend.DiscoverCapabilities(context.Background())
	instance.caps = mergeCapabilitiesWithPolicy(backend.cfg, instance.forceReadOnly, discovered, err)
	if volumeUsageSupported {
		instance.usage.reader, instance.usage.source = &volumeUsageReader{root: backend.root, sizer: backend.sizer}, usageSourceStatfs
	}
	return instance, nil
}

// probeWrite makes and removes a temporary file in the root.
func (b *volumeBackend) probeWrite() error {
	name, err := volumeTempName("")
	if err != nil {
		return err
	}
	probe := filepath.Join(b.root, name)
	file, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	closeErr := file.Close()
	if err := os.Remove(probe); err != nil {
		return err
	}
	return closeErr
}

// relative gives the key relative to the root of the volume. A key of the instance always starts with the root prefix.
func (b *volumeBackend) relative(full string) (string, error) {
	if !strings.HasPrefix(full, b.prefix) {
		return "", apiError{Status: http.StatusForbidden, Code: "volume_outside_root", Message: "the key is outside the root_prefix of the volume"}
	}
	return full[len(b.prefix):], nil
}

func (b *volumeBackend) parse(full string, write bool) (volumeKey, error) {
	relative, err := b.relative(full)
	if err != nil {
		return volumeKey{}, err
	}
	return parseVolumeKey(relative, write)
}

// volumeETag is the weak validator of a file: its size and its modification time in nanoseconds.
func volumeETag(info os.FileInfo) string {
	return fmt.Sprintf(`W/"%d-%d"`, info.Size(), info.ModTime().UnixNano())
}

// volumeTextTypes are the content types of common text files. The table of the mime package of Go does not have them
// without /etc/mime.types, and the release image has no such file.
var volumeTextTypes = map[string]string{
	".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8", ".md": "text/markdown; charset=utf-8",
	".csv": "text/csv; charset=utf-8", ".tsv": "text/tab-separated-values; charset=utf-8",
	".yaml": "application/yaml", ".yml": "application/yaml", ".toml": "application/toml",
	".zip": "application/zip", ".gz": "application/gzip", ".tar": "application/x-tar",
}

// volumeContentType gives the content type of a key from the extension of its name.
func volumeContentType(key string) string {
	if value := volumeTextTypes[strings.ToLower(path.Ext(key))]; value != "" {
		return value
	}
	if value := previewContentType(key); value != "" {
		return value
	}
	return "application/octet-stream"
}

func (b *volumeBackend) object(key string, info os.FileInfo) objectInfo {
	return objectInfo{
		Key: b.prefix + key, Size: info.Size(), LastModified: info.ModTime().UTC(), ETag: volumeETag(info),
		ContentType: volumeContentType(key),
	}
}

// --- Listing --------------------------------------------------------------------------------------------------------

type volumeEntryKind int

const (
	volumeEntryFile volumeEntryKind = iota
	volumeEntryFolder
	// volumeEntryUnreadable is a symbolic link that leaves the root or that does not resolve (each symbolic link with
	// RefuseLinks), or a file that is not a regular file (device, socket, pipe). The listing with a delimiter shows it. No operation reads it.
	volumeEntryUnreadable
)

type volumeEntry struct {
	name string
	// sortKey is the name, with "/" after the name of a folder: the entries then have the order of the keys of S3.
	sortKey string
	kind    volumeEntryKind
	// link is true for a symbolic link. real is then its target, in the root.
	link bool
	real string
}

// readFolder reads the entries of the real folder whose names start with namePrefix, in the order of their keys. The
// temporary files of Stowr are left out.
func (b *volumeBackend) readFolder(ctx context.Context, real, namePrefix string) ([]volumeEntry, error) {
	folder, err := os.Open(real)
	if err != nil {
		return nil, volumeError(err)
	}
	defer folder.Close()
	entries := make([]volumeEntry, 0, 64)
	for {
		batch, readErr := folder.ReadDir(1024)
		for _, item := range batch {
			name := item.Name()
			if !strings.HasPrefix(name, namePrefix) {
				continue
			}
			if _, temporary := parseVolumeTempName(name); temporary {
				continue
			}
			entry := volumeEntry{name: name, sortKey: name, kind: volumeEntryUnreadable}
			mode := item.Type()
			switch {
			case mode.IsDir():
				entry.kind = volumeEntryFolder
			case mode.IsRegular():
				entry.kind = volumeEntryFile
			case mode&fs.ModeSymlink != 0:
				entry.link = true
				if b.refuseLinks {
					// The entry stays unreadable: the volume follows no link.
					break
				}
				if target, err := filepath.EvalSymlinks(filepath.Join(real, name)); err == nil && insideRoot(target, b.root) {
					if info, err := os.Stat(target); err == nil && info.IsDir() {
						entry.kind, entry.real = volumeEntryFolder, target
					} else if err == nil && info.Mode().IsRegular() {
						entry.kind, entry.real = volumeEntryFile, target
					}
				}
			}
			if entry.kind == volumeEntryFolder {
				entry.sortKey += "/"
			}
			entries = append(entries, entry)
			if len(entries) > volumeMaxFolderEntries {
				return nil, apiError{Status: http.StatusUnprocessableEntity, Code: "volume_folder_too_large", Message: fmt.Sprintf("the folder has more than %d entries: Stowr does not list it", volumeMaxFolderEntries)}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, volumeError(readErr)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].sortKey < entries[j].sortKey })
	return entries, nil
}

// entryInfo gives the information of a file entry: of its target for a symbolic link.
func entryInfo(real string, entry volumeEntry) (os.FileInfo, error) {
	if entry.link {
		return os.Stat(entry.real)
	}
	return os.Lstat(filepath.Join(real, entry.name))
}

// List gives one page of the keys under the prefix, after the page token and after StartAfter. With the delimiter
// "/", it reads one folder. Without a delimiter, it reads the tree. A prefix that does not exist gives an empty page.
func (b *volumeBackend) List(ctx context.Context, options listOptions) (listPage, error) {
	if options.Delimiter != "" && options.Delimiter != "/" {
		return listPage{}, apiError{Status: http.StatusBadRequest, Code: "invalid_delimiter", Message: "a volume lists with the delimiter \"/\" or without a delimiter"}
	}
	prefix, err := b.relative(options.Prefix)
	if err != nil {
		return listPage{}, err
	}
	marker := ""
	for _, value := range []string{options.StartAfter, options.PageToken} {
		if value == "" {
			continue
		}
		relative, err := b.relative(value)
		if err != nil {
			return listPage{}, apiError{Status: http.StatusBadRequest, Code: "invalid_page_token", Message: "the page token of the listing is not valid: open the folder again"}
		}
		if relative > marker {
			marker = relative
		}
	}
	limit := options.MaxResults
	if limit <= 0 || limit > volumeListPageSize {
		limit = volumeListPageSize
	}
	folderKey, namePrefix := "", prefix
	if slash := strings.LastIndexByte(prefix, '/'); slash >= 0 {
		folderKey, namePrefix = prefix[:slash+1], prefix[slash+1:]
	}
	parsed, err := parseVolumeKey(folderKey, false)
	if err != nil {
		return listPage{}, err
	}
	real, err := b.resolveFolder(parsed.parts, false)
	if err != nil {
		if isStorageNotFound(err) || isVolumeNotFolder(err) {
			return listPage{}, nil
		}
		return listPage{}, err
	}
	page := listPage{}
	count := 0
	// add takes one key of the page. It gives false when the page is full: the key of the last item is then the
	// token of the next page.
	add := func() bool {
		count++
		return count <= limit
	}
	if options.Delimiter == "/" {
		entries, err := b.readFolder(ctx, real, namePrefix)
		if err != nil {
			return listPage{}, err
		}
		for _, entry := range entries {
			key := folderKey + entry.sortKey
			if key <= marker {
				continue
			}
			if !add() {
				page.NextPageToken = lastListKey(page)
				break
			}
			switch entry.kind {
			case volumeEntryFolder:
				page.Prefixes = append(page.Prefixes, b.prefix+key)
			case volumeEntryFile:
				info, err := entryInfo(real, entry)
				if err != nil {
					page.Objects = append(page.Objects, objectInfo{Key: b.prefix + key, Unreadable: true})
					continue
				}
				page.Objects = append(page.Objects, b.object(key, info))
			default:
				page.Objects = append(page.Objects, objectInfo{Key: b.prefix + key, Unreadable: true})
			}
		}
		return page, nil
	}
	err = b.walk(ctx, real, folderKey, namePrefix, marker, func(object objectInfo) bool {
		if !add() {
			page.NextPageToken = lastListKey(page)
			return false
		}
		page.Objects = append(page.Objects, object)
		return true
	})
	return page, err
}

// lastListKey gives the largest key of a page.
func lastListKey(page listPage) string {
	last := ""
	if count := len(page.Objects); count > 0 {
		last = page.Objects[count-1].Key
	}
	if count := len(page.Prefixes); count > 0 && page.Prefixes[count-1] > last {
		last = page.Prefixes[count-1]
	}
	return last
}

// walk gives each key of the tree of the real folder (with the key folderKey) whose name starts with namePrefix, after
// marker, in the order of the keys. A file gives its key. An empty folder gives "<folder>/" with size 0. A symbolic
// link to a folder is not followed (a link can make a loop), and an unreadable entry is left out. walk stops when emit
// gives false.
func (b *volumeBackend) walk(ctx context.Context, real, folderKey, namePrefix, marker string, emit func(objectInfo) bool) error {
	_, err := b.walkFolder(ctx, real, folderKey, namePrefix, marker, emit)
	return err
}

func (b *volumeBackend) walkFolder(ctx context.Context, real, folderKey, namePrefix, marker string, emit func(objectInfo) bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	entries, err := b.readFolder(ctx, real, namePrefix)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 && namePrefix == "" && folderKey != "" && folderKey > marker {
		info, err := os.Stat(real)
		if err != nil {
			return false, volumeError(err)
		}
		object := b.object(folderKey, info)
		object.Size, object.ContentType = 0, volumeFolderContentType
		return emit(object), nil
	}
	for _, entry := range entries {
		key := folderKey + entry.sortKey
		switch {
		case entry.kind == volumeEntryFolder && !entry.link:
			if marker != "" && key < marker && !strings.HasPrefix(marker, key) {
				continue
			}
			more, err := b.walkFolder(ctx, filepath.Join(real, entry.name), key, "", marker, emit)
			if err != nil || !more {
				return more, err
			}
		case entry.kind == volumeEntryFile:
			if key <= marker {
				continue
			}
			info, err := entryInfo(real, entry)
			if err != nil {
				continue
			}
			if !emit(b.object(key, info)) {
				return false, nil
			}
		}
	}
	return true, nil
}

func isVolumeNotFolder(err error) bool {
	var known apiError
	return errors.As(err, &known) && known.Code == "volume_not_folder"
}

// --- Read -----------------------------------------------------------------------------------------------------------

func volumeHeaders(object objectInfo) http.Header {
	headers := make(http.Header)
	headers.Set("Content-Length", strconv.FormatInt(object.Size, 10))
	headers.Set("Content-Type", object.ContentType)
	headers.Set("ETag", object.ETag)
	headers.Set("Last-Modified", object.LastModified.UTC().Format(http.TimeFormat))
	headers.Set("Accept-Ranges", "bytes")
	return headers
}

// stat gives the information of a key: a regular file, or a folder for a key that ends with "/".
func (b *volumeBackend) stat(full string) (string, objectInfo, error) {
	key, err := b.parse(full, false)
	if err != nil {
		return "", objectInfo{}, err
	}
	real, info, err := b.resolveExisting(key)
	if err != nil {
		return "", objectInfo{}, err
	}
	relative, _ := b.relative(full)
	switch {
	case key.folder && info.IsDir():
		object := b.object(relative, info)
		object.Size, object.ContentType = 0, volumeFolderContentType
		return real, object, nil
	case key.folder || info.IsDir():
		return "", objectInfo{}, &upstreamError{StatusCode: http.StatusNotFound, Code: "NoSuchKey", Message: "the file does not exist"}
	case !info.Mode().IsRegular():
		return "", objectInfo{}, apiError{Status: http.StatusForbidden, Code: "volume_not_file", Message: "the path is not a regular file: Stowr does not read it"}
	}
	return real, b.object(relative, info), nil
}

func (b *volumeBackend) Head(_ context.Context, key string) (objectResponse, error) {
	_, object, err := b.stat(key)
	if err != nil {
		return objectResponse{}, err
	}
	return objectResponse{StatusCode: http.StatusOK, Header: volumeHeaders(object)}, nil
}

// volumeFileReader reads a part of an open file and closes the file.
type volumeFileReader struct {
	*io.SectionReader
	file *os.File
}

func (r *volumeFileReader) Close() error { return r.file.Close() }

// Get reads a file with the conditions and the range of the request headers, as an S3 provider does: If-Match and
// If-Unmodified-Since give 412, If-None-Match and If-Modified-Since give 304, one byte range gives 206, and a range
// after the end gives 416. The comparison of an ETag is weak. An If-Range with an ETag never matches (the ETag is weak):
// the answer is then the whole file.
func (b *volumeBackend) Get(_ context.Context, key string, requestHeaders http.Header) (objectResponse, error) {
	real, object, err := b.stat(key)
	if err != nil {
		return objectResponse{}, err
	}
	if strings.HasSuffix(key, "/") {
		return objectResponse{StatusCode: http.StatusOK, Header: volumeHeaders(object), Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	file, err := b.openRead(real)
	if err != nil {
		return objectResponse{}, volumeError(err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return objectResponse{}, volumeError(err)
	}
	relative, _ := b.relative(key)
	object = b.object(relative, info)
	headers := volumeHeaders(object)
	if status := volumePrecondition(requestHeaders, object); status != 0 {
		file.Close()
		if status == http.StatusNotModified {
			headers.Del("Content-Length")
			return objectResponse{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return objectResponse{}, &upstreamError{StatusCode: status, Code: "PreconditionFailed", Message: "the file does not match the condition of the request"}
	}
	start, length, status := volumeRange(requestHeaders, object)
	switch status {
	case http.StatusRequestedRangeNotSatisfiable:
		file.Close()
		return objectResponse{}, &upstreamError{StatusCode: status, Code: "InvalidRange", Message: "the range starts after the end of the file"}
	case http.StatusPartialContent:
		headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, object.Size))
		headers.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	return objectResponse{StatusCode: status, Header: headers, Body: &volumeFileReader{SectionReader: io.NewSectionReader(file, start, length), file: file}}, nil
}

// sameWeakETag compares two ETags without their weak mark (W/).
func sameWeakETag(left, right string) bool {
	return strings.TrimPrefix(strings.TrimSpace(left), "W/") == strings.TrimPrefix(strings.TrimSpace(right), "W/")
}

// etagListMatches tells if the value of If-Match or If-None-Match ("*", or ETags separated by commas) matches etag.
func etagListMatches(value, etag string) bool {
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "*" || item != "" && sameWeakETag(item, etag) {
			return true
		}
	}
	return false
}

// volumePrecondition evaluates the conditions of a read in the order of RFC 9110. It gives 412, 304, or 0 when the
// read goes on. The dates compare to the second, as the HTTP date has seconds only.
func volumePrecondition(headers http.Header, object objectInfo) int {
	modified := object.LastModified.Truncate(time.Second)
	if value := headers.Get("If-Match"); value != "" {
		if !etagListMatches(value, object.ETag) {
			return http.StatusPreconditionFailed
		}
	} else if value := headers.Get("If-Unmodified-Since"); value != "" {
		if since, err := http.ParseTime(value); err == nil && modified.After(since) {
			return http.StatusPreconditionFailed
		}
	}
	if value := headers.Get("If-None-Match"); value != "" {
		if etagListMatches(value, object.ETag) {
			return http.StatusNotModified
		}
	} else if value := headers.Get("If-Modified-Since"); value != "" {
		if since, err := http.ParseTime(value); err == nil && !modified.After(since) {
			return http.StatusNotModified
		}
	}
	return 0
}

// volumeRange gives the part of the file that a read sends: the start, the length and the status (200, 206 or 416).
// A Range header that is not one valid byte range is ignored, as RFC 9110 lets a server do: the answer is the whole
// file. An empty file has no range: the answer is the whole (empty) file.
func volumeRange(headers http.Header, object objectInfo) (int64, int64, int) {
	value := strings.TrimSpace(headers.Get("Range"))
	if value == "" || object.Size == 0 {
		return 0, object.Size, http.StatusOK
	}
	if condition := strings.TrimSpace(headers.Get("If-Range")); condition != "" {
		if strings.HasPrefix(condition, `"`) || strings.HasPrefix(condition, "W/") {
			return 0, object.Size, http.StatusOK
		}
		if since, err := http.ParseTime(condition); err != nil || !object.LastModified.Truncate(time.Second).Equal(since) {
			return 0, object.Size, http.StatusOK
		}
	}
	if !strings.HasPrefix(strings.ToLower(value), "bytes=") || strings.Contains(value, ",") {
		return 0, object.Size, http.StatusOK
	}
	bounds := strings.SplitN(strings.TrimSpace(value[len("bytes="):]), "-", 2)
	if len(bounds) != 2 {
		return 0, object.Size, http.StatusOK
	}
	left, right := strings.TrimSpace(bounds[0]), strings.TrimSpace(bounds[1])
	if left == "" {
		suffix, err := strconv.ParseInt(right, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, object.Size, http.StatusOK
		}
		suffix = min(suffix, object.Size)
		return object.Size - suffix, suffix, http.StatusPartialContent
	}
	start, err := strconv.ParseInt(left, 10, 64)
	if err != nil || start < 0 {
		return 0, object.Size, http.StatusOK
	}
	end := object.Size - 1
	if right != "" {
		if end, err = strconv.ParseInt(right, 10, 64); err != nil || end < start {
			return 0, object.Size, http.StatusOK
		}
		end = min(end, object.Size-1)
	}
	if start >= object.Size {
		return 0, 0, http.StatusRequestedRangeNotSatisfiable
	}
	return start, end - start + 1, http.StatusPartialContent
}

// --- Capabilities and usage -----------------------------------------------------------------------------------------

// DiscoverCapabilities checks the permissions of the process: it reads the root, and, when the server can write in
// the volume, it writes and removes a temporary file.
func (b *volumeBackend) DiscoverCapabilities(_ context.Context) (discoveredCapabilities, error) {
	states := map[string]capabilityState{}
	read := capabilityState{Allowed: true, Verified: true, Source: "volume-read-probe", Reason: "the process can read the folder of the volume"}
	if folder, err := os.Open(b.root); err != nil {
		read = capabilityState{Allowed: false, Verified: true, Source: "volume-read-probe", Reason: "the process cannot read the folder of the volume"}
	} else {
		if _, err := folder.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
			read = capabilityState{Allowed: false, Verified: true, Source: "volume-read-probe", Reason: "the process cannot read the folder of the volume"}
		}
		folder.Close()
	}
	states[permissionRead] = read
	if b.writes {
		write := capabilityState{Allowed: true, Verified: true, Source: "volume-write-probe", Reason: "the process can write and remove a file in the folder of the volume"}
		if err := b.probeWrite(); err != nil {
			write = capabilityState{Allowed: false, Verified: true, Source: "volume-write-probe", Reason: "the process cannot write a file in the folder of the volume"}
		}
		states[permissionWrite], states[permissionDelete] = write, write
	}
	return discoveredCapabilities{States: states}, nil
}

// volumeUsageReader gives the usage of a volume. The part of the bucket is the size of the files of the folder, from
// the count of its sizer (volume_size.go): the read never walks the tree. The part of the storage is the file system of
// the folder (statfs): used = size - available, total = size, free = available. These numbers are for the whole file
// system. They are exact at the time of the read, except on a FUSE or network file system, which can count with a
// delay: they are then approximate.
type volumeUsageReader struct {
	root  string
	sizer *volumeSizer
	// statfs gives the size and the available bytes of a file system, and tells if it counts with a delay. Nil is
	// volumeStatfs. Tests set it.
	statfs func(string) (uint64, uint64, bool, error)
}

// liveUsage: a read of a volume costs no request, so the server reads it at each request.
func (r *volumeUsageReader) liveUsage() {}

// volumeDelayedReason is the reason of an approximate capacity of a FUSE or network file system.
const volumeDelayedReason = "The volume is on a FUSE or network file system: it can count the used bytes with a delay."

func (r *volumeUsageReader) BucketUsage(_ context.Context, _ string) (bucketUsage, error) {
	if r.sizer == nil {
		return bucketUsage{UsedBytes: -1, UsedObjects: -1, AsOf: timeNowUTC()}, nil
	}
	count := r.sizer.snapshot()
	return bucketUsage{UsedBytes: count.bytes, UsedObjects: count.files, SelfState: count.state, AsOf: count.at}, nil
}

func (r *volumeUsageReader) StorageCapacity(_ context.Context) (storageCapacity, error) {
	statfs := r.statfs
	if statfs == nil {
		statfs = volumeStatfs
	}
	total, available, delayed, err := statfs(r.root)
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			return storageCapacity{}, fmt.Errorf("statfs: the file system of the volume does not give its size: %s", errno.Error())
		}
		return storageCapacity{}, fmt.Errorf("statfs: the file system of the volume does not give its size")
	}
	if total == 0 {
		// weed mount of SeaweedFS 4.41 can give a size of 0 for a short time, for example after a delete.
		return storageCapacity{}, fmt.Errorf("statfs: the file system of the volume gives a size of 0")
	}
	used := uint64(0)
	if total > available {
		used = total - available
	}
	capacity := storageCapacity{
		Scope: storageScopeFilesystem, UsedBytes: clampToInt64(used), TotalBytes: clampToInt64(total), FreeBytes: clampToInt64(available),
		Note: "the file system of the folder of the volume", AsOf: timeNowUTC(),
	}
	if delayed {
		capacity.Approximate, capacity.Reason = true, volumeDelayedReason
	}
	return capacity, nil
}

// clampToInt64 gives value, or the largest int64 for a larger value (a file system of more than 8 EiB).
func clampToInt64(value uint64) int64 {
	if value > uint64(1<<63-1) {
		return 1<<63 - 1
	}
	return int64(value)
}
