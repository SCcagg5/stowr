package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
)

// The writes of a volume. Each write of a file goes to a temporary file in the folder of the target, is synchronized
// to the disk, and replaces the target with one rename (refer to volume_backend.go).

// volumeContextReader stops a copy when its context ends.
type volumeContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r volumeContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

// syncFolder writes the entries of a folder to the disk. A file system that cannot do it (some FUSE file systems)
// keeps the rename in its own way: the error is ignored.
func syncFolder(real string) {
	if folder, err := os.Open(real); err == nil {
		_ = folder.Sync()
		_ = folder.Close()
	}
}

// writeFile writes size bytes of body (all the bytes for a size of -1) to a temporary file in the folder of target,
// synchronizes it, and replaces target with it. With exclusive, an existing target stops the write (object_exists).
// With a checksum transfer, the SHA-256 of the bytes must be the digest of the client.
func (b *volumeBackend) writeFile(ctx context.Context, target string, body io.Reader, size int64, transfer *checksumTransfer, exclusive bool) error {
	folder := filepath.Dir(target)
	name, err := volumeTempName("")
	if err != nil {
		return err
	}
	temporary := filepath.Join(folder, name)
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return volumeError(err)
	}
	done := false
	defer func() {
		if !done {
			_ = file.Close()
			_ = os.Remove(temporary)
		}
	}()
	var digest hash.Hash
	writer := io.Writer(file)
	if transfer != nil {
		digest = sha256.New()
		writer = io.MultiWriter(file, digest)
	}
	reader := io.Reader(volumeContextReader{ctx: ctx, reader: body})
	if size >= 0 {
		if _, err := io.CopyN(writer, reader, size); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return apiError{Status: http.StatusBadRequest, Code: "incomplete_body", Message: "the request has fewer bytes than its Content-Length"}
			}
			return volumeError(err)
		}
	} else {
		written, err := io.Copy(writer, io.LimitReader(reader, maximumObjectSize+1))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return volumeError(err)
		}
		if written > maximumObjectSize {
			return apiError{Status: http.StatusRequestEntityTooLarge, Code: "object_too_large", Message: fmt.Sprintf("a file must have %d bytes or less", maximumObjectSize)}
		}
	}
	if digest != nil {
		sum := base64.StdEncoding.EncodeToString(digest.Sum(nil))
		if sum != transfer.SHA256 {
			return apiError{Status: http.StatusBadRequest, Code: "bad_digest", Message: "the SHA-256 of the received bytes is not the checksum of the client: the file is not written"}
		}
		transfer.Echoed = sum
	}
	if err := file.Sync(); err != nil {
		return volumeError(err)
	}
	if err := file.Close(); err != nil {
		done = true
		_ = os.Remove(temporary)
		return volumeError(err)
	}
	done = true
	if err := commitTemporary(temporary, target, exclusive); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	syncFolder(folder)
	return nil
}

// commitTemporary gives the temporary file the name of target. Without exclusive, a rename replaces target. With
// exclusive, a hard link makes target only when it does not exist, and the temporary name is removed. A file system
// without hard links gets a check and a rename.
func commitTemporary(temporary, target string, exclusive bool) error {
	if !exclusive {
		return volumeError(os.Rename(temporary, target))
	}
	err := os.Link(temporary, target)
	switch {
	case err == nil:
		_ = os.Remove(temporary)
		return nil
	case errors.Is(err, fs.ErrExist):
		return apiError{Status: http.StatusConflict, Code: "object_exists", Message: "an object already exists at the extraction destination"}
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		return apiError{Status: http.StatusConflict, Code: "object_exists", Message: "an object already exists at the extraction destination"}
	}
	return volumeError(os.Rename(temporary, target))
}

// makeFolder makes the folder of key and its parents.
func (b *volumeBackend) makeFolder(key volumeKey) error {
	if len(key.parts) == 0 {
		return invalidVolumeKey("the root of the volume exists already")
	}
	_, err := b.resolveFolder(key.parts, true)
	return err
}

// Put writes a file. A key that ends with "/" makes a folder: it takes no content.
func (b *volumeBackend) Put(ctx context.Context, full string, body io.Reader, size int64, _ string) error {
	return b.put(ctx, full, body, size, false)
}

