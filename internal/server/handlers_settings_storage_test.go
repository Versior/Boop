package server

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"boop/internal/config"
	"boop/internal/settings"
)

// storageMasterKey makes the secret paths reachable on a fixture built from a
// configuration, which is where the storage tests live.
func storageFixtureConfig(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	cfg.MasterKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	return cfg
}

// storagePatch is the body the storage form sends: every field of the category,
// the way the page submits them.
func storagePatch(endpoint string) string {
	return `{` +
		`"storage_mode":"object",` +
		`"storage_endpoint":"` + endpoint + `",` +
		`"storage_region":"auto",` +
		`"storage_bucket":"boop-uploads",` +
		`"storage_prefix":"uploads",` +
		`"storage_public_url":"https://uploads.example.com",` +
		`"storage_access_key_id":"an-access-key",` +
		`"storage_secret_access_key":"a-secret-key"` +
		`}`
}

// The whole point of the page is that a save changes where uploads go. Nothing
// short of an upload proves it: the address in the response, the file that is
// or is not on disk, and the redirect the old address answers with are the
// three things an operator would notice.
func TestSavingTheStorageCategoryMovesUploadsWithoutARestart(t *testing.T) {
	endpoint := fakeObjectEndpoint(t)
	c := newContentFixtureWithConfig(t, storageFixtureConfig(t, testConfig()))

	// The site starts on the local directory, which is the documented default.
	asset := c.uploadPNG(t, "雾海.png", pngFixture(t, 8, 5, 13))
	key := asset["storage_key"].(string)
	if asset["url"] != "/uploads/"+key {
		t.Fatalf("url = %v, want a local address before the save", asset["url"])
	}

	rec := c.patchSettings(t, storagePatch(endpoint.URL), c.cookie, c.csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("save storage: status = %d, body %s", rec.Code, rec.Body.String())
	}

	// No restart happened between the two uploads: the save itself is what
	// changed the backend.
	moved := c.uploadPNG(t, "山脊.png", pngFixture(t, 9, 6, 21))
	movedKey := moved["storage_key"].(string)
	want := "https://uploads.example.com/uploads/" + movedKey
	if moved["url"] != want {
		t.Errorf("url = %v, want the bucket address %q", moved["url"], want)
	}
	if files := c.storedFiles(t); len(files) != 1 {
		// The first upload was local and stays local; nothing new was written.
		t.Errorf("stored files = %v, want only the one from before the switch", files)
	}

	redirect := c.do(t, http.MethodGet, "/uploads/"+movedKey, "", nil, nil)
	if redirect.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /uploads/%s: status = %d, want 301", movedKey, redirect.Code)
	}
	if got := redirect.Header().Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

// A refused save has to leave the running backend alone: writing the mode and
// the bucket but not the credentials would point the site at a bucket it cannot
// sign for, and every upload would start failing.
func TestSavingAHalfConfiguredObjectStoreIsRefused(t *testing.T) {
	c := newContentFixtureWithConfig(t, storageFixtureConfig(t, testConfig()))

	rec := c.patchSettings(t, `{"storage_mode":"object","storage_bucket":"boop-uploads"}`, c.cookie, c.csrf)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if code, message, _ := decodeAPIError(t, rec); code != "invalid_settings" || !strings.Contains(message, "incomplete") {
		t.Errorf("error = %q %q, want the refused keys named", code, message)
	}

	// The local directory is still what the process uses.
	asset := c.uploadPNG(t, "雾海.png", pngFixture(t, 8, 5, 13))
	key := asset["storage_key"].(string)
	if asset["url"] != "/uploads/"+key {
		t.Errorf("url = %v, want the local address to survive a refused save", asset["url"])
	}
	if files := c.storedFiles(t); len(files) != 1 {
		t.Errorf("stored files = %v, want the refused save to have written nothing", files)
	}
}

