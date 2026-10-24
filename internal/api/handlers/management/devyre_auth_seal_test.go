package management

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/authseal"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// devyre: with the auth files sealed (internal/authseal), the auth-file routes work as before:
// an upload is stored sealed, the listing names the account's uuid, a download is the JSON, and
// a status change rewrites the file sealed.
func TestDevyreAuthFileRoutesWithSealedFiles(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	if err := authseal.SetKey(bytes.Repeat([]byte{3}, authseal.KeySize)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authseal.SetKey(nil) })

	authDir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	const name = "claude-ada@example.com.json"
	content := `{"type":"claude","email":"ada@example.com","access_token":"sk-ant-oat01-not-real","account_uuid":"0b1e5a5e-uuid"}`
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
	h.UploadAuthFile(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	path := filepath.Join(authDir, name)
	if onDisk, _ := os.ReadFile(path); !authseal.IsSealed(onDisk) || bytes.Contains(onDisk, []byte("not-real")) {
		t.Fatalf("the uploaded file is not sealed: %q", onDisk)
	}
	if auth, ok := manager.GetByID(name); !ok || auth.Metadata["email"] != "ada@example.com" {
		t.Fatalf("uploaded auth: %+v", auth)
	}

	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ctx)
	var listing struct {
		Files []map[string]any `json:"files"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &listing); err != nil || len(listing.Files) != 1 {
		t.Fatalf("listing: %v %s", err, rec.Body.String())
	}
	if got := listing.Files[0]["account_uuid"]; got != "0b1e5a5e-uuid" {
		t.Fatalf("listed account_uuid %#v", got)
	}

	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/download?name="+url.QueryEscape(name), nil)
	h.DownloadAuthFile(ctx)
	// The JSON as the store saved it after the upload (its keys sorted, "disabled" added).
	var downloaded map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &downloaded) != nil ||
		downloaded["access_token"] != "sk-ant-oat01-not-real" || downloaded["email"] != "ada@example.com" {
		t.Fatalf("download: %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"`+name+`","disabled":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchAuthFileStatus(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	onDisk, _ := os.ReadFile(path)
	plaintext, err := authseal.ReadFile(path)
	if !authseal.IsSealed(onDisk) || err != nil {
		t.Fatalf("the disabled file is not sealed: %v", err)
	}
	var saved map[string]any
	if err = json.Unmarshal(plaintext, &saved); err != nil || saved["disabled"] != true || saved["access_token"] != "sk-ant-oat01-not-real" {
		t.Fatalf("saved after disable: %v %#v", err, saved)
	}
	if left, _ := filepath.Glob(filepath.Join(authDir, "*.tmp")); len(left) > 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}