// PutIfAbsent writes a file only when no file has its name.
func (b *volumeBackend) PutIfAbsent(ctx context.Context, full string, body io.Reader, size int64, _ string) error {
	return b.put(ctx, full, body, size, true)
}

func (b *volumeBackend) put(ctx context.Context, full string, body io.Reader, size int64, exclusive bool) error {
	b.sizer.invalidate()
	key, err := b.parse(full, true)
	if err != nil {
		return err
	}
	if key.folder {
		if size > 0 {
			return invalidVolumeKey("a key that ends with \"/\" makes a folder: it takes no content")
		}
		return b.makeFolder(key)
	}
	if len(key.parts) == 0 {
		return invalidVolumeKey("a key of a file is required")
	}
	target, err := b.writeTarget(key)
	if err != nil {
		return err
	}
	return b.writeFile(ctx, target, body, size, checksumTransferFrom(ctx), exclusive)
}

// Delete removes a file, or an empty folder for a key that ends with "/". A key that does not exist, and a folder that
// is not empty, are not errors: a delete of S3 does the same. A symbolic link is removed, not its target.
func (b *volumeBackend) Delete(_ context.Context, full string) error {
	b.sizer.invalidate()
	key, err := b.parse(full, false)
	if err != nil {
		return err
	}
	if len(key.parts) == 0 {
		return invalidVolumeKey("Stowr does not delete the root of the volume")
	}
	folder, err := b.resolveFolder(key.parent(), false)
	if isStorageNotFound(err) || isVolumeNotFolder(err) {
		return nil
	}
	if err != nil {
		return err
	}
	entry := filepath.Join(folder, key.name())
	info, err := os.Lstat(entry)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return volumeError(err)
	}
	switch {
	case key.folder:
		if !info.IsDir() {
			return nil
		}
		if err := os.Remove(entry); err != nil {
			if mapped := volumeError(err); isVolumeFolderNotEmpty(mapped) || isStorageNotFound(mapped) {
				return nil
			}
			return volumeError(err)
		}
	case info.IsDir():
		return nil
	case info.Mode()&fs.ModeSymlink != 0:
		if _, err := resolvePart(b.root, folder, key.name(), b.refuseLinks); err != nil {
			return err
		}
		if err := os.Remove(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return volumeError(err)
		}
	default:
		if err := os.Remove(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return volumeError(err)
		}
	}
	syncFolder(folder)
	return nil
}

func isVolumeFolderNotEmpty(err error) bool {
	var known apiError
	return errors.As(err, &known) && known.Code == "volume_folder_not_empty"
}

// Copy copies a file to a temporary file, then gives it the name of the destination. A folder key copies to a new
// empty folder.
func (b *volumeBackend) Copy(ctx context.Context, source, destination string) error {
	return b.copyFile(ctx, source, destination, nil)
}

// CopyMultipart copies a large file with the same stream as Copy, and gives the progress of the copy. A volume has no
// versions, no metadata and no tags: an option that names them is refused. It lets the jobs of Stowr copy a large
// file with progress and cancel.
func (b *volumeBackend) CopyMultipart(ctx context.Context, source, destination string, options multipartCopyOptions) error {
	if options.SourceVersion != "" || options.SourceETag != "" || options.ReplaceMetadata || options.ReplaceTags || len(options.ExtraHeaders) > 0 {
		return apiError{Status: http.StatusNotImplemented, Code: "volume_unsupported", Message: "a volume has no versions, no metadata and no tags"}
	}
	return b.copyFile(ctx, source, destination, options.Progress)
}

