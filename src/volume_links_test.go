package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// volumeLinksBackend starts the server with one volume block on root, with RefuseLinks as the argument gives it, and
// gives the backend.
func volumeLinksBackend(t *testing.T, root string, refuse bool) *volumeBackend {
	t.Helper()
	cfg, err := decodeVolumeConfig(t, volumeConfigText(`path = "`+root+`"`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Volumes[0].RefuseLinks {
		t.Fatal("an open access block sets RefuseLinks")
	}
	cfg.Volumes[0].RefuseLinks = refuse
	app, err := newApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.close)
	_, backend := volumeInstance(t, app)
	return backend
}

func isLinkRefused(err error) bool {
	var known apiError
	return errors.As(err, &known) && known.Code == "volume_link_refused"
}

func containsString2(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

// The tree of volumeTree: link-in is a link to a folder in the root, link-file a link to a file in the root, link-out
// a link to a folder outside the root, and link-dangling a link that does not resolve.
func TestVolumeRefuseLinksRefusesEachLinkForRead(t *testing.T) {
	t.Parallel()
	backend := volumeLinksBackend(t, volumeTree(t), true)
	ctx := context.Background()
	for _, key := range []string{"link-file", "link-in", "link-in/b.txt", "link-out", "link-out/secret.txt", "link-dangling"} {
		if _, err := backend.Get(ctx, key, http.Header{}); !isLinkRefused(err) {
			t.Errorf("get %q: error = %v", key, err)
		}
		if _, err := backend.Head(ctx, key); !isLinkRefused(err) {
			t.Errorf("head %q: error = %v", key, err)
		}
	}
	// A file that is not a link, in a folder that is not a link, stays readable.
	response, err := backend.Get(ctx, "a/b.txt", http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestVolumeRefuseLinksRefusesEachLinkForWriteDeleteMoveAndCopy(t *testing.T) {
	t.Parallel()
	root := volumeTree(t)
	backend := volumeLinksBackend(t, root, true)
	ctx := context.Background()
	for _, key := range []string{"link-file", "link-in/new.txt", "link-in/b.txt", "link-out/new.txt", "link-out/secret.txt", "link-dangling"} {
		if err := backend.Put(ctx, key, strings.NewReader("changed"), 7, ""); !isLinkRefused(err) {
			t.Errorf("put %q: error = %v", key, err)
		}
		if err := backend.PutIfAbsent(ctx, key, strings.NewReader("changed"), 7, ""); !isLinkRefused(err) {
			t.Errorf("put if absent %q: error = %v", key, err)
		}
	}
	if readVolumeFile(t, root, "a0") != "zero" || readVolumeFile(t, root, "a/b.txt") != "0123456789" {
		t.Fatal("a write went through a link to a file or to a folder in the root")
	}
	if _, err := os.Stat(filepath.Join(root, "a", "new.txt")); err == nil {
		t.Fatal("a write went through a link to a folder in the root")
	}
	if _, err := os.Stat(filepath.Join(root, "..", "outside", "new.txt")); err == nil {
		t.Fatal("a write went through a link that leaves the root")
	}
	for _, key := range []string{"link-file", "link-in/b.txt", "link-out/secret.txt"} {
		if err := backend.Delete(ctx, key); !isLinkRefused(err) {
			t.Errorf("delete %q: error = %v", key, err)
		}
	}
	for _, link := range []string{"link-in", "link-file", "link-out"} {
		if _, err := os.Lstat(filepath.Join(root, link)); err != nil {
			t.Errorf("the delete removed the link %q", link)
		}
	}
	if readVolumeFile(t, root, "a/b.txt") != "0123456789" || readVolumeFile(t, root, "../outside/secret.txt") != "secret" {
		t.Fatal("a delete went through a link")
	}
	for _, pair := range [][2]string{{"link-file", "moved"}, {"link-in/b.txt", "moved"}, {"a0", "link-file"}, {"a0", "link-in/moved"}, {"a0", "link-out/moved"}, {"link-out/secret.txt", "moved"}} {
		if err := backend.Move(ctx, pair[0], pair[1]); !isLinkRefused(err) {
			t.Errorf("move %q to %q: error = %v", pair[0], pair[1], err)
		}
		if err := backend.Copy(ctx, pair[0], pair[1]); !isLinkRefused(err) {
			t.Errorf("copy %q to %q: error = %v", pair[0], pair[1], err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "moved")); err == nil {
		t.Fatal("a move or a copy made a file from a link")
	}
	if readVolumeFile(t, root, "a0") != "zero" {
		t.Fatal("a move or a copy changed the target of a link")
	}
	if _, err := backend.InitiateResumable(ctx, "link-out/upload.bin", 1, ""); !isLinkRefused(err) {
		t.Errorf("start of an upload through a link: error = %v", err)
	}
	if err := backend.RemoveEmptyFolders(ctx, "link-in/"); err != nil && !isLinkRefused(err) {
		t.Errorf("remove empty folders: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "b.txt")); err != nil {
		t.Fatal("the removal of empty folders went through a link")
	}
}

func TestVolumeRefuseLinksShowsLinksAsUnreadableAndWalksLeaveThemOut(t *testing.T) {
	t.Parallel()
	backend := volumeLinksBackend(t, volumeTree(t), true)
	ctx := context.Background()
	page, err := backend.List(ctx, listOptions{Delimiter: "/"})
	if err != nil {
		t.Fatal(err)
	}
	unreadable := map[string]bool{}
	for _, object := range page.Objects {
		unreadable[object.Key] = object.Unreadable
	}
	for _, name := range []string{"link-in", "link-file", "link-out", "link-dangling"} {
		if !unreadable[name] {
			t.Errorf("%q is not shown as unreadable: %+v", name, page.Objects)
		}
	}
	for _, prefix := range page.Prefixes {
		if strings.HasPrefix(prefix, "link-") {
			t.Errorf("the listing shows the link %q as a folder", prefix)
		}
	}
	if unreadable["a0"] || unreadable[".hidden"] || !containsString2(page.Prefixes, "a/") {
		t.Errorf("objects = %+v, prefixes = %v", page.Objects, page.Prefixes)
	}
	tree, err := backend.List(ctx, listOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range pageKeys(tree) {
		if strings.HasPrefix(key, "link-") {
			t.Errorf("the walk gives the link %q", key)
		}
	}
	if !containsString2(pageKeys(tree), "a/b.txt") {
		t.Errorf("the walk keys = %v", pageKeys(tree))
	}
	if _, err := backend.List(ctx, listOptions{Delimiter: "/", Prefix: "link-in/"}); !isLinkRefused(err) {
		t.Errorf("a listing through a link: error = %v", err)
	}
}

func TestVolumeWithoutRefuseLinksFollowsALinkInsideTheRoot(t *testing.T) {
	t.Parallel()
	root := volumeTree(t)
	backend := volumeLinksBackend(t, root, false)
	ctx := context.Background()
	response, err := backend.Get(ctx, "link-file", http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if _, err := backend.Head(ctx, "link-in/b.txt"); err != nil {
		t.Fatalf("head through a link to a folder in the root: %v", err)
	}
	if err := backend.Put(ctx, "link-in/new.txt", strings.NewReader("new"), 3, ""); err != nil {
		t.Fatalf("write through a link to a folder in the root: %v", err)
	}
	if readVolumeFile(t, root, "a/new.txt") != "new" {
		t.Fatal("the write did not reach the target of the link")
	}
	if _, err := backend.Head(ctx, "link-out/secret.txt"); !isLinkRefused(err) {
		t.Fatalf("head through a link that leaves the root: error = %v", err)
	}
	page, err := backend.List(ctx, listOptions{Delimiter: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString2(page.Prefixes, "link-in/") {
		t.Fatalf("prefixes = %v", page.Prefixes)
	}
}

func TestAccessModeSetsRefuseLinksOnEachVolume(t *testing.T) {
	t.Parallel()
	first, second := volumeTempDir(t), volumeTempDir(t)
	volumes := "\nvolume \"one\" {\n  path = \"" + first + "\"\n}\nvolume \"two\" {\n  path = \"" + second + "\"\n}\n"
	cfg, _, err := decodeAccessTestConfig(t, authzenText(goodIdentity, goodPDP)+volumes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Volumes) != 2 || !cfg.Volumes[0].RefuseLinks || !cfg.Volumes[1].RefuseLinks {
		t.Fatalf("volumes = %+v", cfg.Volumes)
	}
	cfg, _, err = decodeAccessTestConfig(t, "access {\n  mode = \"open\"\n}\n"+volumes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Volumes) != 2 || cfg.Volumes[0].RefuseLinks || cfg.Volumes[1].RefuseLinks {
		t.Fatalf("volumes = %+v", cfg.Volumes)
	}
	if strings.Contains(formatEffectiveConfiguration(cfg), "refuse_links") {
		t.Fatal("print-config shows RefuseLinks")
	}
}
