package server

import (
	"net/http"
	"path/filepath"
	"testing"

	"deepaicode2api/internal/register"
	"deepaicode2api/internal/store"
	"deepaicode2api/internal/updater"
)

func TestUpdateEndpointsRequireAdminAndBlockActiveRegistration(t *testing.T) {
	s := newEmptyServer(t)
	s.Store.UpdateSettings(func(settings *store.Settings) { settings.AdminPassword = "admin" })
	root := t.TempDir()
	manager, err := updater.New(root, filepath.Join(root, "data"), "v0.2.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Updater = manager
	if rec := do(t, s, http.MethodGet, "/admin/api/update", "", ""); rec.Code != 401 {
		t.Fatal("update status exposed without admin auth")
	}
	job := &registerJob{ID: "active", running: true, records: map[string]*register.Record{}}
	s.Registers.put(job)
	if rec := do(t, s, http.MethodPost, "/admin/api/update", "admin", `{"action":"apply"}`); rec.Code != 409 {
		t.Fatalf("active registration was interrupted: %d", rec.Code)
	}
}