func (b *volumeBackend) copyFile(ctx context.Context, source, destination string, progress func(int64)) error {
	b.sizer.invalidate()
	from, err := b.parse(source, false)
	if err != nil {
		return err
	}
	to, err := b.parse(destination, true)
	if err != nil {
		return err
	}
	if from.folder != to.folder {
		return invalidVolumeKey("a copy goes from a folder to a folder, or from a file to a file")
	}
	real, object, err := b.stat(source)
	if err != nil {
		return err
	}
	if from.folder {
		return b.makeFolder(to)
	}
	file, err := b.openRead(real)
	if err != nil {
		return volumeError(err)
	}
	defer file.Close()
	target, err := b.writeTarget(to)
	if err != nil {
		return err
	}
	if target == real {
		return nil
	}
	reader := io.Reader(file)
	if progress != nil {
		reader = &volumeProgressReader{reader: file, report: progress}
	}
	err = b.writeFile(ctx, target, reader, object.Size, nil, false)
	if counter, ok := reader.(*volumeProgressReader); ok && err == nil {
		counter.flush()
	}
	return err
}

// volumeProgressReader reports the bytes that a copy read, each volumeCopyProgressStep bytes.
type volumeProgressReader struct {
	reader  io.Reader
	report  func(int64)
	pending int64
}

func (r *volumeProgressReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	r.pending += int64(count)
	if r.pending >= volumeCopyProgressStep {
		r.flush()
	}
	return count, err
}

func (r *volumeProgressReader) flush() {
	if r.pending > 0 {
		r.report(r.pending)
		r.pending = 0
	}
}

// Move gives a file the name of the destination with one rename. A rename to an other file system (EXDEV), and the
// move of a symbolic link, copy the content and remove the source. A folder key makes the destination folder and
// removes the source folder when it is empty.
func (b *volumeBackend) Move(ctx context.Context, source, destination string) error {
	b.sizer.invalidate()
	from, err := b.parse(source, true)
	if err != nil {
		return err
	}
	to, err := b.parse(destination, true)
	if err != nil {
		return err
	}
	if from.folder != to.folder || len(from.parts) == 0 {
		return invalidVolumeKey("a move goes from a folder to a folder, or from a file to a file")
	}
	if from.folder {
		if _, _, err := b.stat(source); err != nil {
			return err
		}
		if err := b.makeFolder(to); err != nil {
			return err
		}
		return b.Delete(ctx, source)
	}
	folder, err := b.resolveFolder(from.parent(), false)
	if err != nil {
		return err
	}
	entry := filepath.Join(folder, from.name())
	info, err := os.Lstat(entry)
	if err != nil {
		return volumeError(err)
	}
	if !info.Mode().IsRegular() {
		if err := b.copyFile(ctx, source, destination, nil); err != nil {
			return err
		}
		return b.Delete(ctx, source)
	}
	target, err := b.writeTarget(to)
	if err != nil {
		return err
	}
	if target == entry {
		return nil
	}
	if err := os.Rename(entry, target); err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == syscall.EXDEV {
			if err := b.copyFile(ctx, source, destination, nil); err != nil {
				return err
			}
			return b.Delete(ctx, source)
		}
		return volumeError(err)
	}
	syncFolder(folder)
	syncFolder(filepath.Dir(target))
	return nil
}

// RemoveEmptyFolders removes the folder of prefix and each folder below it that has no file, from the bottom. A folder
// with a file stays: a delete of the folders never removes a file. The root of the volume stays.
func (b *volumeBackend) RemoveEmptyFolders(ctx context.Context, full string) error {
	b.sizer.invalidate()
	key, err := b.parse(full, false)
	if err != nil {
		return err
	}
	if !key.folder || len(key.parts) == 0 {
		return nil
	}
	folder, err := b.resolveFolder(key.parent(), false)
	if isStorageNotFound(err) || isVolumeNotFolder(err) {
		return nil
	}
	if err != nil {
		return err
	}
	real := filepath.Join(folder, key.name())
	info, err := os.Lstat(real)
	if err != nil || !info.IsDir() {
		return nil
	}
	return removeEmptyTree(ctx, real)
}

func removeEmptyTree(ctx context.Context, real string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return volumeError(err)
	}
	for _, entry := range entries {
		if entry.Type().IsDir() {
			if err := removeEmptyTree(ctx, filepath.Join(real, entry.Name())); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(real); err != nil {
		mapped := volumeError(err)
		if isVolumeFolderNotEmpty(mapped) || isStorageNotFound(mapped) {
			return nil
		}
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == syscall.EEXIST {
			return nil
		}
		return mapped
	}
	syncFolder(filepath.Dir(real))
	return nil
}
