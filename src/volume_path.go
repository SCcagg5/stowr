package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Path safety of a volume.
//
// A key is a path relative to the root of the volume: path, then root_prefix, without symbolic links. The server
// refuses a key with an empty, "." or ".." part, a NUL byte, or a part longer than volumeMaxNameBytes. It resolves each
// part of a path, one after the other. With access.mode = "open", a symbolic link to a place inside the root is
// followed. A symbolic link that leaves the root, or that does not resolve, stops the operation with
// volume_link_refused.
//
// With access.mode = "authzen", the server follows no symbolic link: volumeConfig.RefuseLinks is true. The access
// policy gives a grant on a folder by the key of the object, not by the real path. A link from one folder to another
// folder would let a person read the other folder. Each symbolic link in the path of a key then stops the operation
// with volume_link_refused. The listing shows such a link as an unreadable entry, and a walk leaves it out. The only
// place that follows a link in a path is resolvePart, and the only other place is readFolder. Both read the refusal
// from the same field. The last part of a file that Stowr opens for a read or a copy is opened with O_NOFOLLOW. The
// part of the path that root_prefix gives is resolved at the start, and a link there is not a link of a key.
//
// The server never makes a symbolic link. A key of a write must be valid UTF-8 and must not name a temporary file
// of Stowr.
//
// Limit: a process outside Stowr that changes a symbolic link between the check and the use of a path can race
// the check, for each folder of the path. The API of Stowr cannot make a symbolic link.

const (
	// volumeTempPrefix starts the name of each temporary file of Stowr in a volume. The listings do not show
	// these files: the window "Incomplete uploads" shows them.
	volumeTempPrefix = ".stowr-tmp-"
	// volumeTempIDLength is the length of the random hexadecimal part of a temporary name.
	volumeTempIDLength = 32
	// volumeMaxNameBytes is the longest name of one part of a path (the limit of Linux file systems).
	volumeMaxNameBytes = 255
)

// errVolumeLink is the refusal of a path with a symbolic link that leaves the root of the volume or that does not
// resolve. With RefuseLinks, it is also the refusal of each symbolic link.
var errVolumeLink = apiError{Status: http.StatusForbidden, Code: "volume_link_refused", Message: "the path has a symbolic link that Stowr does not follow: it leaves the volume, it does not resolve, or the access mode refuses each link"}

func invalidVolumeKey(format string, values ...any) error {
	return apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: fmt.Sprintf(format, values...)}
}

// volumeKey is a key of a volume, cut into the parts of its path.
type volumeKey struct {
	// parts are the names of the path. The root of the volume has no part.
	parts []string
	// folder is true for a key that ends with "/": the key names a folder.
	folder bool
}

func (k volumeKey) name() string {
	if len(k.parts) == 0 {
		return ""
	}
	return k.parts[len(k.parts)-1]
}

func (k volumeKey) parent() []string {
	if len(k.parts) == 0 {
		return nil
	}
	return k.parts[:len(k.parts)-1]
}

// parseVolumeKey cuts a key relative to the root of the volume. A key for a write must also be valid UTF-8 and must not
// name a temporary file.
func parseVolumeKey(key string, write bool) (volumeKey, error) {
	parsed := volumeKey{folder: strings.HasSuffix(key, "/")}
	key = strings.TrimSuffix(key, "/")
	if key == "" {
		return parsed, nil
	}
	if strings.ContainsRune(key, 0) {
		return volumeKey{}, invalidVolumeKey("a key of a volume must not contain a NUL byte")
	}
	if write && !utf8.ValidString(key) {
		return volumeKey{}, invalidVolumeKey("a key of a volume must be valid UTF-8 for a write")
	}
	parsed.parts = strings.Split(key, "/")
	for _, part := range parsed.parts {
		switch {
		case part == "" || part == "." || part == "..":
			return volumeKey{}, invalidVolumeKey("a key of a volume must not have an empty, \".\" or \"..\" part")
		case len(part) > volumeMaxNameBytes:
			return volumeKey{}, invalidVolumeKey("a name of a key of a volume must have %d bytes or less", volumeMaxNameBytes)
		case write && strings.HasPrefix(part, volumeTempPrefix):
			return volumeKey{}, invalidVolumeKey("the names that start with %q are for the temporary files of Stowr", volumeTempPrefix)
		}
	}
	return parsed, nil
}