// The page has to say what the process is actually doing, not what the rows
// say: a site that has never saved the category is running on BOOP_R2_*, and
// that is the difference between an owner changing a value and wondering why
// nothing happened.
func TestTheStoragePageSaysWhereUploadsGo(t *testing.T) {
	endpoint := fakeObjectEndpoint(t)
	c := newContentFixtureWithConfig(t, storageFixtureConfig(t, objectStorageConfig(endpoint.URL)))

	page := c.do(t, http.MethodGet, "/admin/settings/storage", "", nil, c.cookie)
	if page.Code != http.StatusOK {
		t.Fatalf("storage page: status = %d", page.Code)
	}
	body := page.Body.String()
	for _, marker := range []string{
		"BOOP_R2_*",
		`<code>boop-uploads</code>`,
		`<code>https://uploads.example.com</code>`,
		// Switching a backend does not move anything, and this page is where
		// that mistake would be made. The warning is part of every render, not
		// a consequence of the current value.
		"不会移动已经存在的对象",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("storage page is missing %q", marker)
		}
	}

	rec := c.patchSettings(t, storagePatch(endpoint.URL), c.cookie, c.csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("save storage: status = %d, body %s", rec.Code, rec.Body.String())
	}
	data := c.settingsData(t, rec)
	if data["storage_environment"] != false {
		t.Errorf("storage_environment = %v, want false once the page has saved", data["storage_environment"])
	}
	if data["storage_effective_mode"] != settings.StorageModeObject {
		t.Errorf("storage_effective_mode = %v, want %q", data["storage_effective_mode"], settings.StorageModeObject)
	}
	if data["storage_access_key_id_set"] != true || data["storage_secret_access_key_set"] != true {
		t.Errorf("the credentials were not stored: %v", data)
	}
}

// Switching back is not throwing the bucket away: the fields stay, so a site
// that spends a week on the local directory does not have to be told the token
// again to go back.
func TestSwitchingBackToTheLocalDirectoryKeepsTheBucketFields(t *testing.T) {
	endpoint := fakeObjectEndpoint(t)
	c := newContentFixtureWithConfig(t, storageFixtureConfig(t, testConfig()))

	if rec := c.patchSettings(t, storagePatch(endpoint.URL), c.cookie, c.csrf); rec.Code != http.StatusOK {
		t.Fatalf("save storage: status = %d, body %s", rec.Code, rec.Body.String())
	}
	rec := c.patchSettings(t, `{"storage_mode":"local"}`, c.cookie, c.csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch to local: status = %d, body %s", rec.Code, rec.Body.String())
	}

	data := c.settingsData(t, rec)
	if data["storage_effective_mode"] != settings.StorageModeLocal {
		t.Errorf("storage_effective_mode = %v, want %q", data["storage_effective_mode"], settings.StorageModeLocal)
	}
	if data["storage_effective_public_url"] != "" || data["storage_effective_bucket"] != "" {
		t.Errorf("the process still reports a bucket: %v", data)
	}
	if data["storage_bucket"] != "boop-uploads" || data["storage_public_url"] != "https://uploads.example.com" {
		t.Errorf("the object fields were lost on the switch to local: %v", data)
	}
	if data["storage_access_key_id_set"] != true || data["storage_secret_access_key_set"] != true {
		t.Errorf("the credentials were lost on the switch to local: %v", data)
	}
}

// A site that has never saved the category follows BOOP_R2_*, which is the
// property that keeps an existing deployment working after the category exists.
func TestASiteThatNeverSavedStorageFollowsTheEnvironment(t *testing.T) {
	c := newContentFixtureWithConfig(t, storageFixtureConfig(t, testConfig()))

	data := c.settingsData(t, c.getSettings(t, c.cookie))
	if data["storage_environment"] != true {
		t.Errorf("storage_environment = %v, want true before any save", data["storage_environment"])
	}
	if data["storage_effective_mode"] != settings.StorageModeLocal {
		t.Errorf("storage_effective_mode = %v, want %q", data["storage_effective_mode"], settings.StorageModeLocal)
	}
}
