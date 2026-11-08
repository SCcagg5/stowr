package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// volumeConfigText gives a configuration with one volume block whose attributes are lines.
func volumeConfigText(lines string) string {
	return "volume \"vol\" {\n" + lines + "\n}\n"
}

func decodeVolumeConfig(t *testing.T, text string) (appConfig, error) {
	t.Helper()
	dir := t.TempDir()
	return decodeConfig(text+testOpenAccess, filepath.Join(dir, "config.hcl"), dir)
}

func TestVolumeBlockDecodesAndPrintsItsKeys(t *testing.T) {
	t.Parallel()
	root := volumeTempDir(t)
	if err := os.Mkdir(filepath.Join(root, "published"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := decodeVolumeConfig(t, volumeConfigText(`path = "`+root+`"
name = "Shared files"
permissions = ["read", "write"]
root_prefix = "published/"
max_scan_pages = 3`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Volumes) != 1 || len(cfg.Buckets) != 0 || len(cfg.Authentications) != 0 {
		t.Fatalf("config = %+v", cfg)
	}
	volume := cfg.Volumes[0]
	if volume.ID != "vol" || volume.Name != "Shared files" || volume.Path != root || volume.RootPrefix != "published/" ||
		volume.MaxScanPages != 3 || !volume.PermissionsDefined || strings.Join(volume.Permissions, ",") != "read,write" {
		t.Fatalf("volume = %+v", volume)
	}
	bucket := volume.bucketConfig()
	if bucket.Provider != providerVolume || bucket.AuthID != volumeHostID || bucket.RootPrefix != "published/" {
		t.Fatalf("instance configuration = %+v", bucket)
	}
	printed := formatEffectiveConfiguration(cfg)
	for _, line := range []string{
		`volume "vol".name = "Shared files"  # file`,
		`volume "vol".path = "` + root + `"  # file`,
		`volume "vol".permissions = ["read", "write"]  # file`,
		`volume "vol".root_prefix = "published/"  # file`,
		`volume "vol".max_scan_pages = 3  # file`,
	} {
		if !strings.Contains(printed, line+"\n") {
			t.Fatalf("print-config has no line %q:\n%s", line, printed)
		}
	}
}

func TestVolumeBlockDefaults(t *testing.T) {
	t.Parallel()
	root := volumeTempDir(t)
	cfg, err := decodeVolumeConfig(t, volumeConfigText(`path = "`+root+`"`))
	if err != nil {
		t.Fatal(err)
	}
	volume := cfg.Volumes[0]
	if volume.Name != "vol" || volume.RootPrefix != "" || volume.MaxScanPages != 1 || volume.PermissionsDefined || !volume.writes() {
		t.Fatalf("volume = %+v", volume)
	}
	printed := formatEffectiveConfiguration(cfg)
	for _, line := range []string{
		`volume "vol".name = "vol"  # default`,
		`volume "vol".permissions = <none>  # default`,
		`volume "vol".root_prefix = ""  # default`,
		`volume "vol".max_scan_pages = 1  # default`,
	} {
		if !strings.Contains(printed, line+"\n") {
			t.Fatalf("print-config has no line %q:\n%s", line, printed)
		}
	}
}

// TestVolumeBlockValuesStopTheStart: a value that does not fit stops the start. The error names the key and the value.
func TestVolumeBlockValuesStopTheStart(t *testing.T) {
	t.Parallel()
	root := volumeTempDir(t)
	file := filepath.Join(root, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := `path = "` + root + `"` + "\n"
	bucket := "auth \"s3\" {\n  provider = \"s3\"\n  endpoint = \"http://localhost:9000\"\n  region = \"test\"\n}\n"
	for name, test := range map[string]struct{ config, want string }{
		"no path":            {volumeConfigText(`name = "x"`), `volume "vol".path is required`},
		"relative path":      {volumeConfigText(`path = "data/volume"`), `volume "vol".path must be an absolute path without "/" at the end and without an empty, "." or ".." part (for example "/data/volume"), got "data/volume"`},
		"path slash":         {volumeConfigText(`path = "` + root + `/"`), `got "` + root + `/"`},
		"path dot dot":       {volumeConfigText(`path = "` + root + `/../x"`), `got "` + root + `/../x"`},
		"path missing":       {volumeConfigText(`path = "` + root + `/missing"`), `volume "vol": path "` + root + `/missing" must be a folder that exists at start`},
		"path file":          {volumeConfigText(`path = "` + file + `"`), `volume "vol": path "` + file + `" must be a folder, got a file`},
		"path empty":         {volumeConfigText(`path = ""`), `volume "vol".path must not be empty`},
		"path type":          {volumeConfigText(`path = 3`), `attribute "path" must be a quoted string`},
		"unknown attribute":  {volumeConfigText(path + `bucket = "x"`), `unknown attribute "bucket" in volume block`},
		"duplicate key":      {volumeConfigText(path + path), `duplicate attribute "path"`},
		"nested block":       {volumeConfigText(path + "usage { source = \"garage\" }"), "nested blocks are not supported inside volume"},
		"no label":           {"volume {\n" + path + "}\n", "volume block requires exactly one quoted identifier"},
		"root prefix slash":  {volumeConfigText(path + `root_prefix = "/published/"`), `volume "vol".root_prefix must be "" or a relative folder path that ends with "/"`},
		"root prefix no end": {volumeConfigText(path + `root_prefix = "published"`), `got "published"`},
		"root prefix dots":   {volumeConfigText(path + `root_prefix = "a/../b/"`), `got "a/../b/"`},
		"root prefix gone":   {volumeConfigText(path + `root_prefix = "missing/"`), `volume "vol": root_prefix "missing/"`},
		"permission case":    {volumeConfigText(path + `permissions = ["Read"]`), `volume "vol".permissions must contain only "read", "write" and "delete", got "Read"`},
		"permission twice":   {volumeConfigText(path + `permissions = ["read", "read"]`), `volume "vol".permissions contains "read" two times`},
		"scan pages":         {volumeConfigText(path + `max_scan_pages = 1000001`), `volume "vol".max_scan_pages must be between 0 and 1000000, got 1000001`},
		"same label":         {bucket + "bucket \"vol\" {\n  auth = \"s3\"\n  bucket = \"b\"\n}\n" + volumeConfigText(path), `duplicate bucket or volume id "vol"`},
		"auth label volume":  {strings.Replace(bucket, `auth "s3"`, `auth "volume"`, 1) + volumeConfigText(path), `auth "volume": the label "volume" is the address word of the volumes`},
		"nothing to show":    {"", "at least one bucket block or one volume block is required"},
		"bucket without auth": {"bucket \"b\" {\n  auth = \"s3\"\n  bucket = \"b\"\n}\n" + volumeConfigText(path),
			"at least one auth block is required"},
	} {
		_, err := decodeVolumeConfig(t, test.config)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: error = %v, want %q", name, err, test.want)
		}
	}
}

func TestVolumeBlockWithBuckets(t *testing.T) {
	t.Parallel()
	root := volumeTempDir(t)
	auth := "auth \"s3\" {\n  provider = \"s3\"\n  endpoint = \"http://localhost:9000\"\n  region = \"test\"\n}\n"
	bucket := "bucket \"b\" {\n  auth = \"s3\"\n  bucket = \"b\"\n}\n"
	volume := "volume \"vol\" {\n  path = \"" + root + "\"\n}\n"
	cfg, err := decodeVolumeConfig(t, auth+bucket+volume)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Buckets) != 1 || len(cfg.Volumes) != 1 {
		t.Fatalf("config = %+v", cfg)
	}
	// The label "volume" of an auth block is valid in a file without a volume block.
	renamed := strings.ReplaceAll(auth+bucket, `"s3"`, `"volume"`)
	renamed = strings.Replace(renamed, `provider = "volume"`, `provider = "s3"`, 1)
	if _, err := decodeVolumeConfig(t, renamed); err != nil {
		t.Fatalf("auth \"volume\" without a volume block: %v", err)
	}
}

func TestVolumeStartRefusesAFolderThatTheProcessCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes in each folder")
	}
	t.Parallel()
	root := volumeTempDir(t)
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	cfg, err := decodeVolumeConfig(t, volumeConfigText(`path = "`+root+`"`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newApplication(cfg); err == nil || !strings.Contains(err.Error(), `volume "vol": the process cannot write in path "`+root+`"`) ||
		!strings.Contains(err.Error(), `give permissions = ["read"]`) {
		t.Fatalf("start error = %v", err)
	}
	cfg, err = decodeVolumeConfig(t, volumeConfigText(`path = "`+root+`"`+"\npermissions = [\"read\"]"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := newApplication(cfg)
	if err != nil {
		t.Fatalf("a read-only volume does not start: %v", err)
	}
	app.close()
	cfg, err = decodeVolumeConfig(t, "server { access_mode = \"force_read_only\" }\n"+volumeConfigText(`path = "`+root+`"`))
	if err != nil {
		t.Fatal(err)
	}
	app, err = newApplication(cfg)
	if err != nil {
		t.Fatalf("force_read_only does not start a volume that the process cannot write: %v", err)
	}
	app.close()
}