// insideRoot tells if the real path target is root or is in root.
func insideRoot(target, root string) bool {
	if root == string(filepath.Separator) {
		return strings.HasPrefix(target, root)
	}
	return target == root || strings.HasPrefix(target, root+string(filepath.Separator))
}

// resolveInside gives the real path of the relative path rel in the folder root. root is a real path. Each part must
// exist. A symbolic link is followed when its target is in root.
func resolveInside(root, rel string) (string, error) {
	current := root
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("the path must not have an empty, \".\" or \"..\" part")
		}
		next, err := resolvePart(root, current, part, false)
		if err != nil {
			return "", err
		}
		current = next
	}
	return current, nil
}

// resolvePart gives the real path of the name part in the real folder current. A symbolic link is followed when its
// target is in root, except with refuseLinks: each symbolic link then gives errVolumeLink. This is the one place that
// follows a link in a path. The error of a part that does not exist is fs.ErrNotExist.
func resolvePart(root, current, part string, refuseLinks bool) (string, error) {
	next := filepath.Join(current, part)
	info, err := os.Lstat(next)
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return next, nil
	}
	if refuseLinks {
		return "", errVolumeLink
	}
	target, err := filepath.EvalSymlinks(next)
	if err != nil || !insideRoot(target, root) {
		return "", errVolumeLink
	}
	return target, nil
}

// openRead opens the real file for a read. With refuseLinks, a symbolic link at the end of the path gives an error, also
// when a process outside Stowr made the link after the check of resolvePart.
func (b *volumeBackend) openRead(real string) (*os.File, error) {
	if !b.refuseLinks {
		return os.Open(real)
	}
	return os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// resolveFolder gives the real path of the folder of parts. With create, a folder that does not exist is made, with
// the permissions 0777 and the umask of the process, and a part that is a file gives volume_not_folder. Without create,
// a part that does not exist or that is a file gives the 404 of S3.
func (b *volumeBackend) resolveFolder(parts []string, create bool) (string, error) {
	current := b.root
	for _, part := range parts {
		next, err := resolvePart(b.root, current, part, b.refuseLinks)
		if errors.Is(err, fs.ErrNotExist) && create {
			next = filepath.Join(current, part)
			if err = os.Mkdir(next, 0o777); errors.Is(err, fs.ErrExist) {
				// An other request made the folder at the same time.
				next, err = resolvePart(b.root, current, part, b.refuseLinks)
			}
		}
		if err != nil {
			return "", volumeError(err)
		}
		info, err := os.Stat(next)
		if err != nil {
			return "", volumeError(err)
		}
		if !info.IsDir() && !create {
			return "", &upstreamError{StatusCode: http.StatusNotFound, Code: "NoSuchKey", Message: "the file does not exist"}
		}
		if !info.IsDir() {
			return "", apiError{Status: http.StatusConflict, Code: "volume_not_folder", Message: "a part of the path is a file, not a folder"}
		}
		current = next
	}
	return current, nil
}

// resolveExisting gives the real path of a key that exists, and its information. A symbolic link at the end is
// followed when its target is in the root.
func (b *volumeBackend) resolveExisting(key volumeKey) (string, os.FileInfo, error) {
	folder, err := b.resolveFolder(key.parent(), false)
	if err != nil {
		return "", nil, err
	}
	target := folder
	if name := key.name(); name != "" {
		if target, err = resolvePart(b.root, folder, name, b.refuseLinks); err != nil {
			return "", nil, volumeError(err)
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", nil, volumeError(err)
	}
	return target, info, nil
}

// writeTarget gives the real path that a write of the file key replaces or makes. The folders of the path are made
// when they do not exist. A symbolic link at the end is followed when its target is a file in the root. A folder at
// the path stops the write.
func (b *volumeBackend) writeTarget(key volumeKey) (string, error) {
	folder, err := b.resolveFolder(key.parent(), true)
	if err != nil {
		return "", err
	}
	target := filepath.Join(folder, key.name())
	info, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return target, nil
	case err != nil:
		return "", volumeError(err)
	case info.Mode()&fs.ModeSymlink != 0:
		resolved, err := resolvePart(b.root, folder, key.name(), b.refuseLinks)
		if err != nil {
			return "", err
		}
		if info, err = os.Stat(resolved); err != nil {
			return "", volumeError(err)
		}
		target = resolved
	}
	if info.IsDir() {
		return "", apiError{Status: http.StatusConflict, Code: "volume_folder_exists", Message: "a folder has this name: Stowr does not replace a folder with a file"}
	}
	if !info.Mode().IsRegular() {
		return "", apiError{Status: http.StatusConflict, Code: "volume_not_file", Message: "the path is not a regular file: Stowr does not replace it"}
	}
	return target, nil
}

// volumeTempName gives a new name of a temporary file. The name of the target follows the random part when the whole
// name has volumeMaxNameBytes bytes or less, so that the window "Incomplete uploads" can show it.
func volumeTempName(target string) (string, error) {
	var random [volumeTempIDLength / 2]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate a temporary name: %w", err)
	}
	name := volumeTempPrefix + hex.EncodeToString(random[:])
	if target != "" && len(name)+1+len(target) <= volumeMaxNameBytes {
		name += "-" + target
	}
	return name, nil
}

