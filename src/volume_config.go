package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The volume block: one folder of the file system of the server that the browser shows as it shows a bucket. The
// folder can be a Docker volume, an NFS or CephFS mount, or a FUSE mount such as `weed mount` of SeaweedFS. The server
// reads and writes the files with the system calls of the process, under the identity of the process. Refer to
// docs/ARCHITECTURE.md, "Volumes".

const (
	// providerVolume is the provider of the instance of a volume block.
	providerVolume = "fs"
	// volumeHostID is the host word of each volume in the address of the interface: /-/volume/<id>/. It is also the
	// value of instanceInfo.Host. An auth block cannot have this label when the file has a volume block.
	volumeHostID = "volume"
)

// volumeConfig is one volume block.
type volumeConfig struct {
	ID   string
	Name string
	// Path is the absolute path of the folder, as the file gives it.
	Path               string
	Permissions        []string
	PermissionsDefined bool
	// RootPrefix is "" or a relative folder path that ends with "/". The browser shows only this folder of Path.
	RootPrefix   string
	MaxScanPages int
	// RefuseLinks is true when access.mode is authzen: the volume then follows no symbolic link. It is not a key of
	// the file. The decode of the configuration sets it after the access block.
	RefuseLinks bool
}

// bucketConfig gives the configuration of the storage instance of the volume. The instance has the host word
// volumeHostID and the provider providerVolume.
func (v volumeConfig) bucketConfig() bucketConfig {
	return bucketConfig{
		ID: v.ID, Name: v.Name, AuthID: volumeHostID, Provider: providerVolume, Bucket: v.ID,
		Permissions: append([]string(nil), v.Permissions...), PermissionsDefined: v.PermissionsDefined,
		RootPrefix: v.RootPrefix, MaxScanPages: v.MaxScanPages,
	}
}

// writes tells if the server can change the files of the volume: the permissions include write or delete, or the
// block has no permissions key.
func (v volumeConfig) writes() bool {
	return !v.PermissionsDefined || containsString(v.Permissions, permissionWrite) || containsString(v.Permissions, permissionDelete)
}

// decodeVolumeBlock reads one volume block. It checks that the folder exists, is a folder and that the process can
// read it. The check of the write permission is at the start of the server (newVolumeBackend): it writes a file.
func decodeVolumeBlock(block hclBlock) (volumeConfig, error) {
	if len(block.Labels) != 1 {
		return volumeConfig{}, block.errorf("volume block requires exactly one quoted identifier")
	}
	if err := rejectUnknownAttrs(block, attributeNames("volume")...); err != nil {
		return volumeConfig{}, err
	}
	kinds := map[string]hclValueKind{
		"name": hclString, "path": hclString, "permissions": hclList, "root_prefix": hclString, "max_scan_pages": hclNumber,
	}
	if err := requireAttrKinds(block, kinds); err != nil {
		return volumeConfig{}, err
	}
	if len(block.Blocks) != 0 {
		return volumeConfig{}, block.errorf("nested blocks are not supported inside volume")
	}
	volume := volumeConfig{ID: block.Labels[0], Name: block.Labels[0], MaxScanPages: 1}
	if !instanceIDPattern.MatchString(volume.ID) {
		return volumeConfig{}, block.errorf("volume label must match %s, got %q", instanceIDPattern.String(), volume.ID)
	}
	path := "volume " + strconv.Quote(volume.ID)
	if value, ok, err := textAttr(block, path, "name"); err != nil {
		return volumeConfig{}, err
	} else if ok {
		volume.Name = value
	}
	value, ok, err := textAttr(block, path, "path")
	if err != nil {
		return volumeConfig{}, err
	}
	if !ok {
		return volumeConfig{}, block.errorf("%s.path is required: the absolute path of the folder (for example \"/data/volume\")", path)
	}
	// The value applies as it is: the server does not clean it.
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return volumeConfig{}, block.errorf("%s.path must be an absolute path without \"/\" at the end and without an empty, \".\" or \"..\" part (for example \"/data/volume\"), got %q", path, value)
	}
	volume.Path = value
	if value, ok := block.stringAttr("root_prefix"); ok {
		if value != "" && !validVolumeRootPrefix(value) {
			return volumeConfig{}, block.errorf("%s.root_prefix must be \"\" or a relative folder path that ends with \"/\", without an empty, \".\" or \"..\" part (for example \"published/\"), got %q", path, value)
		}
		volume.RootPrefix = value
	}
	if value, ok := block.intAttr("max_scan_pages"); ok {
		if value < 0 || value > 1_000_000 {
			return volumeConfig{}, block.errorf("%s.max_scan_pages must be between 0 and 1000000, got %d", path, value)
		}
		volume.MaxScanPages = int(value)
	}
	if values, ok := block.stringListAttr("permissions"); ok {
		volume.PermissionsDefined = true
		for _, value := range values {
			switch value {
			case permissionRead, permissionWrite, permissionDelete:
				if containsString(volume.Permissions, value) {
					return volumeConfig{}, block.errorf("%s.permissions contains %q two times", path, value)
				}
				volume.Permissions = append(volume.Permissions, value)
			default:
				return volumeConfig{}, block.errorf("%s.permissions must contain only \"read\", \"write\" and \"delete\", got %q", path, value)
			}
		}
	}
	if err := checkVolumeFolder(volume); err != nil {
		return volumeConfig{}, block.errorf("%s: %v", path, err)
	}
	return volume, nil
}

// validVolumeRootPrefix tells if value is a relative folder path that ends with "/", with parts that are not empty,
// "." or "..", and without a NUL byte.
func validVolumeRootPrefix(value string) bool {
	if strings.HasPrefix(value, "/") || !strings.HasSuffix(value, "/") || strings.Contains(value, "\x00") {
		return false
	}
	for _, part := range strings.Split(strings.TrimSuffix(value, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// checkVolumeFolder checks the folder of a volume at start: path and path/root_prefix exist, are folders, stay inside
// path after the symbolic links, and the process can read them.
func checkVolumeFolder(volume volumeConfig) error {
	info, err := os.Stat(volume.Path)
	if err != nil {
		return fmt.Errorf("path %q must be a folder that exists at start: %v", volume.Path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("path %q must be a folder, got a file", volume.Path)
	}
	root, err := filepath.EvalSymlinks(volume.Path)
	if err != nil {
		return fmt.Errorf("path %q: cannot resolve the symbolic links: %v", volume.Path, err)
	}
	top := root
	if volume.RootPrefix != "" {
		top, err = resolveInside(root, strings.TrimSuffix(volume.RootPrefix, "/"))
		if err != nil {
			return fmt.Errorf("root_prefix %q: %v", volume.RootPrefix, err)
		}
		info, err := os.Stat(top)
		if err != nil {
			return fmt.Errorf("root_prefix %q must be a folder that exists at start in path %q: %v", volume.RootPrefix, volume.Path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("root_prefix %q must be a folder in path %q, got a file", volume.RootPrefix, volume.Path)
		}
	}
	folder, err := os.Open(top)
	if err != nil {
		return fmt.Errorf("the process cannot read the folder %q: %v", top, err)
	}
	defer folder.Close()
	if _, err := folder.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("the process cannot read the folder %q: %v", top, err)
	}
	return nil
}