// parseVolumeTempName tells if name is the name of a temporary file of Stowr, and gives the name of its target
// ("" when the temporary name does not have it).
func parseVolumeTempName(name string) (string, bool) {
	if !strings.HasPrefix(name, volumeTempPrefix) || len(name) < len(volumeTempPrefix)+volumeTempIDLength {
		return "", false
	}
	random := name[len(volumeTempPrefix) : len(volumeTempPrefix)+volumeTempIDLength]
	for _, r := range random {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", false
		}
	}
	rest := name[len(volumeTempPrefix)+volumeTempIDLength:]
	switch {
	case rest == "":
		return "", true
	case strings.HasPrefix(rest, "-") && len(rest) > 1:
		return rest[1:], true
	}
	return "", false
}

// volumeError gives the error of the API for an error of the file system. The message never shows a path of the
// server.
func volumeError(err error) error {
	var known apiError
	if err == nil || errors.As(err, &known) {
		return err
	}
	var errno syscall.Errno
	errors.As(err, &errno)
	switch {
	case errors.Is(err, fs.ErrNotExist), errno == syscall.ENOTDIR:
		return &upstreamError{StatusCode: http.StatusNotFound, Code: "NoSuchKey", Message: "the file does not exist"}
	case errors.Is(err, fs.ErrPermission):
		return &upstreamError{StatusCode: http.StatusForbidden, Code: "AccessDenied", Message: "the file system refused the access to the process of Stowr"}
	case errno == syscall.ELOOP:
		return errVolumeLink
	case errno == syscall.ENOSPC || errno == syscall.EDQUOT:
		return apiError{Status: http.StatusInsufficientStorage, Code: "volume_full", Message: "the volume has no free space"}
	case errno == syscall.EROFS:
		return apiError{Status: http.StatusForbidden, Code: "volume_read_only", Message: "the file system of the volume is read only"}
	case errno == syscall.ENOTEMPTY:
		return apiError{Status: http.StatusConflict, Code: "volume_folder_not_empty", Message: "the folder is not empty"}
	case errno == syscall.EISDIR:
		return apiError{Status: http.StatusConflict, Code: "volume_folder_exists", Message: "a folder has this name"}
	case errno == syscall.ENAMETOOLONG:
		return invalidVolumeKey("a name of the path is too long for the file system of the volume")
	case errno != 0:
		return apiError{Status: http.StatusBadGateway, Code: "volume_error", Message: "the file system of the volume refused the operation: " + errno.Error()}
	}
	return apiError{Status: http.StatusBadGateway, Code: "volume_error", Message: "the file system of the volume refused the operation"}
}
